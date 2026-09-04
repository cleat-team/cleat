package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// The two-phase terminal transition, at the store layer, on every dialect.
// IMPROVEMENT-PLAN 3.75 step 2, decision D6.
//
// What these assert is an ORDERING, not a call. Before this, TerminateWorkflow
// wrote the terminal status and released the workflow's resources in the same
// breath -- so the host dropped the concurrency keys and the sticky assignment
// while the defer that was supposed to release them had never run. The property
// is: the resources a terminated workflow holds are still held while its defer
// phase runs, and released when the recorded outcome is applied.
//
// A dialect that arranged that some other way would rightly pass.

// startTerminableWorkflow creates a workflow and, when withDefers is set, gives
// it the one thing that decides which transition it gets: a defer registration
// in its history.
func startTerminableWorkflow(t *testing.T, ctx context.Context, store WorkflowStore, name string, withDefers bool) string {
	t.Helper()

	const defName = "defer-phase-transition"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	wfID := fmt.Sprintf("%s-%d", name, time.Now().UnixNano())
	if _, _, err := store.StartNewRun(ctx, wfID, defName, 1,
		json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	if withDefers {
		if err := store.AppendEventHistoryBatch(ctx, wfID, []EventRecord{
			{Step: 0, EventType: EventTypeDefer, DeferID: "defer-0", DeferDescription: "release the lock"},
		}); err != nil {
			t.Fatalf("AppendEventHistoryBatch (defer): %v", err)
		}
	}
	return wfID
}

// claimByID claims and returns one specific workflow, or fails.
func claimByID(t *testing.T, ctx context.Context, store WorkflowStore, workerID, wfID string) *WorkflowInstance {
	t.Helper()
	claimed, err := store.ClaimWorkflows(ctx, workerID, 50)
	if err != nil {
		t.Fatalf("ClaimWorkflows: %v", err)
	}
	for _, wf := range claimed {
		if wf.ID == wfID {
			return wf
		}
	}
	t.Fatalf("workflow %s was not claimed; got %d workflow(s)", wfID, len(claimed))
	return nil
}

// claimBoth claims in one call and asserts every named workflow came back. Two
// separate claimByID calls would not do: the first takes everything runnable,
// and the second reports the rest as unclaimable.
func claimBoth(t *testing.T, ctx context.Context, store WorkflowStore, workerID string, wfIDs ...string) {
	t.Helper()
	claimed, err := store.ClaimWorkflows(ctx, workerID, 50)
	if err != nil {
		t.Fatalf("ClaimWorkflows: %v", err)
	}
	got := make(map[string]bool, len(claimed))
	for _, wf := range claimed {
		got[wf.ID] = true
	}
	for _, id := range wfIDs {
		if !got[id] {
			t.Fatalf("workflow %s was not claimed; got %d workflow(s)", id, len(claimed))
		}
	}
}

// setDeferPhaseDeadline backdates the deadline so the expiry sweep can be
// tested without waiting out deferPhaseTimeout. It writes a database-generated
// timestamp rather than a Go one: the Go process and the database server do not
// share a clock, and the sweep compares against the database's.
func setDeferPhaseDeadline(t *testing.T, store WorkflowStore, wfID string, secondsAgo int) {
	t.Helper()
	switch s := store.(type) {
	case *PostgresStore:
		if _, err := s.db.Exec(
			`UPDATE workflow_instances SET defer_phase_deadline = now() - ($1 * interval '1 second') WHERE id = $2`,
			secondsAgo, wfID); err != nil {
			t.Fatalf("setDeferPhaseDeadline (postgres): %v", err)
		}
	case *MySQLStore:
		if _, err := s.db.Exec(
			`UPDATE workflow_instances SET defer_phase_deadline = NOW(6) - INTERVAL ? SECOND WHERE id = ?`,
			secondsAgo, wfID); err != nil {
			t.Fatalf("setDeferPhaseDeadline (mysql): %v", err)
		}
	case *MSSQLStore:
		if _, err := s.db.Exec(
			`UPDATE workflow_instances SET defer_phase_deadline = DATEADD(SECOND, @p1, SYSUTCDATETIME()) WHERE id = @p2`,
			-secondsAgo, wfID); err != nil {
			t.Fatalf("setDeferPhaseDeadline (mssql): %v", err)
		}
	default:
		t.Fatalf("setDeferPhaseDeadline: unknown store type %T", store)
	}
}

// backdateHeartbeat makes a claimed workflow look stale to ReapStaleInstances.
func backdateHeartbeat(t *testing.T, store WorkflowStore, wfID string, secondsAgo int) {
	t.Helper()
	switch s := store.(type) {
	case *PostgresStore:
		if _, err := s.db.Exec(
			`UPDATE workflow_instances SET heartbeat_at = now() - ($1 * interval '1 second') WHERE id = $2`,
			secondsAgo, wfID); err != nil {
			t.Fatalf("backdateHeartbeat (postgres): %v", err)
		}
	case *MySQLStore:
		if _, err := s.db.Exec(
			`UPDATE workflow_instances SET heartbeat_at = NOW(6) - INTERVAL ? SECOND WHERE id = ?`,
			secondsAgo, wfID); err != nil {
			t.Fatalf("backdateHeartbeat (mysql): %v", err)
		}
	case *MSSQLStore:
		if _, err := s.db.Exec(
			`UPDATE workflow_instances SET heartbeat_at = DATEADD(SECOND, @p1, SYSUTCDATETIME()) WHERE id = @p2`,
			-secondsAgo, wfID); err != nil {
			t.Fatalf("backdateHeartbeat (mssql): %v", err)
		}
	default:
		t.Fatalf("backdateHeartbeat: unknown store type %T", store)
	}
}

// A workflow with registered defers does not terminate here. It acquires a
// defer phase: 'terminating', with the outcome recorded on the row and the
// workflow still claimable so the phase can replay it.
func TestTerminatingAWorkflowWithDefersEntersItsDeferPhase(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			wfID := startTerminableWorkflow(t, ctx, store, "with-defers", true)
			if err := store.TerminateWorkflow(ctx, wfID, "terminated by test"); err != nil {
				t.Fatalf("TerminateWorkflow: %v", err)
			}

			wf := mustGetWorkflow(t, ctx, store, wfID)
			if wf.Status != statusTerminating {
				t.Fatalf("status = %q, want %q: a workflow that owes cleanup must not be "+
					"terminal yet, or there is no live instance to run its defers in",
					wf.Status, statusTerminating)
			}

			claimed := claimByID(t, ctx, store, "worker-defer-phase", wfID)
			if claimed.PendingTerminalStatus != "terminated" {
				t.Fatalf("claim carried PendingTerminalStatus %q, want %q -- without it the "+
					"executor cannot tell a defer segment from ordinary work and would run "+
					"the workflow body",
					claimed.PendingTerminalStatus, "terminated")
			}
		})
	}
}

// Terminating a workflow that is already running its defer phase terminates it
// now, and takes the marker with it. The alternative -- restarting the clock,
// or refusing -- would leave an operator with no way to stop a cleanup that is
// itself stuck, and a marker on a terminal row is a state nothing else in the
// system knows how to read.
func TestTerminatingADeferPhaseCutsItShort(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			wfID := startTerminableWorkflow(t, ctx, store, "twice", true)
			if err := store.TerminateWorkflow(ctx, wfID, "first"); err != nil {
				t.Fatalf("TerminateWorkflow (first): %v", err)
			}
			if wf := mustGetWorkflow(t, ctx, store, wfID); wf.Status != statusTerminating {
				t.Fatalf("status = %q after the first terminate, want %q", wf.Status, statusTerminating)
			}

			if err := store.TerminateWorkflow(ctx, wfID, "second"); err != nil {
				t.Fatalf("TerminateWorkflow (second): %v", err)
			}
			if wf := mustGetWorkflow(t, ctx, store, wfID); wf.Status != "terminated" {
				t.Fatalf("status = %q after the second terminate, want \"terminated\"", wf.Status)
			}
			// And the marker went with it: the row is not claimable, so a
			// marker left behind would be one nothing could ever clear.
			claimed, err := store.ClaimWorkflows(ctx, "worker-twice", 50)
			if err != nil {
				t.Fatalf("ClaimWorkflows: %v", err)
			}
			for _, wf := range claimed {
				if wf.ID == wfID {
					t.Fatal("a terminated workflow was claimed")
				}
			}
			// The deadline sweep must find nothing, which is the observable
			// form of "the marker is gone".
			if dps, ok := store.(DeferPhaseStore); ok {
				setDeferPhaseDeadline(t, store, wfID, 60)
				if n, err := dps.ExpireDeferPhases(ctx); err != nil {
					t.Fatalf("ExpireDeferPhases: %v", err)
				} else if n != 0 {
					t.Fatalf("the deadline sweep found %d phase(s) on a terminated workflow: "+
						"the second terminate left its marker behind", n)
				}
			}
		})
	}
}

// The control, and it is not decoration: with no defers registered there is no
// body to run, and paying a claim, a replay and a WASM instantiation to run
// nothing would be a regression for every workflow in most deployments.
//
// It is also what keeps the test above honest. If terminate took the two-phase
// path unconditionally, that test would pass while saying nothing about the
// defers it names.
func TestTerminatingAWorkflowWithNoDefersStaysOnePhase(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			wfID := startTerminableWorkflow(t, ctx, store, "no-defers", false)
			if err := store.TerminateWorkflow(ctx, wfID, "terminated by test"); err != nil {
				t.Fatalf("TerminateWorkflow: %v", err)
			}

			wf := mustGetWorkflow(t, ctx, store, wfID)
			if wf.Status != "terminated" {
				t.Fatalf("status = %q, want \"terminated\": a workflow with nothing to clean "+
					"up has no reason to spend a segment on it", wf.Status)
			}
		})
	}
}

// The ordering property the whole mechanism exists for.
//
// A concurrency key is the sharpest case because it is contended: while the
// terminated workflow still holds it, nothing else can have it, and the moment
// it is released the queue behind it moves. So "released" and "still held" are
// both directly observable, at each of the three points that matter.
func TestTheDeferPhaseHoldsTheWorkflowsResourcesUntilItFinishes(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			wfID := startTerminableWorkflow(t, ctx, store, "holds-lock", true)
			otherID := startTerminableWorkflow(t, ctx, store, "queued-behind", false)

			acquired, err := store.AcquireConcurrencyKey(ctx, "key-defer-phase", wfID, time.Hour)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey: %v", err)
			}
			if !acquired {
				t.Fatal("the first acquire must succeed, or the rest of this test is vacuous")
			}
			// Control: the key is genuinely contended, so a later successful
			// acquire means it was released rather than never held.
			if taken, err := store.AcquireConcurrencyKey(ctx, "key-defer-phase", otherID, time.Hour); err != nil {
				t.Fatalf("AcquireConcurrencyKey (contended): %v", err)
			} else if taken {
				t.Fatal("a second workflow acquired a held key, so this test cannot tell a " +
					"released slot from one that was never held")
			}

			if err := store.TerminateWorkflow(ctx, wfID, "terminated by test"); err != nil {
				t.Fatalf("TerminateWorkflow: %v", err)
			}

			// (1) Still held. This is the assertion the old one-phase
			// transition fails: it released here, before any defer had run.
			if taken, err := store.AcquireConcurrencyKey(ctx, "key-defer-phase", otherID, time.Hour); err != nil {
				t.Fatalf("AcquireConcurrencyKey (during defer phase): %v", err)
			} else if taken {
				t.Fatal("the key was released at mark time: the host dropped the resource " +
					"before the defer that releases it had a chance to run, which is the " +
					"pre-emption IMPROVEMENT-PLAN 3.75 describes")
			}

			// (2) The phase runs. A store-level test has no guest, so this
			// stands in for the segment: what it proves is that the FINALIZE
			// is what releases, not the mark.
			claimed := claimByID(t, ctx, store, "worker-holds-lock", wfID)
			dps, ok := store.(DeferPhaseStore)
			if !ok {
				t.Fatalf("%T cannot finalize a defer phase, but it marked one", store)
			}
			if err := dps.FinalizeDeferPhase(ctx, wfID, "worker-holds-lock", claimed.Generation, nil); err != nil {
				t.Fatalf("FinalizeDeferPhase: %v", err)
			}

			// (3) Applied and released.
			if wf := mustGetWorkflow(t, ctx, store, wfID); wf.Status != "terminated" {
				t.Fatalf("status = %q after the defer phase, want \"terminated\": the outcome "+
					"recorded at mark time is the one that has to be applied", wf.Status)
			}
			if taken, err := store.AcquireConcurrencyKey(ctx, "key-defer-phase", otherID, time.Hour); err != nil {
				t.Fatalf("AcquireConcurrencyKey (after finalize): %v", err)
			} else if !taken {
				t.Fatal("the key was never released: a terminated workflow is holding a " +
					"concurrency slot that live workflows queue behind")
			}
		})
	}
}

// FinalizeDeferPhase is fenced on the claim like every other segment write, and
// on the marker as well.
//
// The three cases are separated because they are refused by three different
// things, and the first draft of this test got that wrong: it asserted that the
// MARKER predicate is what refuses a repeated finalize, and it passed with that
// predicate deleted. What refuses the repeat is the finalize clearing
// assigned_to, so the fence no longer matches. The marker predicate is for the
// third case below -- a claimed workflow that owes no defer phase at all -- and
// only that case can see it.
func TestFinalizeDeferPhaseIsFencedOnTheClaimAndOnTheMarker(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			dps, ok := store.(DeferPhaseStore)
			if !ok {
				t.Fatalf("%T is not a DeferPhaseStore", store)
			}

			wfID := startTerminableWorkflow(t, ctx, store, "fenced", true)
			if err := store.TerminateWorkflow(ctx, wfID, "terminated by test"); err != nil {
				t.Fatalf("TerminateWorkflow: %v", err)
			}
			claimed := claimByID(t, ctx, store, "worker-fenced", wfID)

			// A stale generation is a worker that was reaped, or one whose
			// phase the deadline sweep took away.
			err := dps.FinalizeDeferPhase(ctx, wfID, "worker-fenced", claimed.Generation-1, nil)
			if !errors.Is(err, ErrFenceLost) {
				t.Fatalf("stale generation: err = %v, want ErrFenceLost", err)
			}
			// Still claimed, so the status here is 'running' rather than
			// 'terminating' -- what matters is that the recorded outcome was
			// NOT applied by a worker whose claim had moved on.
			if wf := mustGetWorkflow(t, ctx, store, wfID); wf.Status == "terminated" {
				t.Fatal("a fenced-out worker applied the terminal outcome")
			}

			// The real one succeeds.
			if err := dps.FinalizeDeferPhase(ctx, wfID, "worker-fenced", claimed.Generation, nil); err != nil {
				t.Fatalf("FinalizeDeferPhase: %v", err)
			}

			// And repeating it does not undo it: the finalize cleared
			// assigned_to, so the same worker and generation no longer
			// satisfy the fence.
			err = dps.FinalizeDeferPhase(ctx, wfID, "worker-fenced", claimed.Generation, nil)
			if !errors.Is(err, ErrFenceLost) {
				t.Fatalf("repeat finalize: err = %v, want ErrFenceLost", err)
			}
			if wf := mustGetWorkflow(t, ctx, store, wfID); wf.Status != "terminated" {
				t.Fatalf("status = %q after a repeated finalize, want \"terminated\"", wf.Status)
			}

			// The marker predicate's own case: a workflow this worker really
			// does hold, at the right generation, that owes no defer phase.
			// The fence is satisfied here -- only `pending_terminal_status IS
			// NOT NULL` refuses it, and what it is refusing is a write of
			// status = NULL over a running workflow. Without it this call
			// does not become correct, it becomes a NOT NULL violation with a
			// message about a column instead of about a fence.
			liveID := startTerminableWorkflow(t, ctx, store, "no-phase", false)
			live := claimByID(t, ctx, store, "worker-live", liveID)
			err = dps.FinalizeDeferPhase(ctx, liveID, "worker-live", live.Generation, nil)
			if !errors.Is(err, ErrFenceLost) {
				t.Fatalf("finalize against a workflow with no defer phase: err = %v, want ErrFenceLost", err)
			}
			if wf := mustGetWorkflow(t, ctx, store, liveID); wf.Status != "running" {
				t.Fatalf("status = %q, want \"running\": a workflow that owes no defer phase "+
					"must come through this call untouched", wf.Status)
			}
		})
	}
}

// Past its deadline, a defer phase that has not finished gives up its cleanup
// and applies the outcome anyway. A terminate that cannot run its defers must
// still terminate.
func TestAnExpiredDeferPhaseTerminatesWithoutItsCleanup(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			dps, ok := store.(DeferPhaseStore)
			if !ok {
				t.Fatalf("%T is not a DeferPhaseStore", store)
			}

			wfID := startTerminableWorkflow(t, ctx, store, "expired", true)
			otherID := startTerminableWorkflow(t, ctx, store, "expired-other", false)
			if acquired, err := store.AcquireConcurrencyKey(ctx, "key-expired", wfID, time.Hour); err != nil {
				t.Fatalf("AcquireConcurrencyKey: %v", err)
			} else if !acquired {
				t.Fatal("the first acquire must succeed, or the rest of this test is vacuous")
			}
			if err := store.TerminateWorkflow(ctx, wfID, "terminated by test"); err != nil {
				t.Fatalf("TerminateWorkflow: %v", err)
			}

			// Control: a phase inside its deadline is left alone. Without
			// this, a sweep that expired everything would pass the assertion
			// below.
			if n, err := dps.ExpireDeferPhases(ctx); err != nil {
				t.Fatalf("ExpireDeferPhases (before the deadline): %v", err)
			} else if n != 0 {
				t.Fatalf("expired %d phase(s) that were still inside their deadline", n)
			}
			if wf := mustGetWorkflow(t, ctx, store, wfID); wf.Status != statusTerminating {
				t.Fatalf("status = %q, want %q before the deadline", wf.Status, statusTerminating)
			}

			setDeferPhaseDeadline(t, store, wfID, 60)

			n, err := dps.ExpireDeferPhases(ctx)
			if err != nil {
				t.Fatalf("ExpireDeferPhases: %v", err)
			}
			if n != 1 {
				t.Fatalf("expired %d phase(s), want 1", n)
			}
			if wf := mustGetWorkflow(t, ctx, store, wfID); wf.Status != "terminated" {
				t.Fatalf("status = %q, want \"terminated\": past the deadline the recorded "+
					"outcome is applied without the cleanup, or the workflow sits in "+
					"'terminating' forever", wf.Status)
			}
			// The resources are released here too. The cleanup did not run,
			// which is exactly why the host's own release still has to.
			if taken, err := store.AcquireConcurrencyKey(ctx, "key-expired", otherID, time.Hour); err != nil {
				t.Fatalf("AcquireConcurrencyKey (after expiry): %v", err)
			} else if !taken {
				t.Fatal("an expired defer phase left the concurrency slot held, so a " +
					"workflow that could not clean up leaks the very thing the cleanup " +
					"was for")
			}
		})
	}
}

// A worker that dies mid-defer-phase is reclaimed by the ordinary heartbeat
// sweep -- but back to 'terminating', not to 'ready'. The marker on the row is
// what makes the next claim a defer segment either way; this is about the
// status telling the truth, which is the whole reason D6 gave the window its
// own status instead of reusing 'ready'.
func TestReapingADeferPhaseReturnsItToTerminating(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			wfID := startTerminableWorkflow(t, ctx, store, "reaped", true)
			ordinaryID := startTerminableWorkflow(t, ctx, store, "reaped-ordinary", false)
			if err := store.TerminateWorkflow(ctx, wfID, "terminated by test"); err != nil {
				t.Fatalf("TerminateWorkflow: %v", err)
			}
			// One claim for both: a second call would find nothing, because
			// the first already took every runnable row.
			claimBoth(t, ctx, store, "worker-doomed", wfID, ordinaryID)

			backdateHeartbeat(t, store, wfID, 600)
			backdateHeartbeat(t, store, ordinaryID, 600)
			if _, err := store.ReapStaleInstances(ctx, 60*time.Second); err != nil {
				t.Fatalf("ReapStaleInstances: %v", err)
			}

			if wf := mustGetWorkflow(t, ctx, store, wfID); wf.Status != statusTerminating {
				t.Fatalf("status = %q after reaping a defer phase, want %q", wf.Status, statusTerminating)
			}
			// Control: an ordinary workflow still comes back 'ready', so the
			// CASE above is a distinction rather than a blanket rewrite.
			if wf := mustGetWorkflow(t, ctx, store, ordinaryID); wf.Status != "ready" {
				t.Fatalf("status = %q after reaping an ordinary workflow, want \"ready\"", wf.Status)
			}
			// And it is still claimable, or the phase would never run again.
			claimed := claimByID(t, ctx, store, "worker-successor", wfID)
			if claimed.PendingTerminalStatus != "terminated" {
				t.Fatalf("the reclaimed claim carried PendingTerminalStatus %q, want %q",
					claimed.PendingTerminalStatus, "terminated")
			}
		})
	}
}

// deferPhaseOwed is the whole of "does this terminate take two phases", and it
// is worth a direct table because two of its answers are the ones a reader will
// get wrong.
//
// 'terminating' answers NO, which reads backwards: a workflow already in its
// defer phase is not given another one, it is terminated on the spot and the
// one-phase UPDATE takes the marker with it.
//
// A compacted workflow answers YES with no defer events, because compaction
// pruned the rows the EXISTS looks for. The conservative answer costs one
// segment that runs nothing; the exact-looking one would skip cleanup on
// exactly the long-running workflows most likely to hold a lock.
func TestDeferPhaseOwed(t *testing.T) {
	for _, tc := range []struct {
		status    string
		hasDefers bool
		compacted bool
		want      bool
		because   string
	}{
		{"running", true, false, true, "the ordinary case: a live workflow with registered defers"},
		{"ready", true, false, true, "not yet claimed, but the defers are registered"},
		{"suspended", true, false, true, "sleeping; the segment replays it and re-suspends"},
		{"running", false, false, false, "nothing to run, so nothing to wait for"},
		{"running", false, true, true, "compacted: the EXISTS cannot see rows compaction pruned"},
		{"terminated", true, false, false, "already terminal; terminate stays idempotent"},
		{"done", true, false, false, "already terminal"},
		{"failed", true, false, false, "already terminal"},
		{"dead_lettered", true, false, false, "not dispatchable, so not replayable"},
		{statusTerminating, true, false, false, "already in a defer phase: the second terminate cuts it short"},
	} {
		tc := tc
		t.Run(fmt.Sprintf("%s/defers=%v/compacted=%v", tc.status, tc.hasDefers, tc.compacted), func(t *testing.T) {
			if got := deferPhaseOwed(tc.status, tc.hasDefers, tc.compacted); got != tc.want {
				t.Errorf("deferPhaseOwed(%q, %v, %v) = %v, want %v -- %s",
					tc.status, tc.hasDefers, tc.compacted, got, tc.want, tc.because)
			}
		})
	}
}
