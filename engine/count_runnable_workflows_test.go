package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// seedRunnableDef deploys the definition StartNewRun needs. setupPluginDepsDB
// creates the schema but no rows, and workflow_instances carries an FK on
// (tenant_id, def_name, def_version) -- so without this every StartNewRun below
// fails with a foreign key violation on all three dialects.
func seedRunnableDef(t *testing.T, store WorkflowStore, name string) {
	t.Helper()
	if err := store.DeployWorkflowDef(context.Background(), &WorkflowDef{
		Name: name, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
}

// TestCountRunnableWorkflowsAgreesWithTheClaim is the property that makes the
// count worth logging: it must answer the question the claim asks.
//
// A count that disagrees with ClaimWorkflows' predicate is worse than no count,
// because it is a number a human will act on. The two ways to get it wrong are
// opposite and both plausible:
//
//   - omit task_queue and it OVER-reports -- rows a worker is correctly
//     declining to claim read as work it is failing to pick up. This was the
//     one flaw in cleat#923 as filed.
//   - omit the tenant scoping and it over-reports across tenants, which on SQL
//     Server is not hypothetical: dbo.fn_tenant_filter is off for the admin
//     role, so `AND tenant_id` IS the whole of the scoping there (3.91).
//
// Asserted by agreement rather than by reading the SQL, because the SQL differs
// per dialect: PostgreSQL leans on RLS, MySQL and SQL Server carry an explicit
// predicate. Agreement is the invariant; the spelling is not.
func TestCountRunnableWorkflowsAgreesWithTheClaim(t *testing.T) {
	for _, backend := range pluginDepsBackends() {
		t.Run(backend.name, func(t *testing.T) {
			_, store := setupPluginDepsDB(t, backend)
			ctx := context.Background()

			counter, ok := store.(interface {
				CountRunnableWorkflows(context.Context) (int, error)
			})
			if !ok {
				t.Fatalf("%s: store does not implement CountRunnableWorkflows", backend.name)
			}

			seedRunnableDef(t, store, "count-agreement-def")

			// Nothing runnable yet: the count must be the claim's answer, not a
			// guess. Both are read before any rows exist so that a count which
			// ignores its predicate entirely -- SELECT count(*) with no WHERE --
			// is caught here rather than passing on an empty table.
			before, err := counter.CountRunnableWorkflows(ctx)
			if err != nil {
				t.Fatalf("CountRunnableWorkflows: %v", err)
			}

			const n = 4
			for i := 0; i < n; i++ {
				if _, _, err := store.StartNewRun(ctx, "", "count-agreement-def", 1,
					json.RawMessage(`{}`), fmt.Sprintf("countable-%d-%d", i, time.Now().UnixNano()),
					DefaultTenantUUID, 0); err != nil {
					t.Fatalf("StartNewRun[%d]: %v", i, err)
				}
			}

			got, err := counter.CountRunnableWorkflows(ctx)
			if err != nil {
				t.Fatalf("CountRunnableWorkflows: %v", err)
			}
			if got != before+n {
				t.Errorf("count went %d -> %d after creating %d runnable workflows, want %d.\n\n"+
					"The count must move exactly with the runnable set. A count that does not "+
					"track it is a number someone will act on that does not mean what it says.",
					before, got, n, before+n)
			}

			// The claim is the authority. Whatever it takes is what was
			// runnable, so the count taken immediately before must equal it.
			claimed, err := store.ClaimWorkflows(ctx, "worker-count-agreement", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows: %v", err)
			}
			if len(claimed) != got {
				t.Errorf("the count said %d runnable, the claim took %d.\n\n"+
					"These must agree: the count exists so a worker can tell 'nothing to run' "+
					"from 'everything was locked', and a count using a different predicate than "+
					"the claim answers a different question. Check task_queue and the tenant "+
					"scoping, which differ per dialect.", got, len(claimed))
			}

			// And after the claim, nothing is runnable: the rows are 'running'.
			after, err := counter.CountRunnableWorkflows(ctx)
			if err != nil {
				t.Fatalf("CountRunnableWorkflows: %v", err)
			}
			if after != 0 {
				t.Errorf("count = %d after everything was claimed, want 0 -- the status predicate "+
					"is not being applied", after)
			}
		})
	}
}

// TestCountRunnableWorkflowsIsBlindToOtherTaskQueues pins the half that
// over-reports, and it is the half a naive implementation gets wrong.
//
// A worker serving queue A must not be told that runnable work exists when
// every runnable row is on queue B. It would be true that work exists and
// false that this worker is failing to claim it -- which is precisely the
// misreading the log line would cause.
func TestCountRunnableWorkflowsIsBlindToOtherTaskQueues(t *testing.T) {
	for _, backend := range pluginDepsBackends() {
		t.Run(backend.name, func(t *testing.T) {
			db, store := setupPluginDepsDB(t, backend)
			ctx := context.Background()

			counter, ok := store.(interface {
				CountRunnableWorkflows(context.Context) (int, error)
			})
			if !ok {
				t.Fatalf("%s: store does not implement CountRunnableWorkflows", backend.name)
			}

			seedRunnableDef(t, store, "other-queue-def")
			if _, _, err := store.StartNewRun(ctx, "", "other-queue-def", 1,
				json.RawMessage(`{}`), fmt.Sprintf("otherqueue-%d", time.Now().UnixNano()),
				DefaultTenantUUID, 0); err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			base, err := counter.CountRunnableWorkflows(ctx)
			if err != nil {
				t.Fatalf("CountRunnableWorkflows: %v", err)
			}
			if base == 0 {
				t.Fatalf("no runnable work before moving it, so this test cannot measure the move")
			}

			// Move every runnable row onto a queue this store does not serve.
			//
			// Through prepareRawAccess, not the bare pool. SQL Server's FILTER
			// PREDICATE hides every row from a connection with no tenant session
			// context, so a direct UPDATE matches ZERO rows and silently does
			// nothing -- and the test then reports a count that never moved as
			// though the count were broken. That is a failure of the test rather
			// than of the code, and it is what happened on the first run here.
			var exec interface {
				ExecContext(context.Context, string, ...any) (sql.Result, error)
			} = db
			if backend.prepareRawAccess != nil {
				conn := backend.prepareRawAccess(t, db)
				defer conn.Close()
				exec = conn
			}
			res, err := exec.ExecContext(ctx, `UPDATE workflow_instances
				SET task_queue = 'some-other-queue'
				WHERE status IN ('ready','terminating')`)
			if err != nil {
				t.Fatalf("moving rows to another queue: %v", err)
			}
			// Assert the UPDATE actually moved something. Without this the
			// assertion below can only fail for the right reason by luck.
			if moved, mErr := res.RowsAffected(); mErr == nil && moved == 0 {
				t.Fatalf("the UPDATE moved 0 rows, so nothing was moved off this worker's queue " +
					"and the assertion below would pass or fail for reasons unrelated to the count")
			}

			got, err := counter.CountRunnableWorkflows(ctx)
			if err != nil {
				t.Fatalf("CountRunnableWorkflows: %v", err)
			}
			if got != 0 {
				t.Errorf("count = %d after every runnable row moved to a queue this worker does "+
					"not serve, want 0.\n\n"+
					"The count omits task_queue, so it over-reports: a worker would be told it is "+
					"failing to claim work it is correctly declining. That was the flaw in "+
					"cleat#923 as filed.", got)
			}
		})
	}
}
