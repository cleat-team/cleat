package auth

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/internal/tenantctx"
)

// auth and engine must read the SAME context key. cleat#1277.
//
// engine cannot import auth -- middleware_test.go in this package imports
// engine, so that direction is a cycle in the test build -- so the key lives
// in internal/tenantctx and auth delegates to it. The failure mode if someone
// "restores" a local key here is silent and total: the middleware would set
// one key, the plugin adapter would read another, every statement would look
// untenanted, and with #1277's policy in place every plugin query would be
// refused. Nothing about that says "wrong key".
//
// A context key is compared by TYPE IDENTITY, so two identical-looking
// struct{} declarations in different packages are different keys. That is not
// visible at a glance, which is why it is asserted here.
func TestTheTenantContextKeyIsSharedWithTheEngineSide(t *testing.T) {
	want := uuid.New()

	// Set the way the HTTP middleware does, read the way the plugin adapter
	// does.
	if got, ok := tenantctx.From(WithTenantID(context.Background(), want)); !ok || got != want {
		t.Errorf("a tenant set through auth.WithTenantID reads back as (%v, %v) "+
			"on the engine side, want (%v, true).\n\n"+
			"The two sides are using different context keys, so the plugin "+
			"adapter cannot see the tenant the middleware set.", got, ok, want)
	}

	// And the reverse, so a future change that moves either accessor is
	// caught from both directions.
	if got, ok := TenantIDFromContext(tenantctx.With(context.Background(), want)); !ok || got != want {
		t.Errorf("a tenant set through tenantctx.With reads back as (%v, %v) "+
			"through auth.TenantIDFromContext, want (%v, true)", got, ok, want)
	}
}

// An empty context reports no tenant rather than the zero UUID. The plugin
// adapter branches on this to decide whether to scope a statement at all, and
// scoping to the zero UUID would match nothing and read as an empty table.
func TestAnEmptyContextReportsNoTenant(t *testing.T) {
	if _, ok := tenantctx.From(context.Background()); ok {
		t.Error("an empty context reports a tenant")
	}
	if _, ok := TenantIDFromContext(context.Background()); ok {
		t.Error("an empty context reports a tenant through auth")
	}
}
