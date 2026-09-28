// Multi-backend behavioural tests for the trial-expiry sweep, run against
// real database backends (PostgreSQL always, MySQL and MSSQL when their
// CLEAT_TEST_* DSNs are set). migrations.go provides UpMySQL and UpMSSQL, so
// there is no dialect this suite skips.
package tenantlifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestSweep_MultiBackend is the sweep's own falsification target: a
// 1-tenant fixture cannot tell a real filter from a loop that iterates once
// and suspends unconditionally, so this uses two -- one expired, one not.
func TestSweep_MultiBackend(t *testing.T) {
	backends := testutil.NewPluginTestBackends(t)
	for _, be := range backends {
		t.Run(be.Name, func(t *testing.T) {
			// t.Cleanup, not defer: t.Cleanup runs LIFO, so registering
			// be.Cleanup here (before any insertTrial call) makes it run
			// LAST -- after every insertTrial fixture's own t.Cleanup
			// DELETE. A plain `defer be.Cleanup()` closes the pool the
			// moment this closure returns, which is BEFORE the testing
			// package runs any t.Cleanup callback -- insertTrial's DELETE
			// would then run against an already-closed pool and fail
			// silently (cleat-review on #2590; NewPluginTestBackends'
			// own doc says a double Cleanup call is safe).
			t.Cleanup(be.Cleanup)

			p := &Plugin{}
			ctx := context.Background()
			pluginDialect := plugin.Dialect(string(be.Dialect))

			loaded := []*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}
			if err := plugin.RunMigrations(ctx, be.DB, pluginDialect, nil, loaded); err != nil {
				t.Fatalf("RunMigrations for backend %q: %v", be.Name, err)
			}

			p.dialect = pluginDialect
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: pluginDialect}
			p.logger = slog.Default()

			expiredTenant := uuid.New()
			liveTenant := uuid.New()

			insertTrial(t, p, expiredTenant, time.Now().Add(-time.Hour))
			insertTrial(t, p, liveTenant, time.Now().Add(time.Hour))

			var suspendCalls []uuid.UUID
			var handledAtCallTime bool
			p.env = &plugin.Environment{
				SetTenantSuspended: func(ctx context.Context, tenantID uuid.UUID, suspended bool) error {
					suspendCalls = append(suspendCalls, tenantID)
					if !suspended {
						t.Fatalf("SetTenantSuspended called with suspended=false; the sweep only ever suspends")
					}
					// Read back handled BEFORE this call returns, to prove
					// suspend really does happen before the row is marked --
					// the crash-safety order the sweep's own comment commits
					// to (suspend first, so a crash before marking re-tries
					// idempotently on the next tick, rather than marking
					// first and losing the suspension on a crash).
					handledAtCallTime = trialHandled(t, p, tenantID)
					return nil
				},
			}

			p.sweep(ctx)

			if len(suspendCalls) != 1 {
				t.Fatalf(
					"WHAT: wrong number of tenants suspended\n"+
						"WHERE: p.sweep(ctx) with one expired and one live trial\n"+
						"WHY:   a fixture with only one tenant cannot tell a real filter from a\n"+
						"       loop that iterates once and suspends unconditionally -- this is why\n"+
						"       the fixture has two\n"+
						"CLARITY: expected exactly 1 call, got %d: %v",
					len(suspendCalls), suspendCalls,
				)
			}
			if suspendCalls[0] != expiredTenant {
				t.Fatalf("expected the EXPIRED tenant (%s) to be suspended, got %s", expiredTenant, suspendCalls[0])
			}
			if handledAtCallTime {
				t.Fatalf("tenant_trials.handled was already true when SetTenantSuspended was called; " +
					"the sweep must suspend BEFORE marking handled, not after")
			}
			if !trialHandled(t, p, expiredTenant) {
				t.Fatalf("expired tenant's trial was not marked handled after a successful suspend")
			}
			if trialHandled(t, p, liveTenant) {
				t.Fatalf("live tenant's trial was marked handled; it was never expired")
			}
		})
	}
}

// TestSweep_NilGrant_MultiBackend falsifies the nil-seam the coordinator
// flagged: with SetTenantSuspended unwired, the sweep must fail loudly
// (log and leave the row unhandled) rather than silently no-op and mark it
// handled anyway, which would make an expired tenant permanently invisible
// to every future tick.
func TestSweep_NilGrant_MultiBackend(t *testing.T) {
	backends := testutil.NewPluginTestBackends(t)
	for _, be := range backends {
		t.Run(be.Name, func(t *testing.T) {
			// t.Cleanup, not defer: t.Cleanup runs LIFO, so registering
			// be.Cleanup here (before any insertTrial call) makes it run
			// LAST -- after every insertTrial fixture's own t.Cleanup
			// DELETE. A plain `defer be.Cleanup()` closes the pool the
			// moment this closure returns, which is BEFORE the testing
			// package runs any t.Cleanup callback -- insertTrial's DELETE
			// would then run against an already-closed pool and fail
			// silently (cleat-review on #2590; NewPluginTestBackends'
			// own doc says a double Cleanup call is safe).
			t.Cleanup(be.Cleanup)

			p := &Plugin{}
			ctx := context.Background()
			pluginDialect := plugin.Dialect(string(be.Dialect))

			loaded := []*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}
			if err := plugin.RunMigrations(ctx, be.DB, pluginDialect, nil, loaded); err != nil {
				t.Fatalf("RunMigrations for backend %q: %v", be.Name, err)
			}

			p.dialect = pluginDialect
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: pluginDialect}
			p.logger = slog.Default()
			p.env = &plugin.Environment{} // SetTenantSuspended left nil

			expiredTenant := uuid.New()
			insertTrial(t, p, expiredTenant, time.Now().Add(-time.Hour))

			p.sweep(ctx)

			if trialHandled(t, p, expiredTenant) {
				t.Fatalf(
					"WHAT: trial marked handled with no suspend grant wired\n" +
						"WHY:  a nil Environment.SetTenantSuspended must leave the row for the " +
						"next tick, not mark it handled -- otherwise a misconfigured worker " +
						"silently never suspends anyone and never says so past one log line",
				)
			}
		})
	}
}

// TestSweep_ErrTenantNotFound_MultiBackend falsifies the starvation fix
// cleat-review flagged on #2590: a tenant_trials row naming a tenant that no
// longer exists must be marked handled, not left to retry forever. Reverting
// suspendExpiredTenant's errors.Is(err, auth.ErrTenantNotFound) case back to
// a plain `return` (the shape that starves the sweep, per its own doc
// comment) turns this red -- the row stays handled=false and the second
// sweep calls the grant again for the same tenant.
func TestSweep_ErrTenantNotFound_MultiBackend(t *testing.T) {
	backends := testutil.NewPluginTestBackends(t)
	for _, be := range backends {
		t.Run(be.Name, func(t *testing.T) {
			// t.Cleanup, not defer: t.Cleanup runs LIFO, so registering
			// be.Cleanup here (before any insertTrial call) makes it run
			// LAST -- after every insertTrial fixture's own t.Cleanup
			// DELETE. A plain `defer be.Cleanup()` closes the pool the
			// moment this closure returns, which is BEFORE the testing
			// package runs any t.Cleanup callback -- insertTrial's DELETE
			// would then run against an already-closed pool and fail
			// silently (cleat-review on #2590; NewPluginTestBackends'
			// own doc says a double Cleanup call is safe).
			t.Cleanup(be.Cleanup)

			p := &Plugin{}
			ctx := context.Background()
			pluginDialect := plugin.Dialect(string(be.Dialect))

			loaded := []*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}
			if err := plugin.RunMigrations(ctx, be.DB, pluginDialect, nil, loaded); err != nil {
				t.Fatalf("RunMigrations for backend %q: %v", be.Name, err)
			}

			p.dialect = pluginDialect
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: pluginDialect}
			p.logger = slog.Default()

			// A trial for a tenant that was never created -- tenant_trials
			// carries no FK to admin.tenants (migrations.go), so this row is
			// exactly the dangling shape suspendExpiredTenant's doc comment
			// describes: "the tenant was dropped after the trial was set."
			danglingTenant := uuid.New()
			insertTrial(t, p, danglingTenant, time.Now().Add(-time.Hour))

			// Counted PER TENANT, not as a bare total. This was a workaround
			// for a real leak (fixed above, t.Cleanup(be.Cleanup)) and is
			// kept anyway as defense in depth: TestSweep_MultiBackend and
			// TestSweep_NilGrant_MultiBackend both assert about their own
			// tenant id rather than a raw count for the same reason -- this
			// test's fixture is not the only thing that could ever leave a
			// row in tenant_trials, and scoping the assertion to what this
			// test itself created is cheap insurance against whatever does.
			suspendCallsForDangling := 0
			p.env = &plugin.Environment{
				SetTenantSuspended: func(ctx context.Context, tenantID uuid.UUID, suspended bool) error {
					if tenantID == danglingTenant {
						suspendCallsForDangling++
					}
					return fmt.Errorf("suspend tenant: %w", auth.ErrTenantNotFound)
				},
			}

			p.sweep(ctx)

			if suspendCallsForDangling != 1 {
				t.Fatalf("first sweep: SetTenantSuspended called %d times for the dangling tenant, want 1",
					suspendCallsForDangling)
			}
			if !trialHandled(t, p, danglingTenant) {
				t.Fatalf("WHAT: a trial for a nonexistent tenant was not marked handled after " +
					"ErrTenantNotFound\n" +
					"WHY:  tenant_trials has no FK to admin.tenants, so no tenant is EVER going to " +
					"appear for this row -- leaving it unhandled means queryExpiredTrials' " +
					"ORDER BY expires_at LIMIT 100 selects it again every tick, keeping its slot " +
					"among the 100 and starving later trials out of the sweep entirely " +
					"(cleat-review on #2590)")
			}

			// The second sweep is the falsification's whole point: if the
			// row were left unhandled, this call would find it again (its
			// expires_at has not changed) and call the grant a second time
			// for the same tenant.
			p.sweep(ctx)
			if suspendCallsForDangling != 1 {
				t.Fatalf("second sweep: SetTenantSuspended called again (total %d calls) for the "+
					"dangling tenant, already marked handled after ErrTenantNotFound -- the row was "+
					"not excluded from the next tick's scan", suspendCallsForDangling)
			}
		})
	}
}

func insertTrial(t *testing.T, p *Plugin, tenantID uuid.UUID, expiresAt time.Time) {
	t.Helper()
	ctx := plugin.AcrossAllTenants(context.Background(), "test fixture: seeding tenant_trials across tenants")
	stmt := insertTrialSQL.For(p.dialect)
	if _, err := p.db.Exec(ctx, stmt, tenantID, expiresAt); err != nil {
		t.Fatalf("insert trial fixture for tenant %s: %v", tenantID, err)
	}
	t.Cleanup(func() {
		cleanupCtx := plugin.AcrossAllTenants(context.Background(), "test fixture: cleaning up tenant_trials across tenants")
		_, _ = p.db.Exec(cleanupCtx, `DELETE FROM tenant_trials WHERE tenant_id = $1`, tenantID)
	})
}

func trialHandled(t *testing.T, p *Plugin, tenantID uuid.UUID) bool {
	t.Helper()
	ctx := plugin.AcrossAllTenants(context.Background(), "test fixture: reading tenant_trials across tenants")
	row := p.db.QueryRow(ctx, `SELECT handled FROM tenant_trials WHERE tenant_id = $1`, tenantID)
	var handled bool
	if err := row.Scan(&handled); err != nil {
		t.Fatalf("read handled for tenant %s: %v", tenantID, err)
	}
	return handled
}

var insertTrialSQL = plugin.Query{
	Default: `INSERT INTO tenant_trials (tenant_id, expires_at, handled) VALUES ($1, $2, false)`,
	MySQL:   `INSERT INTO tenant_trials (tenant_id, expires_at, handled) VALUES ($1, $2, false)`,
	MSSQL:   `INSERT INTO tenant_trials (tenant_id, expires_at, handled) VALUES ($1, $2, 0)`,
}
