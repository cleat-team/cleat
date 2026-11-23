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
//   - the memory of a held instance is measured for MINIMAL guests, not for real workflows: Go in design section 9a, Python and C# in WS-cleat1's component-held-
//     instances measurement (2026-11-20, cleat-internal/measurements/component-held-instances-2026-11-20/README.md, arm64 only). Go, Python and C# are admitted. Each
//     figure below is the measured one ROUNDED UP to a whole MiB, and the more expensive of the measured guests where there were two. What a guest ALLOCATES before it
//     blocks is not in any figure and adds on top roughly one for one (measured: a guest that touched 64 MiB cost 64 MiB more, in both Python and C#), so a table
//     entry is a floor for a guest that does real work, never a ceiling.
//   - the replay cost model (a fixed start-up of 0.44 to 0.67 s per claim) is measured; the cost PER EVENT is not, so it defaults to zero and the
//     multiplier has no effect until it is. With the defaults the window is the floor.
//   - the 10x multiplier, the 10 s floor and the 5 min cap are a starting point, not measurements. Tuning them on real workflows is cleat#4187.

// defaultResidentInstanceBytes is what admitting one held instance of a language is charged against the resident-memory budget. A language absent
// from this table is REFUSED: there is no figure for it, and "unknown, so probably small" is the wrong way to fail. Java, Rust and JavaScript have no figure.
var defaultResidentInstanceBytes = map[string]uint64{
	// design section 9a: 2.5 MiB for a minimal Go guest and 19.6 MiB after the guest had allocated and dropped about 256 MiB. Rounded up from the second.
	"go": 24 << 20,
	// The rule for every language is the one Go's line already follows: the measured figure, rounded up, because a too-high charge can only make residency park less.
	//
	// Python: 1.31 MiB per held instance (Pss, a fit over N = 1 to 64 held instances, three repetitions each, R2 = 1.0000, 2026-11-22) once a component is built with the dense
	// data segments of cleat#4387, which let wasmtime share the CPython module's memory image between instances; 12.1 MiB before it. Plus 1 MiB for the store and the session,
	// 2.3, rounded up to 3. THE CAVEAT: a component built BEFORE cleat#4387 still costs 12.1 MiB, so this charges it about 9 MiB short; the engine cannot tell the two apart
	// from the bytes, and --memory-soft-limit, which reads the cgroup's real use, is what catches it.
	"python": 3 << 20,
	// C#: 16.7 MiB per held instance (the same fit, 2026-11-22) at the 8 MiB GC segments of cleat#4386, which stop the runtime zero-filling three 16 MiB segments; 48.7 MiB before.
	// Plus 1 MiB, 17.7, rounded up to 18. THE SAME CAVEAT: a component built before cleat#4386 costs 48.7 MiB, so this charges it about 31 MiB short.
	// What a guest ALLOCATES before it waits adds on top roughly one for one (measured: a guest that touched 64 MiB cost 64 MiB more, in both), so every figure here is a floor
	// for a guest that does real work and never a ceiling. The earlier figures (Python 17, C# 66) were the larger of the measurements taken after a guest had allocated and dropped
	// about 256 MiB; those were NOT re-measured after the two changes, so this table is lower for two reasons: the instances really are smaller, and the high-water allowance is gone.
	"csharp": 18 << 20,
}

// WHAT IS DELIBERATELY NOT CHARGED: the compiled component. WS-cleat1 measured a per-process fixed cost for Python (+493 to +510 MiB) and C# (+81 MiB), and an earlier
// version of this file charged it to the budget once per distinct component. It was removed, on the coordinator's reading and the code's: compiled components live in an
// LRU cache (componentCache, cleat#3748) that holds them whether or not anything is held, so a worker that ran the workflow at all already carries that memory and holding
// does not add it, and the measured figure also contained the Go runtime and wasmtime baseline, which holding obviously does not add either. Charging it made the default
// budget refuse a second Python definition (512 MiB each against a default of about 860 MiB) for memory the worker was already using. It belongs to the worker's own
// memory guard (--memory-soft-limit), not to this budget. NOT MEASURED, and so not charged: that a held instance PINS a compiled component the LRU would otherwise have
// evicted (an instance holds its code alive, so it can); if that is measured to matter, charge that part and only that part.

// Why a wait was not parked, and why a parked run was sent back. They are metric label values and are read by tests, so they are constants.
const (
	residentRefusedKind     = "kind"           // a kind with no wake source here (only timers and signals have one), or a signal wait that carries no Recheck
	residentRefusedLanguage = "language"       // no measured cost for this language
	residentRefusedWindow   = "window"         // due later than the window allows: parking it would hold the run for nothing
	residentRefusedCount    = "count"          // the resident-run limit is reached
	residentRefusedMemory   = "memory"         // admitting it would exceed the resident-memory budget
	residentRefusedPressure = "pressure"       // the worker is at or over its memory soft limit
	residentRefusedDraining = "draining"       // the worker is shutting down
	residentRefusedCode     = "code"           // the cache has already evicted the compiled code the run executes (see CodeCached)
	residentRefusedDeadline = "deadline"       // the run's own context (the wall-clock ceiling, the workflow deadline) ends at or before the wake
	residentEndedDeadline   = "deadline"       // the timer fired: the caller's clock decides whether the wait is over
	residentEndedCancelled  = "cancelled"      // the run's context ended while parked
	residentEndedEvicted    = "evicted"        // the set sent the run back (see the eviction reasons)
	residentEndedWoke       = "woke"           // a signal wait's recheck found a delivery: the call reads and consumes it
	residentEndedRecheckErr = "recheck_failed" // a signal wait's recheck could not tell, so the hold ended as a refusal and the run suspends
)

// The results of one recheck, the label of cleat_resident_rechecks_total.
const (
	residentRecheckFound = "found"
	residentRecheckNone  = "none"
	residentRecheckError = "error"
)

// The reasons a parked run is sent back, exported because the worker names them when it evicts.
const (
	ResidentEvictedShutdown = "shutdown" // the worker began draining
	ResidentEvictedPressure = "pressure" // the worker reached its memory hard limit
	ResidentEvictedCode     = "code"     // the cache evicted the compiled code under the run (EvictCode): a hold never outlives its code's cache entry
)

// ResidencyConfig is the whole policy. The zero value is OFF: MaxRuns is zero, so every wait suspends as it always has.
type ResidencyConfig struct {
	// MaxRuns is the most runs held at once. Zero turns residency off. It is deliberately small when it is on, because a parked instance holds an OS
	// thread (design 4.6: about one per instance) and the thread cost at larger counts was not measured.
	MaxRuns int
	// MemoryBudgetBytes bounds the sum of the per-instance charges of the runs held. Zero means MaxRuns times the dearest admitted language, which is the bound MaxRuns
	// already implies. That is the bound the TABLE's figures imply and nothing more: the table is the cost of the RUNTIME, not of what a guest allocates before it blocks
	// (measured: that adds on top roughly one for one), so the default can still be exceeded by the real memory of real guests, which is what --memory-soft-limit is for;
	// it is NOT a promise that nothing is refused on memory, and it must not be read as one.
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
	// RecheckInterval is how often a held SIGNAL wait asks the store whether its delivery has arrived. The worker sets it to --poll, so on a dialect with no NOTIFY a held run waits at most one
	// interval for its signal. On Postgres the NOTIFY listener calls Nudge, which makes the recheck run at once, and this interval is the backstop that finds what a lost nudge would. Zero means signal waits are not held, which is
	// the safe reading of an unset field. Each recheck is one store read per awaited name per held run, bounded by MaxRuns; it is not a claim check (that runs once, when the wait ends).
	RecheckInterval time.Duration
	// NudgeMinInterval is the least time between two rechecks of one held signal wait when a Nudge is what triggers them (the interval tick is not limited by it). Nudge is driven by
	// the worker's NOTIFY listener, whose channel carries EVERY dispatch wake-up (a start, a completion, a signal: eleven write paths), so without a floor a write-heavy database would
	// turn each held wait into one store read per write. A nudge that arrives inside the gap is not dropped: it is answered once, when the gap ends, so the wait is never more than this
	// late and never more than one read per gap. Zero means no floor.
	NudgeMinInterval time.Duration
	// InstanceBytes is the per-language charge for each held instance; nil means defaultResidentInstanceBytes.
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
		RecheckInterval:  500 * time.Millisecond, // the worker's --poll default
		NudgeMinInterval: 50 * time.Millisecond,
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
	case c.RecheckInterval < 0:
		return fmt.Errorf("resident recheck interval %s must not be negative", c.RecheckInterval)
	case c.NudgeMinInterval < 0:
		return fmt.Errorf("resident nudge interval %s must not be negative", c.NudgeMinInterval)
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
	// codeCached, when set (AttachToCodeCache), says whether the compiled code with this identity is still in the cache. It is read under mu, in the same critical section
	// that parks a run, which is what closes the race EvictCode alone leaves open: the cache reports an eviction AFTER it releases its own lock, so either admission sees the
	// code gone and refuses, or the run is already parked when EvictCode takes mu and finds it. There is no lock inversion: nothing holds the cache's lock while taking mu.
	codeCached func(codeID string) bool
	// nudge is closed and replaced by Nudge, so every held signal wait that fetched it before its last recheck wakes at once (a broadcast; guarded by mu).
	nudge chan struct{}
}

type residentSlot struct {
	bytes uint64
	// codeKey is the identity of the code the run executes (ResidentRunInfo.CodeKey); empty means unknown, which EvictCode never matches.
	codeKey string
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
	// The budget is a fixed fact of the set, so it is recorded once here and the charged figure moves with each hold (cleat#4377).
	if m := cfg.Metrics; m != nil && cfg.MaxRuns > 0 && cfg.MemoryBudgetBytes > 0 {
		m.AddResidentMemoryBudget(context.Background(), int64(cfg.MemoryBudgetBytes))
	}
	return &ResidentSet{cfg: cfg, parked: map[*residentSlot]struct{}{}, nudge: make(chan struct{})}, nil
}

// DearestResidentInstanceBytes is the largest per-language charge in the default table. The worker uses it for the one case with no better number: a host whose memory
// it cannot read, where the budget falls back to a few runs of the dearest language instead of a share of a size it does not know (cleat#4377).
func DearestResidentInstanceBytes() uint64 {
	var dearest uint64
	for _, b := range defaultResidentInstanceBytes {
		if b > dearest {
			dearest = b
		}
	}
	return dearest
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

// EvictCode sends back every held run whose code is cached under key, and leaves the set open. It is called when the compiled-code cache evicts that key
// (wasmtimeBackend.OnModuleEvicted): a held instance refcounts the code it runs, so without this a hold would keep alive code the cache had decided to free, up to
// --resident-max-runs times the size of a compiled component (about 500 MiB for Python) of memory no bound accounts for. A run sent back suspends as a refusal would
// have, nothing is lost, and the next claim replays it (which recompiles the code). Holds therefore never outlive their code's cache entry.
func (s *ResidentSet) EvictCode(key string) {
	if s == nil || key == "" {
		return
	}
	s.mu.Lock()
	for slot := range s.parked {
		if slot.codeKey == key && slot.reason == "" {
			slot.reason = ResidentEvictedCode
			close(slot.evict)
		}
	}
	s.mu.Unlock()
}

// AttachToCodeCache connects the set to a backend's compiled-code cache, so that when the cache evicts code every held run of it is sent back (EvictCode). It reports
// whether the backend has a cache to attach to: a backend that is not the wasmtime one, or a build without cgo, has none and nothing is lost by that (nothing is held
// there). It is a method, and not three lines in the worker's main, so that the end-to-end row exercises the same wiring the worker uses.
func (s *ResidentSet) AttachToCodeCache(backend any) bool {
	h, ok := backend.(interface {
		OnModuleEvicted(func(string))
		CodeCached(string) bool
	})
	if !ok {
		return false
	}
	s.mu.Lock()
	s.codeCached = h.CodeCached
	s.mu.Unlock()
	h.OnModuleEvicted(s.EvictCode)
	return true
}

// ResidentRunInfo is what the set needs to know about one run to decide on it.
type ResidentRunInfo struct {
	WorkflowID string
	// CodeKey is the identity of the run's compiled code (CodeID of its wasm bytes); EvictCode matches on it. With no cache attached it is not consulted. WITH one attached an
	// empty CodeKey is REFUSED, not skipped: a run that names no code can never be sent back by an eviction, so holding it would be a pin nothing could see. The worker always sets it.
	CodeKey string
	// Language is the guest's language as its metadata declares it; only a language with a measured cost is admitted.
	Language string
	// EventCount is how many events the run's history held when it was claimed, which is what the replay estimate is made from.
	EventCount int
	// ClaimCheck reports whether the store still has this execution assigned to this worker: nil if it does, an error if it does not OR if that could not be
	// established. It is run every time a held run is about to go back to its guest (parkOrSuspend), because a terminate, a cancel or a reclaim made while the
	// run was held is otherwise learned only at the next heartbeat, and in the meantime the guest could act: measured on a real worker, a run terminated while
	// held reached its next outbound call, which a suspended run cannot. REQUIRED: WithResidency installs no wake source for a handle without one.
	ClaimCheck func(ctx context.Context) error
	// PublishState writes the run's query state to the store WITHOUT ending its segment, and is called at HOLD ADMISSION (parkOrSuspend), just before the run is held, whenever the
	// run has query state the store does not yet hold. A held run never returns to the worker, and the worker writes query state only when an execution returns, so without this
	// a client asking what the run is doing sees nothing it set before its wait, for the whole hold (cleat#4162, the AI agent platform scenario). An error REFUSES the hold: the
	// run suspends instead, and a suspension writes the state. nil means the handle cannot publish: a run with NO query state is still held (there is nothing to publish), and
	// a run WITH some is refused, so an omission fails closed and never silent.
	PublishState func(ctx context.Context, state map[string]string) error
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

// Wait parks the run until the wait is over, the run's context ends, or the set sends it back. A timer ends when it is due. A signal wait ends when its Recheck finds a delivery,
// or at its deadline. The other kinds have no wake source here, and a kind that can be requested but cannot be woken is refused, never parked.
func (r *ResidentRun) Wait(ctx context.Context, req parkRequest) parkOutcome {
	s := r.set
	refuse := func(reason string) parkOutcome {
		if m := s.cfg.Metrics; m != nil {
			m.RecordResidentRefusal(ctx, req.Kind.String(), reason)
		}
		return parkRefused
	}
	if req.Kind != wakeTimer && req.Kind != wakeSignal {
		return refuse(residentRefusedKind)
	}
	// A signal wait is woken by a store write, so it is held only if the call supplied the read that finds the write and the set knows how often to run it. Held without one, it would
	// sit until its deadline, which is the latency regression this kind exists to avoid.
	if req.Kind == wakeSignal && (req.Recheck == nil || s.cfg.RecheckInterval <= 0) {
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

	slot := &residentSlot{bytes: cost, codeKey: r.info.CodeKey, evict: make(chan struct{})}
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
	case s.codeCached != nil && (r.info.CodeKey == "" || !s.codeCached(r.info.CodeKey)):
		// The code was evicted while this run was still executing, so EvictCode had no slot to reach; or the run names no code at all, or code the cache cannot place, and EvictCode could
		// never reach it either. Holding any of them would pin code the cache decided to free, invisibly. A refused hold suspends as with residency off: it costs one replay, not
		// correctness, so refusing is the cheap direction and a silent pin is the expensive one.
		s.mu.Unlock()
		resume(false)
		return refuse(residentRefusedCode)
	}
	s.parked[slot] = struct{}{}
	s.bytes += cost
	s.mu.Unlock()
	if m := s.cfg.Metrics; m != nil {
		m.AddResidentRuns(ctx, 1)
		m.AddResidentMemoryCharged(ctx, int64(cost))
	}

	outcome, ended := parkRefused, residentEndedCancelled
	if req.Kind == wakeSignal {
		outcome, ended = r.holdForSignal(ctx, req, slot, remaining)
	} else {
		fired, release := s.cfg.clock.After(remaining)
		defer release()
		select {
		case <-fired:
			outcome, ended = parkDeadline, residentEndedDeadline
		case <-ctx.Done():
		case <-slot.evict:
			ended = residentEndedEvicted
		}
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
		m.AddResidentMemoryCharged(ctx, -int64(cost))
		m.RecordResidentPark(ctx, req.Kind.String(), ended)
		if ended == residentEndedEvicted {
			m.RecordResidentEviction(ctx, reason)
		}
	}
	return outcome
}

// Nudge makes every held signal wait recheck the store NOW instead of at its next interval tick. It is a hint and never the correctness path: the interval recheck still finds whatever a
// missed nudge would have, so a nudge that is lost, early or spurious costs at most one extra store read. It carries no payload and names no run, because the NOTIFY that drives it
// carries none (pg_notify with an empty payload): it wakes every held signal wait on this worker, and each one asks its own store question and goes back to holding if the answer is no.
// A nudge changes nothing else about a hold: a recheck that finds nothing does not return, so it does not restart the run's limits.
//
// Safe on a nil set and on a closed one. It is the Postgres half of cleat#4302; the same-worker delivery nudge (handleSignal) is still that issue's.
func (s *ResidentSet) Nudge() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	close(s.nudge)
	s.nudge = make(chan struct{})
}

// nudgeChan is the channel the NEXT Nudge closes. A hold fetches it BEFORE it runs a recheck, never after: a nudge that lands while the recheck is running closes the channel the hold is
// already holding and is seen at once, and a nudge that lands before the fetch is covered by the recheck that follows it (the write it announced is committed before the NOTIFY).
func (s *ResidentSet) nudgeChan() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nudge
}

// holdForSignal is the hold of a signal wait. The run is already admitted: it is in the set, its limits are paused, and its charge is taken. It asks the call's Recheck once STRAIGHT
// AWAY and then every RecheckInterval, all inside this one hold, and it ends when the recheck finds a delivery (parkWoke), the deadline passes (parkDeadline), the run's context ends,
// the set sends the run back, or the recheck cannot tell (a refusal, so the run suspends and a later claim reads the store itself).
//
// The first recheck is the ordering that closes the lost-wakeup window: the call read the store, then asked to be held, and a signal committed between those two is found here and not
// missed until the next tick. It is the same shape as the cache check at admission (a check made after the thing that could make it stale is registered), and it is run before the
// first timer is armed so that a delivery that is already there never waits an interval.
//
// WHAT THIS DOES NOT DO: it does not return a wake that found nothing. A return that is not a refusal restarts the run's limits (Wait's tail), so a periodic woke would restart the
// wall-clock ceiling on every tick. The ticks are rechecks, not returns.
func (r *ResidentRun) holdForSignal(ctx context.Context, req parkRequest, slot *residentSlot, remaining time.Duration) (parkOutcome, string) {
	s := r.set
	recheck := func() (found bool, err error) {
		found, err = req.Recheck(ctx)
		if m := s.cfg.Metrics; m != nil {
			result := residentRecheckNone
			switch {
			case err != nil:
				result = residentRecheckError
			case found:
				result = residentRecheckFound
			}
			m.RecordResidentRecheck(ctx, req.Kind.String(), result)
		}
		return found, err
	}
	if ctx.Err() != nil {
		return parkRefused, residentEndedCancelled
	}
	// Fetched BEFORE the first recheck (see nudgeChan), and again before every later one.
	nudge := s.nudgeChan()
	lastCheck := s.cfg.clock.Now()
	if found, err := recheck(); err != nil {
		return parkRefused, residentEndedRecheckErr
	} else if found {
		return parkWoke, residentEndedWoke
	}
	deadline, releaseDeadline := s.cfg.clock.After(remaining)
	defer releaseDeadline()
	// again rechecks now: it refetches the nudge channel first, and reports whether the hold is over.
	again := func() (parkOutcome, string, bool) {
		nudge = s.nudgeChan()
		lastCheck = s.cfg.clock.Now()
		found, err := recheck()
		switch {
		case err != nil:
			return parkRefused, residentEndedRecheckErr, true
		case found:
			return parkWoke, residentEndedWoke, true
		}
		return 0, "", false
	}
	// gap is armed when a nudge arrived inside NudgeMinInterval of the last recheck. While it is, the nudge channel is not watched (nudge is nil): the recheck that ends the gap reads
	// everything committed before it, so the nudges in between are answered by that one read.
	var gap <-chan time.Time
	releaseGap := func() {}
	defer func() { releaseGap() }()
	for {
		tick, releaseTick := s.cfg.clock.After(s.cfg.RecheckInterval)
		select {
		case <-deadline:
			releaseTick()
			return parkDeadline, residentEndedDeadline
		case <-ctx.Done():
			releaseTick()
			return parkRefused, residentEndedCancelled
		case <-slot.evict:
			releaseTick()
			return parkRefused, residentEndedEvicted
		case <-tick:
			releaseTick()
			releaseGap()
			gap, releaseGap = nil, func() {}
			if out, why, done := again(); done {
				return out, why
			}
		case <-nudge:
			releaseTick()
			if wait := s.cfg.NudgeMinInterval - s.cfg.clock.Now().Sub(lastCheck); wait > 0 {
				nudge = nil
				releaseGap()
				gap, releaseGap = s.cfg.clock.After(wait)
				continue
			}
			if out, why, done := again(); done {
				return out, why
			}
		case <-gap:
			releaseTick()
			gap, releaseGap = nil, func() {}
			if out, why, done := again(); done {
				return out, why
			}
		}
	}
}
