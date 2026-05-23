package clewservice

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/cleat-team/cleat/internal/auth"
)

// handleHealth returns a simple health check.
func (p *Plugin) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleAuthCheck verifies the request has a valid API key.
func (p *Plugin) handleAuthCheck(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := auth.TenantIDFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": false,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"tenant":        tenantID.String(),
	})
}

// handleProjectsGet lists all projects from the filesystem.
func (p *Plugin) handleProjectsGet(w http.ResponseWriter, r *http.Request) {
	var projects []ProjectInfo

	// Enumerate projects/ directory.
	projDir, err := p.safePath("projects")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "resolve projects path: "+err.Error())
		return
	}

	entries, err := os.ReadDir(projDir)
	if err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, "read projects dir: "+err.Error())
		return
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// Count tasks.
		count := 0
		tasksPath := filepath.Join(projDir, name, "tasks.json")
		if data, err := os.ReadFile(tasksPath); err == nil {
			var tasks TasksJSON
			if json.Unmarshal(data, &tasks) == nil {
				count = len(tasks.Tasks)
			}
		}
		projects = append(projects, ProjectInfo{Name: name, TasksCount: count})
	}

	// Also report the default "clew" project.
	clewCount := 0
	tasksPath, err := p.safePath(filepath.Join("task_state", "tasks.json"))
	if err == nil {
		if data, err := os.ReadFile(tasksPath); err == nil {
			var tasks TasksJSON
			if json.Unmarshal(data, &tasks) == nil {
				clewCount = len(tasks.Tasks)
			}
		}
	}
	projects = append(projects, ProjectInfo{Name: "clew", TasksCount: clewCount})

	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

// handleProjectsPost creates a new project directory with tasks.json skeleton.
func (p *Plugin) handleProjectsPost(w http.ResponseWriter, r *http.Request) {
	var req CreateProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	// Validate project name (alphanumeric, hyphens, underscores).
	for _, c := range req.Name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_') {
			writeError(w, http.StatusBadRequest, "invalid project name: "+req.Name)
			return
		}
	}

	projDir, err := p.safePath(filepath.Join("projects", req.Name))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Check if project already exists.
	if _, err := os.Stat(projDir); err == nil {
		writeError(w, http.StatusConflict, "project already exists: "+req.Name)
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if err := os.MkdirAll(projDir, 0755); err != nil {
		p.logger.Error("create project dir", "project", req.Name, "error", err)
		writeError(w, http.StatusInternalServerError, "create project directory: "+err.Error())
		return
	}

	// Create skeleton tasks.json.
	tasks := TasksJSON{
		Version: "1",
		Updated: Timestamp(),
		Tasks:   map[string]TaskEntry{},
	}

	tasksPath := filepath.Join(projDir, "tasks.json")
	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "marshal tasks.json: "+err.Error())
		return
	}
	data = append(data, '\n')

	if err := os.WriteFile(tasksPath, data, 0644); err != nil {
		p.logger.Error("write tasks.json", "project", req.Name, "error", err)
		writeError(w, http.StatusInternalServerError, "write tasks.json: "+err.Error())
		return
	}

	p.logger.Info("project created", "name", req.Name)
	writeJSON(w, http.StatusCreated, ProjectInfo{Name: req.Name, TasksCount: 0})
}

// handleTasksGet returns tasks.json content, optionally filtered by ?since=<RFC3339>.
func (p *Plugin) handleTasksGet(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	tasks, err := p.readTasksJSON(project)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "tasks.json not found")
			return
		}
		p.logger.Error("read tasks.json", "project", project, "error", err)
		writeError(w, http.StatusInternalServerError, "read tasks.json: "+err.Error())
		return
	}

	// Polling support: return full response if no ?since= parameter.
	// If ?since=<timestamp>, filter tasks updated after that timestamp.
	since := r.URL.Query().Get("since")
	if since != "" {
		filtered := map[string]TaskEntry{}
		for id, t := range tasks.Tasks {
			if t.Updated > since {
				filtered[id] = t
			}
		}
		tasks.Tasks = filtered
	}

	writeJSON(w, http.StatusOK, tasks)
}

// handleTasksPost creates a new task directory with TASK.md and STATUS.md.
func (p *Plugin) handleTasksPost(w http.ResponseWriter, r *http.Request) {
	var req CreateTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if req.Subject == "" {
		writeError(w, http.StatusBadRequest, "subject is required")
		return
	}

	// Validate task ID format (e.g., "clew-140").
	if !strings.Contains(req.ID, "-") {
		writeError(w, http.StatusBadRequest, "invalid task ID format: "+req.ID)
		return
	}

	project := r.URL.Query().Get("project")

	p.mu.Lock()
	defer p.mu.Unlock()

	// Read tasks.json.
	tasks, err := p.readTasksJSON(project)
	if err != nil {
		p.logger.Error("read tasks.json", "project", project, "error", err)
		writeError(w, http.StatusInternalServerError, "read tasks.json: "+err.Error())
		return
	}

	// Check task doesn't already exist.
	if _, exists := tasks.Tasks[req.ID]; exists {
		writeError(w, http.StatusConflict, "task already exists: "+req.ID)
		return
	}

	if req.Priority <= 0 {
		req.Priority = 3
	}

	now := Timestamp()
	entry := TaskEntry{
		ID:       req.ID,
		Subject:  req.Subject,
		Status:   "queued",
		Priority: req.Priority,
		Children: []string{},
		DependsOn: []string{},
		Cost: TaskCost{
			BudgetUSD: 0,
			SpentUSD:  0,
		},
		Created: now,
		Updated: now,
	}

	if req.Parent != "" {
		entry.Parent = &req.Parent
		// Add to parent's children list.
		if parent, ok := tasks.Tasks[req.Parent]; ok {
			parent.Children = append(parent.Children, req.ID)
			tasks.Tasks[req.Parent] = parent
		}
	}

	tasks.Tasks[req.ID] = entry
	tasks.Updated = now

	// Create task directory.
	taskDir, err := p.taskDir(req.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		p.logger.Error("create task dir", "task", req.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "create task directory: "+err.Error())
		return
	}

	// Write TASK.md.
	taskMD := []byte("# " + req.ID + " — " + req.Subject + "\n\n**Status:** queued\n**Created:** " + now + "\n")
	if err := atomicWrite(filepath.Join(taskDir, "TASK.md"), taskMD, 0644); err != nil {
		p.logger.Error("write TASK.md", "task", req.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "write TASK.md: "+err.Error())
		return
	}

	// Write STATUS.md.
	statusMD := []byte("# Status — " + req.ID + "\n\n**Phase:** queued\n**Created:** " + now + "\n")
	if err := atomicWrite(filepath.Join(taskDir, "STATUS.md"), statusMD, 0644); err != nil {
		p.logger.Error("write STATUS.md", "task", req.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "write STATUS.md: "+err.Error())
		return
	}

	// Write tasks.json.
	if err := p.writeTasksJSON(project, tasks); err != nil {
		p.logger.Error("write tasks.json", "project", project, "error", err)
		writeError(w, http.StatusInternalServerError, "write tasks.json: "+err.Error())
		return
	}

	p.logger.Info("task created", "id", req.ID, "subject", req.Subject)
	writeJSON(w, http.StatusCreated, entry)
}

// writeJSON sends a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError sends a JSON error response.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorResponse{Error: msg})
}
