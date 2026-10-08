package tenantlifecycle

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// cleat#2487. New, Run and RegisterRoutes had no direct test: the existing
// suites exercise sweep and handleGetLifecycleStatus (the logic) but never
// the plugin-lifecycle wiring around them.

func TestNewReturnsAUsablePlugin(t *testing.T) {
	p := New()
	if p == nil {
		t.Fatal("New() returned nil")
	}
	if _, ok := p.(*Plugin); !ok {
		t.Fatalf("New() returned %T, want *Plugin", p)
	}
}

func TestTenantLifecycleInit(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		DB:     &engine.SQLDBAdapter{DB: &sql.DB{}, Dialect: plugin.DialectPostgres},
		Logger: slog.Default(),
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.db == nil {
		t.Error("expected db to be set after Init")
	}
	if p.logger == nil {
		t.Error("expected logger to be set after Init")
	}
	if p.env != env {
		t.Error("expected env to be stored after Init")
	}
}

func TestTenantLifecycleInitWithNilLoggerDefaults(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{DB: &engine.SQLDBAdapter{DB: &sql.DB{}, Dialect: plugin.DialectPostgres}}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.logger == nil {
		t.Error("expected logger to default to slog.Default() when env.Logger is nil")
	}
}

func TestRegisterRoutesRefusesANilMux(t *testing.T) {
	p := &Plugin{}
	if err := p.RegisterRoutes(nil); err == nil {
		t.Fatal("RegisterRoutes(nil) returned nil error, want a refusal")
	}
}

func TestRegisterRoutesWiresTheLifecycleEndpoint(t *testing.T) {
	p := &Plugin{logger: slog.Default()}
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/tenant/lifecycle", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	// No tenant in context -- handleGetLifecycleStatus's own no-tenant path,
	// already covered by TestGetLifecycleStatus_NoTenant. What this proves
	// is narrower and what RegisterRoutes is actually for: the route is
	// REACHABLE through the mux it registers on, not a 404.
	if rec.Code == http.StatusNotFound {
		t.Fatal("GET /api/tenant/lifecycle was not registered on the mux")
	}
}

// TestRun_NoDatabaseWaitsForCancellation covers Run's nil-db branch: no
// database means the sweep is disabled, and Run must still respect context
// cancellation rather than returning immediately or blocking forever.
func TestRun_NoDatabaseWaitsForCancellation(t *testing.T) {
	p := &Plugin{logger: slog.Default()}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	select {
	case err := <-done:
		t.Fatalf("Run returned %v before cancellation, want it to block on ctx.Done()", err)
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v after cancellation, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of context cancellation")
	}
}

// TestRun_WithDatabaseSweepsOnceThenStopsOnCancellation covers the ticker
// setup, the immediate on-startup sweep, and the ctx.Done() exit of the
// select loop -- everything in Run except the 60s ticker tick itself, which
// no unit test should wait real time for.
func TestRun_WithDatabaseSweepsOnceThenStopsOnCancellation(t *testing.T) {
	backends := testutil.NewPluginTestBackends(t)
	be := backends[0] // PostgreSQL always runs; one backend is enough here.
	t.Cleanup(be.Cleanup)

	p := &Plugin{}
	ctx := context.Background()
	pluginDialect := plugin.Dialect(string(be.Dialect))

	loaded := []*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}
	if err := plugin.RunMigrations(ctx, be.DB, pluginDialect, nil, loaded); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	p.dialect = pluginDialect
	p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: pluginDialect}
	p.logger = slog.Default()

	var suspendCalls int
	p.env = &plugin.Environment{
		SetTenantSuspended: func(ctx context.Context, tenantID uuid.UUID, suspended bool) error {
			suspendCalls++
			return nil
		},
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- p.Run(runCtx) }()

	// The on-startup sweep runs synchronously before Run enters its select
	// loop, so by the time Run is in the loop the sweep has already
	// happened -- there is nothing expired in this empty fixture, so
	// suspendCalls staying 0 is the expected, correct result; what this
	// test is actually proving is that Run REACHES the loop and exits on
	// cancellation, not any particular suspendCalls count.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v after cancellation, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of context cancellation")
	}
	if suspendCalls != 0 {
		t.Fatalf("suspendCalls = %d on an empty tenant_trials table, want 0", suspendCalls)
	}
}
