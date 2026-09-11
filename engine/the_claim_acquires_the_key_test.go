package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// Two runs want the same key and nobody holds it yet. Exactly one is claimed.
//
// cleat#1186, and this is the half the deferral predicate cannot do on its own.
// That predicate asks "does anybody else hold this key" -- and when the answer
// is no for BOTH runs, both are claimable and both run. Mutual exclusion needs
// the claim to TAKE the key, not merely to check it.
//
// # Why the runs are claimed in one call rather than two
//
// One ClaimWorkflows call with both runs in its candidate set is the harder
// case and the one a single worker actually produces. It is also the case a
// naive fix gets wrong: the acquisition happens inside one statement, so it
// cannot be serialised by taking a lock around the call. Postgres answers it
// with ON CONFLICT DO NOTHING -- measured, not assumed: two rows with the same
// key in one INSERT do not error, exactly one is inserted, and RETURNING yields
// only that one.
//
// The two-worker case is worth having too and is NOT this test; SKIP LOCKED
// already separates those candidate sets, so it exercises a different mechanism.
func TestOnlyOneOfTwoRunsWantingTheSameKeyIsClaimed(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			starter, ok := store.(interface {
				StartNewRunWithConcurrencyKey(context.Context, string, string, int, json.RawMessage, string, string, int, string) (string, bool, error)
			})
			if !ok {
				// Fatal, not Skip. All three registered backends implement it
				// -- that is what this test is about -- so a store that does
				// not is a regression, and a skip would report it as a pass.
				t.Fatalf("%T cannot record a concurrency key", store)
			}

			const key = "one-at-a-time"
			stamp := time.Now().UnixNano()
			var ids []string
			for i := 0; i < 2; i++ {
				id, _, err := starter.StartNewRunWithConcurrencyKey(ctx,
					fmt.Sprintf("ck-race-%d-%d", i, stamp), "test-workflow", 1,
					json.RawMessage(`{}`), "", DefaultTenantUUID, 0, key)
				if err != nil {
					t.Fatalf("StartNewRunWithConcurrencyKey[%d]: %v", i, err)
				}
				ids = append(ids, id)
			}

			claimed, err := store.ClaimWorkflows(ctx, "worker-1", 10)
			if err != nil {
				t.Fatalf("ClaimWorkflows: %v", err)
			}
			got := 0
			var claimedID string
			for _, wf := range claimed {
				for _, id := range ids {
					if wf.ID == id {
						got++
						claimedID = wf.ID
					}
				}
			}
			if got != 1 {
				t.Fatalf("%d of the two runs sharing key %q were claimed, want exactly 1.\n\n"+
					"This is the half the predicate cannot do alone: when NOBODY holds the key, "+
					"every candidate passes the \"is it held\" test, so the claim has to take it. "+
					"0 would mean the acquire is refusing everyone; 2 means it is taking nobody.",
					got, key)
			}

			// The loser is not lost -- it is still claimable once the winner
			// finishes. That is the difference between deferral and rejection,
			// and it is the whole point of cleat#1186.
			if err := store.TerminateWorkflow(ctx, claimedID, "done with the key"); err != nil {
				t.Fatalf("TerminateWorkflow(winner): %v", err)
			}
			after, err := store.ClaimWorkflows(ctx, "worker-2", 10)
			if err != nil {
				t.Fatalf("ClaimWorkflows after release: %v", err)
			}
			found := false
			for _, wf := range after {
				if wf.ID != claimedID {
					for _, id := range ids {
						if wf.ID == id {
							found = true
						}
					}
				}
			}
			if !found {
				t.Errorf("after the winner finished, the other run was still not claimable.\n\n"+
					"It was deferred, not rejected -- so releasing the key must make it "+
					"runnable. If this fails the run is stranded, which is a worse outcome "+
					"than the 409 this replaced. (winner=%s)", claimedID)
			}
		})
	}
}
