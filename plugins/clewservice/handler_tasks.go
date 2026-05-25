package clewservice

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// taskIDPattern matches new-task.sh's ID validation.
// Must match: ^[a-z]+(-[a-z]+)*-[0-9]{3}[a-z]?$
var taskIDPattern = regexp.MustCompile(`^[a-z]+(-[a-z]+)*-[0-9]{3}[a-z]?$`)

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

	// Validate task ID format matching new-task.sh.
	if !taskIDPattern.MatchString(req.ID) {
		writeError(w, http.StatusBadRequest, "invalid task ID format: "+req.ID)
		return
	}

	// Default budget to 10 (matching new-task.sh).
	if req.Budget <= 0 {
		req.Budget = 10
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

	// Check parent exists if specified.
	if req.Parent != "" {
		if _, ok := tasks.Tasks[req.Parent]; !ok {
			writeError(w, http.StatusBadRequest, "parent task not found: "+req.Parent)
			return
		}
	}

	if req.Priority <= 0 {
		req.Priority = 3
	}

	now := TimestampDate()
	entry := TaskEntry{
		ID:       req.ID,
		Subject:  req.Subject,
		Status:   "queued",
		Priority: req.Priority,
		Children: []string{},
		DependsOn: []string{},
		Cost: TaskCost{
			BudgetUSD: float64(req.Budget),
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
	tasks.Updated = Timestamp()

	// Create task directory with logs/ and artifacts/ subdirectories.
	taskDir, err := p.taskDir(req.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.MkdirAll(filepath.Join(taskDir, "logs"), 0755); err != nil {
		p.logger.Error("create logs dir", "task", req.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "create task directory: "+err.Error())
		return
	}
	if err := os.MkdirAll(filepath.Join(taskDir, "artifacts"), 0755); err != nil {
		p.logger.Error("create artifacts dir", "task", req.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "create task directory: "+err.Error())
		return
	}

	// Parent string for TASK.md.
	parentStr := req.Parent
	if parentStr == "" {
		parentStr = "none"
	}

	// Write TASK.md matching new-task.sh format exactly.
	taskMD := []byte(
		"# " + req.ID + " — " + req.Subject + "\n\n" +
			"**Status:** queued\n" +
			"**Priority:** " + fmt.Sprintf("%d", req.Priority) + "\n" +
			"**Parent:** " + parentStr + "\n" +
			"**Depends on:** none\n\n" +
			"## What\n\nTODO\n\n" +
			"## Scope\n\nTODO\n\n" +
			"## Acceptance\n\n- [ ] TODO\n",
	)
	if err := atomicWrite(filepath.Join(taskDir, "TASK.md"), taskMD, 0644); err != nil {
		p.logger.Error("write TASK.md", "task", req.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "write TASK.md: "+err.Error())
		return
	}

	// Write STATUS.md matching new-task.sh format exactly.
	statusMD := buildStatusMD(req.ID, "queued", req.Budget)
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

// handleTaskGet returns full task details including STATUS.md, session.json,
// logs, and artifacts.
func (p *Plugin) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
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

	entry, ok := tasks.Tasks[taskID]
	if !ok {
		writeError(w, http.StatusNotFound, "task not found: "+taskID)
		return
	}

	// Read STATUS.md.
	statusData, err := p.readTaskFile(taskID, "STATUS.md")
	var status TaskStatus
	if err != nil {
		if !os.IsNotExist(err) {
			p.logger.Error("read STATUS.md", "task", taskID, "error", err)
			writeError(w, http.StatusInternalServerError, "read STATUS.md: "+err.Error())
			return
		}
		status.Phase = "queued"
	} else {
		status = parseStatusMD(statusData)
	}

	// Try session.json (optional, don't error if missing).
	var session *SessionJSON
	s, err := p.readSessionJSON(taskID)
	if err == nil {
		session = s
	}

	// List logs (reverse chron, max 20) and artifacts.
	taskDir, err := p.taskDir(taskID)
	logs := []string{}
	artifacts := []string{}
	if err == nil {
		logs, _ = listDir(filepath.Join(taskDir, "logs"), 20, true)
		artifacts, _ = listDir(filepath.Join(taskDir, "artifacts"), 0, false)
	}

	writeJSON(w, http.StatusOK, TaskDetailResponse{
		Task:      entry,
		Status:    status,
		Session:   session,
		Logs:      logs,
		Artifacts: artifacts,
	})
}

// handleTaskContentGet serves a specific file from a task directory.
// GET /api/tasks/{id}/content?file=<filename>
func (p *Plugin) handleTaskContentGet(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	filename := r.URL.Query().Get("file")
	if filename == "" {
		writeError(w, http.StatusBadRequest, "file parameter is required")
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

	// Determine Content-Type from file extension.
	contentType := "application/octet-stream"
	if strings.HasSuffix(filename, ".md") {
		contentType = "text/markdown; charset=utf-8"
	} else if strings.HasSuffix(filename, ".json") {
		contentType = "application/json"
	}

	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// handleTaskResultPost handles phase transition result submission.
func (p *Plugin) handleTaskResultPost(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	project := r.URL.Query().Get("project")

	var req SubmitResultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if !IsValidPhase(req.Phase) {
		writeError(w, http.StatusBadRequest, "invalid phase: "+req.Phase)
		return
	}

	if !IsValidOutcome(req.Outcome) {
		writeError(w, http.StatusBadRequest, "invalid outcome: "+req.Outcome)
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Verify task exists.
	tasks, err := p.readTasksJSON(project)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read tasks.json: "+err.Error())
		return
	}
	entry, ok := tasks.Tasks[taskID]
	if !ok {
		writeError(w, http.StatusNotFound, "task not found: "+taskID)
		return
	}

	// Read current STATUS.md for phase transition validation.
	statusData, err := p.readTaskFile(taskID, "STATUS.md")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read STATUS.md: "+err.Error())
		return
	}
	currentStatus := parseStatusMD(statusData)

	if !CanTransition(currentStatus.Phase, req.Phase) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("invalid transition: %s -> %s", currentStatus.Phase, req.Phase))
		return
	}

	// Compute token cost.
	costDelta := computeTokenCost(req.TokenUsage)

	// Update STATUS.md atomically.
	newStatus := TaskStatus{Phase: req.Phase, PhaseUpdate: Timestamp(), Updated: TimestampDate()}
	patched := patchStatusMD(statusData, newStatus, taskID)
	statusPath, err := p.safePath(filepath.Join("task_state", taskID, "STATUS.md"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := atomicWrite(statusPath, patched, 0644); err != nil {
		writeError(w, http.StatusInternalServerError, "write STATUS.md: "+err.Error())
		return
	}

	// Update tasks.json.
	entry.Status = req.Phase
	entry.Updated = TimestampDate()
	entry.Cost.SpentUSD += costDelta
	tasks.Tasks[taskID] = entry
	tasks.Updated = Timestamp()
	if err := p.writeTasksJSON(project, tasks); err != nil {
		writeError(w, http.StatusInternalServerError, "write tasks.json: "+err.Error())
		return
	}

	// Update session.json if it exists.
	if session, err := p.readSessionJSON(taskID); err == nil {
		session.Phase = req.Phase
		session.ExitCode = outcomeToExitCode(req.Outcome)
		if req.TokenUsage.Input+req.TokenUsage.Output > 0 {
			total := req.TokenUsage.Input + req.TokenUsage.Output + req.TokenUsage.CacheRead
			session.TokenUsage = &total
		}
		if err := p.writeSessionJSON(taskID, session); err != nil {
			p.logger.Warn("write session.json", "task", taskID, "error", err)
		}
	}

	// Write TASK.md if Content is provided.
	if req.Content != "" {
		taskMDPath, err := p.safePath(filepath.Join("task_state", taskID, "TASK.md"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := atomicWrite(taskMDPath, []byte(req.Content), 0644); err != nil {
			writeError(w, http.StatusInternalServerError, "write TASK.md: "+err.Error())
			return
		}
	}

	p.logger.Info("result submitted", "task", taskID, "phase", req.Phase)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"phase": req.Phase,
	})
}

// outcomeToExitCode converts a text outcome to an exit code.
func outcomeToExitCode(outcome string) *int {
	if outcome == "" {
		return nil
	}
	c := 0
	switch outcome {
	case "pass", "success", "done":
	case "should_fix":
		c = 1
	case "fail", "failed":
		c = 2
	default:
		return nil
	}
	return &c
}

// ── clew-133c stubs ──

func (p *Plugin) handleTaskLogsGet(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	dir, err := p.taskDir(taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logs, err := listDir(filepath.Join(dir, "logs"), 0, true)
	if err != nil {
		p.logger.Error("list logs", "task", taskID, "error", err)
		writeError(w, http.StatusInternalServerError, "list logs: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs})
}

func (p *Plugin) handleTaskArtifactsGet(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	dir, err := p.taskDir(taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	artifacts, err := listDir(filepath.Join(dir, "artifacts"), 0, false)
	if err != nil {
		p.logger.Error("list artifacts", "task", taskID, "error", err)
		writeError(w, http.StatusInternalServerError, "list artifacts: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": artifacts})
}
