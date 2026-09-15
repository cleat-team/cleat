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
