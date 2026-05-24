package clewservice

import "time"

// --- Project types ---

// ProjectInfo describes a project directory.
type ProjectInfo struct {
	Name       string `json:"name"`
	TasksCount int    `json:"tasks_count"`
}

// CreateProjectRequest is the request body for POST /api/projects.
type CreateProjectRequest struct {
	Name string `json:"name"`
}

// --- Task types ---

// TaskEntry is a single task in tasks.json.
type TaskEntry struct {
	ID            string   `json:"id"`
	Subject       string   `json:"subject"`
	Description   string   `json:"description,omitempty"`
	Status        string   `json:"status"`
	Priority      int      `json:"priority"`
	Parent        *string  `json:"parent"`
	Children      []string `json:"children"`
	DependsOn     []string `json:"depends_on"`
	Contract      *string  `json:"contract"`
	AssignedAgent *string  `json:"assigned_agent"`
	Cost          TaskCost `json:"cost"`
	Created       string   `json:"created"`
	Updated       string   `json:"updated"`
}

// TaskCost tracks budget and spending.
type TaskCost struct {
	BudgetUSD float64 `json:"budget_usd"`
	SpentUSD  float64 `json:"spent_usd"`
}

// TasksJSON is the root structure of tasks.json.
type TasksJSON struct {
	Version string               `json:"version"`
	Updated string               `json:"updated"`
	Tasks   map[string]TaskEntry `json:"tasks"`
}

// CreateTaskRequest is the request body for POST /api/tasks.
type CreateTaskRequest struct {
	ID       string `json:"id"`
	Subject  string `json:"subject"`
	Parent   string `json:"parent,omitempty"`
	Priority int    `json:"priority"`
	Budget   int    `json:"budget,omitempty"`
}

// --- Status types ---

// TaskStatus holds parsed STATUS.md information.
type TaskStatus struct {
	Phase       string `json:"phase"`
	PhaseUpdate string `json:"phase_updated,omitempty"`
	Notes       string `json:"notes,omitempty"`
	Updated     string `json:"updated,omitempty"`
}

// --- Session types ---

// SessionJSON is the session.json dispatch state.
type SessionJSON struct {
	TaskID        string `json:"task_id"`
	Role          string `json:"role"`
	Tool          string `json:"tool"`
	Model         string `json:"model,omitempty"`
	Started       string `json:"started,omitempty"`
	Ended         string `json:"ended,omitempty"`
	Phase         string `json:"phase,omitempty"`
	Status        string `json:"status"`
	WorkflowRunID string `json:"workflow_run_id,omitempty"`
	ExitCode      *int   `json:"exit_code,omitempty"`
	TokenUsage    *int   `json:"token_usage,omitempty"`
	HeartbeatAt   string `json:"heartbeat_at,omitempty"`
	AgentID       string `json:"agent_id,omitempty"`
}

// --- Result types ---

// TokenUsage records token consumption from agent runs.
type TokenUsage struct {
	Input     int `json:"input"`
	Output    int `json:"output"`
	CacheRead int `json:"cache_read,omitempty"`
}

// SubmitResultRequest is the request body for POST /api/tasks/{id}/result.
type SubmitResultRequest struct {
	Phase            string     `json:"phase"`
	Outcome          string     `json:"outcome"`
	TokenUsage       TokenUsage `json:"token_usage,omitempty"`
	ArtifactsWritten []string   `json:"artifacts_written,omitempty"`
	FindingsCount    int        `json:"findings_count,omitempty"`
	Notes            string     `json:"notes,omitempty"`
	Content          string     `json:"content,omitempty"`
}

// DispatchRequest is the request body for POST /api/tasks/{id}/dispatch.
type DispatchRequest struct {
	Role  string `json:"role"`
	Tool  string `json:"tool"`
	Model string `json:"model"`
}

// HeartbeatRequest is the request body for POST /api/agent/heartbeat.
type HeartbeatRequest struct {
	TaskID  string `json:"task_id"`
	AgentID string `json:"agent_id"`
}

// CancelRequest is the request body for POST /api/tasks/{id}/cancel.
type CancelRequest struct {
	TaskID string `json:"task_id"`
}

// --- Dashboard types ---

// DashboardSummary is the response from GET /api/dashboard/summary.
type DashboardSummary struct {
	TotalTasks     int             `json:"total_tasks"`
	TasksByStatus  map[string]int  `json:"tasks_by_status"`
	TotalSpentUSD  float64         `json:"total_spent_usd"`
	TotalBudgetUSD float64         `json:"total_budget_usd"`
	RecentActivity []ActivityEntry `json:"recent_activity"`
}

// ActivityEntry is a single activity event for the dashboard feed.
type ActivityEntry struct {
	TaskID    string `json:"task_id"`
	Timestamp string `json:"timestamp"`
	Action    string `json:"action"`
}

// --- Task detail response ---

// TaskDetailResponse is the response from GET /api/tasks/{id}.
type TaskDetailResponse struct {
	Task      TaskEntry    `json:"task"`
	Status    TaskStatus   `json:"status"`
	Session   *SessionJSON `json:"session,omitempty"`
	Logs      []string     `json:"logs"`
	Artifacts []string     `json:"artifacts"`
}

// --- Agent poll types ---

// PollResponse is the response from GET /api/agent/poll.
type PollResponse struct {
	TaskID   string `json:"task_id"`
	Priority int    `json:"priority"`
	Subject  string `json:"subject"`
	TaskPath string `json:"task_path"`
}

// --- Error response ---

// ErrorResponse is the standard error response for all endpoints.
type ErrorResponse struct {
	Error string `json:"error"`
}

// --- Valid phases for the task state machine ---

var ValidPhases = []string{
	"queued", "exploring", "planning", "plan_review",
	"implementing", "impl_review", "done",
	"failed", "blocked", "waiting_on_children",
}

// PhaseOrder maps each phase to its ordinal for transition validation.
var PhaseOrder = map[string]int{
	"queued":              0,
	"exploring":           1,
	"planning":            2,
	"plan_review":         3,
	"implementing":        4,
	"impl_review":         5,
	"done":                6,
	"failed":              -1,
	"blocked":             -1,
	"waiting_on_children": -1,
}

// IsValidPhase returns true if the phase is a recognized state.
func IsValidPhase(phase string) bool {
	_, ok := PhaseOrder[phase]
	return ok
}

// ValidOutcomes is the set of recognized outcome strings.
var ValidOutcomes = map[string]bool{
	"":           true,
	"pass":       true,
	"success":    true,
	"done":       true,
	"should_fix": true,
	"fail":       true,
	"failed":     true,
}

// IsValidOutcome returns true if the outcome is a recognized value.
func IsValidOutcome(outcome string) bool {
	return ValidOutcomes[outcome]
}

// IsTerminalPhase returns true for terminal states (done, failed, blocked, waiting_on_children).
func IsTerminalPhase(phase string) bool {
	return phase == "done" || phase == "failed" || phase == "blocked" || phase == "waiting_on_children"
}

// CanTransition checks if moving from one phase to another is valid.
// Terminal phases (failed, blocked) can transition to anything.
// Forward progress of exactly one step is required in the linear chain.
// Same-phase transitions are allowed (retry support).
func CanTransition(from, to string) bool {
	if !IsValidPhase(to) {
		return false
	}
	fromOrd, fromOK := PhaseOrder[from]
	toOrd, toOK := PhaseOrder[to]
	if !fromOK || !toOK {
		return false
	}
	// Terminal states can transition to any valid phase (restart).
	if fromOrd < 0 {
		return true
	}
	// Target terminal states are always allowed.
	if toOrd < 0 {
		return true
	}
	// Forward progression of exactly one step, or same phase (retry).
	return toOrd == fromOrd || toOrd == fromOrd+1
}

// Timestamp returns the current time as an RFC 3339 string.
func Timestamp() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// TimestampDate returns the current date as YYYY-MM-DD (for task entry fields
// and STATUS.md / TASK.md date fields).
func TimestampDate() string {
	return time.Now().UTC().Format("2006-01-02")
}

// computeTokenCost calculates USD cost from token usage using Claude Sonnet pricing.
func computeTokenCost(u TokenUsage) float64 {
	const (
		inputCostPerMTok     = 3.0
		outputCostPerMTok    = 15.0
		cacheReadCostPerMTok = 1.5
	)
	input := float64(u.Input) * inputCostPerMTok / 1_000_000.0
	output := float64(u.Output) * outputCostPerMTok / 1_000_000.0
	cacheRead := float64(u.CacheRead) * cacheReadCostPerMTok / 1_000_000.0
	return input + output + cacheRead
}
