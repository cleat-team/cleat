package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cleat-team/cleat/monitoring/prometheus"
)

// Residency: a wait that cannot be answered yet may PARK, holding the run on its worker until the wait is due, instead of suspending it and replaying
// it on a later claim (cleat#4162; the design and the measurements behind every default here are docs/design/resident-suspended-runs.md). The choice
// is parkOrSuspend's (park.go); this file is the wake source it asks.
//
// EVERYTHING HERE IS A REFUSAL BY DEFAULT. A refusal is always safe: the wait suspends exactly as it does today, so the worst a wrong number in this file
// can do is park less than it could. What it must never do is park something it cannot get back, which is why the set is closed by a drain, why a park
// ends on its context, and why a language with no measured cost is not admitted at all.
//
// WHAT IS AND IS NOT MEASURED, because the defaults below are only as good as these:
//   - the memory of a parked instance is measured (design section 9a) for MINIMAL guests of Go, Python and C#, not for real workflows. Only Go is admitted,
//     and its cost here is the larger of the two measured figures (a guest that had made garbage, 19.6 MiB) rounded UP, not the 2.5 MiB of a minimal one.
//   - the replay cost model (a fixed start-up of 0.44 to 0.67 s per claim) is measured; the cost PER EVENT is not, so it defaults to zero and the
//     multiplier has no effect until it is. With the defaults the window is the floor.
//   - the 10x multiplier, the 10 s floor and the 5 min cap are the owner's starting point, not measurements. Tuning them on real workflows is cleat#4187.

// defaultResidentInstanceBytes is what admitting one parked instance of a language is charged against the resident-memory budget. A language absent
// from this table is REFUSED: there is no figure for it, and "unknown, so probably small" is the wrong way to fail.
var defaultResidentInstanceBytes = map[string]uint64{
	// design section 9a: 2.5 MiB for a minimal Go guest and 19.6 MiB after the guest had allocated and dropped about 256 MiB. Rounded up from the second.
	"go": 24 << 20,
}

// Why a wait was not parked, and why a parked run was sent back. They are metric label values and are read by tests, so they are constants.
const (
	residentRefusedKind     = "kind"      // only timers have a caller on the park helper so far
	residentRefusedLanguage = "language"  // no measured cost for this language
	residentRefusedWindow   = "window"    // due later than the window allows: parking it would hold the run for nothing
	residentRefusedCount    = "count"     // the resident-run limit is reached
	residentRefusedMemory   = "memory"    // admitting it would exceed the resident-memory budget
	residentRefusedPressure = "pressure"  // the worker is at or over its memory soft limit
	residentRefusedDraining = "draining"  // the worker is shutting down
	residentRefusedDeadline = "deadline"  // the run's own context (the wall-clock ceiling, the workflow deadline) ends at or before the wake
	residentEndedDeadline   = "deadline"  // the timer fired: the caller's clock decides whether the wait is over
	residentEndedCancelled  = "cancelled" // the run's context ended while parked
	residentEndedEvicted    = "evicted"   // the set sent the run back (see the eviction reasons)
)

// The reasons a parked run is sent back, exported because the worker names them when it evicts.
const (
	ResidentEvictedShutdown = "shutdown" // the worker began draining
	ResidentEvictedPressure = "pressure" // the worker reached its memory hard limit
)

// ResidencyConfig is the whole policy. The zero value is OFF: MaxRuns is zero, so every wait suspends as it always has.
type ResidencyConfig struct {
	// MaxRuns is the most runs held at once. Zero turns residency off. It is deliberately small when it is on, because a parked instance holds an OS
	// thread (design 4.6: about one per instance) and the thread cost at larger counts was not measured.
	MaxRuns int
	// MemoryBudgetBytes bounds the sum of the per-language costs of the runs held. Zero means MaxRuns times the dearest admitted language, which is
	// the bound MaxRuns already implies.
	MemoryBudgetBytes uint64
	// WindowFloor and WindowCap bound how far ahead a wait may be due and still be parked: window = clamp(ReplayMultiplier x estimated replay, floor, cap).
	WindowFloor, WindowCap time.Duration
	// ReplayMultiplier is how many times the cost of the replay a park saves a run is willing to spend holding it.
	ReplayMultiplier float64
	// ReplayFixed and ReplayPerEvent estimate what replaying the run would cost: fixed start-up plus a cost per recorded event. The first is
	// measured (0.44 to 0.67 s); the second is not and defaults to zero.
	ReplayFixed, ReplayPerEvent time.Duration
	// DeadlineMargin is how much room a wait must leave before a deadline that CANNOT be paused: a wait due at or after (deadline - margin) is refused. The run's own
	// limits (the workflow deadline and the ceiling) CAN be paused and are, for the hold (execution_limits.go), so this applies only to a deadline on a context the
	// caller supplied. The margin is for the late timer and for the run to come back as a suspension before that deadline; it is not for the work after the wake,
	// which restarts the run's own limits. Measured on real wasmtime, a MINIMAL guest, 120 samples: a hold ended by a cancel was back as a suspension in a median of
	// 0.3 ms, p90 about 1 ms and at most 9 ms (6.8 ms under -race) on a loaded machine, so 100 ms is about ten times the worst sample. It is not measured for a guest
	// with real defers or a large history.
	DeadlineMargin time.Duration
	// InstanceBytes is the per-language charge; nil means defaultResidentInstanceBytes.
	InstanceBytes map[string]uint64
	// Pressure reports the worker's memory pressure from 0 (under the soft limit) up to 1 (at the hard limit). A park is refused while it is above
	// zero. nil means no second guard, which only tests use.
	Pressure func() float64
	// Metrics receives the residency counters; nil records nothing.
	Metrics *prometheus.Metrics

	// clock is replaced in tests, so that no assertion depends on wall-clock time.
	clock residentClock
}

// DefaultResidencyConfig is the policy with residency OFF and every other number at its conservative default.
func DefaultResidencyConfig() ResidencyConfig {
	return ResidencyConfig{
		WindowFloor:      10 * time.Second,
		WindowCap:        5 * time.Minute,
		ReplayMultiplier: 10,
		ReplayFixed:      500 * time.Millisecond,
		DeadlineMargin:   100 * time.Millisecond,
	}
}

// Validate refuses a policy that cannot mean what it says, at boot, where an operator reads it, and not as a silent refusal on every park.
func (c ResidencyConfig) Validate() error {
	switch {
	case c.MaxRuns < 0:
		return fmt.Errorf("resident run limit %d is negative", c.MaxRuns)
	case c.WindowFloor < 0 || c.WindowCap < 0:
		return fmt.Errorf("resident window floor %s and cap %s must not be negative", c.WindowFloor, c.WindowCap)
	case c.WindowCap < c.WindowFloor:
		return fmt.Errorf("resident window cap %s is below its floor %s", c.WindowCap, c.WindowFloor)
	case c.ReplayMultiplier <= 0:
		return fmt.Errorf("resident replay multiplier %g must be positive", c.ReplayMultiplier)
	case c.ReplayFixed < 0 || c.ReplayPerEvent < 0:
		return fmt.Errorf("resident replay cost estimates must not be negative")
	case c.DeadlineMargin < 0:
		return fmt.Errorf("resident deadline margin %s must not be negative", c.DeadlineMargin)
	}
	return nil
}

// window is how far ahead a wait of a run with eventCount recorded events may be due and still be parked.
func (c ResidencyConfig) window(eventCount int) time.Duration {
	if eventCount < 0 {
		eventCount = 0
	}
	est := c.ReplayFixed + time.Duration(eventCount)*c.ReplayPerEvent
	w := time.Duration(float64(est) * c.ReplayMultiplier)
	if w < c.WindowFloor {
		w = c.WindowFloor
	}
	if w > c.WindowCap {
		w = c.WindowCap
	}
	return w
}

// residentClock is the two things the set needs from time, so a test can fire a timer without waiting for one.
type residentClock interface {
	Now() time.Time
	// After returns a channel that receives when d has passed, and a function that releases the timer.
	After(d time.Duration) (<-chan time.Time, func())
}

type realResidentClock struct{}

func (realResidentClock) Now() time.Time { return time.Now() }
func (realResidentClock) After(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// ResidentSet is the worker's shared record of the runs it is holding. It decides admission and can send every run back; it never decides that a wait
// is OVER, which stays the caller's, by the clock (park.go).
type ResidentSet struct {
	cfg ResidencyConfig

	mu     sync.Mutex
	parked map[*residentSlot]struct{}
	bytes  uint64
	closed bool
}

type residentSlot struct {
	bytes uint64
	// evict is closed exactly once, by evictAll, to send the run back; reason says why.
	evict  chan struct{}
	reason string
}

// NewResidentSet builds the set. It validates the policy, so a bad one fails where it is configured.
func NewResidentSet(cfg ResidencyConfig) (*ResidentSet, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.InstanceBytes == nil {
		cfg.InstanceBytes = defaultResidentInstanceBytes
	}
	if cfg.clock == nil {
		cfg.clock = realResidentClock{}
	}
	if cfg.MemoryBudgetBytes == 0 && cfg.MaxRuns > 0 {
		var dearest uint64
		for _, b := range cfg.InstanceBytes {
			if b > dearest {
				dearest = b
			}
		}
		cfg.MemoryBudgetBytes = dearest * uint64(cfg.MaxRuns)
	}
	return &ResidentSet{cfg: cfg, parked: map[*residentSlot]struct{}{}}, nil
}

// Enabled says whether the set will ever park anything.
func (s *ResidentSet) Enabled() bool { return s != nil && s.cfg.MaxRuns > 0 }

// Len is how many runs are held now.
func (s *ResidentSet) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.parked)
}

// Drain sends every held run back and refuses every park from now on. It is idempotent and is what a worker calls when it starts to shut down: a run
// that is parked is in flight, so a drain that waited for it would wait for its wake.
func (s *ResidentSet) Drain() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.EvictAll(ResidentEvictedShutdown)
}

// EvictAll sends every held run back, and leaves the set open for new parks. A run sent back suspends as a refusal would have: nothing is lost, the
// next claim replays it.
func (s *ResidentSet) EvictAll(reason string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	for slot := range s.parked {
		if slot.reason == "" {
			slot.reason = reason
			close(slot.evict)
		}
	}
	s.mu.Unlock()
}

// ResidentRunInfo is what the set needs to know about one run to decide on it.
type ResidentRunInfo struct {
	WorkflowID string
	// Language is the guest's language as its metadata declares it; only a language with a measured cost is admitted.
	Language string
	// EventCount is how many events the run's history held when it was claimed, which is what the replay estimate is made from.
	EventCount int
}

// ResidentRun is one run's handle on the set: it is the engine's wakeSource for that run, and carries what Wait does not receive.
type ResidentRun struct {
	set  *ResidentSet
	info ResidentRunInfo
}

// ForRun returns the wake source for one run, or nil when residency is off, which is exactly "no wait parks".
func (s *ResidentSet) ForRun(info ResidentRunInfo) *ResidentRun {
	if !s.Enabled() {
		return nil
	}
	return &ResidentRun{set: s, info: info}
}

// Wait parks the run until the timer is due, the run's context ends, or the set sends it back. Only a timer is parked so far: the other kinds have no
// caller on the park helper yet (park.go), and a kind that can be requested but cannot be woken is refused, never parked.
func (r *ResidentRun) Wait(ctx context.Context, req parkRequest) parkOutcome {
	s := r.set
	refuse := func(reason string) parkOutcome {
		if m := s.cfg.Metrics; m != nil {
			m.RecordResidentRefusal(ctx, req.Kind.String(), reason)
		}
		return parkRefused
	}
	if req.Kind != wakeTimer {
		return refuse(residentRefusedKind)
	}
	cost, ok := s.cfg.InstanceBytes[r.info.Language]
	if !ok {
		return refuse(residentRefusedLanguage)
	}
	// The clock decides, and a wait already due has nothing to park for.
	remaining := time.UnixMilli(req.DeadlineMs).Sub(s.cfg.clock.Now())
	if remaining <= 0 {
		return parkDeadline
	}
	if remaining > s.cfg.window(r.info.EventCount) {
		return refuse(residentRefusedWindow)
	}
	if s.cfg.Pressure != nil && s.cfg.Pressure() > 0 {
		return refuse(residentRefusedPressure)
	}

	// The run's own wall-clock limits (the workflow deadline and the ceiling) are PAUSED for the hold and restarted when it ends, as a suspension and its
	// resume would give the next segment fresh ones (execution_limits.go). A limit that has already ended, or whose timer is firing, cannot be paused and the
	// run is not held: the expiry is on its way, and a hold would end in "execution timed out" where a suspension would not.
	resume, ok := pauseExecutionLimits(ctx)
	if !ok {
		return refuse(residentRefusedDeadline)
	}
	// What is left in ctx.Deadline() now is a deadline that CANNOT be paused (a caller's own context). A wait due within DeadlineMargin of it is refused: the
	// hold would end in the same timeout. Compared as absolute wall-clock instants; one due at or after (deadline - margin) is refused, so only a deadline
	// strictly later than wake + margin leaves the run room to come back as a suspension. The margin is measured (see DeadlineMargin).
	if dl, ok := ctx.Deadline(); ok && !dl.After(time.UnixMilli(req.DeadlineMs).Add(s.cfg.DeadlineMargin)) {
		resume(false)
		return refuse(residentRefusedDeadline)
	}

	slot := &residentSlot{bytes: cost, evict: make(chan struct{})}
	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		resume(false)
		return refuse(residentRefusedDraining)
	case len(s.parked) >= s.cfg.MaxRuns:
		s.mu.Unlock()
		resume(false)
		return refuse(residentRefusedCount)
	case s.bytes+cost > s.cfg.MemoryBudgetBytes:
		s.mu.Unlock()
		resume(false)
		return refuse(residentRefusedMemory)
	}
	s.parked[slot] = struct{}{}
	s.bytes += cost
	s.mu.Unlock()
	if m := s.cfg.Metrics; m != nil {
		m.AddResidentRuns(ctx, 1)
	}

	fired, release := s.cfg.clock.After(remaining)
	defer release()
	outcome, ended := parkRefused, residentEndedCancelled
	select {
	case <-fired:
		outcome, ended = parkDeadline, residentEndedDeadline
	case <-ctx.Done():
	case <-slot.evict:
		ended = residentEndedEvicted
	}

	s.mu.Lock()
	delete(s.parked, slot)
	s.bytes -= cost
	reason := slot.reason
	s.mu.Unlock()
	// However the hold ended, the segment it was in is over: restart the limits (and the fence), so that the work after the wake, or the unwind of a run that
	// was sent back, has a segment's worth of time and not what was left when the run was paused.
	resume(true)
	if m := s.cfg.Metrics; m != nil {
		m.AddResidentRuns(ctx, -1)
		m.RecordResidentPark(ctx, req.Kind.String(), ended)
		if ended == residentEndedEvicted {
			m.RecordResidentEviction(ctx, reason)
		}
	}
	return outcome
}
