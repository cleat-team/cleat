package engine

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// IMPROVEMENT-PLAN 3.39: re-acquiring a concurrency key you already hold
// answered differently per dialect.
//
// Measured 2026-08-06 and again 2026-08-31, before the fix:
//
//	postgres  first=true  self-reacquire=false
//	mysql     first=true  self-reacquire=true     <- the odd one out
//	mssql     first=true  self-reacquire=false
//
// So the same primitive was re-entrant on one backend and not the other two,
// and a workflow's behaviour depended on which database was deployed under it.
//
// The contract is now "never re-entrant" on all three. That is not just a
// majority vote: ReleaseConcurrencyKey takes only the key and deletes the row
// unconditionally, with no hold count anywhere in the system. Under the old
// MySQL answer, acquire(k); acquire(k); release(k) left the key free while the
// workflow still believed it held it -- precisely the failure a mutual
// exclusion primitive exists to prevent.
//
// Reverting mysql_ops.go to `return ownerID == workflowID` fails the
// self-reacquire subtest on mysql and leaves the other two dialects passing,
// which is the divergence itself.
func TestAcquireConcurrencyKeyIsNeverReentrant(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			runID := seedWorkflowForLock(t, store)
			otherRunID := seedWorkflowForLock(t, store)
			key := fmt.Sprintf("reentrancy-%s-%d", backend.Name(), time.Now().UnixNano())

			// A long TTL: this test is about ownership, not expiry, and a short
			// one would let the key lapse mid-test and pass for the wrong
			// reason. Expiry is 3.34's subject, in concurrency_key_ttl_test.go.
			const ttl = 30 * time.Second

			acquired, err := store.AcquireConcurrencyKey(ctx, key, runID, ttl)
			if err != nil {
				t.Fatalf("first acquire: %v", err)
			}
			if !acquired {
				t.Fatal("first acquire of a fresh key returned false")
			}

			// The defect.
			again, err := store.AcquireConcurrencyKey(ctx, key, runID, ttl)
			if err != nil {
				t.Fatalf("self re-acquire: %v", err)
			}
			if again {
				t.Errorf("re-acquiring a key this workflow already holds returned true.\n\n" +
					"The contract is never re-entrant, on every dialect. Returning true " +
					"here is unsafe under the current release API: ReleaseConcurrencyKey " +
					"takes only the key and has no hold count, so acquire+acquire+release " +
					"frees a lock the workflow still believes it holds.")
			}

			// Control 1: mutual exclusion still holds. Without this, making
			// acquire always return false would pass the assertion above.
			other, err := store.AcquireConcurrencyKey(ctx, key, otherRunID, ttl)
			if err != nil {
				t.Fatalf("other-workflow acquire: %v", err)
			}
			if other {
				t.Error("a second workflow acquired a key that was already held")
			}

			// Control 2: the key is refusable, not poisoned. This is the other
			// way "always false" would slip through -- and it also pins that a
			// released key is genuinely reusable.
			if err := store.ReleaseConcurrencyKey(ctx, key); err != nil {
				t.Fatalf("release: %v", err)
			}
			reacquired, err := store.AcquireConcurrencyKey(ctx, key, otherRunID, ttl)
			if err != nil {
				t.Fatalf("acquire after release: %v", err)
			}
			if !reacquired {
				t.Error("a released key could not be acquired again; acquire is " +
					"returning false unconditionally rather than answering the question")
			}
		})
	}
}
