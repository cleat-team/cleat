package engine

// IMPROVEMENT-PLAN 3.11: four MySQL store methods touch tenant-scoped tables
// with no tenant_id in the statement. MySQL has no row-level security, so on
// that dialect a missing Go-level filter is the whole of the isolation.
//
// The tests below are three-dialect on purpose rather than MySQL-only. The
// audit that produced 3.11 read the MySQL store, but the same four statements
// are unscoped in the PostgreSQL and SQL Server stores too; what differs is
// what sits underneath them, and that turns out to differ per method rather
// than per dialect:
//
//   - PostgresStore.QueueDepth and DeleteExpiredEvents run inside
//     beginTxWithRLS, so the policies scope them and the missing predicate
//     costs nothing there.
//   - PostgresStore.GetWASMLength and GetAllowedSignalCallers use s.db
//     directly, with no transaction and therefore no cleat.tenant_id.
//   - MSSQLStore sets its session context per connection, so its security
//     policies apply. Since 2.71 the test schema is built from the shipped
//     migrations and defines all seven of them, so what runs here is now the
//     same arrangement production runs; before that the test schema had no
//     policies and the Go-level filter was the only thing these assertions
//     could see.
//
// Running every dialect means the test says which of those is true today
// rather than assuming, and the Go-level filter is what makes all three agree.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

const (
	unscopedTenantA = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
	unscopedTenantB = "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb"
)

// twoTenantStores returns a store per tenant plus an administrative DB handle
// for the fixture work that has no store method.
func twoTenantStores(t *testing.T, backend StoreBackend) (a, b WorkflowStore, admin *sql.DB) {
	t.Helper()
	tb, ok := backend.(tenantSetupBackend)
	if !ok {
		t.Fatalf("backend %T does not implement SetupForTenant; this test needs two tenants", backend)
	}
	storeA, teardownA := tb.SetupForTenant(t, unscopedTenantA)
	t.Cleanup(teardownA)
	storeB, teardownB := tb.SetupForTenant(t, unscopedTenantB)
	t.Cleanup(teardownB)

	// Named admin and, since adminDBFor, actually is one on every dialect. It
	// used to be a plain MSSQLTestDB pool here, and on the shipped schema a
	// plain pool is subject to the security policies -- so the fixture UPDATE
	// in setAllowedSignals matched no rows and reported no error, and the test
	// failed one assertion later with "tenant B cannot read its own allowed
	// callers".
	return storeA, storeB, adminDBFor(t, backend)
}

// seedReadyRuns deploys a definition for one tenant and starts n ready
// workflows on it, returning the definition name and the run IDs.
func seedReadyRuns(t *testing.T, store WorkflowStore, tenant, defName string, n int) []string {
	t.Helper()
	ctx := context.Background()
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d, byte(len(defName))},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef(%s): %v", defName, err)
	}
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id, _, err := store.StartNewRun(ctx, "", defName, 1,
			json.RawMessage(`{}`), fmt.Sprintf("%s-key-%d-%d", defName, i, time.Now().UnixNano()),
			tenant, 0)
		if err != nil {
			t.Fatalf("StartNewRun(%s, %d): %v", defName, i, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// TestQueueDepthCountsOnlyTheCallersTenant — QueueDepth drives autoscaling and
// the admin dashboard's backlog figure. Counting every tenant's ready
// workflows makes one tenant's burst look like everyone's.
func TestQueueDepthCountsOnlyTheCallersTenant(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			storeA, storeB, _ := twoTenantStores(t, backend)
			ctx := context.Background()

			seedReadyRuns(t, storeA, unscopedTenantA, "qd-def-a", 2)
			seedReadyRuns(t, storeB, unscopedTenantB, "qd-def-b", 3)

			gotA, err := storeA.QueueDepth(ctx)
			if err != nil {
				t.Fatalf("QueueDepth(A): %v", err)
			}
			if gotA != 2 {
				t.Errorf("tenant A's queue depth is %d, want 2: it is counting tenant B's "+
					"three ready workflows as well", gotA)
			}

			gotB, err := storeB.QueueDepth(ctx)
			if err != nil {
				t.Fatalf("QueueDepth(B): %v", err)
			}
			if gotB != 3 {
				t.Errorf("tenant B's queue depth is %d, want 3", gotB)
			}
		})
	}
}

// TestGetWASMLengthIsScopedToTenant — definition names are chosen by whoever
// deploys, so asking for another tenant's name is not an attack, it is what
// happens when two customers both call something "order-processor". The size
// of their compiled WASM is not the caller's to learn.
func TestGetWASMLengthIsScopedToTenant(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			storeA, storeB, _ := twoTenantStores(t, backend)
			ctx := context.Background()

			const defB = "wasmlen-def-b"
			bBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0xBB, 0xBB, 0xBB, 0xBB}
			if err := storeB.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: defB, Version: 1, WASMBytes: bBytes, ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef(B): %v", err)
			}

			// Tenant B can measure its own.
			if n, err := storeB.GetWASMLength(ctx, defB, 1); err != nil || n != int64(len(bBytes)) {
				t.Fatalf("GetWASMLength(B, own) = %d, %v; want %d, nil", n, err, len(bBytes))
			}

			// Tenant A must not. Either error or zero is acceptable -- what is
			// not acceptable is B's byte count.
			n, err := storeA.GetWASMLength(ctx, defB, 1)
			if err == nil && n == int64(len(bBytes)) {
				t.Errorf("tenant A read the size of tenant B's definition %q: %d bytes", defB, n)
			}
		})
	}
}

// TestGetAllowedSignalCallersIsScopedToTenant — this one reads authorization
// data by workflow ID. See IMPROVEMENT-PLAN 3.15 for the larger problem with
// the column it reads; the tenant scope is worth having regardless of who
// writes it.
func TestGetAllowedSignalCallersIsScopedToTenant(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			storeA, storeB, admin := twoTenantStores(t, backend)
			ctx := context.Background()

			ids := seedReadyRuns(t, storeB, unscopedTenantB, "sigauth-def-b", 1)
			runB := ids[0]

			// Nothing in the product writes allowed_signals (3.15), so the
			// fixture does it the only way there is.
			setAllowedSignals(t, backend, admin, runB, `["caller-def"]`)

			if got, err := storeB.GetAllowedSignalCallers(ctx, runB); err != nil {
				t.Fatalf("GetAllowedSignalCallers(B, own): %v", err)
			} else if len(got) != 1 || got[0] != "caller-def" {
				t.Fatalf("tenant B cannot read its own allowed callers: %v", got)
			}

			got, err := storeA.GetAllowedSignalCallers(ctx, runB)
			if err != nil {
				t.Fatalf("GetAllowedSignalCallers(A, B's workflow): %v", err)
			}
			if len(got) != 0 {
				t.Errorf("tenant A read tenant B's signal authorization list: %v", got)
			}
		})
	}
}

// TestDeleteExpiredEventsDeletesOnlyTheCallersTenant is the one that destroys
// data rather than leaking it. The retention sweep runs on a timer; unscoped,
// one tenant's sweep takes every tenant's history with it.
func TestDeleteExpiredEventsDeletesOnlyTheCallersTenant(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			storeA, storeB, _ := twoTenantStores(t, backend)
			ctx := context.Background()

			ids := seedReadyRuns(t, storeB, unscopedTenantB, "retention-def-b", 1)
			runB := ids[0]
			if err := storeB.AppendEventHistory(ctx, runB, EventRecord{
				Step: 1, EventType: "call", Service: "svc", Op: "op",
			}); err != nil {
				t.Fatalf("AppendEventHistory(B): %v", err)
			}
			// Claim then complete, so the workflow is terminal with a
			// completed_at the sweep will consider expired.
			if _, err := storeB.ClaimWorkflow(ctx, "test-worker"); err != nil {
				t.Fatalf("ClaimWorkflow(B): %v", err)
			}
			if err := storeB.CompleteWorkflow(ctx, runB, "test-worker", 1, `{"done":true}`, nil); err != nil {
				t.Fatalf("CompleteWorkflow(B): %v", err)
			}

			// Tenant A sweeps with a cutoff far in the future, which matches
			// every completed workflow in the database. Only B's rows are old
			// enough to be at risk, and none of them are A's to delete.
			if _, err := storeA.DeleteExpiredEvents(ctx, time.Now().Add(24*time.Hour)); err != nil {
				t.Fatalf("DeleteExpiredEvents(A): %v", err)
			}

			events, err := storeB.LoadEventHistory(ctx, runB)
			if err != nil {
				t.Fatalf("LoadEventHistory(B): %v", err)
			}
			if len(events) == 0 {
				t.Errorf("tenant A's retention sweep deleted tenant B's event history")
			}
		})
	}
}

// setAllowedSignals writes workflow_instances.allowed_signals directly. There
// is no store method: see IMPROVEMENT-PLAN 3.15.
func setAllowedSignals(t *testing.T, backend StoreBackend, admin *sql.DB, workflowID, jsonList string) {
	t.Helper()
	var q string
	switch backend.Name() {
	case "postgres":
		q = `UPDATE workflow_instances SET allowed_signals = $2::jsonb WHERE id = $1`
	case "mysql":
		q = `UPDATE workflow_instances SET allowed_signals = ? WHERE id = ?`
	case "mssql":
		q = `UPDATE workflow_instances SET allowed_signals = @p2 WHERE id = @p1`
	default:
		t.Fatalf("setAllowedSignals: unknown backend %q", backend.Name())
	}
	var err error
	if backend.Name() == "mysql" {
		_, err = admin.Exec(q, jsonList, workflowID)
	} else {
		_, err = admin.Exec(q, workflowID, jsonList)
	}
	if err != nil {
		t.Fatalf("set allowed_signals: %v", err)
	}
}
