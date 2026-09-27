package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// TestACancelledRunReadsBackAsCancelled.
//
// `cancellation_requested` and `cancellation_reason` have existed on every
// dialect since the first schema. POST /api/workflows/:id/cancel wrote them,
// the guest read them through PollCancellation(), and NO read path returned
// either -- so a cancelled run reported `status: "ready"` and an operator who
// cancelled it could not confirm the workflow had been told (cleat#1351).
//
// WHY THAT MATTERS MORE HERE THAN IT WOULD ELSEWHERE: cleat's cancellation is
// COOPERATIVE. The workflow polls and may legitimately ignore the request --
// the ports suite asserts exactly that -- so "cancelled but still running" is a
// normal, expected, possibly permanent state, and it was the one state no read
// could show. All three of "the request never landed", "it landed and the
// workflow has not polled yet" and "it landed and the workflow is ignoring it"
// read identically.
//
// THIS ASSERTS THE READ, NOT THE WRITE, and that is the whole point. The store
// tests already cover the column being set, and the column being set was never
// in doubt -- the defect was that a true database and a silent read path
// disagreed. A test that checked the database would have passed throughout.
func TestACancelledRunReadsBackAsCancelled(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)

			const defName = "cancel-readback"
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			id := fmt.Sprintf("cancel-readback-%d", time.Now().UnixNano())
			if _, _, err := store.StartNewRun(ctx, id, defName, 1,
				json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			// THE CONTROL, and it runs first on purpose: without it, a read
			// path that reported every run as cancelled would satisfy the
			// assertion below. A bool field has only two values, so "it says
			// true after a cancel" is half a measurement.
			before, err := store.GetWorkflowByID(ctx, id)
			if err != nil {
				t.Fatalf("GetWorkflowByID(before): %v", err)
			}
			if before == nil {
				t.Fatal("GetWorkflowByID(before) returned no run")
			}
			if before.CancellationRequested {
				t.Error("a run that was never cancelled reads back as cancelled")
			}
			if before.CancellationReason != "" {
				t.Errorf("a run that was never cancelled has a reason: %q", before.CancellationReason)
			}

			const reason = "INCIDENT-4242 operator cancelled, duplicate submission"
			if err := store.RequestCancellation(ctx, id, reason); err != nil {
				t.Fatalf("RequestCancellation: %v", err)
			}

			after, err := store.GetWorkflowByID(ctx, id)
			if err != nil {
				t.Fatalf("GetWorkflowByID(after): %v", err)
			}
			if after == nil {
				t.Fatal("GetWorkflowByID(after) returned no run")
			}
			if !after.CancellationRequested {
				t.Errorf("the run was cancelled and reads back as not cancelled.\n\n"+
					"status is %q, which is what an operator saw INSTEAD of the "+
					"cancellation before cleat#1351: the database said the run had "+
					"been asked to stop and every read path said otherwise.", after.Status)
			}
			if after.CancellationReason != reason {
				t.Errorf("the reason did not survive the round trip:\n  want %q\n  got  %q\n\n"+
					"A reason is supplied by a human for the benefit of another "+
					"human. Stored and unreadable, it never reaches one.",
					reason, after.CancellationReason)
			}
		})
	}
}

// TestACancellationWithNoReasonReadsBackWithNoReason covers the arm where the
// caller supplies none.
//
// Separate from the round trip above because it is the case that distinguishes
// "no reason given" from "reason lost in transit", and a single test that only
// ever passes a reason cannot tell those apart. An empty string here also has
// to survive as an empty string rather than becoming a NULL that a later
// COALESCE turns into something else.
func TestACancellationWithNoReasonReadsBackWithNoReason(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)

			const defName = "cancel-noreason"
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			id := fmt.Sprintf("cancel-noreason-%d", time.Now().UnixNano())
			if _, _, err := store.StartNewRun(ctx, id, defName, 1,
				json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			if err := store.RequestCancellation(ctx, id, ""); err != nil {
				t.Fatalf("RequestCancellation: %v", err)
			}

			wf, err := store.GetWorkflowByID(ctx, id)
			if err != nil || wf == nil {
				t.Fatalf("GetWorkflowByID: wf=%v err=%v", wf, err)
			}
			if !wf.CancellationRequested {
				t.Error("a cancellation with no reason did not set the flag")
			}
			if wf.CancellationReason != "" {
				t.Errorf("no reason was given but one came back: %q", wf.CancellationReason)
			}
		})
	}
}

// TestTheListingShowsWhichRunsHaveBeenAskedToStop.
//
// The listing is where an operator scans for a cancelled-but-still-running
// workflow, which cooperative cancellation makes a normal and possibly
// permanent state. It is also the path where a mistake is a RUNTIME failure
// rather than a compile one: the column list and the Scan are two lists that
// must agree in length and order, written separately per dialect, and Go
// cannot check that. So this runs the listing on every dialect rather than
// reasoning about the arity.
func TestTheListingShowsWhichRunsHaveBeenAskedToStop(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)

			const defName = "cancel-listing"
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			stamp := time.Now().UnixNano()
			cancelled := fmt.Sprintf("cancel-listing-yes-%d", stamp)
			untouched := fmt.Sprintf("cancel-listing-no-%d", stamp)
			for _, id := range []string{cancelled, untouched} {
				if _, _, err := store.StartNewRun(ctx, id, defName, 1,
					json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
					t.Fatalf("StartNewRun(%s): %v", id, err)
				}
			}
			if err := store.RequestCancellation(ctx, cancelled, "listing probe"); err != nil {
				t.Fatalf("RequestCancellation: %v", err)
			}

			runs, err := store.ListWorkflows(ctx, WorkflowFilter{})
			if err != nil {
				t.Fatalf("ListWorkflows: %v", err)
			}

			// BOTH runs are checked, because a listing that reported every row
			// as cancelled would satisfy a test that only looked at the
			// cancelled one -- the same reason the round-trip test reads the
			// run before cancelling it.
			seen := map[string]bool{}
			for _, r := range runs {
				switch r.ID {
				case cancelled, untouched:
					seen[r.ID] = true
					want := r.ID == cancelled
					if r.CancellationRequested != want {
						t.Errorf("listing reports CancellationRequested=%v for %s, want %v",
							r.CancellationRequested, r.ID, want)
					}
				}
			}
			for _, id := range []string{cancelled, untouched} {
				if !seen[id] {
					t.Errorf("%s did not appear in the listing at all, so nothing above was asserted", id)
				}
			}
		})
	}
}
