package engine

import (
	"context"
	"net/http/httptest"
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
	s := stepSession([]EngineOption{WithClock(func() int64 { return clock.nowMs.Load() }), WithResidency(set.ForRun(ResidentRunInfo{Language: "go"}))}, nil)
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
	s := stepSession([]EngineOption{WithClock(func() int64 { return clock.nowMs.Load() }), WithResidency(set.ForRun(ResidentRunInfo{Language: "go"}))}, nil)
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
