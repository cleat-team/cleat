package clewservice

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
)

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

	p.mu.Lock()
	defer p.mu.Unlock()

	// Check if project already exists (inside mutex to prevent TOCTOU).
	if _, err := os.Stat(projDir); err == nil {
		writeError(w, http.StatusConflict, "project already exists: "+req.Name)
		return
	}

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

	if err := atomicWrite(tasksPath, data, 0644); err != nil {
		p.logger.Error("write tasks.json", "project", req.Name, "error", err)
		writeError(w, http.StatusInternalServerError, "write tasks.json: "+err.Error())
		return
	}

	p.logger.Info("project created", "name", req.Name)
	writeJSON(w, http.StatusCreated, ProjectInfo{Name: req.Name, TasksCount: 0})
}
