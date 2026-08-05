package engine

// Found while auditing the ~89 unaudited MySQLStore s.tenantID call sites
// (IMPROVEMENT-PLAN.md 1.7 / 2.12). The audit's premise is that MySQL has no
// RLS, so a missing Go-level tenant filter is an unbacked cross-tenant leak.
// This one is worse than a missing filter: there is nothing to filter on.
//
// idempotency_keys is keyed by key_hash alone on all three dialects --
// `key_hash BYTEA NOT NULL PRIMARY KEY` (postgres), `PRIMARY KEY (key_hash)`
// (mysql), `CONSTRAINT pk_idempotency_keys PRIMARY KEY (key_hash)` (mssql) --
// and the table has no tenant_id column at all. The hash is
// sha256.Sum256([]byte(idempotencyKey)), with the tenant nowhere in it.
//
// So an Idempotency-Key is global. Two tenants that pick the same string --
// "order-123", "daily-report", "1" -- collide, and the collision is not an
// adversarial edge case but the expected outcome of two customers naming
// things the way people name things.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// tenantSetupBackend is the per-tenant setup every registered backend
// implements but which StoreBackend does not declare.
type tenantSetupBackend interface {
	SetupForTenant(t *testing.T, tenantID string) (WorkflowStore, func())
}

// TestIdempotencyKeyIsScopedToTenant asserts the property the feature needs:
// one tenant's idempotency key must not resolve to another tenant's workflow.
func TestIdempotencyKeyIsScopedToTenant(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			const (
				tenantA = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
				tenantB = "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb"
			)

			tb, ok := backend.(tenantSetupBackend)
			if !ok {
				t.Fatalf("backend %T does not implement SetupForTenant; this test needs two tenants", backend)
			}

			storeA, teardown := tb.SetupForTenant(t, tenantA)
			defer teardown()
			ctx := context.Background()

			storeB, teardownB := tb.SetupForTenant(t, tenantB)
			defer teardownB()

			// One definition per tenant, with different names, because
			// workflow_defs' primary key is (name, version) on all three
			// dialects with no tenant in it -- so two tenants deploying the
			// same def name collide in a way that has nothing to do with what
			// this test is about. (That collision is real and is recorded
			// separately as IMPROVEMENT-PLAN 3.12.)
			//
			// This test also does not call setupTestData/truncateAll. Those
			// insert under DefaultTenantUUID, and storeA here is a genuinely
			// RLS-enforcing PostgreSQL connection scoped to tenant A, which
			// rejects the write:
			//
			//	new row violates row-level security policy for table
			//	"workflow_instances" (42501)
			//
			// -- so the postgres arm used to fail during setup, one layer
			// above the property under test, and could not have observed the
			// idempotency defect either way.
			defNameFor := map[string]string{
				tenantA: "idem-tenant-def-a",
				tenantB: "idem-tenant-def-b",
			}
			for tenant, st := range map[string]WorkflowStore{tenantA: storeA, tenantB: storeB} {
				if err := st.DeployWorkflowDef(ctx, &WorkflowDef{
					Name: defNameFor[tenant], Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
					ABIVersion: 1, MinVersion: 1,
				}); err != nil {
					t.Fatalf("DeployWorkflowDef(%s): %v", tenant, err)
				}
			}

			// The same string two different customers might reasonably choose.
			key := fmt.Sprintf("order-%d", time.Now().UnixNano())

			idA := fmt.Sprintf("idem-a-%d", time.Now().UnixNano())
			gotA, existedA, err := storeA.StartNewRun(ctx, idA, defNameFor[tenantA], 1,
				json.RawMessage(`{"tenant":"A"}`), key, tenantA, 0)
			if err != nil {
				t.Fatalf("StartNewRun(A): %v", err)
			}
			if existedA {
				t.Fatalf("tenant A's first use of a fresh key reported already-existing")
			}

			idB := fmt.Sprintf("idem-b-%d", time.Now().UnixNano())
			gotB, existedB, err := storeB.StartNewRun(ctx, idB, defNameFor[tenantB], 1,
				json.RawMessage(`{"tenant":"B"}`), key, tenantB, 0)
			if err != nil {
				t.Fatalf("StartNewRun(B): %v", err)
			}

			if existedB {
				t.Errorf("tenant B's first use of its own idempotency key %q reported already-existing: "+
					"tenant A's key collided with it. B's workflow was never started", key)
			}
			if gotB == gotA {
				t.Errorf("tenant B was handed tenant A's workflow ID %q for its own idempotency key %q -- "+
					"a cross-tenant information leak on a user-supplied value", gotA, key)
			}

			// B must have a workflow of its own, and it must be the one B
			// started.
			//
			// Identified by the input payload rather than by
			// WorkflowInstance.TenantID: only the MySQL store selects
			// tenant_id into that field. PostgreSQL leaves it to RLS and SQL
			// Server filters in SQL, so on those two the field comes back ""
			// for every workflow and an assertion on it passes or fails for
			// reasons that have nothing to do with tenancy.
			wfB, err := storeB.GetWorkflowByID(ctx, gotB)
			if err != nil {
				t.Fatalf("GetWorkflowByID(B): %v", err)
			}
			if wfB == nil {
				t.Fatalf("tenant B has no workflow after StartNewRun returned %q", gotB)
			}
			// Decoded rather than string-compared: PostgreSQL stores input as
			// JSONB and hands it back reformatted ({"tenant": "B"}).
			var inputB struct {
				Tenant string `json:"tenant"`
			}
			if err := json.Unmarshal(wfB.Input, &inputB); err != nil {
				t.Fatalf("decode tenant B's workflow input %s: %v", wfB.Input, err)
			}
			if inputB.Tenant != "B" {
				t.Errorf("the workflow tenant B was given carries input %s, not B's own", wfB.Input)
			}

			// And neither tenant can read the other's workflow at all --
			// which is what makes the two IDs above genuinely separate rather
			// than two names for one shared row.
			if wf, err := storeA.GetWorkflowByID(ctx, gotB); err == nil && wf != nil {
				t.Errorf("tenant A can read tenant B's workflow %q", gotB)
			}
			if wf, err := storeB.GetWorkflowByID(ctx, gotA); err == nil && wf != nil {
				t.Errorf("tenant B can read tenant A's workflow %q", gotA)
			}
		})
	}
}
