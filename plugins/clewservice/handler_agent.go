package clewservice

import (
	"encoding/json"
	"net/http"
	"sort"
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
