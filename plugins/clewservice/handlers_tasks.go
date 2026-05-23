package clewservice

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

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

// ── clew-133c stubs (501 Not Implemented) ──

func (p *Plugin) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not implemented")
}

func (p *Plugin) handleTaskContentGet(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not implemented")
}

func (p *Plugin) handleTaskResultPost(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not implemented")
}

func (p *Plugin) handleTaskDispatchPost(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not implemented")
}

func (p *Plugin) handleTaskCancelPost(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not implemented")
}

func (p *Plugin) handleTaskLogsGet(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not implemented")
}

func (p *Plugin) handleTaskArtifactsGet(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not implemented")
}

func (p *Plugin) handleAgentPoll(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not implemented")
}

func (p *Plugin) handleAgentHeartbeat(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not implemented")
}
