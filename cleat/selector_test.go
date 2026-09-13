package cleat

import (
	"testing"
	"time"
)

// fakeClockHost is a HostCalls whose durable clock behaves the way the engine's
// does: Now() is a recorded value, and DurableSleep advances it.
//
// The point of a fake here rather than a real guest is ATTRIBUTION. cleat#1404
// reports a Selector timer firing immediately and attributes it to the guard in
// Select's timer check, but that guard is one of two places that can return
// SelectorTimer -- the no-signals path below it returns unconditionally after a
// sleep. A fake whose clock is correct by construction separates "the Selector
// reasons about time wrongly" from "the host did not sleep".
type fakeClockHost struct {
	nowMs    int64
	slept    []int64
	signals  map[string]string
	awaitFn  func(names []string, timeout time.Duration) SignalResult
	sleepFn  func(ms int64)
	awaitErr error
}

func newFakeClockHost(startMs int64) (*fakeClockHost, HostCalls) {
	f := &fakeClockHost{nowMs: startMs, signals: map[string]string{}}
	impl := &HostCallsImpl{}
	impl.now = func() int64 { return f.nowMs }
	impl.durableSleep = func(ms int64) {
		f.slept = append(f.slept, ms)
		if f.sleepFn != nil {
			f.sleepFn(ms)
			return
		}
		// A durable sleep ADVANCES the clock. That is the property under test:
		// a fake that slept without advancing would make the timer fire on the
		// next loop for the wrong reason, and the test would pass on a broken
		// Selector.
		f.nowMs += ms
	}
	impl.pollSignal = func(name string) (string, bool, error) {
		v, ok := f.signals[name]
		return v, ok, nil
	}
	// durableAwaitSignals, which is what AwaitSignals ultimately calls. Faking
	// the wrapper would skip AwaitSignals' own rounding guard (cleat#1331),
	// and that guard is on the path the Selector uses.
	impl.durableAwaitSignals = func(names []string, timeoutMs int64) (string, string, bool, error) {
		if f.awaitFn != nil {
			r := f.awaitFn(names, time.Duration(timeoutMs)*time.Millisecond)
			return r.Name, r.Payload, r.TimedOut, r.Err
		}
		f.nowMs += timeoutMs
		return "", "", true, nil
	}
	return f, HostCalls{HostCallsImpl: impl}
}

// TestASelectorTimerWaitsForItsDeadline.
//
// cleat#1404: a Selector with one timer and no signals returned immediately,
// with the durable clock unmoved -- a 1500 ms deadline satisfied in sixteen
// milliseconds, generation 1, no suspension.
//
// The winner alone is not the assertion. SelectorTimer is the CORRECT answer
// here; the defect is that it arrives without waiting. So this asserts the
// sleep happened and the clock moved, which is the part that was wrong.
func TestASelectorTimerWaitsForItsDeadline(t *testing.T) {
	f, h := newFakeClockHost(1_000_000)

	var fired bool
	s := NewSelector(h)
	s.AddTimer(1500*time.Millisecond, &fired)

	winner := s.Select()

	if winner != SelectorTimer {
		t.Errorf("winner = %q, want %q", winner, SelectorTimer)
	}
	if !fired {
		t.Error("the timer's fired flag was not set")
	}
	if len(f.slept) == 0 {
		t.Fatalf("Select returned %q without sleeping at all.\n\n"+
			"That is cleat#1404: a 1500ms deadline satisfied with the durable "+
			"clock unmoved and the workflow never suspended. The winner is "+
			"right and the waiting is what was missing.", winner)
	}
	if got := f.nowMs - 1_000_000; got != 1500 {
		t.Errorf("the durable clock advanced %dms, want 1500ms (slept: %v)", got, f.slept)
	}
}

// TestASelectorTimerThatHasAlreadyPassedFiresWithoutSleeping is the control.
//
// Without it, the assertion above is satisfied by a Selector that always
// sleeps -- including past a deadline that is already behind it, which would
// stall a replaying workflow.
func TestASelectorTimerThatHasAlreadyPassedFiresWithoutSleeping(t *testing.T) {
	f, h := newFakeClockHost(1_000_000)

	var fired bool
	s := NewSelector(h)
	s.AddTimer(1500*time.Millisecond, &fired)

	// The deadline is now in the past: the clock moved on without the Selector.
	f.nowMs += 5000

	if winner := s.Select(); winner != SelectorTimer {
		t.Errorf("winner = %q, want %q", winner, SelectorTimer)
	}
	if !fired {
		t.Error("the timer's fired flag was not set")
	}
	if len(f.slept) != 0 {
		t.Errorf("a deadline already in the past slept %v; it must fire at once", f.slept)
	}
}

// TestASelectorPrefersASignalThatIsAlreadyThere covers the branch ahead of the
// timer, so that "the timer fired" is never the answer to a question a signal
// had already settled.
func TestASelectorPrefersASignalThatIsAlreadyThere(t *testing.T) {
	f, h := newFakeClockHost(1_000_000)
	f.signals["approved"] = `{"ok":true}`

	var payload string
	var fired bool
	s := NewSelector(h)
	s.AddSignal("approved", &payload)
	s.AddTimer(1500*time.Millisecond, &fired)

	if winner := s.Select(); winner != "approved" {
		t.Errorf("winner = %q, want \"approved\"", winner)
	}
	if payload != `{"ok":true}` {
		t.Errorf("payload = %q", payload)
	}
	if fired {
		t.Error("the timer fired even though a signal was already present")
	}
	if len(f.slept) != 0 {
		t.Errorf("slept %v despite a signal being available immediately", f.slept)
	}
}

// TestASelectorReportsAnAwaitSignalsError.
//
// cleat#1404's second half: an AwaitSignals error was returned as an empty
// winner with Err() unset, so a caller switching on the winner fell through
// every case and had no way to learn anything had gone wrong.
func TestASelectorReportsAnAwaitSignalsError(t *testing.T) {
	f, h := newFakeClockHost(1_000_000)
	boom := &selectorTestError{"host: signal channel closed"}
	f.awaitFn = func(names []string, timeout time.Duration) SignalResult {
		return SignalResult{Err: boom}
	}

	var payload string
	s := NewSelector(h)
	s.AddSignal("approved", &payload)

	winner := s.Select()

	if s.Err() == nil {
		t.Errorf("AwaitSignals failed and Err() is nil, so the caller cannot "+
			"learn anything went wrong. Select returned %q, which a switch "+
			"over expected winners falls straight through (cleat#1404).", winner)
	}
}

type selectorTestError struct{ msg string }

func (e *selectorTestError) Error() string { return e.msg }
