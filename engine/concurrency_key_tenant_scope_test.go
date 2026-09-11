package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// storeForTenant scopes a backend's store to a tenant.
//
// WithTenant is not on the WorkflowStore interface -- each store declares its
// own, returning its own concrete type -- so a cross-dialect test needs the
// switch. Anything unrecognised is a Fatal rather than a skip: a new backend
// arriving here must be scoped deliberately, and a skip would report this test
// as passing on a dialect it never ran.
func storeForTenant(t *testing.T, store WorkflowStore, tenantID string) WorkflowStore {
	t.Helper()
	switch s := store.(type) {
	case *PostgresStore:
		return s.WithTenant(tenantID)
	case *MySQLStore:
		return s.WithTenant(tenantID)
	case *MSSQLStore:
		return s.WithTenant(tenantID)
	}
	t.Fatalf("storeForTenant: %T has no WithTenant; scope it deliberately rather than "+
		"letting this test report a pass on a dialect it did not exercise", store)
	return nil
}

// seedRunForTenant creates a workflow row this tenant can hang a lock off.
// concurrency_keys.workflow_id is a foreign key to workflow_instances on every
// dialect, so the lock needs a real run.
func seedRunForTenant(t *testing.T, store WorkflowStore, tenantID, defName string) string {
	t.Helper()
	ctx := context.Background()
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef for %s: %v", tenantID, err)
	}
	runID, _, err := store.StartNewRun(ctx, "", defName, 1, json.RawMessage(`{}`),
		fmt.Sprintf("scope-%s-%d", tenantID[:8], time.Now().UnixNano()), tenantID, 0)
	if err != nil {
		t.Fatalf("StartNewRun for %s: %v", tenantID, err)
	}
	return runID
}

// Two tenants must be able to hold the same concurrency key at the same time.
//
// cleat#1189. concurrency_keys was `PRIMARY KEY (key_hash)` with the hash
// computed from the key text alone -- digest(<key>, 'sha256'), no tenant. The
// key namespace was global while every operation on the table is tenant-scoped
// and the table is under RLS, so the uniqueness dimension and the access
// dimension disagreed.
//
// The consequence is worse than a refusal. Tenant 2 could not acquire; could
// not release, because its DELETE carries `AND tenant_id = <its own>` and
// matched nothing; and could not see the blocking row, because RLS correctly
// hides another tenant's. Blocked, unclearable and invisible until the TTL ran
// out. The colliding names are the ones everyone picks -- "nightly", "sync",
// "cleanup".
//
// THIS IS THE SECOND INSTANCE OF A CLASS THAT ALREADY HAD A WRITTEN FIX.
// idempotency_keys had the identical shape and was repaired by migration 010,
// whose post-mortem is quoted in store_lifecycle.go: two customers both
// choosing "order-123" collided and the second was handed the first's workflow
// ID. Same client-supplied string, same global namespace. The sibling table was
// left behind.
//
// EACH DIALECT REFUSED FOR A DIFFERENT REASON, which is why the fix is not one
// change:
//
//	postgres  ON CONFLICT (key_hash) DO NOTHING     -- conflict target was the
//	                                                   whole key
//	mysql     INSERT IGNORE                          -- relies on the PRIMARY
//	                                                   KEY, so the migration
//	                                                   alone fixes it
//	mssql     WHERE NOT EXISTS (... key_hash = @p1)  -- no tenant predicate at
//	                                                   all, so it refuses even
//	                                                   with the key changed
//
// A test that ran only on PostgreSQL would have reported the MSSQL path fixed.
func TestTwoTenantsCanHoldTheSameConcurrencyKey(t *testing.T) {
	const (
		tenantA = "a1a1a1a1-1111-4111-8111-a1a1a1a1a1a1"
		tenantB = "b2b2b2b2-2222-4222-8222-b2b2b2b2b2b2"
	)
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			base, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			sa := storeForTenant(t, base, tenantA)
			sb := storeForTenant(t, base, tenantB)
			runA := seedRunForTenant(t, sa, tenantA, "ck-scope-a")
			runB := seedRunForTenant(t, sb, tenantB, "ck-scope-b")

			// The names two tenants actually collide on.
			key := fmt.Sprintf("nightly-%d", time.Now().UnixNano())
			t.Cleanup(func() {
				_ = sa.ReleaseConcurrencyKey(context.Background(), key, runA)
				_ = sb.ReleaseConcurrencyKey(context.Background(), key, runB)
			})

			gotA, err := sa.AcquireConcurrencyKey(ctx, key, runA, time.Minute)
			if err != nil {
				t.Fatalf("tenant A acquire: %v", err)
			}
			if !gotA {
				t.Fatal("tenant A could not acquire a key nobody holds")
			}

			gotB, err := sb.AcquireConcurrencyKey(ctx, key, runB, time.Minute)
			if err != nil {
				t.Fatalf("tenant B acquire: %v", err)
			}
			if !gotB {
				t.Fatalf("tenant B was refused the key %q because tenant A holds it. The "+
					"key namespace is global while every operation on the table is "+
					"tenant-scoped, so B is blocked by a row it cannot see and cannot "+
					"release -- its DELETE carries its own tenant_id and matches nothing. "+
					"Only expiry frees it (cleat#1189).", key)
			}

			// Each tenant must still exclude ITSELF: scoping the key per tenant
			// must not turn the mutex off. Without this, "two tenants can hold
			// it" is satisfied by a lock that excludes nobody.
			again, err := sa.AcquireConcurrencyKey(ctx, key, runA, time.Minute)
			if err != nil {
				t.Fatalf("tenant A re-acquire: %v", err)
			}
			if again {
				t.Error("tenant A acquired a key it already holds -- the mutex no longer " +
					"excludes within a tenant, which is the property the key exists for")
			}

			// And a release must free only the releasing tenant's row.
			if err := sa.ReleaseConcurrencyKey(ctx, key, runA); err != nil {
				t.Fatalf("tenant A release: %v", err)
			}
			stillB, err := sb.AcquireConcurrencyKey(ctx, key, runB, time.Minute)
			if err != nil {
				t.Fatalf("tenant B re-acquire after A released: %v", err)
			}
			if stillB {
				t.Error("tenant A's release freed tenant B's lock: B re-acquired a key it " +
					"was already holding, so one tenant's release reaches another's row")
			}
		})
	}
}
