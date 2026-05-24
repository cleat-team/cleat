package clewservice

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"sort"
	"time"
)

// handleAgentPoll returns the highest-priority queued task with satisfied deps.
// GET /api/agent/poll
func (p *Plugin) handleAgentPoll(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")

	tasks, err := p.readTasksJSON(project)
	if err != nil {
		p.logger.Error("read tasks.json", "project", project, "error", err)
		writeError(w, http.StatusInternalServerError, "read tasks.json: "+err.Error())
		return
	}

	var candidates []TaskEntry
	for _, t := range tasks.Tasks {
		if t.Status != "queued" {
			continue
		}
		if !depsSatisfied(tasks.Tasks, t) {
			continue
		}
		// Skip if an active session.json exists (prevents double-dispatch).
		if _, err := p.readSessionJSON(t.ID); err == nil {
			continue
		}
		candidates = append(candidates, t)
	}

	if len(candidates) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Sort by priority ascending (1 highest), then by ID for stability.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority < candidates[j].Priority
		}
		return candidates[i].ID < candidates[j].ID
	})

	c := candidates[0]
	writeJSON(w, http.StatusOK, PollResponse{
		TaskID:   c.ID,
		Priority: c.Priority,
		Subject:  c.Subject,
		TaskPath: "task_state/" + c.ID,
	})
}

// depsSatisfied returns true if all of t.DependsOn have status "done".
func depsSatisfied(all map[string]TaskEntry, t TaskEntry) bool {
	for _, depID := range t.DependsOn {
		dep, ok := all[depID]
		if !ok || dep.Status != "done" {
			return false
		}
	}
	return true
}

// handleAgentHeartbeat updates the heartbeat_at timestamp in session.json.
// POST /api/agent/heartbeat
func (p *Plugin) handleAgentHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.TaskID == "" {
		writeError(w, http.StatusBadRequest, "task_id is required")
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	session, err := p.readSessionJSON(req.TaskID)
	if err != nil {
		writeError(w, http.StatusNotFound, "session not found: "+req.TaskID)
		return
	}

	session.HeartbeatAt = Timestamp()
	if req.AgentID != "" {
		session.AgentID = req.AgentID
	}

	if err := p.writeSessionJSON(req.TaskID, session); err != nil {
		p.logger.Error("write session.json", "task", req.TaskID, "error", err)
		writeError(w, http.StatusInternalServerError, "write session.json: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleTaskDispatchPost launches an agent session via clew-run.sh.
// POST /api/tasks/{id}/dispatch
func (p *Plugin) handleTaskDispatchPost(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")

	var req DispatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if taskID == "" {
		writeError(w, http.StatusBadRequest, "task_id is required")
		return
	}
	if !taskIDPattern.MatchString(taskID) {
		writeError(w, http.StatusBadRequest, "invalid task ID format: "+taskID)
		return
	}

	project := r.URL.Query().Get("project")

	p.mu.Lock()
	defer p.mu.Unlock()

	// Verify task exists.
	tasks, err := p.readTasksJSON(project)
	if err != nil {
		writeError(w, http.StatusBadRequest, "tasks.json not found")
		return
	}
	entry, ok := tasks.Tasks[taskID]
	if !ok {
		writeError(w, http.StatusBadRequest, "task not found: "+taskID)
		return
	}

	// Check dependencies are satisfied.
	if !depsSatisfied(tasks.Tasks, entry) {
		writeError(w, http.StatusBadRequest, "task dependencies not satisfied: "+taskID)
		return
	}

	// Check for existing active session.
	if session, err := p.readSessionJSON(taskID); err == nil {
		if session.Status == "dispatched" || session.Status == "running" {
			writeError(w, http.StatusConflict, "task already has an active session")
			return
		}
	}

	// Build clew-run.sh command.
	args := []string{p.newTaskScript, taskID, "--execute"}
	if req.Role != "" {
		args = append(args, "--role", req.Role)
	}
	if req.Tool != "" {
		args = append(args, "--tool", req.Tool)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if project != "" && project != "clew" {
		args = append(args, "--project", project)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = p.projectRoot
	if err := cmd.Run(); err != nil {
		p.logger.Error("dispatch failed", "task", taskID, "error", err)
		writeError(w, http.StatusInternalServerError, "dispatch failed: "+err.Error())
		return
	}

	// Read session.json for workflow_run_id.
	session, err := p.readSessionJSON(taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session.json not found after dispatch")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"workflow_run_id": session.WorkflowRunID,
	})
}

// handleTaskCancelPost cancels a running agent session.
// POST /api/tasks/{id}/cancel
func (p *Plugin) handleTaskCancelPost(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	if taskID == "" {
		writeError(w, http.StatusBadRequest, "task_id is required")
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	session, err := p.readSessionJSON(taskID)
	if err != nil {
		writeError(w, http.StatusNotFound, "no session found for task: "+taskID)
		return
	}

	// Already terminal — nothing to cancel.
	if session.Status == "completed" || session.Status == "failed" || session.Status == "cancelled" {
		writeError(w, http.StatusNotFound, "session already terminal: "+taskID)
		return
	}

	session.Status = "cancelled"
	session.Ended = Timestamp()

	if err := p.writeSessionJSON(taskID, session); err != nil {
		p.logger.Error("write session.json", "task", taskID, "error", err)
		writeError(w, http.StatusInternalServerError, "write session.json: "+err.Error())
		return
	}

	p.logger.Info("session cancelled", "task", taskID)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
