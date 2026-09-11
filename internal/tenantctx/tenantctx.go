// Package tenantctx carries the tenant a request is acting for, in a context.
//
// WHY IT IS ITS OWN PACKAGE. The value is set by HTTP middleware in auth and
// read by the plugin database adapter in engine, and those two cannot depend
// on each other: engine importing auth is a cycle the moment any test in auth
// imports engine, which auth/middleware_test.go does. A context key has to
// live below both.
//
// Identity, not shape, is what matters here. The key is an unexported type, so
// a second declaration of an identical struct elsewhere would be a DIFFERENT
// key that silently reads nothing -- which is the reason auth delegates to
// this package rather than keeping a parallel copy. cleat#1277.
package tenantctx

import (
	"context"

	"github.com/google/uuid"
)

type tenantIDKey struct{}

// With returns a context carrying tenantID.
func With(ctx context.Context, tenantID uuid.UUID) context.Context {
	return context.WithValue(ctx, tenantIDKey{}, tenantID)
}

// From reports the tenant the context is acting for, and whether one is set.
//
// The boolean is load-bearing rather than a convention: "no tenant" is a
// legitimate state on the host-call and background paths, and it is what tells
// the plugin adapter to leave a statement unscoped instead of scoping it to
// the zero UUID -- which would match nothing and look like an empty table.
func From(ctx context.Context) (uuid.UUID, bool) {
	tid, ok := ctx.Value(tenantIDKey{}).(uuid.UUID)
	return tid, ok
}
