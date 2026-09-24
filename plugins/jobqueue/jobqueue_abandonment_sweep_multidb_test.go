// Multi-backend tests for the abandonment sweep and the finalize
// write-back, against real databases. cleat#1715.
//
// abandonedJobsQuery is exactly the shape cleat#1133/#1134/#1141 already
// burned this plugin on twice: a per-dialect statement (LIMIT and subquery
// syntax differ enough that plugin.Rebind cannot paper over them) that
// nothing had ever executed against a real server. PostgreSQL's arm runs
// through admin.in_flight_workflow_ids() (migration 073), an
// EXECUTE-AS/SECURITY-DEFINER-impersonated principal RLS admits by name;
// MySQL's is a direct subquery against workflow_instances, since it has no
// row-level security to work around. SQL Server has no arm here at all,
// cleat#2125 -- see sweepAbandonedJobsPerTenant in background.go: task_queue
// is TenantScoped, so the fix loops the same direct-subquery statement once
// per tenant instead of impersonating a bypass principal, after cleat-review
// measured what an impersonation disjunct on dbo.fn_tenant_filter (the first
// attempt, migration 102) does to every OTHER table sharing that predicate.
// All three shapes are exercised here -- the fake-driver suite proves the
// GUARD LOGIC (see TestSweepAbandonedJobs/execSweepAbandoned in
// jobqueue_behavioral_test.go), but it pattern-matches the query string and
// would accept SQL no database would. Only a real server settles whether the
// statement parses.
package jobqueue

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// defaultTenantID is the one tenant migrations/mysql/002_defaults.sql (and
// its Postgres/MSSQL counterparts) seed on every database. seedTenant
// returns this on MySQL instead of creating a second one -- see its own
// comment for why.
var defaultTenantID = uuid.MustParse("00000000-0000-0000-0000-000000000000")

// seedTenant adds tenant to admin.tenants and returns the id fixtures should
// actually use. Needed only so SQL Server's sweepAbandonedJobsPerTenant --
// which enumerates plugin.AllTenantIDs rather than reading task_queue's
// tenant_id column directly -- visits this test's tenant at all; PostgreSQL
// never consults that table for this sweep, but the row does no harm there
// either.
//
// MYSQL IS DIFFERENT, and not just "consults no such table": tiers.yaml's D1
// makes a second tenant impossible to create at all.
// migrations/mysql/038_single_tenant_guard.sql enforces exactly one row in
// tenants with a UNIQUE index on a constant column, so inserting a second
// one fails with "Duplicate entry '1' for key
// '...single_tenant_only_see_tiers_yaml_d1'". Nothing here needs a SECOND
// tenant on MySQL in the first place -- only SQL Server's arm ever reads
// this table for this sweep -- so on MySQL this returns the pre-existing
// default tenant id unchanged, for callers to use as the fixture's tenant_id
// instead of the one they generated.
func seedTenant(t *testing.T, conn *sql.Conn, ctx context.Context, testDialect testutil.Dialect, dialect plugin.Dialect, tenant uuid.UUID) uuid.UUID {
	t.Helper()
	if testDialect == testutil.DialectMySQL {
		return defaultTenantID
	}
	insert := map[testutil.Dialect]string{
		testutil.DialectPostgres: `INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`,
		testutil.DialectMSSQL:    `INSERT INTO admin.tenants (tenant_id, name) VALUES (@p1, @p2)`,
	}[testDialect]
	if _, err := conn.ExecContext(ctx, insert, tenant.String(), "jobqueue-2125-"+tenant.String()); err != nil {
		t.Fatalf("seed admin.tenants for %s: %v", dialect, err)
	}
	return tenant
}

// cleanupTenant is seedTenant's teardown counterpart. A no-op on MySQL:
// seedTenant created nothing there, and deleting the sole row in tenants
// would break every other MySQL fixture that assumes it exists.
func cleanupTenant(t *testing.T, conn *sql.Conn, testDialect testutil.Dialect, tenant uuid.UUID) {
	t.Helper()
	if testDialect == testutil.DialectMySQL {
		return
	}
	del := map[testutil.Dialect]string{
		testutil.DialectPostgres: `DELETE FROM admin.tenants WHERE tenant_id = $1`,
		testutil.DialectMSSQL:    `DELETE FROM admin.tenants WHERE tenant_id = @p1`,
	}[testDialect]
	if _, err := conn.ExecContext(context.Background(), del, tenant.String()); err != nil {
		t.Errorf("cleanup admin.tenants: %v", err)
	}
}

// TestSweepAbandonedJobs_MultiBackend covers the case that needs no
// workflow_instances fixture at all: a dispatched job whose run_id names no
// row anywhere is the plainest "gone" case the sweep exists for, and an
// empty table is enough to prove admin.in_flight_workflow_ids() (Postgres),
// the per-tenant loop (SQL Server) and the direct workflow_instances
// subquery (MySQL) all resolve without error.
func TestSweepAbandonedJobs_MultiBackend(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			// baseCtx carries no tenant marker: sweepAbandonedJobs's SQL
			// Server arm loops plugin.ForTenant on top of it per
			// admin.tenants row, which a cross-tenant-marked ctx would defeat
			// (beginTenantTx checks the bypass before the tenant). ctx is
			// still what fixture setup below uses.
			baseCtx := context.Background()
			ctx := plugin.AcrossAllTenants(baseCtx,
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
			// this file. It also has to run before CrossTenantConn: on MSSQL
			// that now routes through MSSQLAdminDB, which Fatals if core RLS
			// is enforced but migration 012's cleat_admin role does not exist
			// yet. cleat#2226.
			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("jobqueue migrations on %s: %v", be.Name, err)
			}
			fixtureDB := be.CrossTenantConn(t, context.Background(),
				"jobqueue abandonment sweep fixture: seeds a row for a tenant it invents")
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: plugin.Dialect(be.Dialect)}
			p.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

			tenant := uuid.New()
			job := uuid.New()
			runID := "gone-" + uuid.New().String()

			// The tenant must exist in admin.tenants (or MySQL's tenants) for
			// SQL Server's per-tenant loop to visit it at all -- see
			// sweepAbandonedJobsPerTenant in background.go.
			tenant = seedTenant(t, fixtureDB, ctx, be.Dialect, dialect, tenant)

			defer func() {
				if _, err := fixtureDB.ExecContext(context.Background(),
					plugin.Rebind(`DELETE FROM task_queue WHERE tenant_id = $1`, dialect), tenant); err != nil {
					t.Errorf("cleanup task_queue on %s: %v", be.Name, err)
				}
				cleanupTenant(t, fixtureDB, be.Dialect, tenant)
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
			n := p.sweepAbandonedJobs(baseCtx)
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
			defer be.Cleanup()
			// baseCtx carries no tenant marker -- see the identical comment in
			// TestSweepAbandonedJobs_MultiBackend above.
			baseCtx := context.Background()
			ctx := plugin.AcrossAllTenants(baseCtx,
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
			// this file. It also has to run before CrossTenantConn: on MSSQL
			// that now routes through MSSQLAdminDB, which Fatals if core RLS
			// is enforced but migration 012's cleat_admin role does not exist
			// yet. cleat#2226.
			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("jobqueue migrations on %s: %v", be.Name, err)
			}
			fixtureDB := be.CrossTenantConn(t, context.Background(),
				"jobqueue abandonment sweep fixture: seeds task_queue and workflow_instances "+
					"rows for a tenant and run it invents")
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: plugin.Dialect(be.Dialect)}
			p.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

			tenant := uuid.New()
			defName := "sweep-arm-test-" + uuid.New().String()
			runID := "in-flight-" + uuid.New().String()
			job := uuid.New()

			// The tenant must exist in admin.tenants for SQL Server's
			// per-tenant loop to visit it -- see seedTenant's own comment.
			tenant = seedTenant(t, fixtureDB, ctx, be.Dialect, dialect, tenant)

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
				cleanupTenant(t, fixtureDB, be.Dialect, tenant)
			}()

			// tenant_id here must equal the value the workflow_instances row
			// below carries: workflow_instances_def_fkey / fk_instances_def is
			// a composite (tenant_id, def_name, def_version) FK since migration
			// 035/038 (workflow_defs_tenant_in_key) folded tenant_id into it, so
			// a workflow_defs row at its OWN tenant_id default while the
			// instance carries a different one is not a visibility question at
			// all -- it is a row the constraint genuinely does not consider a
			// match, on every dialect, and the earlier version of this fixture
			// only got away with leaving both at the default because neither
			// side ever set tenant_id to anything else.
			if _, err := fixtureDB.ExecContext(ctx, plugin.Rebind(
				`INSERT INTO workflow_defs (name, version, wasm_bytes, tenant_id) VALUES ($1, $2, $3, $4)`,
				dialect), defName, 1, []byte{0}, tenant); err != nil {
				t.Fatalf("insert workflow_defs on %s: %v", be.Name, err)
			}
			// status is left to its column default, which is 'ready' on every
			// dialect -- one of the two values the sweep treats as in flight.
			if _, err := fixtureDB.ExecContext(ctx, plugin.Rebind(
				`INSERT INTO workflow_instances (id, def_name, def_version, tenant_id) VALUES ($1, $2, $3, $4)`,
				dialect), runID, defName, 1, tenant); err != nil {
				t.Fatalf("insert workflow_instances on %s: %v", be.Name, err)
			}
			if _, err := fixtureDB.ExecContext(ctx, plugin.Rebind(
				`INSERT INTO task_queue (tenant_id, queue_name, job_id, status, started_at, run_id)
				 VALUES ($1, $2, $3, 'dispatched', now(), $4)`, dialect),
				tenant, "sweep-arm-test", job, runID); err != nil {
				t.Fatalf("insert dispatched job on %s: %v", be.Name, err)
			}

			n := p.sweepAbandonedJobs(baseCtx)
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
			defer be.Cleanup()
			dialect := plugin.Dialect(string(be.Dialect))
			p := &Plugin{dialect: dialect}

			// Before CrossTenantConn -- see TestSweepAbandonedJobs_MultiBackend
			// above.
			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			if err := plugin.RunMigrations(context.Background(), be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("jobqueue migrations on %s: %v", be.Name, err)
			}
			fixtureDB := be.CrossTenantConn(t, context.Background(),
				"jobqueue finalize-observer fixture: seeds a row for a tenant it invents")
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
