package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// reclaim_count counts RECLAIMS, and only reclaims.
//
// This is the column cleat#1008 was missing. The reaper's fleet-wide counter
// (cleat_reaper_instances_claimed_total) has always existed, but it cannot be
// attributed: a thousand reclaims of one wedged workflow and one reclaim each
// of a thousand healthy ones produce the same number, so "has THIS workflow
// been reclaimed twenty times" had no answer anywhere.
//
// generation cannot answer it either, and the companion test
// TestGenerationCountsClaimsSoItIsNotAReclaimCount pins why: six functions
// bump it and two of them are the ordinary claim path, so a workflow that
// completed successfully without ever being reclaimed has been measured at
// generation 12.
//
// So the two halves below are the whole point of the column, and the SECOND is
// the one that distinguishes it from generation:
//
//  1. a reclaim advances it, and
//  2. an ordinary claim/release cycle does NOT -- while generation does.
//
// Part 2 is not redundant with part 1. A column that advanced on both would
// pass part 1 and be exactly as useless as generation, which is the mistake
// this whole issue exists to avoid making twice.
//
// It asserts nothing about a BOUND. Nothing in the engine compares this value
// to a limit, deliberately: every cause of repeated reclaim that survives the
// worker's default limits is infrastructure (host OOM, node failure, deploy,
// SIGKILL), and dead-lettering past a threshold would convert a node being
// redeployed into permanent failure of a workflow that did nothing wrong. If
// that decision is ever revisited it now has something to read.
func TestReclaimCountAdvancesOnReclaimAndNotOnAnOrdinaryClaim(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()
			id, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{"k":"reclaim"}`), "reclaim-count", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			fresh, err := store.GetWorkflowByID(ctx, id)
			if err != nil || fresh == nil {
				t.Fatalf("GetWorkflowByID on a new run: %v (nil=%v)", err, fresh == nil)
			}
			if fresh.ReclaimCount != 0 {
				t.Fatalf("a workflow that has never run starts at reclaim_count %d, want 0",
					fresh.ReclaimCount)
			}

			// --- part 1: two reclaims, counted -------------------------------
			//
			// Each cycle claims the workflow and then reaps it with a negative
			// timeout, which is the established way to say "every running row
			// is stale now" (see adaptive_flush_test.go). That is a worker that
			// stopped heartbeating, which is the only thing that reaches this
			// counter.
			const reclaims = 2
			for i := 0; i < reclaims; i++ {
				claimByID(t, ctx, store, "reclaim-worker", id)
				n, err := store.ReapStaleInstances(ctx, -1*time.Second)
				if err != nil {
					t.Fatalf("ReapStaleInstances on cycle %d: %v", i+1, err)
				}
				if n < 1 {
					t.Fatalf("ReapStaleInstances reclaimed %d rows on cycle %d, want at least 1 -- "+
						"the workflow was claimed immediately before, so it was running and stale",
						n, i+1)
				}
			}

			reclaimed, err := store.GetWorkflowByID(ctx, id)
			if err != nil || reclaimed == nil {
				t.Fatalf("GetWorkflowByID after %d reclaims: %v (nil=%v)", reclaims, err, reclaimed == nil)
			}
			if reclaimed.ReclaimCount != reclaims {
				t.Fatalf("reclaim_count is %d after %d reclaims, want %d",
					reclaimed.ReclaimCount, reclaims, reclaims)
			}

			// --- part 2: ordinary claims, NOT counted ------------------------
			//
			// The same suspend/resume cycle the companion test uses to drive
			// generation up. No worker dies and ReapStaleInstances is not
			// called, so this must leave reclaim_count exactly where part 1
			// left it -- while generation keeps moving.
			const cycles = 2
			for i := 0; i < cycles; i++ {
				claimed := claimByID(t, ctx, store, "ordinary-worker", id)
				if err := store.ReleaseWorkflow(ctx, id, "ordinary-worker", claimed.Generation,
					time.Now().Add(-time.Minute)); err != nil {
					t.Fatalf("ReleaseWorkflow on cycle %d: %v", i+1, err)
				}
			}

			after, err := store.GetWorkflowByID(ctx, id)
			if err != nil || after == nil {
				t.Fatalf("GetWorkflowByID after %d ordinary claims: %v (nil=%v)", cycles, err, after == nil)
			}

			if after.ReclaimCount != reclaimed.ReclaimCount {
				t.Errorf("reclaim_count moved %d -> %d across %d ordinary claim/release cycles "+
					"with no reclaim.\n\n"+
					"That is the defect this column exists to avoid: a counter that also advances "+
					"on the suspend/resume path is exactly as useless for finding a reclaim loop "+
					"as generation, which is what cleat#1008 established.",
					reclaimed.ReclaimCount, after.ReclaimCount, cycles)
			}
			if after.Generation <= reclaimed.Generation {
				t.Errorf("generation went %d -> %d across %d ordinary claims, want it to advance -- "+
					"without that this test cannot show the two columns disagree",
					reclaimed.Generation, after.Generation, cycles)
			}
			t.Logf("after %d reclaims and then %d ordinary claims: reclaim_count %d -> %d, "+
				"generation %d -> %d",
				reclaims, cycles, reclaimed.ReclaimCount, after.ReclaimCount,
				reclaimed.Generation, after.Generation)
		})
	}
}

// The zero must be VISIBLE on the wire, which is a property of the struct tag
// rather than of any handler: cmd/cleat-worker's handleGetWorkflow serialises
// engine.WorkflowInstance whole, so whatever the tag says is what a client
// gets.
//
// `omitempty` would be the natural thing to write here and it would be wrong.
// 0 is not an absent value on this field, it is the answer for the
// overwhelming majority of workflows -- "this one has never been reclaimed" --
// and it is the reading an operator checking a suspected reclaim loop most
// needs to be able to trust. Drop it from the response and "never reclaimed"
// becomes indistinguishable from "this build does not report it".
//
// Worth pinning rather than assuming, because the neighbouring question was
// got wrong twice: cleat#1009 concluded error_code never reaches a client, and
// two sessions agreed, because both enumerated JSON tags in cmd/cleat-worker/
// and the field is declared one package away in engine/. Nothing about a field
// being returned is visible from the handler.
func TestReclaimCountIsSerialisedEvenWhenItIsZero(t *testing.T) {
	body, err := json.Marshal(&WorkflowInstance{ID: "wf-1", ReclaimCount: 0})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(body, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, ok := round["reclaim_count"]
	if !ok {
		t.Fatalf("reclaim_count is absent from the serialised workflow when it is 0.\n\n"+
			"An operator asking whether a workflow has been reclaimed cannot tell that "+
			"apart from a build that does not report the field at all. Remove omitempty.\n"+
			"got: %s", body)
	}
	if got != float64(0) {
		t.Errorf("reclaim_count serialised as %#v, want 0", got)
	}
}
