package engine

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/internal/tenantctx"
	"github.com/cleat-team/cleat/plugin"
)

// TestBothPluginCallPathsCarryTheTenant is cleat#1278.
//
// THE POINT IS THE WORD "BOTH". A plugin is invoked from two places in
// engine/plugins.go -- freshPluginCallInternal for a unary call and the
// streaming path for PluginCallStreaming -- and until this change each built
// its call context inline, with eleven identical lines. Adding the tenant
// bridge to one of them would have produced a plugin that is tenant-scoped
// over `h.PluginCall(...)` and unscoped over `h.PluginCallStreaming(...)`,
// which is not a difference any existing test could see: every plugin test in
// this package drives one path or the other, never both.
//
// So this test is not really about the tenant arriving. It is about the two
// paths agreeing, and it is written to fail if a third call site appears that
// does not go through pluginCallContext -- add one, forget the bridge, and
// whichever of these two assertions covers it goes red.
//
// It uses no database on purpose. What is being asserted is that the value
// reaches the context a plugin function is handed; what the database then does
// with it is engine/plugindb_tenant.go's business and is covered separately by
// TestAPluginStatementIsScopedToTheWorkflowsTenant, which pays for a
// connection to prove the other half.
func TestBothPluginCallPathsCarryTheTenant(t *testing.T) {
	const tenant = "3f2b1c00-0000-4000-8000-00000000abcd"
	want := uuid.MustParse(tenant)

	// --- the unary path -------------------------------------------------
	var unarySaw uuid.UUID
	var unaryOK bool
	pr := NewPluginRegistry()
	pr.RegisterWithPolicy("test-plugin", "unary",
		func(ctx context.Context, inputJSON string) (string, error) {
			unarySaw, unaryOK = tenantctx.From(ctx)
			return `{}`, nil
		}, ReplayPolicy{})

	s := newTestExecSession()
	s.engine.pluginRegistry = pr
	s.tenantID = tenant

	buf := make([]byte, 256)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	if res := s.PluginCall(ctx, nil, "test-plugin", "unary", `{}`, 0, 255); byte(res&0xFF) != 0 {
		t.Fatalf("the unary call itself failed (errCode %d), so its context assertion "+
			"would be about a call that did not happen", byte(res&0xFF))
	}

	// --- the streaming path ---------------------------------------------
	var streamSaw uuid.UUID
	var streamOK bool
	var streamRan bool
	psr := NewPluginStreamRegistry()
	if err := psr.Register("test-plugin", "streaming",
		func(ctx context.Context, inputJSON string) (<-chan plugin.StreamEvent, error) {
			streamSaw, streamOK = tenantctx.From(ctx)
			streamRan = true
			ch := make(chan plugin.StreamEvent, 1)
			close(ch)
			return ch, nil
		}); err != nil {
		t.Fatalf("registering the streaming function: %v", err)
	}

	s2 := newTestExecSession()
	s2.engine.pluginStreamRegistry = psr
	s2.tenantID = tenant
	buf2 := make([]byte, 256)
	ctx2 := contextWithRawMemBuf(context.Background(), buf2)
	s2.PluginCallStreaming(ctx2, nil, "test-plugin", "streaming", `{}`, 0, 255)

	// The streaming result code is not asserted -- a zero-chunk stream is an
	// edge the streaming path has its own opinions about, and this test has no
	// business relitigating them. What IS asserted is that the function ran,
	// because a context assertion against a function that was never invoked is
	// the emptiest green there is.
	if !streamRan {
		t.Fatal("the streaming plugin function was never invoked, so nothing below " +
			"is a measurement of what its context carried")
	}

	// --- both, and say which ---------------------------------------------
	for _, c := range []struct {
		path string
		ok   bool
		got  uuid.UUID
	}{
		{"PluginCall", unaryOK, unarySaw},
		{"PluginCallStreaming", streamOK, streamSaw},
	} {
		if !c.ok {
			t.Errorf("%s handed the plugin a context with NO tenant. "+
				"engine/plugindb_tenant.go's beginTenantTx reads tenantctx and nothing "+
				"else, so a plugin statement on this path cannot satisfy any row-level "+
				"policy -- it fails with \"cleat.tenant_id is not set\" as soon as the "+
				"table is non-empty.", c.path)
			continue
		}
		if c.got != want {
			t.Errorf("%s carried tenant %s, want %s", c.path, c.got, want)
		}
	}
}

// TestAnUnparseableTenantDoesNotFailThePluginCall is the other half of
// pluginCallContext's decision, and it is here because "warn and continue" is
// the kind of choice that silently becomes "fail" under a later edit.
//
// The call must still succeed, and it must not carry a tenant -- claiming one
// derived from a value that would not parse is worse than carrying none.
func TestAnUnparseableTenantDoesNotFailThePluginCall(t *testing.T) {
	var sawTenant bool
	pr := NewPluginRegistry()
	pr.RegisterWithPolicy("test-plugin", "unary",
		func(ctx context.Context, inputJSON string) (string, error) {
			_, sawTenant = tenantctx.From(ctx)
			return `{}`, nil
		}, ReplayPolicy{})

	s := newTestExecSession()
	s.engine.pluginRegistry = pr
	s.tenantID = "not-a-uuid"

	buf := make([]byte, 256)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	res := s.PluginCall(ctx, nil, "test-plugin", "unary", `{}`, 0, 255)
	if errCode := byte(res & 0xFF); errCode != 0 {
		t.Fatalf("an unparseable tenant failed the plugin call (errCode %d); it must "+
			"warn and run unscoped, which is what the call did before the bridge existed",
			errCode)
	}
	if sawTenant {
		t.Fatal("a tenant reached the plugin from a value that does not parse as a UUID")
	}
}
