package plugin

import (
	"context"
	"database/sql"
)

// CallContext carries per-invocation metadata injected by the engine
// before calling plugin host functions.
type CallContext struct {
	TenantID   string `json:"tenant_id"`
	WorkflowID string `json:"workflow_id"`

	// RunID and Step together identify THIS call the same way
	// engine.DurableCallIdempotencyKey identifies a durable call: stable
	// across a crash and a replay of the same logical step, because both
	// come from state the engine already persists (the workflow's own id,
	// and event_history's own step count) rather than from anything a retry
	// recomputes. cleat#2614.
	//
	// USE THEM TO BUILD AN IDEMPOTENCY KEY, NOT AS A GENERAL SEQUENCE
	// NUMBER. `PluginCall` dispatches to a plugin host function BEFORE
	// recording the event (`engine/plugins.go`'s freshPluginCallInternal) --
	// unlike the durable-call path, which is wired for
	// commit-intent-then-dispatch -- so every plugin host function is
	// AtLeastOnce and a crash between the function returning and the event
	// landing replays the call with the SAME RunID and Step. A plugin
	// author who needs "did I already do this" (record_event's hash chain
	// is the motivating case: two correctly-linked rows asserting one event
	// happened twice, from a worker killed after its own commit but before
	// the engine's) derives a key from (WorkflowID, RunID, Step) the way
	// DurableCallIdempotencyKey does, not from hashing the call's input --
	// an input-hash was considered and rejected, because two
	// textually-identical calls at different points in one workflow (a
	// repeated "step completed" event is the ordinary case, not a corner
	// one) would collide, and the second would be silently dropped as
	// already-recorded. That trades a visible weakness for an invisible
	// one, which is worse.
	//
	// RunID is not yet distinct from WorkflowID in this engine (both come
	// from execRunID, which is currently always set to the workflow id --
	// see engine/plugin_call_context.go) but is carried as its own field so
	// a caller building a key does not have to know that, or update its key
	// derivation if that ever changes.
	RunID string `json:"run_id,omitempty"`
	Step  int    `json:"step"`

	// TraceID is this run's W3C trace-id, or empty when the run has none.
	//
	// Present so a plugin making an outbound HTTP call can pass it to
	// SetTraceparent and keep the caller's trace intact. Every plugin that
	// talks to a third party is currently a point where the chain breaks --
	// cleat#1596 counts the sites -- and they cannot fix it without being told
	// which trace they are in, which is what this field is for.
	TraceID string `json:"trace_id,omitempty"`

	DB *sql.DB // tenant-scoped database connection
}

type callContextKeyType struct{}

// WithCallContext injects call context into the context.
func WithCallContext(ctx context.Context, cc *CallContext) context.Context {
	return context.WithValue(ctx, callContextKeyType{}, cc)
}

// CallContextFromContext extracts call context from the context.
// Returns nil if not present.
func CallContextFromContext(ctx context.Context) *CallContext {
	cc, _ := ctx.Value(callContextKeyType{}).(*CallContext)
	return cc
}
