package engine

// IMPROVEMENT-PLAN 3.15: signal authorization consults a list nothing can
// write. SetAllowedSignalCallers is that writer.
//
// Three-dialect, and tenant-scoped assertions included, for the reason 3.11
// records: MySQL has no row-level security, so on that dialect the Go-level
// `AND tenant_id = ?` is the whole of the isolation. A writer is worse than a
// reader to get wrong here -- a missed predicate on the getter leaks a list, a
// missed predicate on the setter lets one tenant grant callers on another
// tenant's workflow.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// readAllowedSignalsColumn reads allowed_signals as the database holds it,
// bypassing the store.
//
// The getter normalises NULL, "" and "null" to a nil slice, so a round trip
// through Set/Get cannot distinguish "wrote NULL" from "wrote []". Both deny
// everyone, so the behaviour is the same either way -- but only one of them is
// what the column is documented to hold, and a test that cannot tell them apart
// would pass against a setter writing anything the getter happens to forgive.
func readAllowedSignalsColumn(t *testing.T, backend StoreBackend, admin *sql.DB, workflowID string) sql.NullString {
	t.Helper()
	var q string
	switch backend.Name() {
	case "postgres":
		q = `SELECT allowed_signals::text FROM workflow_instances WHERE id = $1`
	case "mysql":
		q = `SELECT allowed_signals FROM workflow_instances WHERE id = ?`
	case "mssql":
		q = `SELECT allowed_signals FROM workflow_instances WHERE id = @p1`
	default:
		t.Fatalf("readAllowedSignalsColumn: unknown backend %q", backend.Name())
	}
	var raw sql.NullString
	if err := admin.QueryRow(q, workflowID).Scan(&raw); err != nil {
		t.Fatalf("read allowed_signals column: %v", err)
	}
	return raw
}

// startOneRun deploys a definition for a tenant and starts a single workflow on
// it, returning the run ID.
func startOneRun(t *testing.T, store WorkflowStore, tenant, defName string) string {
	t.Helper()
	return seedReadyRuns(t, store, tenant, defName, 1)[0]
}

// TestSetAllowedSignalCallersRoundTrips is the feature: what the setter writes
// is what the getter reads, on every dialect.
func TestSetAllowedSignalCallersRoundTrips(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			storeA, _, _ := twoTenantStores(t, backend)
			ctx := context.Background()
			id := startOneRun(t, storeA, unscopedTenantA, "as-roundtrip")

			// A workflow starts with no allowed callers. That is the deny-all
			// default and it is deliberate; the defect was that it could not be
			// changed, not that it was wrong.
			before, err := storeA.GetAllowedSignalCallers(ctx, id)
			if err != nil {
				t.Fatalf("GetAllowedSignalCallers before: %v", err)
			}
			if len(before) != 0 {
				t.Fatalf("a freshly started workflow already allows %v, want none", before)
			}

			want := []string{"billing-service", "fraud-service"}
			if err := storeA.SetAllowedSignalCallers(ctx, id, want); err != nil {
				t.Fatalf("SetAllowedSignalCallers: %v", err)
			}
			got, err := storeA.GetAllowedSignalCallers(ctx, id)
			if err != nil {
				t.Fatalf("GetAllowedSignalCallers after: %v", err)
			}
			if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
				t.Errorf("read back %v, want %v", got, want)
			}
		})
	}
}

// TestSetAllowedSignalCallersReplacesRatherThanAppends pins the PUT semantics
// the API endpoint depends on. If this merged instead, two operators granting
// one caller each would produce a list neither of them asked for, and revoking
// would need a verb that does not exist.
func TestSetAllowedSignalCallersReplacesRatherThanAppends(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			storeA, _, _ := twoTenantStores(t, backend)
			ctx := context.Background()
			id := startOneRun(t, storeA, unscopedTenantA, "as-replace")

			if err := storeA.SetAllowedSignalCallers(ctx, id, []string{"first"}); err != nil {
				t.Fatalf("first set: %v", err)
			}
			if err := storeA.SetAllowedSignalCallers(ctx, id, []string{"second"}); err != nil {
				t.Fatalf("second set: %v", err)
			}
			got, err := storeA.GetAllowedSignalCallers(ctx, id)
			if err != nil {
				t.Fatalf("GetAllowedSignalCallers: %v", err)
			}
			if len(got) != 1 || got[0] != "second" {
				t.Errorf("read back %v, want exactly [second]", got)
			}
		})
	}
}

// TestSetAllowedSignalCallersEmptyWritesNull — clearing the list has to leave
// the column in the state the getter reports as nil, so that "cleared" and
// "never set" are the same row and not two.
//
// Asserted against the column rather than through the getter, which forgives
// the difference. See readAllowedSignalsColumn.
func TestSetAllowedSignalCallersEmptyWritesNull(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			storeA, _, admin := twoTenantStores(t, backend)
			ctx := context.Background()
			id := startOneRun(t, storeA, unscopedTenantA, "as-clear")

			if err := storeA.SetAllowedSignalCallers(ctx, id, []string{"*"}); err != nil {
				t.Fatalf("set: %v", err)
			}
			for _, empty := range [][]string{nil, {}} {
				if err := storeA.SetAllowedSignalCallers(ctx, id, empty); err != nil {
					t.Fatalf("clear with %#v: %v", empty, err)
				}
				if raw := readAllowedSignalsColumn(t, backend, admin, id); raw.Valid {
					t.Errorf("clearing with %#v left allowed_signals = %q, want SQL NULL",
						empty, raw.String)
				}
				got, err := storeA.GetAllowedSignalCallers(ctx, id)
				if err != nil {
					t.Fatalf("get after clear: %v", err)
				}
				if len(got) != 0 {
					t.Errorf("after clearing with %#v the list reads %v, want empty", empty, got)
				}
			}
		})
	}
}

// TestSetAllowedSignalCallersRejectsAnotherTenantsWorkflow is the tenancy
// assertion, and it is the one that matters most on MySQL.
//
// Two halves, because the first alone would pass against a setter that
// correctly refuses but has already written: B must be told the workflow does
// not exist, *and* A's list must be untouched.
func TestSetAllowedSignalCallersRejectsAnotherTenantsWorkflow(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			storeA, storeB, _ := twoTenantStores(t, backend)
			ctx := context.Background()
			id := startOneRun(t, storeA, unscopedTenantA, "as-cross-tenant")

			if err := storeA.SetAllowedSignalCallers(ctx, id, []string{"a-service"}); err != nil {
				t.Fatalf("tenant A set: %v", err)
			}

			err := storeB.SetAllowedSignalCallers(ctx, id, []string{"*"})
			if err == nil {
				t.Fatalf("tenant B set allowed_signals on tenant A's workflow %s and was told it succeeded", id)
			}
			if !errors.Is(err, ErrWorkflowNotFound) {
				t.Errorf("tenant B got %v, want ErrWorkflowNotFound -- distinguishing "+
					"\"not yours\" from \"no such workflow\" makes this an existence oracle", err)
			}

			got, err := storeA.GetAllowedSignalCallers(ctx, id)
			if err != nil {
				t.Fatalf("tenant A get: %v", err)
			}
			if len(got) != 1 || got[0] != "a-service" {
				t.Errorf("tenant A's list is now %v, want [a-service]: tenant B's write landed", got)
			}
		})
	}
}

// TestSetAllowedSignalCallersUnknownWorkflow — a write that matched no row must
// say so. Reporting success would tell an operator they had granted a caller
// when they had mistyped an id, and the mistake would only surface as a denied
// signal much later.
func TestSetAllowedSignalCallersUnknownWorkflow(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			storeA, _, _ := twoTenantStores(t, backend)
			missing := fmt.Sprintf("no-such-workflow-%d", time.Now().UnixNano())
			err := storeA.SetAllowedSignalCallers(context.Background(), missing, []string{"*"})
			if !errors.Is(err, ErrWorkflowNotFound) {
				t.Errorf("got %v, want ErrWorkflowNotFound", err)
			}
		})
	}
}

// TestSetAllowedSignalCallersIsIdempotent guards a dialect difference rather
// than a behaviour anyone asked for: MySQL's RowsAffected reports 0 for a row
// that matched but did not change, so a setter that reads 0 as "no such row"
// turns re-setting a list to its current value into a spurious not-found. The
// other two dialects report 1 and would not have caught it.
func TestSetAllowedSignalCallersIsIdempotent(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			storeA, _, _ := twoTenantStores(t, backend)
			ctx := context.Background()
			id := startOneRun(t, storeA, unscopedTenantA, "as-idempotent")

			for i := 0; i < 2; i++ {
				if err := storeA.SetAllowedSignalCallers(ctx, id, []string{"same-caller"}); err != nil {
					t.Fatalf("set #%d: %v", i+1, err)
				}
			}
		})
	}
}

// TestSetAllowedSignalCallersWritesValidJSON — SQL Server carries
// ck_workflow_instances_allowed_signals (ISJSON), added by migration 037, so a
// setter writing a bare string would be refused there and accepted by the other
// two. Asserting the column parses as a JSON array says the three dialects
// agree about the encoding, not merely that each one accepted its own write.
func TestSetAllowedSignalCallersWritesValidJSON(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			storeA, _, admin := twoTenantStores(t, backend)
			ctx := context.Background()
			id := startOneRun(t, storeA, unscopedTenantA, "as-json")

			want := []string{"a", "b"}
			if err := storeA.SetAllowedSignalCallers(ctx, id, want); err != nil {
				t.Fatalf("set: %v", err)
			}
			raw := readAllowedSignalsColumn(t, backend, admin, id)
			if !raw.Valid {
				t.Fatal("allowed_signals is NULL after setting a non-empty list")
			}
			var decoded []string
			if err := json.Unmarshal([]byte(raw.String), &decoded); err != nil {
				t.Fatalf("allowed_signals holds %q, which is not a JSON array: %v", raw.String, err)
			}
			if len(decoded) != 2 || decoded[0] != "a" || decoded[1] != "b" {
				t.Errorf("allowed_signals holds %v, want [a b]", decoded)
			}
		})
	}
}
