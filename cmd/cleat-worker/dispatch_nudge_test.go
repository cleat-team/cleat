package main

// cleat#4491: a suspended run whose timer comes due is claimed when it is due, not at the next rung of the idle backoff ladder.
//
// Three layers, each of which can be wrong while the others are right, so each has its own test: the nudger's arithmetic (when, how many, how far ahead), the idle loop's
// reading of the channel, and the call site that gives the nudger the wake time the store was just given. The last is the one a unit test of the nudger cannot see: a nudger
// nothing calls passes every test of its own and changes nothing (the failure CLAUDE.md records as "a mechanism that exists and is wired to nothing"). The end-to-end claim, that a
// real worker serves a suspended timer within tens of milliseconds, is measured by the residency benchmark's `timer` row (tests/chaos, tags `chaos bench`), not asserted here.

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// scheduled records what a nudger asked its timer for, and lets a test fire it by hand.
type scheduled struct {
	d []time.Duration
	f []func()
}

func newFakeNudger(now time.Time) (*dispatchNudger, chan struct{}, *scheduled) {
	ch := make(chan struct{}, 1)
	s := &scheduled{}
	n := newDispatchNudger(ch)
	n.now = func() time.Time { return now }
	n.afterFunc = func(d time.Duration, f func()) { s.d = append(s.d, d); s.f = append(s.f, f) }
	return n, ch, s
}

func TestADueTimerIsNudgedAtItsWakeTimePlusTheSlack(t *testing.T) {
	now := time.Date(2026, 11, 27, 5, 0, 0, 0, time.UTC)
	n, ch, s := newFakeNudger(now)

	if !n.at(now.Add(2 * time.Second)) {
		t.Fatal("a timer 2 s ahead was not scheduled")
	}
	if len(s.d) != 1 || s.d[0] != 2*time.Second+dispatchNudgeSlack {
		t.Fatalf("scheduled %v, want one timer of 2s + %s: the slack keeps the claim from running before next_wake_at", s.d, dispatchNudgeSlack)
	}
	select {
	case <-ch:
		t.Fatal("the nudge was sent when it was scheduled, not when it fired")
	default:
	}
	s.f[0]()
	select {
	case <-ch:
	default:
		t.Fatal("firing the timer sent no nudge")
	}
	if got := n.pending.Load(); got != 0 {
		t.Errorf("pending = %d after the only timer fired, want 0", got)
	}
}

func TestSeveralDueTimersCoalesceIntoOneWaitingToken(t *testing.T) {
	now := time.Date(2026, 11, 27, 5, 0, 0, 0, time.UTC)
	n, ch, s := newFakeNudger(now)
	n.at(now.Add(time.Second))
	n.at(now.Add(time.Second))
	n.at(now.Add(time.Second))
	for _, f := range s.f {
		f() // none of these may block, with one token and nobody receiving
	}
	if len(ch) != 1 {
		t.Fatalf("%d tokens waiting, want 1: the loop polls once for all of them", len(ch))
	}
	if got := n.pending.Load(); got != 0 {
		t.Errorf("pending = %d, want 0", got)
	}
}

func TestANudgeIsNotScheduledWhereItWouldBeWasteful(t *testing.T) {
	now := time.Date(2026, 11, 27, 5, 0, 0, 0, time.UTC)

	// Beyond the horizon: pinned in memory for nothing, against a lateness that is under a percent of the sleep.
	n, _, s := newFakeNudger(now)
	if n.at(now.Add(dispatchNudgeHorizon+time.Second)) || len(s.d) != 0 {
		t.Errorf("a timer past the %s horizon was scheduled: %v", dispatchNudgeHorizon, s.d)
	}
	// The boundary itself IS scheduled: the control that says the refusal above is the horizon and not a wrong comparison.
	if !n.at(now.Add(dispatchNudgeHorizon)) {
		t.Errorf("a timer exactly at the horizon was refused")
	}

	// A wake that has already passed is due now, so it is scheduled for the slack and not for a negative duration.
	n, _, s = newFakeNudger(now)
	if !n.at(now.Add(-5*time.Second)) || len(s.d) != 1 || s.d[0] != dispatchNudgeSlack {
		t.Errorf("an overdue wake scheduled %v, want one timer of %s", s.d, dispatchNudgeSlack)
	}

	// A suspension with no wake time (zero), a nudger that was never built, and one with no channel: all no-ops, none a panic.
	n, _, s = newFakeNudger(now)
	if n.at(time.Time{}) || len(s.d) != 0 {
		t.Errorf("a zero wake time was scheduled")
	}
	var nilNudger *dispatchNudger
	if nilNudger.at(now.Add(time.Second)) {
		t.Errorf("a nil nudger scheduled something")
	}
	if newDispatchNudger(nil).at(now.Add(time.Second)) {
		t.Errorf("a nudger with no channel scheduled something")
	}
}

func TestThePendingTimersAreBoundedAndTheBoundIsGivenBack(t *testing.T) {
	now := time.Date(2026, 11, 27, 5, 0, 0, 0, time.UTC)
	n, _, s := newFakeNudger(now)
	for i := 0; i < dispatchNudgeMax; i++ {
		if !n.at(now.Add(time.Minute)) {
			t.Fatalf("timer %d of %d was refused below the bound", i+1, dispatchNudgeMax)
		}
	}
	if n.at(now.Add(time.Minute)) {
		t.Fatalf("a timer past the bound of %d was scheduled", dispatchNudgeMax)
	}
	if got := n.pending.Load(); got != dispatchNudgeMax {
		t.Fatalf("pending = %d after a refusal, want %d: a refused timer must not be counted", got, dispatchNudgeMax)
	}
	s.f[0]() // one fires
	if !n.at(now.Add(time.Minute)) {
		t.Errorf("a slot freed by a fired timer was not reusable")
	}
}

// The idle backoff reads the channel. The poll interval is an hour so that nothing but the nudge can end the first sleep: a second claim inside the test's window IS the nudge.
func TestAnIdleDispatchLoopPollsAgainWhenATimerWakeArrives(t *testing.T) {
	var claims atomic.Int64
	ms := &mockStore{}
	ms.claimStickyWorkflowsFn = func(context.Context, string, int) ([]*engine.WorkflowInstance, error) {
		claims.Add(1)
		return nil, nil
	}
	ms.claimWorkflowsFn = func(context.Context, string, int) ([]*engine.WorkflowInstance, error) { return nil, nil }
	ms.countRunnableWorkflowsFn = func(context.Context) (int, error) { return 0, nil }

	w := newTestWorker(ms)
	w.pollInterval = time.Hour
	w.timerWakeCh = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	w.wg.Add(1)
	go func() { w.dispatchLoop(); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	waitFor := func(want int64, within time.Duration) bool {
		deadline := time.Now().Add(within)
		for time.Now().Before(deadline) {
			if claims.Load() >= want {
				return true
			}
			time.Sleep(2 * time.Millisecond)
		}
		return claims.Load() >= want
	}
	if !waitFor(1, 5*time.Second) {
		t.Fatal("the loop never made its first claim")
	}
	// The control: with a one-hour poll and no nudge the loop is asleep, so no second claim arrives. Without this, "a second claim arrived" could be a loop that polls on its own.
	if waitFor(2, 300*time.Millisecond) {
		t.Fatal("the loop claimed again with no nudge and a one-hour poll: this test does not isolate the nudge")
	}
	w.timerWakeCh <- struct{}{}
	if !waitFor(2, 5*time.Second) {
		t.Fatal("the loop did not poll when a timer wake arrived: the idle backoff ignores timerWakeCh, so a due timer waits for the next rung of the ladder")
	}
}

// ---- the call site -----------------------------------------------------------------------------------------------

// sleepingGuest is a backend whose guest makes one durable sleep and returns, so the engine's own session produces the suspension the worker acts on.
type sleepingGuest struct{ ms int64 }

func (g *sleepingGuest) Execute(ctx context.Context, _ []byte, _ string, _ json.RawMessage, h engine.HostHandler) (*engine.ExecResult, error) {
	h.DurableSleep(ctx, nil, g.ms)
	return &engine.ExecResult{}, nil
}
func (g *sleepingGuest) Close(context.Context) error                   { return nil }
func (g *sleepingGuest) Name() string                                  { return "sleeping-guest" }
func (g *sleepingGuest) PerExecution(time.Duration) engine.WasmBackend { return g }

// finishingGuest returns without suspending: the control that a finished run schedules nothing.
type finishingGuest struct{}

func (finishingGuest) Execute(context.Context, []byte, string, json.RawMessage, engine.HostHandler) (*engine.ExecResult, error) {
	return &engine.ExecResult{}, nil
}
func (finishingGuest) Close(context.Context) error                   { return nil }
func (finishingGuest) Name() string                                  { return "finishing-guest" }
func (finishingGuest) PerExecution(time.Duration) engine.WasmBackend { return finishingGuest{} }

// runSuspension drives executeWorkflow with the given guest and finalize outcome, and reports the wake time the store was given and what the nudger scheduled.
func runSuspension(t *testing.T, guest engine.WasmBackend, finalizeErr error) (finalStatus string, wake time.Time, now time.Time, s *scheduled) {
	t.Helper()
	ms := &mockStore{
		loadWASMFn: func(context.Context, string, int) ([]byte, error) {
			return []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}, nil
		},
		finalizeWorkflowSegmentFn: func(_ context.Context, _, _ string, _ int64, _ []engine.EventRecord, status, _, _, _ string, _ map[string]string, nextWakeAt time.Time) error {
			finalStatus, wake = status, nextWakeAt
			return finalizeErr
		},
		releaseWorkflowFn: func(context.Context, string, string, int64, time.Time) error { return nil },
		failWorkflowFn: func(context.Context, string, string, int64, string, string, string, map[string]string) error {
			return nil
		},
	}
	// The engine's session reads a process-wide clock seed that dispatchLoop refreshes; a test that ran for seconds before this one leaves it stale, and a 5 s sleep then reads as already
	// elapsed and never suspends (measured: the mutant run of the loop test below, which takes 5 s, turned these tests "done"). Refresh it, so this test does not depend on its neighbours.
	engine.UpdateNowMs()
	w := newTestWorker(ms)
	w.wasmtimeBackend = guest
	now = time.Now()
	w.timerNudge, _, s = newFakeNudger(now)
	wf := &engine.WorkflowInstance{ID: "run-1", DefName: "sleeper", DefVersion: 1, TenantID: "t-1", Generation: 3, Input: json.RawMessage(`{"__entry_point":"test"}`)}
	w.wg.Add(1)
	w.executeWorkflow(wf)
	return finalStatus, wake, now, s
}

func TestASuspendedTimerIsNudgedFromTheWakeTimeTheStoreWasGiven(t *testing.T) {
	status, wake, now, s := runSuspension(t, &sleepingGuest{ms: 5000}, nil)
	if status != "ready" || wake.IsZero() {
		t.Fatalf("the segment finalized as %q with wake %v: the guest's sleep did not suspend the run, so this test did not reach the branch it is about", status, wake)
	}
	if len(s.d) != 1 {
		t.Fatalf("the nudger was asked for %d timer(s), want 1: a suspended run's wake time must reach the dispatch loop (cleat#4491)", len(s.d))
	}
	// EXACT, not a tolerance: the nudge is for the wake time the store was given, so it cannot drift from the row it is waiting for.
	if want := wake.Sub(now) + dispatchNudgeSlack; s.d[0] != want {
		t.Errorf("the nudge was scheduled in %s, want %s: the wake time the store was given, less now, plus the slack", s.d[0], want)
	}
}

func TestAFinishedRunSchedulesNoNudge(t *testing.T) {
	status, _, _, s := runSuspension(t, finishingGuest{}, nil)
	if status != "done" {
		t.Fatalf("the segment finalized as %q, want done: the control did not finish", status)
	}
	if len(s.d) != 0 {
		t.Errorf("a finished run scheduled %v: there is nothing to wake for", s.d)
	}
}

func TestASuspensionTheStoreRefusedSchedulesNoNudge(t *testing.T) {
	status, _, _, s := runSuspension(t, &sleepingGuest{ms: 5000}, engine.ErrFenceLost)
	if status != "ready" {
		t.Fatalf("the segment finalized as %q, want ready: this did not reach the finalize it refuses", status)
	}
	if len(s.d) != 0 {
		t.Errorf("a finalize that lost its fence scheduled %v: another worker owns the run, and a nudge for it is a wasted claim", s.d)
	}
}
