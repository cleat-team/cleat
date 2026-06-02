package plugin

import (
	"context"
	"database/sql"
)

// CallContext carries per-invocation metadata injected by the engine
// before calling plugin host functions.
type CallContext struct {
	TenantID   string  `json:"tenant_id"`
	WorkflowID string  `json:"workflow_id"`
	DB         *sql.DB // tenant-scoped database connection
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
