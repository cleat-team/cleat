package clewservice

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// handleTaskGet returns full task detail: tasks.json entry + STATUS.md + session.json.
func (p *Plugin) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	project := r.URL.Query().Get("project")

	// Read from tasks.json.
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

	entry, ok := tasks.Tasks[taskID]
	if !ok {
		writeError(w, http.StatusNotFound, "task not found: "+taskID)
		return
	}

	// Read STATUS.md.
	statusData, err := p.readTaskFile(taskID, "STATUS.md")
	status := TaskStatus{Phase: "queued"}
	if err == nil {
		status = parseStatusMD(statusData)
	}

	// Read session.json (optional).
	var session *SessionJSON
	sessionData, err := p.readTaskFile(taskID, "session.json")
	if err == nil {
		var s SessionJSON
		if json.Unmarshal(sessionData, &s) == nil {
			session = &s
		}
	}

	writeJSON(w, http.StatusOK, TaskDetailResponse{
		Task:    entry,
		Status:  status,
		Session: session,
	})
}

// handleTaskContentGet returns raw file content from a task directory.
func (p *Plugin) handleTaskContentGet(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	filename := r.URL.Query().Get("file")
	if filename == "" {
		writeError(w, http.StatusBadRequest, "file query parameter is required (e.g., ?file=TASK.md)")
		return
	}

	// Validate filename: only allow specific files and subdirectories.
	validPrefixes := []string{
		"TASK.md", "STATUS.md", "session.json",
		"logs/", "artifacts/",
	}
	valid := false
	for _, prefix := range validPrefixes {
		if filename == prefix || strings.HasPrefix(filename, prefix) {
			valid = true
			break
		}
	}
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid file: "+filename)
		return
	}

	data, err := p.readTaskFile(taskID, filename)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "file not found: "+filename)
			return
		}
		p.logger.Error("read task file", "task", taskID, "file", filename, "error", err)
		writeError(w, http.StatusInternalServerError, "read file: "+err.Error())
		return
	}

	// Content type based on file extension.
	contentType := "text/plain; charset=utf-8"
	if strings.HasSuffix(filename, ".md") {
		contentType = "text/markdown; charset=utf-8"
	} else if strings.HasSuffix(filename, ".json") {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// handleTaskResultPost accepts agent result and updates STATUS.md + tasks.json.
func (p *Plugin) handleTaskResultPost(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")

	var req SubmitResultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Phase == "" {
		writeError(w, http.StatusBadRequest, "phase is required")
		return
	}
	if !IsValidPhase(req.Phase) {
		writeError(w, http.StatusBadRequest, "invalid phase: "+req.Phase)
		return
	}

	project := r.URL.Query().Get("project")

	p.mu.Lock()
	defer p.mu.Unlock()

	// Read current state.
	tasks, err := p.readTasksJSON(project)
	if err != nil {
		p.logger.Error("read tasks.json for result", "task", taskID, "error", err)
		writeError(w, http.StatusInternalServerError, "read tasks.json: "+err.Error())
		return
	}

	entry, ok := tasks.Tasks[taskID]
	if !ok {
		writeError(w, http.StatusNotFound, "task not found: "+taskID)
		return
	}

	// Validate phase transition.
	if !CanTransition(entry.Status, req.Phase) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("invalid phase transition: %s → %s", entry.Status, req.Phase))
		return
	}

	// Update task entry.
	now := Timestamp()
	entry.Status = req.Phase
	entry.Updated = now
	entry.Cost.SpentUSD += float64(req.TokenUsage.Input+req.TokenUsage.Output) * 0.000015 // ~$15/MTok estimate
	tasks.Tasks[taskID] = entry
	tasks.Updated = now

	// Write STATUS.md.
	statusNotes := taskID
	if req.Notes != "" {
		statusNotes = req.Notes
	}
	status := TaskStatus{
		Phase:       req.Phase,
		PhaseUpdate: now,
		Notes:       statusNotes,
	}
	statusMD := buildStatusMD(status)
	if err := p.writeTaskFile(taskID, "STATUS.md", statusMD, 0644); err != nil {
		p.logger.Error("write STATUS.md", "task", taskID, "error", err)
		writeError(w, http.StatusInternalServerError, "write STATUS.md: "+err.Error())
		return
	}

	// Write tasks.json.
	if err := p.writeTasksJSON(project, tasks); err != nil {
		p.logger.Error("write tasks.json", "project", project, "error", err)
		writeError(w, http.StatusInternalServerError, "write tasks.json: "+err.Error())
		return
	}

	p.logger.Info("result submitted", "task", taskID, "phase", req.Phase)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleTaskDispatchPost launches an agent via clew-run.sh subprocess.
func (p *Plugin) handleTaskDispatchPost(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")

	var req DispatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Role == "" {
		writeError(w, http.StatusBadRequest, "role is required")
		return
	}
	if req.Tool == "" {
		req.Tool = "claude"
	}

	project := r.URL.Query().Get("project")

	p.mu.Lock()
	defer p.mu.Unlock()

	// Verify task exists and is queued.
	tasks, err := p.readTasksJSON(project)
	if err != nil {
		p.logger.Error("read tasks.json for dispatch", "task", taskID, "error", err)
		writeError(w, http.StatusInternalServerError, "read tasks.json: "+err.Error())
		return
	}

	entry, ok := tasks.Tasks[taskID]
	if !ok {
		writeError(w, http.StatusNotFound, "task not found: "+taskID)
		return
	}

	if entry.Status != "queued" {
		writeError(w, http.StatusBadRequest, "task is not queued: status is "+entry.Status)
		return
	}

	// Get task path for the subprocess.
	taskDir, err := p.taskDir(taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := os.Stat(taskDir); os.IsNotExist(err) {
		writeError(w, http.StatusNotFound, "task directory not found: "+taskID)
		return
	}

	// Write session.json before launching.
	now := Timestamp()
	session := SessionJSON{
		TaskID:  taskID,
		Role:    req.Role,
		Tool:    req.Tool,
		Model:   req.Model,
		Started: now,
		Phase:   entry.Status,
		Status:  "dispatched",
	}
	if err := p.writeSessionJSON(taskID, &session); err != nil {
		p.logger.Error("write session.json for dispatch", "task", taskID, "error", err)
		writeError(w, http.StatusInternalServerError, "write session.json: "+err.Error())
		return
	}

	// Update task status in tasks.json.
	entry.Status = "dispatched"
	entry.Updated = now
	tasks.Tasks[taskID] = entry
	tasks.Updated = now
	if err := p.writeTasksJSON(project, tasks); err != nil {
		p.logger.Error("write tasks.json for dispatch", "task", taskID, "error", err)
		writeError(w, http.StatusInternalServerError, "write tasks.json: "+err.Error())
		return
	}

	// Shell out to clew-run.sh asynchronously.
	script := p.newTaskScript
	if script == "" {
		script = filepath.Join(p.projectRoot, "src", "clew-run.sh")
	}

	args := []string{taskID, "--role", req.Role, "--tool", req.Tool, "--execute"}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if project != "" && project != "clew" {
		args = append(args, "--project", project)
	}

	cmd := exec.Command(script, args...)
	cmd.Dir = p.projectRoot
	if err := cmd.Start(); err != nil {
		p.logger.Error("launch clew-run.sh", "task", taskID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to launch agent: "+err.Error())
		return
	}

	// Fire and forget — the agent runs asynchronously.
	go func() {
		if err := cmd.Wait(); err != nil {
			p.logger.Error("clew-run.sh exited with error", "task", taskID, "error", err)
		}
	}()

	p.logger.Info("agent dispatched", "task", taskID, "role", req.Role)
	writeJSON(w, http.StatusCreated, map[string]any{
		"task_id":          taskID,
		"status":           "dispatched",
		"workflow_run_id":  session.WorkflowRunID,
	})
}

// handleTaskCancelPost cancels a running session for a task.
func (p *Plugin) handleTaskCancelPost(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")

	p.mu.Lock()
	defer p.mu.Unlock()

	// Read and update session.json.
	session, err := p.readSessionJSON(taskID)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "no active session for task: "+taskID)
			return
		}
		p.logger.Error("read session.json for cancel", "task", taskID, "error", err)
		writeError(w, http.StatusInternalServerError, "read session.json: "+err.Error())
		return
	}

	now := Timestamp()
	session.Status = "cancelled"
	session.Ended = now

	if err := p.writeSessionJSON(taskID, session); err != nil {
		p.logger.Error("write session.json for cancel", "task", taskID, "error", err)
		writeError(w, http.StatusInternalServerError, "write session.json: "+err.Error())
		return
	}

	p.logger.Info("session cancelled", "task", taskID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// handleAgentPoll returns the next queued task by priority.
func (p *Plugin) handleAgentPoll(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	tasks, err := p.readTasksJSON(project)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNoContent, nil)
			return
		}
		p.logger.Error("read tasks.json for poll", "project", project, "error", err)
		writeError(w, http.StatusInternalServerError, "read tasks.json: "+err.Error())
		return
	}

	// Collect queued tasks with satisfied dependencies.
	type candidate struct {
		task     TaskEntry
		taskPath string
	}
	var candidates []candidate
	taskIDs := make(map[string]bool)

	for id, t := range tasks.Tasks {
		if t.Status != "queued" {
			continue
		}
		// Check depends_on are all satisfied.
		depsSatisfied := true
		for _, depID := range t.DependsOn {
			if dep, ok := tasks.Tasks[depID]; !ok || dep.Status != "done" {
				depsSatisfied = false
				break
			}
		}
		if !depsSatisfied {
			continue
		}
		taskIDs[id] = true
		candidates = append(candidates, candidate{task: t, taskPath: "task_state/" + id})
	}

	if len(candidates) == 0 {
		writeJSON(w, http.StatusNoContent, nil)
		return
	}

	// Sort by priority (lowest number = highest priority).
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].task.Priority < candidates[j].task.Priority
	})

	c := candidates[0]
	writeJSON(w, http.StatusOK, PollResponse{
		TaskID:   c.task.ID,
		Priority: c.task.Priority,
		Subject:  c.task.Subject,
		TaskPath: c.taskPath,
	})
}

// handleAgentHeartbeat updates the heartbeat timestamp for an agent.
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
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "no session for task: "+req.TaskID)
			return
		}
		p.logger.Error("read session.json for heartbeat", "task", req.TaskID, "error", err)
		writeError(w, http.StatusInternalServerError, "read session.json: "+err.Error())
		return
	}

	session.HeartbeatAt = Timestamp()
	if req.AgentID != "" {
		session.AgentID = req.AgentID
	}

	if err := p.writeSessionJSON(req.TaskID, session); err != nil {
		p.logger.Error("write session.json for heartbeat", "task", req.TaskID, "error", err)
		writeError(w, http.StatusInternalServerError, "write session.json: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleTaskLogsGet lists log files in a task directory.
func (p *Plugin) handleTaskLogsGet(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	logs, err := p.listDirFiles(taskID, "logs")
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, map[string]any{"logs": []string{}})
			return
		}
		p.logger.Error("list logs", "task", taskID, "error", err)
		writeError(w, http.StatusInternalServerError, "list log files: "+err.Error())
		return
	}
	if logs == nil {
		logs = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs})
}

// handleTaskArtifactsGet lists artifact files in a task directory.
func (p *Plugin) handleTaskArtifactsGet(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	artifacts, err := p.listDirFiles(taskID, "artifacts")
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, map[string]any{"artifacts": []string{}})
			return
		}
		p.logger.Error("list artifacts", "task", taskID, "error", err)
		writeError(w, http.StatusInternalServerError, "list artifact files: "+err.Error())
		return
	}
	if artifacts == nil {
		artifacts = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": artifacts})
}
