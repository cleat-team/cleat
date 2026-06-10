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
