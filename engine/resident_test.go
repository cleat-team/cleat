package engine

import (
	"context"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/monitoring/prometheus"
)

// fakeResidentClock is the set's clock in a test: time moves only when the test says so, and a timer fires only when the test fires it, so no
// assertion below depends on how long anything took.
type fakeResidentClock struct {
	nowMs  atomic.Int64
	mu     sync.Mutex
	timers []chan time.Time
	// armed receives once for every timer a park starts, so a test can wait for "the run is parked" without sleeping.
	armed chan struct{}
}

func newFakeResidentClock(nowMs int64) *fakeResidentClock {
	c := &fakeResidentClock{armed: make(chan struct{}, 64)}
	c.nowMs.Store(nowMs)
	return c
}

func (c *fakeResidentClock) Now() time.Time { return time.UnixMilli(c.nowMs.Load()) }

func (c *fakeResidentClock) After(time.Duration) (<-chan time.Time, func()) {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	c.timers = append(c.timers, ch)
	c.mu.Unlock()
	c.armed <- struct{}{}
	return ch, func() {}
}

// fireAll fires every timer that has been started.
func (c *fakeResidentClock) fireAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ch := range c.timers {
		select {
		case ch <- c.Now():
		default:
		}
	}
}

const residentT = int64(1_800_000_000_000)

func residentTestConfig(clock *fakeResidentClock, mut func(*ResidencyConfig)) ResidencyConfig {
	cfg := DefaultResidencyConfig()
	cfg.MaxRuns = 4
	cfg.clock = clock
	if mut != nil {
		mut(&cfg)
	}
	return cfg
}

func newTestResidentSet(t *testing.T, cfg ResidencyConfig) *ResidentSet {
	t.Helper()
	s, err := NewResidentSet(cfg)
	if err != nil {
		t.Fatalf("NewResidentSet: %v", err)
	}
	return s
}

func timerReq(deadlineMs int64) parkRequest {
	return parkRequest{Kind: wakeTimer, DeadlineMs: deadlineMs, Reason: "cleat_sleep", Until: time.UnixMilli(deadlineMs)}
}

// residentCounter sums a counter's series whose label text contains every given fragment; it is 0 when the counter was never emitted.
func residentCounter(t *testing.T, m *prometheus.Metrics, name string, fragments ...string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	m.ServeHTTP().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	total := 0
lines:
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, name+"{") && !strings.HasPrefix(line, name+" ") {
			continue
		}
		for _, f := range fragments {
			if !strings.Contains(line, f) {
				continue lines
			}
		}
		f := strings.Fields(line)
		n, err := strconv.ParseFloat(f[len(f)-1], 64)
		if err != nil {
			t.Fatalf("cannot read the value of %q: %v", line, err)
		}
		total += int(n)
	}
	return total
}

// park runs one wait on its own goroutine and returns what it was told once it ends.
func park(run *ResidentRun, ctx context.Context, req parkRequest) <-chan parkOutcome {
	out := make(chan parkOutcome, 1)
	go func() { out <- run.Wait(ctx, req) }()
	return out
}

// cancelledCtx is for a wait that MUST be refused: if a mutant admitted it anyway, an already-ended context sends it straight back, so the test fails on
// its assertion (a hold counted where a refusal was expected) instead of hanging on a fake clock that nobody fires.
func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func awaitArmed(t *testing.T, c *fakeResidentClock) {
	t.Helper()
	select {
	case <-c.armed:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never parked: no timer was started")
	}
}

func mustOutcome(t *testing.T, ch <-chan parkOutcome, want parkOutcome, what string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Errorf("%s: the wait ended with %d, want %d", what, got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: the wait never ended", what)
	}
}

func TestAWaitDueInsideTheWindowIsHeldAndEndsAtItsDeadline(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	set := newTestResidentSet(t, residentTestConfig(clock, nil))
	run := set.ForRun(ResidentRunInfo{WorkflowID: "wf", Language: "go"})

	out := park(run, context.Background(), timerReq(residentT+5000))
	awaitArmed(t, clock)
	if n := set.Len(); n != 1 {
		t.Fatalf("a parked run is not counted: Len = %d, want 1", n)
	}
	clock.fireAll()
	mustOutcome(t, out, parkDeadline, "a timer that fired")
	if n := set.Len(); n != 0 {
		t.Errorf("a run that left was not released: Len = %d, want 0", n)
	}
}

func TestAWaitThatIsNotHeldSaysWhyAndSuspendsAsItAlwaysDid(t *testing.T) {
	const memory = 24 << 20
	rows := []struct {
		name   string
		mut    func(*ResidencyConfig)
		info   ResidentRunInfo
		req    parkRequest
		reason string
	}{
		{"only a timer has a caller on the park helper", nil, ResidentRunInfo{Language: "go"}, parkRequest{Kind: wakeSignal, Key: "s", DeadlineMs: residentT + 5000}, "kind"},
		{"a language with no measured cost", nil, ResidentRunInfo{Language: "python"}, timerReq(residentT + 5000), "language"},
		{"a language the metadata spells another way", nil, ResidentRunInfo{Language: "tinygo"}, timerReq(residentT + 5000), "language"},
		{"no language at all", nil, ResidentRunInfo{}, timerReq(residentT + 5000), "language"},
		{"due later than the window", nil, ResidentRunInfo{Language: "go"}, timerReq(residentT + 10_001), "window"},
		{"a worker under memory pressure", func(c *ResidencyConfig) { c.Pressure = func() float64 { return 0.01 } }, ResidentRunInfo{Language: "go"}, timerReq(residentT + 5000), "pressure"},
		{"a memory budget smaller than one instance", func(c *ResidencyConfig) { c.MemoryBudgetBytes = memory - 1 }, ResidentRunInfo{Language: "go"}, timerReq(residentT + 5000), "memory"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			clock := newFakeResidentClock(residentT)
			m := newReplayMetrics(t)
			set := newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) {
				c.Metrics = m
				if r.mut != nil {
					r.mut(c)
				}
			}))
			got := set.ForRun(r.info).Wait(cancelledCtx(), r.req)
			if got != parkRefused {
				t.Fatalf("returned %d, want the refusal %d", got, parkRefused)
			}
			if set.Len() != 0 {
				t.Errorf("a refused wait was counted as held")
			}
			if n := residentCounter(t, m, "cleat_resident_refusals_total", `reason="`+r.reason+`"`); n != 1 {
				t.Errorf("the refusal was counted %d time(s) under reason %q, want 1", n, r.reason)
			}
			if n := residentCounter(t, m, "cleat_resident_parks_total"); n != 0 {
				t.Errorf("a wait that had to be refused was HELD (%d hold(s) counted): it was admitted when it should not have been", n)
			}
		})
	}

	// The control for every row above: the same session and request, with only the thing in question removed, IS held. Without it each row is also
	// true of a source that refuses everything.
	t.Run("CONTROL: nothing wrong is held", func(t *testing.T) {
		clock := newFakeResidentClock(residentT)
		set := newTestResidentSet(t, residentTestConfig(clock, nil))
		out := park(set.ForRun(ResidentRunInfo{Language: "go"}), context.Background(), timerReq(residentT+5000))
		awaitArmed(t, clock)
		clock.fireAll()
		mustOutcome(t, out, parkDeadline, "the control")
	})
}

func TestTheWindowIsInclusiveAtItsEdgeAndAWaitAlreadyDueIsNotHeld(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	set := newTestResidentSet(t, residentTestConfig(clock, nil)) // the floor, 10 s, is the window with the default estimate
	run := set.ForRun(ResidentRunInfo{Language: "go"})

	out := park(run, context.Background(), timerReq(residentT+10_000))
	awaitArmed(t, clock)
	clock.fireAll()
	mustOutcome(t, out, parkDeadline, "a wait due exactly at the window")

	// Already due: the clock has nothing left to wait for, so nothing is parked and the answer is the deadline's.
	if got := run.Wait(context.Background(), timerReq(residentT)); got != parkDeadline {
		t.Errorf("a wait already due returned %d, want the deadline %d", got, parkDeadline)
	}
	if len(clock.armed) != 0 {
		t.Errorf("a wait already due started a timer")
	}
}

func TestTheResidentCountLimitRefusesTheNextRunAndFreesASlotWhenOneLeaves(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	m := newReplayMetrics(t)
	set := newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) { c.MaxRuns = 2; c.Metrics = m }))
	run := func() *ResidentRun { return set.ForRun(ResidentRunInfo{Language: "go"}) }

	a := park(run(), context.Background(), timerReq(residentT+5000))
	awaitArmed(t, clock)
	b := park(run(), context.Background(), timerReq(residentT+5000))
	awaitArmed(t, clock)
	if got := run().Wait(cancelledCtx(), timerReq(residentT+5000)); got != parkRefused {
		t.Fatalf("a third run over a limit of two returned %d, want the refusal", got)
	}
	if n := residentCounter(t, m, "cleat_resident_refusals_total", `reason="count"`); n != 1 {
		t.Errorf("the count refusal was counted %d time(s), want 1", n)
	}
	clock.fireAll()
	mustOutcome(t, a, parkDeadline, "the first run")
	mustOutcome(t, b, parkDeadline, "the second run")

	again := park(run(), context.Background(), timerReq(residentT+5000))
	awaitArmed(t, clock)
	clock.fireAll()
	mustOutcome(t, again, parkDeadline, "a run after both left")
	if g := residentCounter(t, m, "cleat_resident_runs"); g != 0 {
		t.Errorf("the held-runs gauge reads %d after every run left, want 0", g)
	}
}

func TestDrainSendsEveryHeldRunBackAndRefusesAnyThatFollows(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	m := newReplayMetrics(t)
	set := newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) { c.Metrics = m }))
	run := func() *ResidentRun { return set.ForRun(ResidentRunInfo{Language: "go"}) }

	a := park(run(), context.Background(), timerReq(residentT+9000))
	awaitArmed(t, clock)
	b := park(run(), context.Background(), timerReq(residentT+9000))
	awaitArmed(t, clock)
	set.Drain()
	mustOutcome(t, a, parkRefused, "the first held run")
	mustOutcome(t, b, parkRefused, "the second held run")
	if set.Len() != 0 {
		t.Errorf("Len = %d after a drain, want 0", set.Len())
	}
	if n := residentCounter(t, m, "cleat_resident_evictions_total", `reason="shutdown"`); n != 2 {
		t.Errorf("shutdown evictions counted %d, want 2", n)
	}
	if got := run().Wait(cancelledCtx(), timerReq(residentT+5000)); got != parkRefused {
		t.Errorf("a park after a drain returned %d, want the refusal", got)
	}
	if n := residentCounter(t, m, "cleat_resident_refusals_total", `reason="draining"`); n != 1 {
		t.Errorf("the draining refusal was counted %d time(s), want 1", n)
	}
}

func TestEvictAllLeavesTheSetOpenWhereDrainDoesNot(t *testing.T) {
	// The control for the drain row: sending the runs back for memory pressure is not a shutdown, so the next park is admitted.
	clock := newFakeResidentClock(residentT)
	set := newTestResidentSet(t, residentTestConfig(clock, nil))
	a := park(set.ForRun(ResidentRunInfo{Language: "go"}), context.Background(), timerReq(residentT+9000))
	awaitArmed(t, clock)
	set.EvictAll(ResidentEvictedPressure)
	mustOutcome(t, a, parkRefused, "a run evicted for pressure")

	b := park(set.ForRun(ResidentRunInfo{Language: "go"}), context.Background(), timerReq(residentT+9000))
	awaitArmed(t, clock)
	clock.fireAll()
	mustOutcome(t, b, parkDeadline, "a run parked after an eviction")
}

func TestAHeldRunEndsWhenItsContextEndsAndIsReleased(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	m := newReplayMetrics(t)
	set := newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) { c.Metrics = m }))
	ctx, cancel := context.WithCancel(context.Background())
	out := park(set.ForRun(ResidentRunInfo{Language: "go"}), ctx, timerReq(residentT+9000))
	awaitArmed(t, clock)
	cancel()
	mustOutcome(t, out, parkRefused, "a cancelled context")
	if set.Len() != 0 {
		t.Errorf("Len = %d after the context ended, want 0", set.Len())
	}
	if n := residentCounter(t, m, "cleat_resident_parks_total", `outcome="cancelled"`); n != 1 {
		t.Errorf("the cancelled hold was counted %d time(s), want 1", n)
	}
}

func TestTheWindowIsTheEstimatedReplayTimesTheMultiplierClampedToTheFloorAndCap(t *testing.T) {
	cfg := DefaultResidencyConfig()
	cfg.ReplayFixed, cfg.ReplayPerEvent, cfg.ReplayMultiplier = time.Second, 10*time.Millisecond, 10
	rows := []struct {
		events int
		want   time.Duration
	}{
		{0, 10 * time.Second},      // 10 x 1 s = 10 s, which is the floor
		{100, 20 * time.Second},    // 10 x (1 s + 1 s)
		{1_000, 110 * time.Second}, // 10 x (1 s + 10 s)
		{100_000, 5 * time.Minute}, // capped
		{-5, 10 * time.Second},     // a negative count is no events
	}
	for _, r := range rows {
		if got := cfg.window(r.events); got != r.want {
			t.Errorf("window(%d events) = %s, want %s", r.events, got, r.want)
		}
	}
	// With the defaults the per-event cost is zero, which is unmeasured, so every run gets the floor: the multiplier does nothing until it is measured.
	def := DefaultResidencyConfig()
	if a, b := def.window(0), def.window(1_000_000); a != b || a != def.WindowFloor {
		t.Errorf("with the default (zero) per-event cost the window should be the floor %s for every run, got %s and %s", def.WindowFloor, a, b)
	}
}

func TestAPolicyThatCannotMeanWhatItSaysIsRefusedAtBoot(t *testing.T) {
	bad := map[string]func(*ResidencyConfig){
		"a negative run limit":      func(c *ResidencyConfig) { c.MaxRuns = -1 },
		"a cap below the floor":     func(c *ResidencyConfig) { c.WindowCap = time.Second },
		"a negative floor":          func(c *ResidencyConfig) { c.WindowFloor = -1 },
		"a zero multiplier":         func(c *ResidencyConfig) { c.ReplayMultiplier = 0 },
		"a negative replay cost":    func(c *ResidencyConfig) { c.ReplayFixed = -1 },
		"a negative per-event cost": func(c *ResidencyConfig) { c.ReplayPerEvent = -1 },
	}
	for name, mut := range bad {
		cfg := DefaultResidencyConfig()
		mut(&cfg)
		if _, err := NewResidentSet(cfg); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := NewResidentSet(DefaultResidencyConfig()); err != nil {
		t.Errorf("the default policy was refused: %v", err)
	}
}

// claimStillHeld is the claim check of a run the store still has assigned to this worker, for the tests that are about something else.
func claimStillHeld(context.Context) error { return nil }

func TestResidencyIsOffWhenTheRunLimitIsZeroAndAnOffSetHandsOutNoWakeSource(t *testing.T) {
	set := newTestResidentSet(t, DefaultResidencyConfig()) // MaxRuns is zero
	if set.Enabled() {
		t.Fatal("a zero run limit left residency enabled")
	}
	run := set.ForRun(ResidentRunInfo{Language: "go"})
	if run != nil {
		t.Fatalf("an off set handed out %#v, want nil", run)
	}
	// nil is what the option must treat as "nothing": the engine keeps no wake source, so every wait suspends.
	e := NewEngine(nil, nil, WithResidency(run))
	if e.wakeSource != nil {
		t.Errorf("WithResidency(nil) installed a wake source %#v", e.wakeSource)
	}
	var nilSet *ResidentSet
	if nilSet.Enabled() || nilSet.ForRun(ResidentRunInfo{}) != nil {
		t.Error("a nil set is not off")
	}
	nilSet.Drain() // must not panic: a worker without residency drains too
	nilSet.EvictAll(ResidentEvictedPressure)
}

// The first caller end to end: a real DurableSleep, a real engine given the run's handle, the set's clock and the engine's clock the same one. The
// sleep is HELD (no suspension is set, the call does not return), and it completes only when the timer has fired AND the clock agrees.
func TestADurableSleepIsHeldByTheResidentSetAndCompletedWhenItsTimerFires(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	set := newTestResidentSet(t, residentTestConfig(clock, nil))
	s := stepSession([]EngineOption{WithClock(func() int64 { return clock.nowMs.Load() }), WithResidency(set.ForRun(ResidentRunInfo{Language: "go", ClaimCheck: claimStillHeld}))}, nil)
	s.nowMs = residentT

	done := make(chan int64, 1)
	go func() { done <- s.DurableSleep(context.Background(), nil, 5000) }()
	awaitArmed(t, clock)
	select {
	case got := <-done:
		t.Fatalf("a sleep that was held returned %#x before its timer fired", got)
	default:
	}
	if s.suspendErr != nil {
		t.Fatalf("a held sleep has already set a suspension: %+v", s.suspendErr)
	}
	clock.nowMs.Store(residentT + 5000)
	clock.fireAll()
	select {
	case got := <-done:
		if got != packSleepResult(sleepStatusCompleted, 0) {
			t.Errorf("a held sleep whose timer fired returned %#x, want completed", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the held sleep never returned")
	}
	if s.suspendErr != nil {
		t.Errorf("a completed held sleep set a suspension: %+v", s.suspendErr)
	}
}

func TestADurableSleepWhoseRunIsSentBackSuspendsAsItAlwaysDid(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	set := newTestResidentSet(t, residentTestConfig(clock, nil))
	s := stepSession([]EngineOption{WithClock(func() int64 { return clock.nowMs.Load() }), WithResidency(set.ForRun(ResidentRunInfo{Language: "go", ClaimCheck: claimStillHeld}))}, nil)
	s.nowMs = residentT

	done := make(chan int64, 1)
	go func() { done <- s.DurableSleep(context.Background(), nil, 5000) }()
	awaitArmed(t, clock)
	set.Drain()
	select {
	case got := <-done:
		if got != packSleepResult(sleepStatusSuspend, 5000) {
			t.Errorf("a sleep sent back returned %#x, want the suspend result", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a sleep sent back never returned")
	}
	if s.suspendErr == nil || s.suspendErr.Reason != "cleat_sleep(5000ms)" {
		t.Errorf("a sleep sent back set %+v, want the suspension it sets when nothing parks", s.suspendErr)
	}
}

// A run's own context carries the wall-clock ceiling and the workflow deadline (executor.go), and a hold that outlives it is worse than the suspension
// it replaced: the sleep still suspends, but the executor then sees execCtx.Err() == DeadlineExceeded and FAILS the run with a timeout, where the
// same sleep without residency would have suspended at once. So a wait whose context ends at or before its wake is refused, not held (cleat#4162, PR 2).
func TestAWaitWhoseContextEndsBeforeItsWakeIsRefusedNotHeld(t *testing.T) {
	// The wake is on the fake clock's epoch (year 2027), so any real deadline in the next seconds is before it, which is the shape of a run close to
	// its ceiling. The fake clock never fires, so a wrongly admitted wait ends only when the context does: it fails on the metrics below rather than hanging.
	clock := newFakeResidentClock(residentT)
	m := newReplayMetrics(t)
	set := newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) { c.Metrics = m }))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if got := set.ForRun(ResidentRunInfo{Language: "go"}).Wait(ctx, timerReq(residentT+5000)); got != parkRefused {
		t.Fatalf("returned %d, want the refusal %d", got, parkRefused)
	}
	if n := residentCounter(t, m, "cleat_resident_parks_total"); n != 0 {
		t.Errorf("a wait whose context ends before its wake was HELD (%d hold(s) counted): at the ceiling it would fail the run with a timeout instead of suspending it", n)
	}
	if n := residentCounter(t, m, "cleat_resident_refusals_total", `reason="deadline"`); n != 1 {
		t.Errorf("the refusal was counted %d time(s) under reason \"deadline\", want 1", n)
	}
}

// The boundary: a context that ends at EXACTLY the wake is refused too. Both would happen at the same instant, and the executor treats an expired context
// as a timeout whichever got there first, so only a deadline strictly after the wake leaves room to complete.
func TestAWaitWhoseContextEndsAtExactlyItsWakeIsRefusedNotHeld(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	m := newReplayMetrics(t)
	set := newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) { c.Metrics = m }))
	ctx, cancel := context.WithDeadline(context.Background(), time.UnixMilli(residentT+5000))
	defer cancel()
	// Bounded: a wrongly admitted hold blocks on a clock nobody fires and a context that ends in 2027, so it must fail here and not hang.
	out := park(set.ForRun(ResidentRunInfo{Language: "go"}), ctx, timerReq(residentT+5000))
	select {
	case got := <-out:
		if got != parkRefused {
			t.Fatalf("returned %d, want the refusal %d", got, parkRefused)
		}
	case <-time.After(2 * time.Second):
		clock.fireAll() // release the wrongly held wait so the test can end
		t.Fatal("a context ending at exactly the wake was HELD, not refused")
	}
	if n := residentCounter(t, m, "cleat_resident_refusals_total", `reason="deadline"`); n != 1 {
		t.Errorf("a context ending at exactly the wake was counted %d time(s) as a deadline refusal, want 1", n)
	}
}

// The control for the row above: a context whose deadline is AFTER the wake does not stop the hold, or the refusal could be for any context with a deadline.
func TestAWaitWhoseContextOutlivesItsWakeIsStillHeld(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	set := newTestResidentSet(t, residentTestConfig(clock, nil))
	ctx, cancel := context.WithDeadline(context.Background(), time.UnixMilli(residentT+60_000))
	defer cancel()
	out := park(set.ForRun(ResidentRunInfo{Language: "go"}), ctx, timerReq(residentT+5000))
	awaitArmed(t, clock)
	if set.Len() != 1 {
		t.Fatalf("a wait whose context outlives its wake was not held")
	}
	clock.fireAll()
	mustOutcome(t, out, parkDeadline, "the timer")
}

// The wasmtime paths call the handler with context.Background() (wasmtime_hostfuncs_core.go, component_callbacks.go), so these tests call it the same way
// and put the limits only where production puts them: on the session's execution context. A test that handed the limits to the host call itself would pass
// whether or not the session's context reaches the wait, which is the thing that is wrong if it does not.

func TestAHeldSleepEndsWhenItsExecutionIsCancelled(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	m := newReplayMetrics(t)
	set := newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) { c.Metrics = m }))
	s := stepSession([]EngineOption{WithClock(func() int64 { return clock.nowMs.Load() }), WithResidency(set.ForRun(ResidentRunInfo{Language: "go", ClaimCheck: claimStillHeld}))}, nil)
	s.nowMs = residentT
	ctx, cancel := context.WithCancel(context.Background())
	s.execCtx = ctx

	done := make(chan int64, 1)
	go func() { done <- s.DurableSleep(context.Background(), nil, 5000) }()
	awaitArmed(t, clock)
	if set.Len() != 1 {
		t.Fatalf("PRECONDITION FAILED: the sleep was not held (%d held)", set.Len())
	}
	cancel() // a terminate or a cancel: the execution's context ends while the run is held
	select {
	case got := <-done:
		if got != packSleepResult(sleepStatusSuspend, 5000) {
			t.Errorf("a held sleep whose execution was cancelled returned %#x, want the suspend result", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a cancelled execution's held sleep never returned: the hold does not see the session's context, so a terminate would wait for the timer")
	}
	if set.Len() != 0 {
		t.Errorf("%d run(s) still held after the execution ended", set.Len())
	}
}

func TestASleepTooCloseToTheExecutionsDeadlineIsNotHeld(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	m := newReplayMetrics(t)
	set := newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) { c.Metrics = m }))
	s := stepSession([]EngineOption{WithClock(func() int64 { return clock.nowMs.Load() }), WithResidency(set.ForRun(ResidentRunInfo{Language: "go", ClaimCheck: claimStillHeld}))}, nil)
	s.nowMs = residentT
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	s.execCtx = ctx

	done := make(chan int64, 1)
	go func() { done <- s.DurableSleep(context.Background(), nil, 5000) }()
	select {
	case got := <-done:
		if got != packSleepResult(sleepStatusSuspend, 5000) {
			t.Errorf("returned %#x, want the suspend result", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the sleep never returned")
	}
	if n := residentCounter(t, m, "cleat_resident_parks_total"); n != 0 {
		t.Errorf("a sleep whose execution deadline was 30ms away was HELD (%d): at the ceiling the executor would fail the run with a timeout", n)
	}
	if n := residentCounter(t, m, "cleat_resident_refusals_total", `reason="deadline"`); n != 1 {
		t.Errorf("the refusal was counted %d time(s) under \"deadline\", want 1", n)
	}
}

// Every place the executor builds the execution context hands it to the session, after the ceiling: a path that forgot would hold runs through limits
// that only the session's context can show them. Counted against the places the ceiling is applied, so a third site is also caught.
func TestEveryExecutionContextTheExecutorBuildsIsHandedToTheSession(t *testing.T) {
	raw, err := os.ReadFile("executor.go")
	if err != nil {
		t.Fatalf("read executor.go: %v", err)
	}
	src := string(raw)
	ceilings := strings.Count(src, "execCtx, cancel = withRestartableTimeout(execCtx, ceiling)")
	handed := strings.Count(src, "session.execCtx = execCtx")
	if ceilings < 2 {
		t.Fatalf("found %d place(s) the ceiling is applied, want at least 2: this test reads the wrong thing", ceilings)
	}
	if handed != ceilings {
		t.Errorf("the ceiling is applied at %d place(s) and the context is handed to the session at %d", ceilings, handed)
	}
	// And after, not before: a hand-off before the ceiling carries a context without it.
	for _, seg := range strings.Split(src, "execCtx, cancel = withRestartableTimeout(execCtx, ceiling)")[1:] {
		next := strings.Index(seg, "session.execCtx = execCtx")
		if next < 0 || next > 400 {
			t.Errorf("a ceiling is not followed by the hand-off within the same block")
		}
	}
}

// The margin: a wait due within DeadlineMargin of the run's deadline is refused, and the boundary is exact. Only a deadline strictly later than wake + margin
// leaves the run room to come back as a suspension before the limit, so wake + margin is refused and one millisecond more is held. The margin is read from the
// config the test builds, so a mutant that ignores it is caught on either side of the line.
func TestAWaitDueWithinTheMarginOfItsDeadlineIsRefusedAndOneJustOutsideIsHeld(t *testing.T) {
	const margin = 100 * time.Millisecond
	for _, c := range []struct {
		name       string
		deadlineMs int64 // relative to the wake
		held       bool
	}{
		{"deadline = wake + margin - 1ms", 99, false},
		{"deadline = wake + margin", 100, false},
		{"deadline = wake + margin + 1ms", 101, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			clock := newFakeResidentClock(residentT)
			m := newReplayMetrics(t)
			set := newTestResidentSet(t, residentTestConfig(clock, func(cfg *ResidencyConfig) { cfg.Metrics = m; cfg.DeadlineMargin = margin }))
			wake := residentT + 5000
			ctx, cancel := context.WithDeadline(context.Background(), time.UnixMilli(wake+c.deadlineMs))
			defer cancel()
			out := park(set.ForRun(ResidentRunInfo{Language: "go"}), ctx, timerReq(wake))
			if c.held {
				awaitArmed(t, clock)
				if set.Len() != 1 {
					t.Fatalf("a wait with %dms to spare past its wake was not held", c.deadlineMs)
				}
				clock.fireAll()
				mustOutcome(t, out, parkDeadline, "the timer")
				return
			}
			select {
			case got := <-out:
				if got != parkRefused {
					t.Fatalf("returned %d, want the refusal %d", got, parkRefused)
				}
			case <-time.After(2 * time.Second):
				clock.fireAll()
				t.Fatalf("a wait whose deadline is %dms after its wake (margin %s) was HELD", c.deadlineMs, margin)
			}
			if n := residentCounter(t, m, "cleat_resident_refusals_total", `reason="deadline"`); n != 1 {
				t.Errorf("counted %d deadline refusal(s), want 1", n)
			}
		})
	}
}

func TestANegativeDeadlineMarginIsRefused(t *testing.T) {
	cfg := DefaultResidencyConfig()
	cfg.DeadlineMargin = -time.Millisecond
	if err := cfg.Validate(); err == nil {
		t.Error("a negative margin was accepted")
	}
	if got := DefaultResidencyConfig().DeadlineMargin; got != 100*time.Millisecond {
		t.Errorf("the default margin is %s, want 100ms (about ten times the worst measured unwind)", got)
	}
}

// A loop of sleeps in ONE execution: each hold is admitted against the same absolute deadline, so the holds add up and the one that would not leave room is
// refused (and suspends). That is what makes a loop of sleeps safe under a ceiling, and it is the reason a lower window cap does not remove the known limit:
// it shortens each hold, and the holds still add up.
func TestHoldsInOneExecutionAddUpAgainstOneDeadlineAndTheLastIsRefused(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	m := newReplayMetrics(t)
	set := newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) { c.Metrics = m; c.DeadlineMargin = 100 * time.Millisecond }))
	ctx, cancel := context.WithDeadline(context.Background(), time.UnixMilli(residentT+10_000)) // the execution's one ceiling
	defer cancel()
	run := set.ForRun(ResidentRunInfo{Language: "go"})
	for i, wake := range []int64{3000, 6000, 9000} { // three sleeps, each well inside the ceiling
		out := park(run, ctx, timerReq(residentT+wake))
		awaitArmed(t, clock)
		clock.nowMs.Store(residentT + wake)
		clock.fireAll()
		mustOutcome(t, out, parkDeadline, "sleep "+strconv.Itoa(i+1))
	}
	// The fourth is due 400ms before the ceiling but inside what is left of it after three holds: still admitted, because the margin is 100ms.
	out := park(run, ctx, timerReq(residentT+9_850))
	awaitArmed(t, clock)
	clock.fireAll()
	mustOutcome(t, out, parkDeadline, "a sleep 150ms inside the ceiling")
	// And one due inside the margin is refused: the execution is out of room, so the loop suspends instead of timing out.
	select {
	case got := <-park(run, ctx, timerReq(residentT+9_950)):
		if got != parkRefused {
			t.Fatalf("a wait due 50ms before the ceiling returned %d, want the refusal", got)
		}
	case <-time.After(2 * time.Second):
		clock.fireAll()
		t.Fatal("a wait due 50ms before the ceiling was held")
	}
	if n := residentCounter(t, m, "cleat_resident_refusals_total", `reason="deadline"`); n != 1 {
		t.Errorf("counted %d deadline refusal(s), want exactly the last one", n)
	}
}

// The wake source against the REAL restartable limits (execution_limits.go): the paths the backend rows cannot reach one at a time.

func limitedSetCtx(t *testing.T, d time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()
	base := withExecutionLimits(context.Background(), &executionLimits{})
	ctx, cancel := withRestartableTimeout(base, d)
	if _, ok := ctx.(*restartableDeadline); !ok {
		t.Fatal("PRECONDITION FAILED: the limit is not restartable")
	}
	return ctx, cancel
}

func TestAHoldPausesTheRunsLimitsAndRestartsThemWhenItEnds(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	set := newTestResidentSet(t, residentTestConfig(clock, nil))
	ctx, cancel := limitedSetCtx(t, 150*time.Millisecond)
	defer cancel()
	out := park(set.ForRun(ResidentRunInfo{Language: "go"}), ctx, timerReq(residentT+5000))
	awaitArmed(t, clock)
	// The hold lasts three times the limit's whole duration, in real time. A limit that kept running would have ended it.
	time.Sleep(450 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatalf("the limit expired during the hold: %v", ctx.Err())
	}
	clock.fireAll()
	mustOutcome(t, out, parkDeadline, "the timer")
	// Restarted: about the FULL 150ms is left, not what was left when the hold began (which was about 150ms too, so the control is the next row).
	if dl, ok := ctx.Deadline(); !ok || time.Until(dl) < 100*time.Millisecond {
		t.Errorf("after the hold the limit has %v left, want about its full 150ms", time.Until(dl))
	}
}

func TestAHoldThatWasUsedPartOfALimitGivesItBackInFullAtTheWake(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	set := newTestResidentSet(t, residentTestConfig(clock, nil))
	ctx, cancel := limitedSetCtx(t, 400*time.Millisecond)
	defer cancel()
	time.Sleep(300 * time.Millisecond) // only 100ms of the limit is left when the wait starts
	out := park(set.ForRun(ResidentRunInfo{Language: "go"}), ctx, timerReq(residentT+5000))
	awaitArmed(t, clock)
	clock.fireAll()
	mustOutcome(t, out, parkDeadline, "the timer")
	if dl, ok := ctx.Deadline(); !ok || time.Until(dl) < 300*time.Millisecond {
		t.Errorf("after the wake the limit has %v left; a suspension's next segment would have its full 400ms, and so must this (it had 100ms when paused)", time.Until(dl))
	}
}

func TestAWaitRefusedAfterThePauseLeavesTheLimitRunningWithItsRemainingTime(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	m := newReplayMetrics(t)
	// MaxRuns 1 and one run already held: the second run's wait is refused for the COUNT, which is decided after its limits were paused.
	set := newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) { c.MaxRuns = 1; c.Metrics = m }))
	first, cancelFirst := limitedSetCtx(t, time.Hour)
	defer cancelFirst()
	held := park(set.ForRun(ResidentRunInfo{Language: "go"}), first, timerReq(residentT+5000))
	awaitArmed(t, clock)

	ctx, cancel := limitedSetCtx(t, 250*time.Millisecond)
	defer cancel()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if got := set.ForRun(ResidentRunInfo{Language: "go"}).Wait(ctx, timerReq(residentT+5000)); got != parkRefused {
		t.Fatalf("the second run's wait returned %d, want the refusal", got)
	}
	// It was never held, so its limit must carry on with the ~150ms it had, NOT be handed a fresh 250ms (which is what a restart would do).
	if !limitDoneWithin(ctx, 400*time.Millisecond) {
		t.Fatal("the refused run's limit never expired: the pause was not undone")
	}
	if got := time.Since(start); got > 220*time.Millisecond {
		t.Errorf("the refused run's limit lasted %s more after the refusal, want about 150ms: a refusal must not restart the limits", got)
	}
	clock.fireAll()
	mustOutcome(t, held, parkDeadline, "the first run's timer")
}

func limitDoneWithin(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(d):
		return false
	}
}

func TestAWaitWhoseLimitHasAlreadyEndedIsRefusedNotHeld(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	m := newReplayMetrics(t)
	set := newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) { c.Metrics = m }))
	ctx, cancel := limitedSetCtx(t, 5*time.Millisecond)
	defer cancel()
	if !limitDoneWithin(ctx, 2*time.Second) {
		t.Fatal("PRECONDITION FAILED: the limit did not expire")
	}
	if got := set.ForRun(ResidentRunInfo{Language: "go"}).Wait(ctx, timerReq(residentT+5000)); got != parkRefused {
		t.Fatalf("returned %d, want the refusal", got)
	}
	if n := residentCounter(t, m, "cleat_resident_parks_total"); n != 0 {
		t.Errorf("a run whose limit had already ended was HELD (%d)", n)
	}
	if n := residentCounter(t, m, "cleat_resident_refusals_total", `reason="deadline"`); n != 1 {
		t.Errorf("counted %d deadline refusal(s), want 1", n)
	}
}

func TestACancelEndsAHeldRunWhoseLimitsArePaused(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	set := newTestResidentSet(t, residentTestConfig(clock, nil))
	parent, cancelParent := context.WithCancel(context.Background())
	base := withExecutionLimits(parent, &executionLimits{})
	ctx, cancel := withRestartableTimeout(base, time.Hour)
	defer cancel()
	out := park(set.ForRun(ResidentRunInfo{Language: "go"}), ctx, timerReq(residentT+5000))
	awaitArmed(t, clock)
	cancelParent()
	select {
	case got := <-out:
		if got != parkRefused {
			t.Errorf("a held run that was cancelled returned %d, want the refusal (it suspends)", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancel did not end a held run whose limits were paused")
	}
}
