// A real-database counterpart to TestCleanupWithStaleWorkflowRefs and
// TestCleanupWithActiveWorkflowRefs (blobstore_backend_test.go), which run
// against a fake driver that pattern-matches the query string and would
// accept SQL no server would -- exactly the gap jobqueue's own multidb
// suite documents for abandonedJobsQuery, and staleWorkflowRefs is the same
// shape: a per-dialect plugin.Query arm reading workflow_instances (a core
// table, not this plugin's own) that nothing had run against a real MSSQL
// server before cleat#2125.
//
// cleat#2125: on a 'plain'-form SQL Server database (migration 075's
// default since it landed, i.e. every deployment that has not separately
// applied migrations/mssql/optional/cross_tenant_claim.sql), the MSSQL arm
// of staleWorkflowRefs used to read dbo.workflow_instances directly under
// plugin.AcrossAllTenants, a context that sets SESSION_CONTEXT('cross_tenant'),
// a key dbo.fn_tenant_filter does not read. A tenant-less reader of a table
// guarded by that predicate sees zero rows, so `workflow_id NOT IN (SELECT id
// FROM workflow_instances WHERE status IN ('ready','running'))` was true for
// every row -- this DELETEd the blob reference of a workflow that was still
// running, on every sweep tick.
//
// THE FIX IS NOT A SECOND PREDICATE, cleat-review having measured what the
// first attempt (migration 102: an OR USER_NAME() = 'cleat_dispatcher'
// disjunct on dbo.fn_tenant_filter, PostgreSQL 073's shape) does to every
// OTHER table sharing that predicate -- Index Seek to Index Scan on a
// COUNT(*), 4 reads to 1461 on a TOP 50, on all fourteen core tables, not
// just this one. See plugins/blobstore/background.go's
// sweepStaleWorkflowRefsMSSQL: it gathers every tenant's in-flight ids one
// tenant at a time (workflow_blob_refs carries no tenant_id for a per-tenant
// DELETE to scope, so the ids must be gathered whole before anything is
// deleted), then deletes what nothing protects.
package blobstore

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/plugintest"
)

func TestStaleWorkflowRefs_SparesAnInFlightWorkflow_MultiBackend(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			// baseCtx carries no tenant marker: cleanupExpired's second
			// parameter, used only by SQL Server's per-tenant sweep, which
			// layers plugin.ForTenant on top of it per admin.tenants row.
			// Passing the AcrossAllTenants-marked ctx here instead would make
			// every ForTenant a no-op -- see sweepStaleWorkflowRefsMSSQL.
			baseCtx := context.Background()
			ctx := plugin.AcrossAllTenants(baseCtx,
				"blobstore stale-workflow-refs test: cleanupExpired runs over "+
					"every tenant's refs, as Run does")
			dialect := plugin.Dialect(string(be.Dialect))
			p := &Plugin{dialect: dialect}

			// The ENGINE schema, not just the plugin's own -- staleWorkflowRefs
			// joins workflow_instances, a core table no plugin migration
			// creates. See blobstore_expiry_decrement_multidb_test.go's own
			// comment on this same requirement. It must also run before
			// CrossTenantConn: on MSSQL that now routes through MSSQLAdminDB,
			// which Fatals if core RLS is enforced but migration 012's
			// cleat_admin role does not exist yet. cleat#2226.
			testutil.SetupMinimalSchema(t, be.DB, be.Dialect)
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("blobstore migrations on %s: %v", be.Name, err)
			}
			fixtureDB := be.CrossTenantConn(t, context.Background(),
				"blobstore stale-workflow-refs fixture: seeds workflow_instances "+
					"and workflow_blob_refs rows for workflows it invents")
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}
			p.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

			tenant := uuid.New()
			defName := "blobstore-stale-refs-test-" + uuid.New().String()
			liveRunID := "blobstore-live-" + uuid.New().String()
			doneRunID := "blobstore-done-" + uuid.New().String()
			shaLive := make([]byte, 32)
			shaDone := make([]byte, 32)
			shaLive[0], shaDone[0] = 0x44, 0x55

			// The tenant must exist in admin.tenants: on SQL Server,
			// sweepStaleWorkflowRefsMSSQL enumerates plugin.AllTenantIDs and
			// reads each one's workflow_instances under its own
			// SESSION_CONTEXT -- a tenant with no admin.tenants row is
			// invisible to that enumeration, so its "in-flight" workflow
			// would never be gathered and would be deleted right along with
			// the genuinely done one, defeating the very thing this test
			// checks. PostgreSQL does not consult admin.tenants for this
			// statement, but the row does no harm there either.
			//
			// MYSQL IS DIFFERENT: tiers.yaml's D1 makes a second tenant
			// impossible to create at all --
			// migrations/mysql/038_single_tenant_guard.sql enforces exactly
			// one row in tenants with a UNIQUE index on a constant column,
			// so inserting a second one fails with "Duplicate entry '1' for
			// key '...single_tenant_only_see_tiers_yaml_d1'". Nothing here
			// needs a second tenant on MySQL -- its arm of staleWorkflowRefs
			// never reads this table either -- so this uses the pre-existing
			// default tenant id there instead of creating one, matching
			// jobqueue's seedTenant/cleanupTenant (plugins/jobqueue's own
			// multidb test).
			if be.Dialect == testutil.DialectMySQL {
				tenant = uuid.MustParse("00000000-0000-0000-0000-000000000000")
			} else {
				tenantsTable := map[testutil.Dialect]string{
					testutil.DialectPostgres: `INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`,
					testutil.DialectMSSQL:    `INSERT INTO admin.tenants (tenant_id, name) VALUES (@p1, @p2)`,
				}[be.Dialect]
				tenantName := "blobstore-2125-" + tenant.String()
				if _, err := fixtureDB.ExecContext(ctx, tenantsTable, tenant.String(), tenantName); err != nil {
					t.Fatalf("seed admin.tenants on %s: %v", be.Name, err)
				}
			}

			defer func() {
				bg := context.Background()
				if _, err := plugintest.ExecRebound(t, bg, fixtureDB, dialect,
					`DELETE FROM workflow_blob_refs WHERE workflow_id IN ($1, $2)`,
					liveRunID, doneRunID); err != nil {
					t.Errorf("cleanup workflow_blob_refs on %s: %v", be.Name, err)
				}
				if _, err := plugintest.ExecRebound(t, bg, fixtureDB, dialect,
					`DELETE FROM workflow_instances WHERE id IN ($1, $2)`,
					liveRunID, doneRunID); err != nil {
					t.Errorf("cleanup workflow_instances on %s: %v", be.Name, err)
				}
				if _, err := plugintest.ExecRebound(t, bg, fixtureDB, dialect,
					`DELETE FROM workflow_defs WHERE name = $1`, defName); err != nil {
					t.Errorf("cleanup workflow_defs on %s: %v", be.Name, err)
				}
				// A no-op on MySQL: nothing was inserted into tenants there,
				// and deleting the sole row would break every other MySQL
				// fixture that assumes it exists.
				if be.Dialect != testutil.DialectMySQL {
					tenantsDelete := map[testutil.Dialect]string{
						testutil.DialectPostgres: `DELETE FROM admin.tenants WHERE tenant_id = $1`,
						testutil.DialectMSSQL:    `DELETE FROM admin.tenants WHERE tenant_id = @p1`,
					}[be.Dialect]
					if _, err := fixtureDB.ExecContext(bg, tenantsDelete, tenant.String()); err != nil {
						t.Errorf("cleanup admin.tenants on %s: %v", be.Name, err)
					}
				}
			}()

			// tenant_id must match across workflow_defs and workflow_instances:
			// workflow_instances_def_fkey / fk_instances_def is a composite
			// (tenant_id, def_name, def_version) FK since migration 035/038
			// (workflow_defs_tenant_in_key).
			if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect,
				`INSERT INTO workflow_defs (name, version, wasm_bytes, tenant_id) VALUES ($1, $2, $3, $4)`,
				defName, 1, []byte{0}, tenant); err != nil {
				t.Fatalf("insert workflow_defs on %s: %v", be.Name, err)
			}

			if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect,
				`INSERT INTO workflow_instances (id, def_name, def_version, tenant_id, status) VALUES ($1, $2, $3, $4, 'running')`,
				liveRunID, defName, 1, tenant); err != nil {
				t.Fatalf("insert live workflow_instances on %s: %v", be.Name, err)
			}
			if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect,
				`INSERT INTO workflow_instances (id, def_name, def_version, tenant_id, status) VALUES ($1, $2, $3, $4, 'done')`,
				doneRunID, defName, 1, tenant); err != nil {
				t.Fatalf("insert done workflow_instances on %s: %v", be.Name, err)
			}

			if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect,
				`INSERT INTO workflow_blob_refs (workflow_id, sha256) VALUES ($1, $2)`,
				liveRunID, shaLive); err != nil {
				t.Fatalf("insert live workflow_blob_refs on %s: %v", be.Name, err)
			}
			if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect,
				`INSERT INTO workflow_blob_refs (workflow_id, sha256) VALUES ($1, $2)`,
				doneRunID, shaDone); err != nil {
				t.Fatalf("insert done workflow_blob_refs on %s: %v", be.Name, err)
			}

			staleRefs, _, _, err := p.cleanupExpired(ctx, baseCtx)
			if err != nil {
				t.Fatalf("cleanupExpired on %s: %v", be.Name, err)
			}
			if staleRefs != 1 {
				t.Errorf("cleanupExpired staleRefs = %d on %s, want 1 (the done "+
					"workflow's ref only)", staleRefs, be.Name)
			}

			var liveCount, doneCount int
			if err := plugintest.QueryRowRebound(t, ctx, fixtureDB, dialect,
				`SELECT COUNT(*) FROM workflow_blob_refs WHERE workflow_id = $1`,
				liveRunID).Scan(&liveCount); err != nil {
				t.Fatalf("count live ref on %s: %v", be.Name, err)
			}
			if liveCount != 1 {
				t.Errorf("on %s, live workflow's blob ref count = %d, want 1 -- "+
					"cleanupExpired deleted the reference of a workflow that is "+
					"still running (cleat#2125)", be.Name, liveCount)
			}
			if err := plugintest.QueryRowRebound(t, ctx, fixtureDB, dialect,
				`SELECT COUNT(*) FROM workflow_blob_refs WHERE workflow_id = $1`,
				doneRunID).Scan(&doneCount); err != nil {
				t.Fatalf("count done ref on %s: %v", be.Name, err)
			}
			if doneCount != 0 {
				t.Errorf("on %s, done workflow's blob ref count = %d, want 0 -- "+
					"cleanupExpired should have removed it", be.Name, doneCount)
			}
		})
	}
}
