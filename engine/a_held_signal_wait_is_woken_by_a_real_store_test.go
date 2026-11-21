package engine

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A HELD SIGNAL WAIT ON A REAL STORE (cleat#4162, 2b-i). Two things the fake-clock rows cannot show, on every dialect that is configured:
//
//  1. the Recheck the call supplies finds a delivery a real DeliverSignal wrote, WITHOUT consuming it, so the hold ends and the call's own read then delivers it;
//  2. a signal that lands AFTER the last recheck and BEFORE the hold is sent back is not lost: the release (finalize_workflow_status, the 'ready' branch, on each dialect) compares signal_seq
//     with its value at claim and wakes the run NOW instead of at the wait's deadline. That comparison was read about in park.go and never exercised; this row exercises it.
//
// Each row has a control that can fail: a delivery that does not exist leaves the wake time alone, and an unrelated signal does not wake the hold.

func realSignalSession(t *testing.T, store WorkflowStore, wf string) *execSession {
	t.Helper()
	return &execSession{engine: NewEngine(nil, &mockCaller{}, WithSignalStore(store), WithWorkflowID(wf)), workflowID: wf}
}

func realSignalSet(t *testing.T, interval time.Duration) (*ResidentSet, func(name string) int) {
	t.Helper()
	m := newReplayMetrics(t)
	cfg := DefaultResidencyConfig()
	cfg.MaxRuns = 2
	cfg.RecheckInterval = interval
	cfg.Metrics = m
	set, err := NewResidentSet(cfg)
	if err != nil {
		t.Fatalf("NewResidentSet: %v", err)
	}
	return set, func(result string) int {
		return residentCounter(t, m, "cleat_resident_rechecks_total", `result="`+result+`"`)
	}
}

func startRealRun(t *testing.T, store WorkflowStore, key string) string {
	t.Helper()
	wf, _, err := store.StartNewRun(context.Background(), "", "test-workflow", 1, json.RawMessage(`{}`), key, DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	return wf
}

func signalWaitReq(recheck func(context.Context) (bool, error)) parkRequest {
	deadline := time.Now().Add(8 * time.Second)
	return parkRequest{Kind: wakeSignal, Key: `["go"]`, DeadlineMs: deadline.UnixMilli(), Reason: "await_signals([\"go\"], 8000ms)", Until: deadline, Recheck: recheck}
}

func TestAHeldSignalWaitIsWokenByARealDeliveryAndTheRecheckDoesNotConsumeIt(t *testing.T) {
	if testing.Short() {
		t.Skip("no database in -short mode; every per-dialect subtest would skip")
	}
	var ran []string
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()
			wf := startRealRun(t, store, "sig-wake-"+backend.Name())

			var calls atomic.Int64
			production := signalRecheckFor(realSignalSession(t, store, wf), `["go"]`)
			recheck := func(c context.Context) (bool, error) { calls.Add(1); return production(c) }
			set, rechecks := realSignalSet(t, 40*time.Millisecond)
			out := make(chan parkOutcome, 1)
			go func() { out <- set.ForRun(ResidentRunInfo{Language: "go"}).Wait(ctx, signalWaitReq(recheck)) }()

			// CONTROL 1: with nothing delivered the hold keeps rechecking and does not return.
			waitForCalls(t, &calls, 4)
			select {
			case got := <-out:
				t.Fatalf("the hold ended with %d before any delivery", got)
			default:
			}
			// CONTROL 2: a delivery of a name the wait did not ask for must not wake it. (The recheck asks the store for "go" only.)
			if err := store.DeliverSignal(ctx, wf, "other", `{"not":"for you"}`); err != nil {
				t.Fatalf("DeliverSignal(other): %v", err)
			}
			waitForCalls(t, &calls, int(calls.Load())+3)
			select {
			case got := <-out:
				t.Fatalf("the hold ended with %d on a delivery of a different name", got)
			default:
			}

			delivered := time.Now()
			if err := store.DeliverSignal(ctx, wf, "go", `{"approved":true}`); err != nil {
				t.Fatalf("DeliverSignal(go): %v", err)
			}
			select {
			case got := <-out:
				if got != parkWoke {
					t.Fatalf("the hold ended with %d, want parkWoke", got)
				}
				// An upper bound, not a measurement: one interval plus a generous allowance for a database round trip on a loaded machine. The measured comparison with residency off is the
				// timing row; this only shows the wake is the next recheck and not the deadline (8 s).
				if lat := time.Since(delivered); lat > 3*time.Second {
					t.Errorf("the wake came %s after the delivery, against a 40ms recheck interval: it was not the next recheck", lat)
				} else {
					t.Logf("%s: woke %s after the delivery (interval 40ms)", backend.Name(), lat.Round(time.Millisecond))
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the hold never saw a real delivery")
			}
			if rechecks("found") != 1 {
				t.Errorf("counted %d found recheck(s), want 1", rechecks("found"))
			}
			// THE RECHECK IS PURE: it saw the delivery and left it. The call's own read, after the wake, is what consumes it, so the guest still gets it through the one path replay uses.
			d, found, err := store.PollSignal(ctx, wf, "go")
			if err != nil || !found {
				t.Fatalf("after the wake the delivery is gone (found=%v err=%v): the recheck consumed it", found, err)
			}
			if d.Payload != `{"approved":true}` {
				t.Errorf("the delivery reads back as %q", d.Payload)
			}
			ran = append(ran, backend.Name())
		})
	}
	if len(ran) == 0 {
		t.Fatal("no backend ran, so this test measured nothing")
	}
}

func waitForCalls(t *testing.T, calls *atomic.Int64, n int) {
	t.Helper()
	limit := time.After(10 * time.Second)
	for calls.Load() < int64(n) {
		select {
		case <-limit:
			t.Fatalf("the hold made only %d recheck(s), wanted %d", calls.Load(), n)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// lateMode says when, relative to the claim and the hold, the signal under test is delivered.
type lateMode int

const (
	lateNone         lateMode = iota // CONTROL: nothing is delivered
	lateBeforeClaim                  // CONTROL: delivered before the claim, so the claim's snapshot already includes it
	lateAfterRecheck                 // the case: delivered after the hold's last recheck and before it is sent back
)

func (m lateMode) String() string {
	return [...]string{"nothing lands", "a signal landed BEFORE the claim", "a signal lands after the last recheck"}[m]
}

// A signal that lands after the last recheck and before the hold is sent back (an eviction, shutdown, memory pressure) must not be lost. The recheck is FROZEN to model exactly that
// window: it last looked before the delivery, and will not look again. The hold is then sent back and the run is released the way the worker releases a suspension. Because DeliverSignal
// bumped signal_seq after the claim stamped signal_seq_at_claim, the release wakes the run now; a release that ignored it would leave it asleep until the wait's own wake time, an hour out.
//
// The two controls make the row able to fail in both directions: with nothing delivered the release must LEAVE the wake time alone, and with a signal delivered BEFORE the claim it must also
// leave it alone (that signal is in the claim's snapshot, and the run's own first read finds it). A release that always woke at once, or that woke whenever any signal existed, would pass the
// delivery case and fail one of these.
func lateSignalCase(t *testing.T, store WorkflowStore, backend string, mode lateMode) {
	t.Helper()
	ctx := context.Background()
	wf := startRealRun(t, store, "sig-late-"+backend+"-"+boolWord(mode == lateAfterRecheck)+mode.String())
	if mode == lateBeforeClaim {
		if err := store.DeliverSignal(ctx, wf, "go", `{"early":true}`); err != nil {
			t.Fatalf("%s: %s: DeliverSignal: %v", backend, mode, err)
		}
	}
	claimed, err := store.ClaimWorkflow(ctx, "worker-sig-1")
	if err != nil || claimed == nil || claimed.ID != wf {
		t.Fatalf("%s: %s: ClaimWorkflow = %+v, %v, want the run we started", backend, mode, claimed, err)
	}

	var frozen atomic.Bool
	var mu sync.Mutex
	calls := 0
	production := signalRecheckFor(realSignalSession(t, store, wf), `["go"]`)
	recheck := func(c context.Context) (bool, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		if frozen.Load() || mode == lateBeforeClaim {
			return false, nil // it looked before the delivery and will not look again (and the early signal is the run's own first read, not the hold's business)
		}
		return production(c)
	}
	set, _ := realSignalSet(t, 30*time.Millisecond)
	out := make(chan parkOutcome, 1)
	go func() { out <- set.ForRun(ResidentRunInfo{Language: "go"}).Wait(ctx, signalWaitReq(recheck)) }()
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	frozen.Store(true)
	if mode == lateAfterRecheck {
		if err := store.DeliverSignal(ctx, wf, "go", `{"late":true}`); err != nil {
			t.Fatalf("%s: %s: DeliverSignal: %v", backend, mode, err)
		}
	}
	// Several intervals pass with the recheck frozen: the hold must STILL be held, or the row measured a wake and not a send-back.
	time.Sleep(150 * time.Millisecond)
	select {
	case got := <-out:
		t.Fatalf("%s: %s: the hold ended with %d before it was sent back", backend, mode, got)
	default:
	}
	set.EvictAll(ResidentEvictedShutdown)
	select {
	case got := <-out:
		if got != parkRefused {
			t.Fatalf("%s: %s: the sent-back hold ended with %d, want parkRefused (a suspension)", backend, mode, got)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: %s: the hold was never sent back", backend, mode)
	}

	// The release, exactly as the worker makes it for a suspension: 'ready', with the wait's own wake time an hour out.
	wake := time.Now().Add(time.Hour)
	if err := store.FinalizeWorkflowSegment(ctx, wf, "worker-sig-1", claimed.Generation, nil, "ready", "", "", "", nil, wake); err != nil {
		t.Fatalf("%s: %s: FinalizeWorkflowSegment: %v", backend, mode, err)
	}
	got, err := store.GetWorkflowByID(ctx, wf)
	if err != nil || got == nil {
		t.Fatalf("%s: %s: GetWorkflowByID: %v", backend, mode, err)
	}
	until := time.Until(got.NextWakeAt)
	if mode == lateAfterRecheck {
		if until > 5*time.Minute {
			t.Errorf("%s: %s: the run will wake in %s: the signal that landed after the last recheck was NOT noticed by the release, and the run would sleep until its deadline", backend, mode, until.Round(time.Second))
		}
		reclaimed, err := store.ClaimWorkflow(ctx, "worker-sig-2")
		if err != nil || reclaimed == nil || reclaimed.ID != wf {
			t.Errorf("%s: %s: the run is not claimable right after the release (%+v, %v), so the late signal waits for the deadline", backend, mode, reclaimed, err)
		}
		// And the delivery itself is still there for the replay to find.
		if _, found, err := store.PollSignal(ctx, wf, "go"); err != nil || !found {
			t.Errorf("%s: %s: the late delivery is gone after the release (found=%v err=%v)", backend, mode, found, err)
		}
		return
	}
	if until < 50*time.Minute {
		t.Errorf("%s: %s: the run wakes in %s, want about an hour: the row cannot tell a noticed signal from a release that wakes at once or whenever any signal exists", backend, mode, until.Round(time.Second))
	}
	if reclaimed, err := store.ClaimWorkflow(ctx, "worker-sig-2"); err != nil || reclaimed != nil {
		t.Errorf("%s: %s: the run was claimable at once (%+v, %v)", backend, mode, reclaimed, err)
	}
}

func TestASignalLandingAfterTheLastRecheckIsNotLostWhenTheHoldIsSentBack(t *testing.T) {
	if testing.Short() {
		t.Skip("no database in -short mode; every per-dialect subtest would skip")
	}
	var ran []string
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			for _, mode := range []lateMode{lateAfterRecheck, lateNone, lateBeforeClaim} {
				store, teardown := backend.Setup(t)
				setupTestData(t, store)
				truncateAll(t, store)
				lateSignalCase(t, store, backend.Name(), mode)
				teardown()
			}
			ran = append(ran, backend.Name())
		})
	}
	if len(ran) == 0 {
		t.Fatal("no backend ran, so this test measured nothing")
	}
}

func boolWord(b bool) string {
	if b {
		return "delivered"
	}
	return "none"
}
