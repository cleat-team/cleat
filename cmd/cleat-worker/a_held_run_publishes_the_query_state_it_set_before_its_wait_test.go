package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#4162, found by the "AI agent platform scenario" on the PR that turned residency on by default: a run that is HELD through a wait never returns to the worker, and the worker wrote a
// run's query state only when the execution returned (FinalizeWorkflowSegment, at a suspension or the end). So a client asking what a held run is doing (GET .../query, which reads the store)
// saw nothing for the whole hold. The example's approval wait polls with 2 s durable sleeps, each inside the 10 s window and so each held, which made the status it sets before the wait
// (awaiting_approval) invisible for its entire 30 s; with residency off every sleep suspended, and each suspension wrote it.
//
// This runs a real guest (testdata/residentsleep PublishThenSleep) through a real Worker and store, and reads the store DURING the sleep, which is the read the query endpoint makes. The
// control is the same guest with residency off, which must show the state at the suspension: without it, "nothing was visible" is also what a guest that never set anything looks like.
func TestAHeldRunPublishesTheQueryStateItSetBeforeItsWait(t *testing.T) {
	for _, tc := range []struct {
		name      string
		residency bool
	}{
		{"residency off (the control: a suspension writes the state)", false},
		{"residency on (the run is held)", true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := testutil.SuiteTestDB(t, "cleat_worker")
			store := engine.NewPostgresStore(db)

			wasmBytes := buildResidentSleepFixture(t)
			meta, err := wasm.ReadMetadata(wasmBytes)
			if err != nil {
				t.Fatalf("ReadMetadata: %v", err)
			}
			defName := fmt.Sprintf("held-qs-%d", time.Now().UnixNano())
			if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
				Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
				ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}
			wfID := fmt.Sprintf("held-qs-%d", time.Now().UnixNano())
			if _, _, err := store.StartNewRun(ctx, wfID, defName, meta.WorkflowVersion,
				json.RawMessage(`{"__entry_point":"PublishThenSleep"}`), "", engine.DefaultTenantUUID, 0); err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			logs := &syncBuffer{}
			w := newRealDeferPhaseWorker(t, db, store, logs)
			w.id = "held-qs-worker"
			if tc.residency {
				cfg := engine.DefaultResidencyConfig()
				cfg.MaxRuns = 2
				set, err := engine.NewResidentSet(cfg)
				if err != nil {
					t.Fatalf("NewResidentSet: %v", err)
				}
				w.resident = set
			}
			claimed := claimOne(t, ctx, store, w.id, wfID)
			w.inflight.Store(wfID, claimed)
			done := make(chan struct{})
			go func() { defer close(done); runExecuteWorkflow(w, claimed) }()

			// Wait until the run is ASLEEP in the way this variant puts it asleep: held in the resident set, or a suspended row. (Residency off, the worker re-claims its own suspended run
			// within the same executeWorkflow call, so the row is observable between the suspension and the re-claim, not after the call returns.)
			asleep := func() bool {
				if tc.residency {
					return w.resident.Len() > 0
				}
				st := statusOf(t, ctx, store, wfID)
				return st != "running" && st != "done"
			}
			deadline := time.Now().Add(20 * time.Second)
			for !asleep() && time.Now().Before(deadline) {
				select {
				case <-done:
					deadline = time.Now()
				case <-time.After(5 * time.Millisecond):
				}
			}
			if !asleep() {
				t.Fatalf("UNMEASURED: the run was never asleep in the way this variant needs (residency=%v, status %q)\nworker logs:\n%s", tc.residency, statusOf(t, ctx, store, wfID), logs.String())
			}

			got, err := store.ListQueryState(ctx, wfID)
			if err != nil {
				t.Fatalf("ListQueryState: %v", err)
			}
			if got["status"] != "awaiting_approval" || got["approval_wait"] != "entered" {
				t.Errorf("DURING the sleep (residency=%v) the store holds query state %v, want status=awaiting_approval and approval_wait=entered: the state the guest set before its wait is not visible to a client", tc.residency, got)
			}

			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatalf("the run did not finish")
			}
			if !tc.residency {
				// Residency off, the execution returned at the SUSPENSION and the run is a ready row waiting for its wake: it has not finished, so there is no end-of-run state to read. Settle it so
				// it does not outlive the test (the package refuses to leave a ready run in the shared databases).
				if err := store.TerminateWorkflow(ctx, wfID, "held query-state test cleanup"); err != nil {
					t.Fatalf("settling the suspended control run: %v", err)
				}
				return
			}
			end, err := store.ListQueryState(ctx, wfID)
			if err != nil {
				t.Fatalf("ListQueryState after the run: %v", err)
			}
			if end["status"] != "approval_received" {
				t.Errorf("after the run the store holds %v, want status=approval_received: the end-of-run write must still happen", end)
			}
		})
	}
}
