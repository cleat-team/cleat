package webhookingest_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/cleat/cleattest"
	"github.com/cleat-team/cleat/cleat/pluginclients/webhookingest"
)

const (
	miss = `{"found":false}`
	hit  = `{"found":true,"id":"evt-1","event_type":"payment.confirmed","payload":{"amount":4200}}`
)

type waitOutcome struct {
	res webhookingest.AwaitWebhookOutput
	err error
}

// timedOut reports the window closing, which h.WaitForEvent reports as an error.
func (o waitOutcome) timedOut() bool {
	return o.err != nil && strings.Contains(o.err.Error(), "no event for")
}

// startWait runs Wait on its own goroutine, because it parks, and returns the
// channel its outcome arrives on.
func startWait(env *cleattest.TestEnv, in webhookingest.AwaitWebhookInput, timeout time.Duration) <-chan waitOutcome {
	out := make(chan waitOutcome, 1)
	go func() {
		res, err := webhookingest.AwaitWebhookWaiter.WaitFor(env.H(), in, timeout)
		out <- waitOutcome{res, err}
	}()
	return out
}

func finish(t *testing.T, out <-chan waitOutcome) waitOutcome {
	t.Helper()
	select {
	case o := <-out:
		return o
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return within 10s of real time")
		return waitOutcome{}
	}
}

var input = webhookingest.AwaitWebhookInput{
	SourceID: "src-1", EventType: "payment.confirmed", Keys: []string{"ord-1"},
}

func claims(env *cleattest.TestEnv) int {
	n := 0
	for _, c := range env.CallHistory() {
		if c.Service == "webhook-ingest" && c.Operation == "await_webhook" {
			n++
		}
	}
	return n
}

// The ordinary case: the event is already there. One claim, no suspend.
func TestWaitReturnsAnEventThatIsAlreadyThere(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("webhook-ingest", "await_webhook").Return(hit, nil)

	o := finish(t, startWait(env, input, time.Minute))
	if o.err != nil || !o.res.Found || o.res.ID != "evt-1" {
		t.Fatalf("outcome = %+v, want the claimed event", o)
	}
	if got := claims(env); got != 1 {
		t.Errorf("claims = %d, want exactly 1 (an event already there needs no wait)", got)
	}
}

// The event arrives while the workflow is parked: the signal wakes it and the
// second claim finds the event.
func TestWaitWakesOnTheEventSignalAndClaims(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("webhook-ingest", "await_webhook").Return(miss, nil).Return(hit, nil)

	out := startWait(env, input, time.Hour)
	env.WaitForParked(t)
	env.Signal("__evt:payment.confirmed", "")

	o := finish(t, out)
	if o.err != nil || o.res.ID != "evt-1" {
		t.Fatalf("outcome = %+v, want the claimed event", o)
	}
	if got := claims(env); got != 2 {
		t.Errorf("claims = %d, want 2: the first registers, the one after the wake claims", got)
	}
}

// A wake is not proof of a claimable event. A signal for an event an earlier wake
// already claimed must not end the wait; the next real event must.
func TestWaitIsNotEndedByASpuriousWake(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("webhook-ingest", "await_webhook").Return(miss, nil).Return(miss, nil).Return(hit, nil)

	out := startWait(env, input, time.Hour)
	env.WaitForParked(t)
	env.Signal("__evt:payment.confirmed", "") // spurious: the claim after it finds nothing

	// It must still be waiting.
	select {
	case o := <-out:
		t.Fatalf("Wait returned after a wake that claimed nothing: %+v", o)
	case <-time.After(50 * time.Millisecond):
	}

	env.WaitForParked(t)
	env.Signal("__evt:payment.confirmed", "") // the real one
	o := finish(t, out)
	if o.err != nil || o.res.ID != "evt-1" {
		t.Fatalf("outcome = %+v, want the event the second wake claimed", o)
	}
	if got := claims(env); got != 3 {
		t.Errorf("claims = %d, want 3", got)
	}
}

// Nothing arrives: the window closes. h.WaitForEvent reports that as an error
// whose text says no event arrived, which is how the engine does (a timeout is an
// error path there, not a flag).
func TestWaitTimesOutWithAnErrorThatSaysNoEventArrived(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("webhook-ingest", "await_webhook").Return(miss, nil)

	out := startWait(env, input, 5*time.Minute)
	env.WaitForParked(t)
	env.AdvanceTime(6 * time.Minute)

	o := finish(t, out)
	if !o.timedOut() || o.res.Found {
		t.Fatalf("outcome = %+v, want the window-closed error", o)
	}
	if !strings.Contains(o.err.Error(), "webhook-ingest/await_webhook") {
		t.Errorf("err = %v, want it to name the plugin function that was awaited", o.err)
	}
}

// The deadline is the timeout argument, not a constant inside the helper: the
// same wait given a longer window is still waiting where the shorter one gave up.
func TestWaitHonoursItsTimeoutArgument(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("webhook-ingest", "await_webhook").Return(miss, nil)

	out := startWait(env, input, 2*time.Hour)
	env.WaitForParked(t)
	env.AdvanceTime(90 * time.Minute)
	select {
	case o := <-out:
		t.Fatalf("a 2h wait ended after 90m: %+v", o)
	case <-time.After(50 * time.Millisecond):
	}
	env.AdvanceTime(31 * time.Minute)
	if o := finish(t, out); !o.timedOut() {
		t.Fatalf("outcome = %+v, want the window-closed error past 2h", o)
	}
}

// A wait with no deadline has nothing to expire. Refused, as AwaitSignals refuses it
// (h.WaitForEvent applies the same rounding rule).
func TestWaitRefusesAZeroTimeout(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("webhook-ingest", "await_webhook").Return(miss, nil)

	for _, d := range []time.Duration{0, -time.Second, 999 * time.Microsecond} {
		_, err := webhookingest.AwaitWebhookWaiter.WaitFor(env.H(), input, d)
		if err == nil || !strings.Contains(err.Error(), "rounds to 0ms") {
			t.Errorf("timeout %v: err = %v, want a refusal that names the rounding", d, err)
		}
	}
	if got := claims(env); got != 0 {
		t.Errorf("claims = %d: a refused wait must not touch the plugin", got)
	}
}

// A failure of the claim itself is an error, not a timeout.
func TestWaitReturnsThePluginsError(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("webhook-ingest", "await_webhook").Return("", errPlugin)

	o := finish(t, startWait(env, input, time.Minute))
	if o.err == nil || o.timedOut() {
		t.Fatalf("outcome = %+v, want the plugin's error and not a window-closed one", o)
	}
}

type pluginErr struct{}

func (pluginErr) Error() string { return "await_webhook: invalid input" }

var errPlugin error = pluginErr{}

// With no EventType the wait suspends on the plugin's default type, "webhook",
// which is what an event sent with neither header is stored as.
func TestWaitWithNoEventTypeSuspendsOnTheDefault(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnPluginCall("webhook-ingest", "await_webhook").Return(miss, nil).Return(hit, nil)

	out := startWait(env, webhookingest.AwaitWebhookInput{SourceID: "src-1"}, time.Hour)
	env.WaitForParked(t)
	env.Signal("__evt:"+webhookingest.DefaultEventType, "")
	if o := finish(t, out); o.err != nil || !o.res.Found {
		t.Fatalf("outcome = %+v, want the event", o)
	}
}

// Replay-safety: every reading of time goes through h.Now(), which the engine
// records, so a replay computes the same deadline. cleattest cannot show this by
// replaying: it matches a recorded call by function and arguments and returns the
// FIRST match, so a loop that makes two identical claims cannot be replayed in it
// (the replay returns the first claim's answer for both and never ends). So this
// measures the property from two readings that are wrong in different ways.
//
//  1. The boundary, from a simulated clock set to an instant years from now: a
//     5-minute wait is still waiting 1s before the deadline and has ended 1s after
//     it. A deadline built from the wall clock would be nowhere near either.
//  2. The source (TestWaitReadsNoWallClock): wait.go calls no wall-clock function.
//     This is the loose reading; it can only over-report (a comment naming one),
//     which is why it is the second and not the first.
func TestWaitDerivesItsDeadlineFromTheSimulatedClock(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.SetTime(time.Date(2031, 3, 4, 5, 6, 7, 0, time.UTC))
	env.OnPluginCall("webhook-ingest", "await_webhook").Return(miss, nil)

	out := startWait(env, input, 5*time.Minute)
	env.WaitForParked(t)

	env.AdvanceTime(5*time.Minute - time.Second)
	select {
	case o := <-out:
		t.Fatalf("a 5m wait ended 1s early: %+v", o)
	case <-time.After(50 * time.Millisecond):
	}

	env.AdvanceTime(2 * time.Second)
	if o := finish(t, out); !o.timedOut() {
		t.Fatalf("outcome = %+v, want the window-closed error 1s past the deadline", o)
	}
}

func TestWaitReadsNoWallClock(t *testing.T) {
	src, err := os.ReadFile("../../plugin_caller.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"time.Now(", "time.Since(", "time.Until(", "time.After(", "time.Sleep(", "time.NewTimer(", "time.Tick("} {
		if strings.Contains(string(src), banned) {
			t.Errorf("plugin_caller.go contains %s: a wall-clock read in guest code is not replayed", banned)
		}
	}
}
