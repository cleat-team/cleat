package engine

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// A release must not free a key another workflow holds.
//
// cleat#1188. ReleaseConcurrencyKey took only the key, and its DELETE carried
// `AND tenant_id` but no `workflow_id`. Within one tenant, any workflow that
// knew a key string removed the row whoever held it -- so B released A's lock,
// C then acquired it, and A went on running believing it still held mutual
// exclusion. Nothing errored on any side.
//
// THE HAZARD WAS ALREADY WRITTEN DOWN IN THIS PACKAGE, in
// concurrency_key_reentrancy_test.go, as the reason re-entrancy must keep
// returning false:
//
//	"ReleaseConcurrencyKey takes only the key and has no hold count, so
//	 acquire+acquire+release frees a lock the workflow still believes it holds."
//
// That is this defect, described accurately, in a comment, months before it was
// filed. A sentence in a test cannot fail; this test can. The hold-count half
// of that sentence is still true and is still pinned by that test -- what
// changes here is only "takes only the key".
func TestOneWorkflowCannotReleaseAnothersConcurrencyKey(t *testing.T) {
	const tenant = "c3c3c3c3-3333-4333-8333-c3c3c3c3c3c3"
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			base, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			s := storeForTenant(t, base, tenant)
			runA := seedRunForTenant(t, s, tenant, "ck-holder-a")
			runB := seedRunForTenant(t, s, tenant, "ck-holder-b")
			runC := seedRunForTenant(t, s, tenant, "ck-holder-c")

			key := fmt.Sprintf("shared-resource-%d", time.Now().UnixNano())
			t.Cleanup(func() { _, _ = s.ReleaseConcurrencyKey(context.Background(), key, runA) })

			held, err := s.AcquireConcurrencyKey(ctx, key, runA, time.Minute)
			if err != nil {
				t.Fatalf("A acquire: %v", err)
			}
			if !held {
				t.Fatal("A could not acquire a fresh key")
			}

			// B releases a key it does not hold. This is the defect's entry
			// point and it stays a SUCCESS -- a workflow that holds nothing has
			// done nothing wrong, and an expired key is legitimately already
			// gone. What must change is `released`, and the row.
			released, err := s.ReleaseConcurrencyKey(ctx, key, runB)
			if err != nil {
				t.Fatalf("B release: %v", err)
			}
			if released {
				t.Errorf("B released a key held by A.\n\n"+
					"The DELETE matched on key and tenant but not on workflow_id, so any "+
					"workflow naming the key took the row (cleat#1188). key=%q holder=%s "+
					"releaser=%s", key, runA, runB)
			}

			// The consequence, and the assertion that matters: with A's row
			// gone, C acquires and two workflows are inside at once. Asserting
			// only on `released` would pass against a store that reported false
			// and deleted the row anyway.
			got, err := s.AcquireConcurrencyKey(ctx, key, runC, time.Minute)
			if err != nil {
				t.Fatalf("C acquire: %v", err)
			}
			if got {
				t.Errorf("C acquired a key A still holds -- mutual exclusion is gone.\n\n"+
					"This is the end state cleat#1188 describes, and it is the same one "+
					"IMPROVEMENT-PLAN 3.34 reached by a different route (a TTL truncated "+
					"to zero seconds). key=%q holder=%s", key, runA)
			}

			// Control: the holder can still release its own key, and the key is
			// then genuinely reusable. Without this, a store that refused every
			// release would pass both assertions above.
			released, err = s.ReleaseConcurrencyKey(ctx, key, runA)
			if err != nil {
				t.Fatalf("A release: %v", err)
			}
			if !released {
				t.Fatal("A could not release its own key")
			}
			reacquired, err := s.AcquireConcurrencyKey(ctx, key, runC, time.Minute)
			if err != nil {
				t.Fatalf("C acquire after release: %v", err)
			}
			if !reacquired {
				t.Error("the key was not reusable after its holder released it")
			}
			_, _ = s.ReleaseConcurrencyKey(ctx, key, runC)
		})
	}
}
