package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// generation counts CLAIMS, not reclaims, so a reclaim count cannot be
// recovered from it after the fact.
//
// This matters because the reverse is an easy and load-bearing thing to
// believe. ReapStaleInstances bumps generation when it takes a workflow back
// from a dead worker, and that is the only column on workflow_instances that
// moves on a reclaim -- no migration has ever added an attempt or recovery
// count. Reading only that, generation looks like a reclaim count that nobody
// has got round to reading, and cleat#1008 was filed saying the information is
// "arguably derivable" from it.
//
// It is not. Six functions bump it, and the first one is why:
//
//	ClaimWorkflows           <- every ordinary claim
//	ClaimStickyWorkflows     <- every ordinary sticky claim
//	enforceParentClosePolicy
//	ReapStaleInstances       <- the reclaim
//	TerminateWorkflow
//	AdminReReplay
//
// A workflow that suspends and resumes ten times has generation 11 and has
// never been reclaimed; one reclaimed twice may be at 3. Measured on a
// database from a full port-suite run: of 365 workflows that completed
// SUCCESSFULLY -- none reclaimed -- generation ranged from 1 to 12.
//
// So this test pins the ordinary-claim half, which is the half that makes the
// column useless as a reclaim count. It deliberately never calls
// ReapStaleInstances: the point is that generation advances anyway.
//
// It asserts nothing about whether repeated reclaim SHOULD be bounded, which
// is the open question in cleat#1008. It records why the answer cannot come
// from this column, so that whoever settles that question does not start from
// the same wrong premise.
func TestGenerationCountsClaimsSoItIsNotAReclaimCount(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()
			id, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{"k":"generation"}`), "gen-claim-count", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			before, err := store.GetWorkflowByID(ctx, id)
			if err != nil || before == nil {
				t.Fatalf("GetWorkflowByID before any claim: %v (nil=%v)", err, before == nil)
			}

			// Two ordinary claim/release cycles -- a suspend and a resume, the
			// most common thing a durable workflow does. No worker dies here
			// and ReapStaleInstances is never called.
			const cycles = 2
			for i := 0; i < cycles; i++ {
				claimed := claimByID(t, ctx, store, "gen-worker", id)
				if err := store.ReleaseWorkflow(ctx, id, "gen-worker", claimed.Generation,
					time.Now().Add(-time.Minute)); err != nil {
					t.Fatalf("ReleaseWorkflow on cycle %d: %v", i+1, err)
				}
			}

			after, err := store.GetWorkflowByID(ctx, id)
			if err != nil || after == nil {
				t.Fatalf("GetWorkflowByID after %d claims: %v (nil=%v)", cycles, err, after == nil)
			}

			if after.Generation <= before.Generation {
				t.Fatalf("generation went %d -> %d across %d ordinary claims, want it to advance.\n\n"+
					"If this is now stable across claims, generation may have become a "+
					"reclaim counter -- which would change what every fence argument means, "+
					"since callers pass it back to prove they still hold the claim.",
					before.Generation, after.Generation, cycles)
			}

			// The assertion the issue needs: it advanced WITHOUT a reclaim, so
			// its value cannot be read as a number of reclaims.
			if got := after.Generation - before.Generation; got < cycles {
				t.Errorf("generation advanced by %d across %d ordinary claims, want at least %d",
					got, cycles, cycles)
			}
			t.Logf("generation %d -> %d across %d ordinary claims and zero reclaims",
				before.Generation, after.Generation, cycles)
		})
	}
}
