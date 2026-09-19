// Multi-backend tests for the abandonment sweep and the finalize
// write-back, against real databases. cleat#1715.
//
// abandonedJobsQuery is exactly the shape cleat#1133/#1134/#1141 already
// burned this plugin on twice: a per-dialect statement (LIMIT and subquery
// syntax differ enough that plugin.Rebind cannot paper over them) that
// nothing had ever executed against a real server. Both PostgreSQL's arm
// (through admin.in_flight_workflow_ids(), migration 073) and the MySQL/MSSQL
// arms (a direct subquery against workflow_instances) are new text -- the
// fake-driver suite proves the GUARD LOGIC (see
// TestSweepAbandonedJobs/execSweepAbandoned in jobqueue_behavioral_test.go),
// but it pattern-matches the query string and would accept SQL no database
// would. Only a real server settles whether the statement parses.
package jobqueue

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestSweepAbandonedJobs_MultiBackend covers the case that needs no
// workflow_instances fixture at all: a dispatched job whose run_id names no
// row anywhere is the plainest "gone" case the sweep exists for, and an
// empty table is enough to prove admin.in_flight_workflow_ids() (Postgres)
// and the direct workflow_instances subquery (MySQL, MSSQL) both resolve
// without error.
func TestSweepAbandonedJobs_MultiBackend(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		t.Run(be.Name, func(t *testing.T) {
			fixtureDB := be.CrossTenantConn(t, context.Background(),
				"jobqueue abandonment sweep fixture: seeds a row for a tenant it invents")
			defer be.Cleanup()
			ctx := plugin.AcrossAllTenants(context.Background(),
				"jobqueue abandonment sweep test: the sweep operates on every tenant's queue, as Run does")
			dialect := plugin.Dialect(string(be.Dialect))
			p := &Plugin{dialect: dialect}

			// NewPluginTestBackends does not build the core engine schema for
			// MySQL/MSSQL -- only Postgres gets it via TestDB. workflow_instances
			// (and, for the in-flight-fixture test below, workflow_defs) are
			// core tables, not this plugin's own, so they need this call on
			// every dialect or the sweep's subquery has nothing to query and
			// fails closed with "table does not exist" rather than proving
			// anything about the statement's SYNTAX -- the actual subject of
			// this file.
			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("jobqueue migrations on %s: %v", be.Name, err)
			}
			// ON SQL SERVER, THE SWEEP NEEDS A dbo.cleat_admin LOGIN.
			// workflow_instances carries a FILTER PREDICATE keyed on
			// SESSION_CONTEXT('tenant_id'), which a tenant-less background sweep
			// never sets -- same reasoning as admin.in_flight_workflow_ids() on
			// PostgreSQL (cleat#1528), except SQL Server's exemption lives on the
			// LOGIN, not in a callable function. Every other cross-tenant SQL
			// Server statement in this codebase already depends on this
			// (mssql_schedules.go's ClaimDueSchedule, mssql_store.go's
			// SetWorkflowTag), so this is the query joining that list rather than
			// a new requirement. testutil.MSSQLAdminDB provisions exactly that
			// login and opts into the predicate form it needs
			// (migrations/mssql/optional/cross_tenant_claim.sql) -- see
			// engine/mssql_admin_login_schedule_tenant_test.go's adminLoginStores
			// for the same pattern read against the engine's own claim path.
			execDB := be.DB
			if be.Dialect == testutil.DialectMSSQL {
				execDB = testutil.MSSQLAdminDB(t, be.DB)
			}
			p.db = &engine.SQLDBAdapter{DB: execDB, Dialect: plugin.Dialect(be.Dialect)}
			p.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

			tenant := uuid.New()
			job := uuid.New()
			runID := "gone-" + uuid.New().String()

			defer func() {
				if _, err := fixtureDB.ExecContext(context.Background(),
					plugin.Rebind(`DELETE FROM task_queue WHERE tenant_id = $1`, dialect), tenant); err != nil {
					t.Errorf("cleanup task_queue on %s: %v", be.Name, err)
				}
			}()

			if _, err := fixtureDB.ExecContext(ctx, plugin.Rebind(
				`INSERT INTO task_queue (tenant_id, queue_name, job_id, status, started_at, run_id)
				 VALUES ($1, $2, $3, 'dispatched', now(), $4)`, dialect),
				tenant, "sweep-test", job, runID); err != nil {
				t.Fatalf("insert dispatched job on %s: %v", be.Name, err)
			}

			// ASSERT THE EFFECT, NOT THE ABSENCE OF AN ERROR -- sweepAbandonedJobs
			// logs and returns -1 on failure, so a statement that does not run
			// looks exactly like a queue with nothing abandoned in it. That is
			// precisely how the reaper's own broken arms went unnoticed twice
			// (cleat#1133, #1134, #1141); see this file's own package comment.
			n := p.sweepAbandonedJobs(ctx)
			if n < 0 {
				t.Fatalf("sweepAbandonedJobs reported failure on %s; the statement did not execute", be.Name)
			}
			if n != 1 {
				t.Errorf("sweepAbandonedJobs = %d on %s, want 1", n, be.Name)
			}

			var status string
			if err := fixtureDB.QueryRowContext(ctx, plugin.Rebind(
				`SELECT status FROM task_queue WHERE tenant_id = $1 AND queue_name = $2 AND job_id = $3`,
				dialect), tenant, "sweep-test", job).Scan(&status); err != nil {
				t.Fatalf("read back on %s: %v", be.Name, err)
			}
			if status != "abandoned" {
				t.Errorf("on %s status = %q, want \"abandoned\"", be.Name, status)
			}
		})
	}
}

// TestSweepAbandonedJobs_SparesAnInFlightRun_MultiBackend is the positive
// control the test above cannot be: an empty workflow_instances table proves
// the statement PARSES, not that its filter does anything. This inserts a
// real workflow_instances row whose status is the sweep's own definition of
// "in flight" and checks the matching task_queue row survives.
//
// wasm_bytes is bound as a parameter rather than written as a literal
// specifically to stay dialect-portable -- BYTEA, LONGBLOB and VARBINARY(MAX)
// each spell a binary literal differently, and a parameterized []byte needs
// none of them.
func TestSweepAbandonedJobs_SparesAnInFlightRun_MultiBackend(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		t.Run(be.Name, func(t *testing.T) {
			fixtureDB := be.CrossTenantConn(t, context.Background(),
				"jobqueue abandonment sweep fixture: seeds task_queue and workflow_instances "+
					"rows for a tenant and run it invents")
			defer be.Cleanup()
			ctx := plugin.AcrossAllTenants(context.Background(),
				"jobqueue abandonment sweep test: the sweep operates on every tenant's queue, as Run does")
			dialect := plugin.Dialect(string(be.Dialect))
			p := &Plugin{dialect: dialect}

			// NewPluginTestBackends does not build the core engine schema for
			// MySQL/MSSQL -- only Postgres gets it via TestDB. workflow_instances
			// (and, for the in-flight-fixture test below, workflow_defs) are
			// core tables, not this plugin's own, so they need this call on
			// every dialect or the sweep's subquery has nothing to query and
			// fails closed with "table does not exist" rather than proving
			// anything about the statement's SYNTAX -- the actual subject of
			// this file.
			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("jobqueue migrations on %s: %v", be.Name, err)
			}
			// ON SQL SERVER, THE SWEEP NEEDS A dbo.cleat_admin LOGIN.
			// workflow_instances carries a FILTER PREDICATE keyed on
			// SESSION_CONTEXT('tenant_id'), which a tenant-less background sweep
			// never sets -- same reasoning as admin.in_flight_workflow_ids() on
			// PostgreSQL (cleat#1528), except SQL Server's exemption lives on the
			// LOGIN, not in a callable function. Every other cross-tenant SQL
			// Server statement in this codebase already depends on this
			// (mssql_schedules.go's ClaimDueSchedule, mssql_store.go's
			// SetWorkflowTag), so this is the query joining that list rather than
			// a new requirement. testutil.MSSQLAdminDB provisions exactly that
			// login and opts into the predicate form it needs
			// (migrations/mssql/optional/cross_tenant_claim.sql) -- see
			// engine/mssql_admin_login_schedule_tenant_test.go's adminLoginStores
			// for the same pattern read against the engine's own claim path.
			execDB := be.DB
			if be.Dialect == testutil.DialectMSSQL {
				execDB = testutil.MSSQLAdminDB(t, be.DB)
			}
			p.db = &engine.SQLDBAdapter{DB: execDB, Dialect: plugin.Dialect(be.Dialect)}
			p.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

			tenant := uuid.New()
			defName := "sweep-arm-test-" + uuid.New().String()
			runID := "in-flight-" + uuid.New().String()
			job := uuid.New()

			defer func() {
				if _, err := fixtureDB.ExecContext(context.Background(),
					plugin.Rebind(`DELETE FROM task_queue WHERE tenant_id = $1`, dialect), tenant); err != nil {
					t.Errorf("cleanup task_queue on %s: %v", be.Name, err)
				}
				if _, err := fixtureDB.ExecContext(context.Background(),
					plugin.Rebind(`DELETE FROM workflow_instances WHERE id = $1`, dialect), runID); err != nil {
					t.Errorf("cleanup workflow_instances on %s: %v", be.Name, err)
				}
				if _, err := fixtureDB.ExecContext(context.Background(),
					plugin.Rebind(`DELETE FROM workflow_defs WHERE name = $1`, dialect), defName); err != nil {
					t.Errorf("cleanup workflow_defs on %s: %v", be.Name, err)
				}
			}()

			if _, err := fixtureDB.ExecContext(ctx, plugin.Rebind(
				`INSERT INTO workflow_defs (name, version, wasm_bytes) VALUES ($1, $2, $3)`,
				dialect), defName, 1, []byte{0}); err != nil {
				t.Fatalf("insert workflow_defs on %s: %v", be.Name, err)
			}
			// status is left to its column default, which is 'ready' on every
			// dialect -- one of the two values the sweep treats as in flight.
			if _, err := fixtureDB.ExecContext(ctx, plugin.Rebind(
				`INSERT INTO workflow_instances (id, def_name, def_version) VALUES ($1, $2, $3)`,
				dialect), runID, defName, 1); err != nil {
				t.Fatalf("insert workflow_instances on %s: %v", be.Name, err)
			}
			if _, err := fixtureDB.ExecContext(ctx, plugin.Rebind(
				`INSERT INTO task_queue (tenant_id, queue_name, job_id, status, started_at, run_id)
				 VALUES ($1, $2, $3, 'dispatched', now(), $4)`, dialect),
				tenant, "sweep-arm-test", job, runID); err != nil {
				t.Fatalf("insert dispatched job on %s: %v", be.Name, err)
			}

			n := p.sweepAbandonedJobs(ctx)
			if n < 0 {
				t.Fatalf("sweepAbandonedJobs reported failure on %s; the statement did not execute", be.Name)
			}
			if n != 0 {
				t.Errorf("sweepAbandonedJobs = %d on %s, want 0 -- the run is still "+
					"'ready', and the sweep abandoned it anyway", n, be.Name)
			}

			var status string
			if err := fixtureDB.QueryRowContext(ctx, plugin.Rebind(
				`SELECT status FROM task_queue WHERE tenant_id = $1 AND queue_name = $2 AND job_id = $3`,
				dialect), tenant, "sweep-arm-test", job).Scan(&status); err != nil {
				t.Fatalf("read back on %s: %v", be.Name, err)
			}
			if status != "dispatched" {
				t.Errorf("on %s status = %q, want \"dispatched\" -- the sweep touched a "+
					"job whose run is still in flight", be.Name, status)
			}
		})
	}
}

// TestObserveFinalize_MultiBackend proves finalize_observer.go's UPDATE
// executes on all three dialects. Unlike abandonedJobsQuery this is a single
// statement using plugin.Rebind's ordinary $N/now() translation rather than
// a per-dialect plugin.Query arm, but it is new SQL and new SQL is exactly
// what this file exists to check before trusting it.
func TestObserveFinalize_MultiBackend(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		t.Run(be.Name, func(t *testing.T) {
			fixtureDB := be.CrossTenantConn(t, context.Background(),
				"jobqueue finalize-observer fixture: seeds a row for a tenant it invents")
			defer be.Cleanup()
			dialect := plugin.Dialect(string(be.Dialect))
			p := &Plugin{dialect: dialect}

			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			if err := plugin.RunMigrations(context.Background(), be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("jobqueue migrations on %s: %v", be.Name, err)
			}
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: plugin.Dialect(be.Dialect)}
			p.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

			tenant := uuid.New()
			job := uuid.New()
			runID := "finalize-" + uuid.New().String()

			defer func() {
				if _, err := fixtureDB.ExecContext(context.Background(),
					plugin.Rebind(`DELETE FROM task_queue WHERE tenant_id = $1`, dialect), tenant); err != nil {
					t.Errorf("cleanup task_queue on %s: %v", be.Name, err)
				}
			}()

			if _, err := fixtureDB.ExecContext(context.Background(), plugin.Rebind(
				`INSERT INTO task_queue (tenant_id, queue_name, job_id, status, started_at, run_id)
				 VALUES ($1, $2, $3, 'dispatched', now(), $4)`, dialect),
				tenant, "finalize-test", job, runID); err != nil {
				t.Fatalf("insert dispatched job on %s: %v", be.Name, err)
			}

			if err := p.ObserveFinalize(context.Background(), runID, "failed"); err != nil {
				t.Fatalf("ObserveFinalize on %s: %v", be.Name, err)
			}

			var status string
			if err := fixtureDB.QueryRowContext(context.Background(), plugin.Rebind(
				`SELECT status FROM task_queue WHERE tenant_id = $1 AND queue_name = $2 AND job_id = $3`,
				dialect), tenant, "finalize-test", job).Scan(&status); err != nil {
				t.Fatalf("read back on %s: %v", be.Name, err)
			}
			if status != "failed" {
				t.Errorf("on %s status = %q, want \"failed\"", be.Name, status)
			}
		})
	}
}
