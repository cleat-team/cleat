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
// running, on every sweep tick. Migration 102 fixes it the same way
// PostgreSQL's 073 does: admin.fn_in_flight_workflow_ids() runs WITH EXECUTE
// AS 'cleat_dispatcher', a NOLOGIN principal dbo.fn_tenant_filter admits by
// name (OR USER_NAME() = N'cleat_dispatcher'), so the impersonated read sees
// every tenant's rows regardless of the caller's own session context.
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
)

func TestStaleWorkflowRefs_SparesAnInFlightWorkflow_MultiBackend(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		t.Run(be.Name, func(t *testing.T) {
			fixtureDB := be.CrossTenantConn(t, context.Background(),
				"blobstore stale-workflow-refs fixture: seeds workflow_instances "+
					"and workflow_blob_refs rows for workflows it invents")
			defer be.Cleanup()
			ctx := plugin.AcrossAllTenants(context.Background(),
				"blobstore stale-workflow-refs test: cleanupExpired runs over "+
					"every tenant's refs, as Run does")
			dialect := plugin.Dialect(string(be.Dialect))
			p := &Plugin{dialect: dialect}

			// The ENGINE schema, not just the plugin's own -- staleWorkflowRefs
			// joins workflow_instances, a core table no plugin migration
			// creates. See blobstore_expiry_decrement_multidb_test.go's own
			// comment on this same requirement.
			testutil.SetupMinimalSchema(t, be.DB, be.Dialect)
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("blobstore migrations on %s: %v", be.Name, err)
			}
			// SQL Server needs no admin bypass here: staleWorkflowRefs' MSSQL
			// arm reads admin.fn_in_flight_workflow_ids(), which runs as
			// cleat_dispatcher regardless of which principal calls it
			// (migration 102). Any connection can call it.
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}
			p.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

			tenant := uuid.New()
			defName := "blobstore-stale-refs-test-" + uuid.New().String()
			liveRunID := "blobstore-live-" + uuid.New().String()
			doneRunID := "blobstore-done-" + uuid.New().String()
			shaLive := make([]byte, 32)
			shaDone := make([]byte, 32)
			shaLive[0], shaDone[0] = 0x44, 0x55

			defer func() {
				bg := context.Background()
				if _, err := fixtureDB.ExecContext(bg, plugin.Rebind(
					`DELETE FROM workflow_blob_refs WHERE workflow_id IN ($1, $2)`, dialect),
					liveRunID, doneRunID); err != nil {
					t.Errorf("cleanup workflow_blob_refs on %s: %v", be.Name, err)
				}
				if _, err := fixtureDB.ExecContext(bg, plugin.Rebind(
					`DELETE FROM workflow_instances WHERE id IN ($1, $2)`, dialect),
					liveRunID, doneRunID); err != nil {
					t.Errorf("cleanup workflow_instances on %s: %v", be.Name, err)
				}
				if _, err := fixtureDB.ExecContext(bg, plugin.Rebind(
					`DELETE FROM workflow_defs WHERE name = $1`, dialect), defName); err != nil {
					t.Errorf("cleanup workflow_defs on %s: %v", be.Name, err)
				}
			}()

			// tenant_id must match across workflow_defs and workflow_instances:
			// workflow_instances_def_fkey / fk_instances_def is a composite
			// (tenant_id, def_name, def_version) FK since migration 035/038
			// (workflow_defs_tenant_in_key).
			if _, err := fixtureDB.ExecContext(ctx, plugin.Rebind(
				`INSERT INTO workflow_defs (name, version, wasm_bytes, tenant_id) VALUES ($1, $2, $3, $4)`,
				dialect), defName, 1, []byte{0}, tenant); err != nil {
				t.Fatalf("insert workflow_defs on %s: %v", be.Name, err)
			}

			if _, err := fixtureDB.ExecContext(ctx, plugin.Rebind(
				`INSERT INTO workflow_instances (id, def_name, def_version, tenant_id, status) VALUES ($1, $2, $3, $4, 'running')`,
				dialect), liveRunID, defName, 1, tenant); err != nil {
				t.Fatalf("insert live workflow_instances on %s: %v", be.Name, err)
			}
			if _, err := fixtureDB.ExecContext(ctx, plugin.Rebind(
				`INSERT INTO workflow_instances (id, def_name, def_version, tenant_id, status) VALUES ($1, $2, $3, $4, 'done')`,
				dialect), doneRunID, defName, 1, tenant); err != nil {
				t.Fatalf("insert done workflow_instances on %s: %v", be.Name, err)
			}

			if _, err := fixtureDB.ExecContext(ctx, plugin.Rebind(
				`INSERT INTO workflow_blob_refs (workflow_id, sha256) VALUES ($1, $2)`, dialect),
				liveRunID, shaLive); err != nil {
				t.Fatalf("insert live workflow_blob_refs on %s: %v", be.Name, err)
			}
			if _, err := fixtureDB.ExecContext(ctx, plugin.Rebind(
				`INSERT INTO workflow_blob_refs (workflow_id, sha256) VALUES ($1, $2)`, dialect),
				doneRunID, shaDone); err != nil {
				t.Fatalf("insert done workflow_blob_refs on %s: %v", be.Name, err)
			}

			staleRefs, _, _, err := p.cleanupExpired(ctx)
			if err != nil {
				t.Fatalf("cleanupExpired on %s: %v", be.Name, err)
			}
			if staleRefs != 1 {
				t.Errorf("cleanupExpired staleRefs = %d on %s, want 1 (the done "+
					"workflow's ref only)", staleRefs, be.Name)
			}

			var liveCount, doneCount int
			if err := fixtureDB.QueryRowContext(ctx, plugin.Rebind(
				`SELECT COUNT(*) FROM workflow_blob_refs WHERE workflow_id = $1`, dialect),
				liveRunID).Scan(&liveCount); err != nil {
				t.Fatalf("count live ref on %s: %v", be.Name, err)
			}
			if liveCount != 1 {
				t.Errorf("on %s, live workflow's blob ref count = %d, want 1 -- "+
					"cleanupExpired deleted the reference of a workflow that is "+
					"still running (cleat#2125)", be.Name, liveCount)
			}
			if err := fixtureDB.QueryRowContext(ctx, plugin.Rebind(
				`SELECT COUNT(*) FROM workflow_blob_refs WHERE workflow_id = $1`, dialect),
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
