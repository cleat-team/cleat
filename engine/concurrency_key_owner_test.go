package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// Releasing a lock you do not hold must not release it.
//
// cleat#1188. ReleaseConcurrencyKey took only the key, and its DELETE carried
// tenant_id but not workflow_id -- while ReleaseWorkflowConcurrencyKeys, three
// lines below it, did predicate on workflow_id. So the column was available and
// the omission was local to one statement. Within a tenant, any workflow that
// knew a key string could release a lock a different workflow held, and the key
// is an arbitrary string from the guest (cleat_release_lock), so naming
// someone else's is not an exotic input.
//
// WHY THE ASSERTION IS "C IS REFUSED" AND NOT "B'S RELEASE SUCCEEDED".
// The release is a DELETE that reports success either way -- it affects one row
// when it matches and zero when it does not, and freshReleaseLock discards the
// count. A test checking that B's release returned no error cannot see this
// defect at all: it returns no error both before and after the fix. The
// observable difference is downstream, in whether the lock is still held.
//
// So this asserts BOTH halves of that, because either alone is satisfiable by
// the wrong thing:
//
//   - A still holds the key after B's release. On its own this is satisfied by
//     a release that failed for any reason, including one that never ran.
//   - C is refused. On its own this is satisfied by a key that can never be
//     acquired by anyone -- a poisoned row rather than a held one.
//
// The third sub-test is the control for both: A can still release its OWN key,
// and C can then take it. Without that, "the lock is held" and "the lock is
// broken" look identical.
func TestReleasingALockYouDoNotHold(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			const defName = "lock-owner-def"
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}
			// concurrency_keys.workflow_id is a foreign key into
			// workflow_instances on every dialect, so each holder needs a real
			// run rather than an opaque string.
			run := func(tag string) string {
				id, _, err := store.StartNewRun(ctx, "", defName, 1, json.RawMessage(`{}`),
					fmt.Sprintf("owner-%s-%d", tag, time.Now().UnixNano()),
					DefaultTenantUUID, 0)
				if err != nil {
					t.Fatalf("StartNewRun(%s): %v", tag, err)
				}
				return id
			}
			runA, runB, runC := run("a"), run("b"), run("c")

			key := fmt.Sprintf("shared-%d", time.Now().UnixNano())
			t.Cleanup(func() {
				ctx := context.Background()
				for _, owner := range []string{runA, runB, runC} {
					_ = store.ReleaseConcurrencyKey(ctx, key, owner)
				}
			})

			acquired, err := store.AcquireConcurrencyKey(ctx, key, runA, time.Minute)
			if err != nil {
				t.Fatalf("A acquire: %v", err)
			}
			if !acquired {
				t.Fatal("A could not acquire a key nobody holds")
			}

			// B releases a key it does not hold. This returns no error before
			// or after the fix -- the DELETE simply matches nothing now.
			if err := store.ReleaseConcurrencyKey(ctx, key, runB); err != nil {
				t.Fatalf("B release of a key it does not hold returned an error: %v", err)
			}

			// Half one: A still holds it.
			count, err := store.GetConcurrencyKeyCount(ctx, runA)
			if err != nil {
				t.Fatalf("GetConcurrencyKeyCount(A): %v", err)
			}
			if count < 1 {
				t.Errorf("B released a lock A holds: A's key count is %d. "+
					"ReleaseConcurrencyKey's DELETE predicates on tenant_id but not "+
					"workflow_id, and the key is an arbitrary string from the guest "+
					"(cleat#1188)", count)
			}

			// Half two: C cannot take it.
			got, err := store.AcquireConcurrencyKey(ctx, key, runC, time.Minute)
			if err != nil {
				t.Fatalf("C acquire: %v", err)
			}
			if got {
				t.Error("C acquired a lock A holds, after B released it: one workflow " +
					"released another's lock and the mutex stopped excluding (cleat#1188)")
			}

			// Control. Without this, a lock nobody can acquire -- a poisoned row
			// rather than a held one -- passes both assertions above.
			if err := store.ReleaseConcurrencyKey(ctx, key, runA); err != nil {
				t.Fatalf("A release of its OWN key: %v", err)
			}
			got, err = store.AcquireConcurrencyKey(ctx, key, runC, time.Minute)
			if err != nil {
				t.Fatalf("C acquire after A released: %v", err)
			}
			if !got {
				t.Error("C could not acquire after the holder released: the owner " +
					"predicate is refusing a release that should succeed, so the key " +
					"is unreleasable rather than protected")
			}
		})
	}
}
