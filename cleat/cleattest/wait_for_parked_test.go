package cleattest

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/cleat"
)

// waitForParkedTimeoutWorkflow is the shape cleat#3098 could not join: it
// parks in AwaitSignals, and the test has to advance the clock past the
// timeout and then join it.
func waitForParkedTimeoutWorkflow(h cleat.HostCalls) error {
	sr := h.AwaitSignals([]string{"approved"}, 24*time.Hour)
	if sr.TimedOut {
		return fmt.Errorf("approval timed out")
	}
	return nil
}

// fatalfSpy records Fatalf calls without the Goexit a real *testing.T
// performs, so the expiry path can be asserted on rather than ending the
// test that exercises it.
type fatalfSpy struct {
	mu   sync.Mutex
	msgs []string
}

func (f *fatalfSpy) Fatalf(format string, args ...interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, fmt.Sprintf(format, args...))
}

func (f *fatalfSpy) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.msgs...)
}

func TestWaitForParkedReturnsOnASleep(t *testing.T) {
	env := NewTestEnv()
	go func() { env.H().DurableSleep(1 * time.Second) }()
	env.WaitForParked(t) // must return; a hang here is the failure mode
}

func TestWaitForParkedReturnsOnAnAwaitSignals(t *testing.T) {
	env := NewTestEnv()
	go func() { env.H().AwaitSignals([]string{"greeting"}, time.Hour) }()
	env.WaitForParked(t)
}

// The known-positive. A guard that only ever returns cannot be distinguished
// from one that is not checking anything, so park an env with NO goroutine
// and require the bound to fire and to say what it was waiting for.
func TestWaitForParkedFailsWhenNothingParks(t *testing.T) {
	env := NewTestEnv()
	env.parkedTimeout = 50 * time.Millisecond

	spy := &fatalfSpy{}
	env.WaitForParked(spy)

	msgs := spy.messages()
	if len(msgs) == 0 {
		t.Fatal("WaitForParked returned on an env where nothing parks. The bound is " +
			"not enforced, so a caller waiting for a park it will never get hangs " +
			"until the package timeout with nothing saying what it awaited.")
	}
	if !strings.Contains(msgs[0], "WaitForParked") || !strings.Contains(msgs[0], "parked") {
		t.Fatalf("the failure does not name what was awaited, so it cannot be told "+
			"from any other timeout:\n%s", msgs[0])
	}
}

// The end-to-end the issue is about (cleat#3098's finding 1): establish the
// park, THEN move the clock, THEN join. Without WaitForParked the advance
// lands first 20 of 20 times and the deadline is computed off the moved
// clock, so the join blocks -- which is why #3098 recorded the timeout idiom
// as unjoinable.
func TestWaitForParkedMakesAnAdvanceCount(t *testing.T) {
	const n = 20
	for i := 0; i < n; i++ {
		env := NewTestEnv()
		h := env.H()

		done := make(chan error, 1)
		go func() { done <- waitForParkedTimeoutWorkflow(h) }()

		env.WaitForParked(t)
		env.AdvanceTime(25 * time.Hour)

		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("run %d: the parked goroutine returned nil; the advance did "+
					"not fire the AwaitSignals timeout", i)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("run %d: joined goroutine did not return -- the advance landed "+
				"before the park, or was lost", i)
		}
	}
}
