package engine

import (
	"context"
	"errors"
	"testing"
)

// cleat#3245 Phase 3 step 3 piece D: WriteCallIntent/CompleteCallIntent/
// ResolveCallIntent's fence subquery and CheckCancellation's read now read
// workflow_leases rather than workflow_instances.
//
// Same shape as piece A/C's deliberately-diverging tests: ordinary use keeps
// the two tables in sync, so a test that only exercises normal writes cannot
// tell "reads workflow_leases" apart from "reads workflow_instances" -- both
// readings agree. These tests diverge workflow_leases directly (bypassing
// every write path that would mirror the change), so the two readings
// disagree about the outcome, and the result says which table was actually
// read.

func TestWriteCallIntentFenceReadsGenerationFromLeasesNotInstances(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	wfID := newIntentWorkflow(t, ctx, store, "intent-write-reads-leases")
	wf, err := store.ClaimWorkflow(ctx, "worker-A")
	if err != nil || wf == nil || wf.ID != wfID {
		t.Fatalf("ClaimWorkflow: wf=%v err=%v", wf, err)
	}
	originalGeneration := wf.Generation

	// Diverge workflow_leases only. workflow_instances still carries
	// originalGeneration, so a fence check against workflow_instances would
	// wrongly report "held"; one against workflow_leases correctly reports
	// "lost".
	if _, err := db.Exec(`UPDATE workflow_leases SET generation = $1 WHERE id = $2`, originalGeneration+100, wfID); err != nil {
		t.Fatalf("diverge workflow_leases.generation: %v", err)
	}

	st := intentStoreOf(t, store)
	intent := EventRecord{Step: 0, EventType: EventTypeCall, Service: intentService, Op: intentOperation, Request: `{"amount":100}`}
	err = st.WriteCallIntent(ctx, wfID, intent, "worker-A", originalGeneration)
	if !errors.Is(err, ErrFenceLost) {
		t.Fatalf("WriteCallIntent with a diverged workflow_leases generation: err = %v, want ErrFenceLost "+
			"-- a nil error means the fence subquery read the unchanged workflow_instances value instead", err)
	}

	hist, err := store.LoadEventHistory(ctx, wfID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(hist) != 0 {
		t.Fatalf("WriteCallIntent persisted an intent row despite the lost fence: history = %+v", hist)
	}
}

func TestCompleteCallIntentFenceReadsGenerationFromLeasesNotInstances(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	wfID := newIntentWorkflow(t, ctx, store, "intent-complete-reads-leases")
	wf, err := store.ClaimWorkflow(ctx, "worker-A")
	if err != nil || wf == nil || wf.ID != wfID {
		t.Fatalf("ClaimWorkflow: wf=%v err=%v", wf, err)
	}
	originalGeneration := wf.Generation

	st := intentStoreOf(t, store)
	intent := EventRecord{Step: 0, EventType: EventTypeCall, Service: intentService, Op: intentOperation, Request: `{"amount":100}`}
	if err := st.WriteCallIntent(ctx, wfID, intent, "worker-A", originalGeneration); err != nil {
		t.Fatalf("WriteCallIntent (pre-divergence, fence must hold): %v", err)
	}

	// Diverge workflow_leases only, after the pending row is written.
	if _, err := db.Exec(`UPDATE workflow_leases SET generation = $1 WHERE id = $2`, originalGeneration+100, wfID); err != nil {
		t.Fatalf("diverge workflow_leases.generation: %v", err)
	}

	err = st.CompleteCallIntent(ctx, wfID, intent, []byte(`{"status":"ok"}`), "chk-1", "worker-A", originalGeneration)
	// n==0 here routes through intentFenceOrNotPending, which calls Heartbeat
	// -- itself still reading workflow_instances (piece D does not touch it,
	// see the piece D survey) -- against the ORIGINAL generation, which
	// workflow_instances still carries unchanged, so Heartbeat reports the
	// fence held and intentFenceOrNotPending returns nil. The method then
	// falls through to its own n!=1 check and reports errIntentNotPending,
	// NOT ErrFenceLost. If the completing UPDATE's EXISTS subquery had
	// instead (wrongly) read workflow_instances, it would still match the
	// unchanged original generation there, so the UPDATE would succeed
	// (n=1) and this call would return nil -- a directly observable
	// difference from the expected errIntentNotPending.
	if !errors.Is(err, errIntentNotPending) {
		t.Fatalf("CompleteCallIntent with a diverged workflow_leases generation: err = %v, want errIntentNotPending "+
			"-- nil means the fence subquery read the unchanged workflow_instances value instead and completed the intent", err)
	}
}

func TestResolveCallIntentFenceReadsGenerationFromLeasesNotInstances(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	wfID := newIntentWorkflow(t, ctx, store, "intent-resolve-reads-leases")
	wf, err := store.ClaimWorkflow(ctx, "worker-A")
	if err != nil || wf == nil || wf.ID != wfID {
		t.Fatalf("ClaimWorkflow: wf=%v err=%v", wf, err)
	}
	originalGeneration := wf.Generation

	st := intentStoreOf(t, store)
	intent := EventRecord{Step: 0, EventType: EventTypeCall, Service: intentService, Op: intentOperation, Request: `{"amount":100}`}
	if err := st.WriteCallIntent(ctx, wfID, intent, "worker-A", originalGeneration); err != nil {
		t.Fatalf("WriteCallIntent (pre-divergence, fence must hold): %v", err)
	}

	if _, err := db.Exec(`UPDATE workflow_leases SET generation = $1 WHERE id = $2`, originalGeneration+100, wfID); err != nil {
		t.Fatalf("diverge workflow_leases.generation: %v", err)
	}

	err = store.ResolveCallIntent(ctx, wfID, intent, []byte(`{"status":"ok"}`), "worker-A", originalGeneration, nil)
	// Same disambiguation shape as CompleteCallIntent above.
	if !errors.Is(err, errIntentNotPending) {
		t.Fatalf("ResolveCallIntent with a diverged workflow_leases generation: err = %v, want errIntentNotPending "+
			"-- nil means the fence subquery read the unchanged workflow_instances value instead and resolved the intent", err)
	}
}

func TestCheckCancellationReadsFromLeasesNotInstances(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	wfID := newIntentWorkflow(t, ctx, store, "cancellation-reads-leases")

	// workflow_instances.cancellation_requested stays at its StartNewRun
	// default (false) -- RequestCancellation, the only writer, is never
	// called. Setting workflow_leases directly is what makes the two
	// readings disagree.
	if _, err := db.Exec(`UPDATE workflow_leases SET cancellation_requested = true WHERE id = $1`, wfID); err != nil {
		t.Fatalf("diverge workflow_leases.cancellation_requested: %v", err)
	}

	cancelled, _, err := store.CheckCancellation(ctx, wfID)
	if err != nil {
		t.Fatalf("CheckCancellation: %v", err)
	}
	if !cancelled {
		t.Fatalf("CheckCancellation reported cancelled=false, want true " +
			"-- the read must have used workflow_instances (still false) instead of workflow_leases")
	}
}
