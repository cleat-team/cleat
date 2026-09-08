package engine

import (
	"encoding/json"
	"time"
)

type WorkflowDef struct {
	Name       string            `json:"name"`
	Version    int               `json:"version"`
	WASMBytes  []byte            `json:"wasm_bytes,omitempty"`
	ABIVersion int               `json:"abi_version"`
	MinVersion int               `json:"min_version"`
	PluginDeps map[string]string `json:"plugin_deps,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	Deprecated bool              `json:"deprecated"`

	// MaxHistoryLength caps this definition's event history before compaction,
	// overriding the global threshold. 0 means "use the global", which is the
	// column default, so a deploy that does not set it changes nothing.
	//
	// It lives on the deploy payload rather than behind an admin endpoint
	// because the column is keyed (tenant_id, name, version): the value is
	// already a per-VERSION property, so setting it out of band would create a
	// value whose lifecycle does not match its key -- roll the code back and an
	// endpoint-set cap would stay, silently mis-tuning the version it was not
	// chosen for. See cleat#889.
	MaxHistoryLength int `json:"max_history_length,omitempty"`
}

// WorkflowInstance is a row from workflow_instances.
type WorkflowInstance struct {
	ID         string          `json:"id"`
	DefName    string          `json:"def_name"`
	DefVersion int             `json:"def_version"`
	MinVersion int             `json:"min_version"`
	Status     string          `json:"status"`
	Input      json.RawMessage `json:"input"`
	Result     string          `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	ErrorCode  string          `json:"error_code,omitempty"`
	ErrorOp    string          `json:"error_op,omitempty"`
	AssignedTo string          `json:"assigned_to"`
	NextWakeAt time.Time       `json:"next_wake_at"`
	TenantID   string          `json:"tenant_id,omitempty"`
	CreatedAt  time.Time       `json:"created_at,omitempty"`
	Generation int64           `json:"generation"`
	Priority   int             `json:"priority"`
	TraceID    string          `json:"trace_id,omitempty"`

	// PendingTerminalStatus is the outcome a two-phase terminal transition
	// has already decided and has not yet applied: "" for the overwhelming
	// majority of rows, and the status to finalize with once this claim's
	// defer phase completes. See engine/defer_phase.go.
	//
	// It is what tells the dispatch loop that the claim it just took is a
	// defer segment rather than ordinary work, which is why it is carried on
	// the claim rather than read back afterwards. Status cannot carry it:
	// every claim sets status = 'running' and returns the new value.
	PendingTerminalStatus string `json:"pending_terminal_status,omitempty"`

	// SignalSeq is the workflow's delivery counter AS OF THIS CLAIM, and it
	// exists to close a window that next_wake_at cannot see.
	//
	// DeliverSignal pulls next_wake_at forward only for a workflow that is
	// already suspended -- a claimed one is status 'running', and its row is
	// about to be overwritten by finalize anyway. So a signal arriving while
	// the workflow is AWAKE scheduled nothing, the workflow re-suspended with
	// its own timeout deadline, and the delivery sat in workflow_signals until
	// that timeout expired: AwaitSignals reported a timeout with the signal it
	// was waiting for already in the table (cleat#953).
	//
	// The worker carries this value from claim to finalize, which compares it
	// against the stored one inside the same transaction. Different means a
	// delivery landed mid-segment, and the workflow wakes now rather than at
	// its deadline. A COUNTER rather than "are there rows in workflow_signals"
	// because the latter spins: a workflow awaiting {a} with an unrelated {z}
	// pending would wake, poll, find nothing it wants and re-suspend, forever.
	SignalSeq int64 `json:"signal_seq,omitempty"`

	// ContinuedFrom is the run that continued into this one -- the id of the
	// predecessor in a ContinueAsNew chain, or "" for the overwhelming
	// majority of rows, which are not continuations. cleat#826, cleat#887.
	//
	// POPULATED ON THE READ PATH ONLY: GetWorkflowByID sets it, and nothing
	// else does. A claim does not, because the claim queries share one scanner
	// (Dialect.scanWorkflowInstanceExtra) with one column list, and the
	// dispatch loop has no use for the link -- adding it there would widen the
	// hottest query in the system to carry a field nobody reads.
	//
	// So "" means either "not a continuation" OR "you did not get this from
	// GetWorkflowByID". Stated rather than left to be discovered, because a
	// field that is populated on one path and empty on another is exactly the
	// shape that reads as data.
	ContinuedFrom string `json:"continued_from,omitempty"`
}

// Schedule is a row from workflow_schedules.
type Schedule struct {
	Name           string          `json:"name"`
	DefName        string          `json:"def_name"`
	EntryPoint     string          `json:"entry_point"`
	CronExpression string          `json:"cron_expression"`
	Input          json.RawMessage `json:"input"`
	Enabled        bool            `json:"enabled"`
	NextRunAt      time.Time       `json:"next_run_at"`
	LastRunAt      *time.Time      `json:"last_run_at,omitempty"`

	// Timezone is the IANA zone the cron expression's wall-clock fields are
	// evaluated in (see engine.NextCronTimeIn). Empty means
	// DefaultScheduleTimezone; the stores write 'UTC' rather than '' so the
	// column is never ambiguous about whether a zone was chosen.
	Timezone string `json:"timezone"`

	// MisfirePolicy decides what a firing missed during an outage means:
	// "catch_up" (default) delivers the backlog one instant per tick up to
	// CatchUpLimit, "skip" resumes at the next future instant. Empty means
	// the default.
	MisfirePolicy string `json:"misfire_policy"`

	// CatchUpLimit bounds how many owed firings catch_up will work through
	// before giving up and resuming in the future. Zero means the default;
	// see engine.DefaultCatchUpLimit.
	CatchUpLimit int `json:"catch_up_limit"`

	// OverlapPolicy decides what happens when an instant arrives and the run
	// this schedule started last has not finished: "allow" (default, and what
	// the scheduler has always done) or "skip". Empty means the default.
	OverlapPolicy string `json:"overlap_policy"`

	// LastRunID is the run this schedule started most recently. It is what
	// makes OverlapPolicy "skip" answerable at all -- without it there is no
	// way to tell a run this schedule started from any other run of the same
	// definition.
	LastRunID string `json:"last_run_id,omitempty"`

	// TenantID owns this schedule, and is the tenant the runs it starts belong
	// to.
	//
	// Populated on read. It is NOT read from the caller on CreateSchedule --
	// the stores write their own s.tenantID there, so a caller cannot create a
	// schedule for a tenant it is not scoped to.
	TenantID string `json:"tenant_id"`
}

// PromiseInfo holds the state of a cleat promise.
type PromiseInfo struct {
	PromiseID   string     `json:"promise_id"`
	PromiseName string     `json:"promise_name"`
	Status      string     `json:"status"`
	Result      string     `json:"result,omitempty"`
	ErrorMsg    string     `json:"error_msg,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
}

// ConcurrencyKeyInfo holds the state of an acquired concurrency key.
type ConcurrencyKeyInfo struct {
	KeyHash    []byte    `json:"key_hash"`
	KeyText    string    `json:"key_text"`
	WorkflowID string    `json:"workflow_id"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// UpdateRequestInfo holds the state of an incoming update request.
type UpdateRequestInfo struct {
	WorkflowID string    `json:"workflow_id"`
	UpdateName string    `json:"update_name"`
	Payload    string    `json:"payload"`
	PromiseID  string    `json:"promise_id,omitempty"`
	Status     string    `json:"status"`
	Result     string    `json:"result,omitempty"`
	ErrorMsg   string    `json:"error_msg,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// WorkflowMemoryStats holds distribution statistics for per-definition memory usage.
type WorkflowMemoryStats struct {
	DefName     string  `json:"def_name"`
	MinBytes    int64   `json:"min_bytes"`
	AvgBytes    float64 `json:"avg_bytes"`
	MaxBytes    int64   `json:"max_bytes"`
	P10         int64   `json:"p10"`
	P25         int64   `json:"p25"`
	P50         int64   `json:"p50"`
	P75         int64   `json:"p75"`
	P90         int64   `json:"p90"`
	P99         int64   `json:"p99"`
	SampleCount int     `json:"sample_count"`
}

// WorkflowFilter contains optional filter parameters for listing workflow instances.
// Empty/zero values mean "no filter" for that parameter.
type WorkflowFilter struct {
	Status        string
	InputContains string
	ErrorContains string
	Search        string
	Offset        int
	Limit         int
}

// RoutingRule represents a traffic-splitting rule for A/B testing.
type RoutingRule struct {
	ID            string
	WorkflowName  string
	TargetVersion int
	Weight        float64
}

// WorkflowStore is the database interface for the worker.
