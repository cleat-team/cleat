package main

import (
	"bytes"
	"context"
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
					if _, err := fixtureDB.ExecContext(bg, plugin.Rebind(
						`DELETE FROM event_awaiters WHERE workflow_id = $1`, dialect),
						runID); err != nil {
						t.Errorf("cleanup event_awaiters on %s: %v", be.Name, err)
					}
					if _, err := fixtureDB.ExecContext(bg, plugin.Rebind(
						`DELETE FROM workflow_instances WHERE id = $1`, dialect),
						runID); err != nil {
						t.Errorf("cleanup workflow_instances on %s: %v", be.Name, err)
					}
					if _, err := fixtureDB.ExecContext(bg, plugin.Rebind(
						`DELETE FROM workflow_defs WHERE name = $1`, dialect), defName); err != nil {
						t.Errorf("cleanup workflow_defs on %s: %v", be.Name, err)
					}
				}()

				if _, err := fixtureDB.ExecContext(ctx, plugin.Rebind(
					`INSERT INTO workflow_defs (name, version, wasm_bytes, tenant_id) VALUES ($1, $2, $3, $4)`,
					dialect), defName, 1, []byte{0}, tenant.String()); err != nil {
					t.Fatalf("insert workflow_defs on %s: %v", be.Name, err)
				}

				// Already terminal and already past the retention window --
				// exactly the row shape TerminateWorkflow followed by an
				// aged clock would leave, and exactly what
				// DeleteCompletedWorkflows' predicate (engine/retention_predicates.go)
				// selects: status IN ('done','failed','terminated','cancelled')
				// AND completed_at IS NOT NULL AND completed_at < cutoff.
				completedAt := time.Now().Add(-1 * time.Hour)
				if _, err := fixtureDB.ExecContext(ctx, plugin.Rebind(
					`INSERT INTO workflow_instances (id, def_name, def_version, tenant_id, status, completed_at) `+
						`VALUES ($1, $2, $3, $4, 'terminated', $5)`,
					dialect), runID, defName, 1, tenant.String(), completedAt); err != nil {
					t.Fatalf("insert workflow_instances on %s: %v", be.Name, err)
				}

				if _, err := fixtureDB.ExecContext(ctx, plugin.Rebind(
					`INSERT INTO event_awaiters (workflow_id, tenant_id, event_type) VALUES ($1, $2, $3)`,
					dialect), runID, tenant.String(), eventType); err != nil {
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
				var env *plugin.Environment
				env = &plugin.Environment{
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
				if err := fixtureDB.QueryRowContext(ctx, plugin.Rebind(
					`SELECT COUNT(*) FROM event_awaiters WHERE workflow_id = $1 AND event_type = $2`,
					dialect), runID, eventType).Scan(&awaiterCount); err != nil {
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
