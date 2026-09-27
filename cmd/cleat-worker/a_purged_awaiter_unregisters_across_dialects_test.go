package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/eventtriggers"
	"github.com/cleat-team/cleat/plugins/plugintest"
)

// cleat#2213, on real databases, across all three dialects, with and
// without --require-signal-auth.
//
// The history, in order, because two earlier drafts of this comment had it
// backwards. Before cleat#2218, DeliverSignal's INSERT into workflow_signals
// was UNGATED, so signalling a purged (or foreign, or nonexistent) workflow
// failed on a foreign-key violation -- a plain error signalAwaiters (below,
// and publish.go) had no branch for. It fell to the generic-failure path,
// logged a WARN, and did NOT unregister the awaiter -- so an awaiter whose
// workflow had been purged by retention stayed registered forever, and every
// later publish of that event type re-failed it the same way. That is
// cleat#2213.
//
// cleat#2218 gated the INSERT on EXISTS(...) to close a cross-tenant
// existence oracle (cleat#2187) and, as a side effect nobody had asked for,
// turned the zero-rows-affected case into a plain nil return instead of a
// foreign-key error. signalAwaiters' success path unregisters
// unconditionally, so that nil accidentally fixed cleat#2213 -- with one
// wrong side effect of its own: the success path also logs "signal delivered
// to awaiter", which was false. The workflow never received anything.
//
// cleat#2227 (this PR) gives that case its own honest path instead of
// relying on the accident: deliverSignalTx now returns engine.ErrWorkflowNotFound
// when the EXISTS-gated INSERT affects zero rows, translated to
// plugin.ErrWorkflowNotFound at this package's boundary
// (signalPluginWorkflow / signalPluginWorkflowWithAuth, main.go). signalAwaiters
// unregisters on that specifically now, with a log line that says what
// actually happened. Net effect versus develop before #2218: identical
// (unregistered, not leaked). Net effect versus develop right now, before
// this PR: identical outcome, honest log.
//
// What this test actually exercises that no unit test can: a workflow that
// is GENUINELY GONE from workflow_instances, purged the same way retention
// purges it (engine.WorkflowStore.DeleteCompletedWorkflows, not a mock, not
// a fake driver), signalled through the same plugin-boundary functions
// webhookingest and eventtriggers actually call, on a real server of each
// dialect's own SQL. Closes #2213.
func TestPurgedAwaiterUnregistersAcrossDialects(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))

			testutil.SetupMinimalSchema(t, be.DB, be.Dialect)

			etPlugin := eventtriggers.New()
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: etPlugin, Healthy: true}}); err != nil {
				t.Fatalf("eventtriggers migrations on %s: %v", be.Name, err)
			}

			fixtureDB := be.CrossTenantConn(t, context.Background(),
				"cleat#2213 fixture: seeds and purges workflow_instances/"+
					"event_awaiters rows directly")

			// Cleanup, not t.Cleanup: registered AFTER "defer be.Cleanup()"
			// above so LIFO runs it FIRST, while be.DB is still open. A
			// t.Cleanup callback here would run after the function body
			// returns, which is after the deferred be.Cleanup() has already
			// closed that pool -- the SQL below would run on a dead
			// connection and every statement would fail, silently, since
			// nothing here would be watching for it in that ordering.
			//
			// be.DB, NOT fixtureDB. On MSSQL, CrossTenantConn (fixtureDB's
			// source) routes through MSSQLAdminDB -- a login that is only a
			// MEMBER of cleat_admin, for bypassing row-level security
			// predicates, not the login RunMigrations used to create these
			// tables and policies. Measured directly (cleat#2239): a
			// DROP SECURITY POLICY / DROP TABLE through that connection fails
			// "does not exist or you do not have permission" (3701) for an
			// object the SAME connection's own SELECT against
			// sys.security_policies / sys.tables confirms is there --
			// SQL Server's deliberately ambiguous wording for "you can see
			// the catalog row but you may not touch the object." be.DB is the
			// pool migrations actually ran on and has the rights to reverse
			// them.
			//
			// WHY THIS EXISTS AT ALL: this test runs eventtriggers'
			// migrations against testutil's SHARED, PERSISTENT test
			// database, not a private one -- TestDB opens a real server and
			// leaves its schema in place for the rest of the test binary's
			// life (engine/testutil/schema.go's TestDB doc comment). Without
			// this, event_awaiters/event_subscriptions/ingested_events and
			// their admin.plugin_tables registry rows outlive this test and
			// are still there when engine's
			// TestEveryTenantOwnedTableIsEmptiedByDropTenant later scans
			// information_schema.columns for every tenant_id-bearing table in
			// that same database -- three tables it was never written to
			// seed, which that test's own design (see its file comment) fails
			// on by NAME rather than passing on a false "0 rows" negative.
			// Found by cleat-review, cleat#2239.
			defer cleanupEventTriggersSchema(t, be.DB, be.Dialect)

			// MySQL isolates tenants by physical database (tiers.yaml D1: a
			// second tenant row cannot even be created -- see
			// blobstore_stale_workflow_refs_multidb_test.go's own comment on
			// this), so this reuses the pre-existing default tenant there
			// instead of minting one.
			var tenant uuid.UUID
			if be.Dialect == testutil.DialectMySQL {
				tenant = uuid.MustParse("00000000-0000-0000-0000-000000000000")
			} else {
				tenant = uuid.New()
			}

			// The "process store": unscoped, exactly what main() opens once
			// for the whole worker and what signalPluginWorkflow /
			// signalPluginWorkflowWithAuth re-scope per call from ctx.
			// tenantStore is scoped up front, for the purge itself --
			// DeleteCompletedWorkflows deletes only its own store's tenant
			// (see PostgresStore.deleteCompletedWorkflowsBatch's tenant_id
			// parameter) on the two dialects that carry per-call scoping;
			// MySQL's store needs none, matching scopeToTenant's own case
			// for it in main.go.
			//
			// MSSQL specifically CANNOT be engine.NewMSSQLStore(be.DB): a
			// plain pool sets no SESSION_CONTEXT, so a store built on it is
			// "a scope that exists in the process and not in the database"
			// (engine/store_backends_test.go's phrase for exactly this) --
			// its reads are RLS-FILTERed to nothing even though the row is
			// there and even though WithTenant sets the Go-level field.
			// engine/mssql_retention_deletes_the_children_test.go hit this
			// first (cleat#1265): the fix is to go through
			// NewMSSQLStoreFactory.OpenStore, exactly as production and
			// a_signal_plugin_workflow_is_tenant_scoped_test.go's own MSSQL
			// variant do -- the factory's pool sets session context
			// consistently for every connection it hands out.
			var processStore engine.WorkflowStore
			var tenantStore engine.WorkflowStore
			switch be.Dialect {
			case testutil.DialectPostgres:
				s := engine.NewPostgresStore(be.DB)
				processStore, tenantStore = s, s.WithTenant(tenant.String())
			case testutil.DialectMSSQL:
				factory := engine.NewMSSQLStoreFactory(os.Getenv("CLEAT_TEST_MSSQL"))
				ps, psCloser, err := factory.OpenStore(ctx, engine.DefaultTenantUUID, "default")
				if err != nil {
					t.Fatalf("OpenStore(default) on %s: %v", be.Name, err)
				}
				t.Cleanup(func() { _ = psCloser.Close() })
				ts, tsCloser, err := factory.OpenStore(ctx, tenant.String(), "default")
				if err != nil {
					t.Fatalf("OpenStore(%s) on %s: %v", tenant, be.Name, err)
				}
				t.Cleanup(func() { _ = tsCloser.Close() })
				processStore, tenantStore = ps, ts
			case testutil.DialectMySQL:
				s := engine.NewMySQLStore(be.DB)
				processStore, tenantStore = s, s
			default:
				t.Fatalf("unhandled dialect %s", be.Dialect)
			}

			runTenantSignal := func(t *testing.T, requireAuth bool) {
				t.Helper()

				defName := "cleat-2213-" + uuid.New().String()
				runID := "cleat-2213-run-" + uuid.New().String()
				eventType := "cleat-2213.purged." + uuid.New().String()

				defer func() {
					bg := context.Background()
					if _, err := plugintest.ExecRebound(t, bg, fixtureDB, dialect,
						`DELETE FROM event_awaiters WHERE workflow_id = $1`,
						runID); err != nil {
						t.Errorf("cleanup event_awaiters on %s: %v", be.Name, err)
					}
					if _, err := plugintest.ExecRebound(t, bg, fixtureDB, dialect,
						`DELETE FROM workflow_instances WHERE id = $1`,
						runID); err != nil {
						t.Errorf("cleanup workflow_instances on %s: %v", be.Name, err)
					}
					if _, err := plugintest.ExecRebound(t, bg, fixtureDB, dialect,
						`DELETE FROM workflow_defs WHERE name = $1`, defName); err != nil {
						t.Errorf("cleanup workflow_defs on %s: %v", be.Name, err)
					}
				}()

				if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect,
					`INSERT INTO workflow_defs (name, version, wasm_bytes, tenant_id) VALUES ($1, $2, $3, $4)`,
					defName, 1, []byte{0}, tenant.String()); err != nil {
					t.Fatalf("insert workflow_defs on %s: %v", be.Name, err)
				}

				// Already terminal and already past the retention window --
				// exactly the row shape TerminateWorkflow followed by an
				// aged clock would leave, and exactly what
				// DeleteCompletedWorkflows' predicate (engine/retention_predicates.go)
				// selects: status IN ('done','failed','terminated','cancelled')
				// AND completed_at IS NOT NULL AND completed_at < cutoff.
				completedAt := time.Now().Add(-1 * time.Hour)
				if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect,
					`INSERT INTO workflow_instances (id, def_name, def_version, tenant_id, status, completed_at) `+
						`VALUES ($1, $2, $3, $4, 'terminated', $5)`,
					runID, defName, 1, tenant.String(), completedAt); err != nil {
					t.Fatalf("insert workflow_instances on %s: %v", be.Name, err)
				}

				if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect,
					`INSERT INTO event_awaiters (workflow_id, tenant_id, event_type) VALUES ($1, $2, $3)`,
					runID, tenant.String(), eventType); err != nil {
					t.Fatalf("insert event_awaiters on %s: %v", be.Name, err)
				}

				// THE PURGE. The real production method, not a hand-rolled
				// DELETE -- the same call cmd/cleat-worker's retention sweep
				// makes.
				deleted, err := tenantStore.DeleteCompletedWorkflows(ctx, time.Now())
				if err != nil {
					t.Fatalf("DeleteCompletedWorkflows on %s: %v", be.Name, err)
				}
				if deleted != 1 {
					t.Fatalf("DeleteCompletedWorkflows on %s deleted %d rows, want 1 -- "+
						"fixture did not reach the retention predicate", be.Name, deleted)
				}

				var buf bytes.Buffer
				logger := slog.New(slog.NewTextHandler(&buf, nil))

				pdb := &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}
				env := &plugin.Environment{
					DB:      pdb,
					Dialect: dialect,
					Logger:  logger,
					SignalWorkflow: func(sctx context.Context, workflowID, signalName, payload string) error {
						if requireAuth {
							return signalPluginWorkflowWithAuth(sctx, processStore, workflowID, signalName, payload, "event-triggers")
						}
						return signalPluginWorkflow(sctx, processStore, workflowID, signalName, payload)
					},
				}
				if err := etPlugin.Init(ctx, env); err != nil {
					t.Fatalf("eventtriggers Init on %s: %v", be.Name, err)
				}

				publishOnce := func(label string) {
					t.Helper()
					eventID := uuid.New()
					if _, err := eventtriggers.PublishEvent(ctx, pdb, logger, env,
						eventID, tenant, eventType, json.RawMessage(`{}`)); err != nil {
						t.Fatalf("PublishEvent (%s) on %s: %v", label, be.Name, err)
					}
				}

				// Published twice, with distinct event IDs so neither is
				// skipped as an idempotency duplicate: the original bug was
				// that a purged awaiter re-failed on EVERY future publish of
				// its event type, not just the first. One publish proves the
				// awaiter is gone; two prove it stays gone and produces no
				// SECOND round of noise either.
				publishOnce("first")

				var awaiterCount int
				if err := plugintest.QueryRowRebound(t, ctx, fixtureDB, dialect,
					`SELECT COUNT(*) FROM event_awaiters WHERE workflow_id = $1 AND event_type = $2`,
					runID, eventType).Scan(&awaiterCount); err != nil {
					t.Fatalf("count event_awaiters on %s: %v", be.Name, err)
				}
				if awaiterCount != 0 {
					t.Errorf("on %s (requireAuth=%v), event_awaiters row for a purged workflow "+
						"survived one publish -- cleat#2213 unfixed", be.Name, requireAuth)
				}

				publishOnce("second")

				logs := buf.String()
				if strings.Contains(logs, "signal awaiter failed") {
					t.Errorf("on %s (requireAuth=%v), signalAwaiters logged a WARN for a purged "+
						"workflow -- it should take the not-found path silently, not the generic "+
						"failure path:\n%s", be.Name, requireAuth, logs)
				}
				if !strings.Contains(logs, "awaiter's workflow no longer exists, unregistering") {
					t.Errorf("on %s (requireAuth=%v), signalAwaiters never logged the not-found "+
						"unregister path -- the assertions above may be passing for the wrong "+
						"reason (e.g. the awaiter was never inserted at all):\n%s",
						be.Name, requireAuth, logs)
				}
			}

			t.Run("without_signal_auth", func(t *testing.T) { runTenantSignal(t, false) })
			t.Run("with_signal_auth", func(t *testing.T) { runTenantSignal(t, true) })
		})
	}
}

// cleanupEventTriggersSchema undoes eventtriggers.Migrations() against
// TestPurgedAwaiterUnregistersAcrossDialects' shared, persistent test
// database, table by table rather than via plugin.RunDownMigrations: that
// function reverses a plugin's Down SQL, but eventtriggers' v4 migration
// (TenantScoped, cleat#1512) applies its security policy through
// plugin.RunMigrations' own runtime side effect (applyTenantScoping), not
// through any Up/Down SQL the plugin declares -- so a Down pass never drops
// the policy, and on SQL Server the later DROP TABLE for event_awaiters would
// fail while that policy still references it ("...used by a security
// policy..."). Dropping the policies first, by the same
// "<table>_tenant_isolation" name plugin/migration.go's
// applyTenantScopingMSSQL constructs, avoids depending on that ordering at
// all.
//
// admin.plugin_tables (registerTenantScopedTables, Postgres only) is cleared
// too, so a registry row naming a table that no longer exists cannot outlive
// this test either -- see CLAUDE.md's "a resolver must be able to return
// UNKNOWN" family of notes on stale registry rows reading as confident wrong
// answers.
func cleanupEventTriggersSchema(t *testing.T, conn *sql.DB, dialect testutil.Dialect) {
	t.Helper()
	ctx := context.Background()
	const pluginName = "event-triggers"
	tables := []string{"event_awaiters", "event_subscriptions", "ingested_events"}

	exec := func(query string) {
		if _, err := conn.ExecContext(ctx, query); err != nil {
			t.Errorf("cleanupEventTriggersSchema: %s: %v", query, err)
		}
	}
	exists := func(query string, args ...any) bool {
		var n int
		if err := conn.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			t.Errorf("cleanupEventTriggersSchema: existence check %s: %v", query, err)
			return false
		}
		return n > 0
	}

	if dialect == testutil.DialectMSSQL {
		// Checked in Go and dropped unconditionally rather than
		// "IF EXISTS(...) DROP ..." in one batch: measured directly against
		// a real SQL Server (cleat#2239) that the single-batch form raises
		// 3701 ("does not exist or you do not have permission") on an
		// object sys.security_policies confirms IS there, on the very
		// connection running the check -- the IF guard and the DROP do not
		// agree with each other inside one batch here, for a reason not
		// worth chasing further when checking first in Go sidesteps it
		// entirely.
		for _, tbl := range tables {
			policy := tbl + "_tenant_isolation"
			if exists(`SELECT COUNT(*) FROM sys.security_policies WHERE name = @p1`, policy) {
				exec(`DROP SECURITY POLICY dbo.` + policy)
			}
		}
		for _, tbl := range tables {
			if exists(`SELECT COUNT(*) FROM sys.tables WHERE name = @p1`, tbl) {
				exec(`DROP TABLE ` + tbl)
			}
		}
		exec(`DELETE FROM plugin_migrations WHERE plugin_name = '` + pluginName + `'`)
		return
	}

	for _, tbl := range tables {
		exec(`DROP TABLE IF EXISTS ` + tbl)
	}
	exec(`DELETE FROM plugin_migrations WHERE plugin_name = '` + pluginName + `'`)
	if dialect == testutil.DialectPostgres {
		exec(`DELETE FROM admin.plugin_tables WHERE plugin_name = '` + pluginName + `'`)
	}
}
