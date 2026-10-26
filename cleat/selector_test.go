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

	// children completes each child at a durable instant; PollChild answers
	// as of nowMs, the way the engine's does (cleat#3369).
	children    map[string]fakeChild
	awaitedAny  [][]string
	pollsOfKids int
}

type fakeChild struct {
	doneAtMs int64
	result   string
	err      error // a child that failed
}

func newFakeClockHost(startMs int64) (*fakeClockHost, HostCalls) {
	f := &fakeClockHost{nowMs: startMs, signals: map[string]string{}, children: map[string]fakeChild{}}
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
	impl.pollChild = func(runID string) (string, string, error) {
		f.pollsOfKids++
		c, ok := f.children[runID]
		switch {
		case !ok:
			return "failed", "", &selectorTestError{"no such child " + runID}
		case f.nowMs < c.doneAtMs:
			return "running", "", nil
		case c.err != nil:
			return "failed", "", c.err
		}
		return "completed", c.result, nil
	}
	// AwaitChild suspends until the child completes: the clock moves to its
	// instant. The pre-#3369 Select called this on a running child, which is
	// how a child at +5s beat a timer at +1.5s.
	impl.awaitChild = func(runID string) (string, error) {
		c := f.children[runID]
		if c.doneAtMs > f.nowMs {
			f.nowMs = c.doneAtMs
		}
		return c.result, c.err
	}
	// AwaitAnyChild suspends until the first of runIDs completes: the clock
	// moves to that child's instant.
	impl.awaitAnyChild = func(runIDs []string) (string, string, error) {
		f.awaitedAny = append(f.awaitedAny, runIDs)
		first := ""
		for _, id := range runIDs {
			if first == "" || f.children[id].doneAtMs < f.children[first].doneAtMs {
				first = id
			}
		}
		c := f.children[first]
		if c.doneAtMs > f.nowMs {
			f.nowMs = c.doneAtMs
		}
		return first, c.result, c.err
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

// TestASelectorChildThatFinishesAfterTheTimerLosesToIt is cleat#3369.
//
// The children were checked with AwaitChild, which suspends on a running
// child, so a Selector racing a child against a timer woke only when the
// child finished: here at +5s, past a 1.5s deadline. The timer must win, at
// its deadline, with the child never reported.
func TestASelectorChildThatFinishesAfterTheTimerLosesToIt(t *testing.T) {
	for _, withSignal := range []bool{false, true} {
		f, h := newFakeClockHost(1_000_000)
		f.children["child-1"] = fakeChild{doneAtMs: 1_005_000, result: "late"}

		var result, payload string
		var fired bool
		s := NewSelector(h)
		s.AddChildWorkflow("child-1", &result)
		s.AddTimer(1500*time.Millisecond, &fired)
		if withSignal {
			s.AddSignal("approved", &payload)
		}

		winner := s.Select()

		if winner != SelectorTimer || !fired {
			t.Fatalf("signal=%v: winner = %q (fired %v), want the timer: a child finishing at +5s "+
				"won a race against a 1.5s deadline (cleat#3369)", withSignal, winner, fired)
		}
		if result != "" {
			t.Errorf("signal=%v: the losing child's result was written: %q", withSignal, result)
		}
		if got := f.nowMs - 1_000_000; got != 1500 {
			t.Errorf("signal=%v: the timer fired at +%dms, want +1500ms", withSignal, got)
		}
		if len(f.awaitedAny) != 0 {
			t.Errorf("signal=%v: Select suspended on the child alone (AwaitAnyChild %v) with a timer pending", withSignal, f.awaitedAny)
		}
	}
}

// The other half: a child that finishes before the deadline wins, seen at
// the first poll after it finished, not at the deadline.
func TestASelectorChildThatFinishesBeforeTheTimerWins(t *testing.T) {
	f, h := newFakeClockHost(1_000_000)
	f.children["child-1"] = fakeChild{doneAtMs: 1_002_500, result: `{"ok":true}`}

	var result string
	var fired bool
	s := NewSelector(h)
	s.AddChildWorkflow("child-1", &result)
	s.AddTimer(10*time.Second, &fired)

	if winner := s.Select(); winner != "child-1" {
		t.Fatalf("winner = %q, want child-1", winner)
	}
	if result != `{"ok":true}` || fired || s.Err() != nil {
		t.Errorf("result %q, fired %v, err %v", result, fired, s.Err())
	}
	// Polls at +1s and +3s (1s, then doubled): seen at +3s, well before +10s.
	if got := f.nowMs - 1_000_000; got != 3000 {
		t.Errorf("the child was seen at +%dms, want +3000ms (polls at 1s, then 2s more)", got)
	}
}

// A signal that arrives while a child is pending wins: the signal wait is
// bounded by the next poll, and the signal is not lost to it.
func TestASelectorSignalBeatsAPendingChild(t *testing.T) {
	f, h := newFakeClockHost(1_000_000)
	f.children["child-1"] = fakeChild{doneAtMs: 1_600_000}
	f.awaitFn = func(names []string, timeout time.Duration) SignalResult {
		if f.nowMs-1_000_000 >= 3000 {
			return SignalResult{Name: "approved", Payload: "yes"}
		}
		f.nowMs += timeout.Milliseconds()
		return SignalResult{TimedOut: true}
	}

	var payload string
	s := NewSelector(h)
	s.AddChildWorkflow("child-1", new(string))
	s.AddSignal("approved", &payload)

	if winner := s.Select(); winner != "approved" || payload != "yes" {
		t.Fatalf("winner = %q, payload %q, want the signal", winner, payload)
	}
	if len(f.awaitedAny) != 0 {
		t.Errorf("Select suspended on the child alone with a signal pending: %v", f.awaitedAny)
	}
}

// With nothing but children, there is nothing to race: Select suspends in
// AwaitAnyChild until one completes, and polls nothing.
func TestASelectorWithOnlyChildrenAwaitsTheFirst(t *testing.T) {
	f, h := newFakeClockHost(1_000_000)
	f.children["a"] = fakeChild{doneAtMs: 1_009_000, result: "A"}
	f.children["b"] = fakeChild{doneAtMs: 1_004_000, result: "B"}

	var a, b string
	s := NewSelector(h)
	s.AddChildWorkflow("a", &a)
	s.AddChildWorkflow("b", &b)

	if winner := s.Select(); winner != "b" || b != "B" || a != "" {
		t.Fatalf("winner %q, a %q, b %q; want b", winner, a, b)
	}
	if len(f.awaitedAny) != 1 || len(f.slept) != 0 {
		t.Errorf("AwaitAnyChild calls %v, sleeps %v; want one await and no polling sleeps", f.awaitedAny, f.slept)
	}
}

// A child that failed resolves its future: its runID wins, Err() says why,
// and its destination is left alone.
func TestASelectorReportsAFailedChild(t *testing.T) {
	f, h := newFakeClockHost(1_000_000)
	f.children["child-1"] = fakeChild{doneAtMs: 1_000_000, err: &selectorTestError{"child trapped"}}

	result := "untouched"
	var fired bool
	s := NewSelector(h)
	s.AddChildWorkflow("child-1", &result)
	s.AddTimer(time.Minute, &fired)

	if winner := s.Select(); winner != "child-1" {
		t.Fatalf("winner = %q, want child-1", winner)
	}
	if s.Err() == nil || result != "untouched" || fired {
		t.Errorf("err %v, result %q, fired %v", s.Err(), result, fired)
	}
}

// A long race costs a bounded number of durable waits: the poll interval
// doubles from minChildPoll to maxChildPoll, so ten minutes is a couple of
// dozen waits, not one every poll.
func TestASelectorBacksOffWhilePolling(t *testing.T) {
	f, h := newFakeClockHost(1_000_000)
	f.children["child-1"] = fakeChild{doneAtMs: 1 << 62}

	var fired bool
	s := NewSelector(h)
	s.AddChildWorkflow("child-1", new(string))
	s.AddTimer(10*time.Minute, &fired)

	if winner := s.Select(); winner != SelectorTimer {
		t.Fatalf("winner = %q", winner)
	}
	if n := len(f.slept); n > 25 {
		t.Errorf("%d durable waits for a 10-minute race; the polling does not back off: %v", n, f.slept)
	}
	for _, ms := range f.slept {
		if ms > s.maxChildPoll.Milliseconds() {
			t.Errorf("a %dms wait exceeds maxChildPoll", ms)
		}
	}
	if got := f.nowMs - 1_000_000; got != 600_000 {
		t.Errorf("the timer fired at +%dms, want +600000ms", got)
	}
}
