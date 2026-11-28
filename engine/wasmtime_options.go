package engine

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// This file has no build constraint (unlike backend_wasmtime.go, which
// requires cgo) so that cmd/cleat-worker and other callers can construct
// WasmtimeOption values unconditionally, regardless of whether the binary
// was built with CGO. When CGO is disabled, NewWasmtimeBackend (the stub in
// backend_wasmtime_stub.go) simply ignores the options and returns an error.

// DefaultWasmtimeExecutionTimeout bounds a single wasmtime invocation (one
// fresh execution or one replay pass) via epoch interruption when the
// caller has not configured a tighter one. This is a required safety net,
// not just a convenience default: wasmtime-go does not observe Go context
// cancellation while a WASM export call is in progress (see the comment on
// wasmtimeBackend.Execute in backend_wasmtime.go), so without a baked-in
// bound here an infinite loop in a workflow hangs the worker permanently
// regardless of any engine- or worker-level context timeout.
//
// 30s matches the wazero backend's hardcoded Runtime.callTimeout default
// (engine/runtime.go), so both backends behave the same way out of the box.
const DefaultWasmtimeExecutionTimeout = 30 * time.Second

// DefaultWasmWallClockCeiling is the worker's default for --wasm-wall-clock-ceiling: the longest one WASM invocation may take in wall-clock time, host calls
// included. The engine itself falls back to the instance timeout when the option is not set (WithWasmWallClockCeiling); this is the value the worker
// passes, and it lived as a literal in the flag declaration until cleat#4172.
const DefaultWasmWallClockCeiling = 5 * time.Minute

// DefaultWasmtimeMemoryLimitBytes bounds linear memory per wasmtime store
// when the caller has not configured a tighter one via
// WithWasmtimeMemoryLimits. It matches DefaultMemoryLimitPages (256 MiB),
// the wazero backend's default (engine/runtime.go), so an operator sees the
// same memory ceiling regardless of which backend happens to run a given
// workflow.
//
// It is a ceiling, not a reservation, and it must stay above the least memory any supported guest runtime needs to START: a .NET guest
// compiled with NativeAOT-LLVM dies at startup with ".NET runtime failed to initialize" below 52 MiB (measured on a 704 KB hello-world,
// cleat#4077), a message that names neither the limit nor the cause. Until cleat#4171 the default was 32 MiB and a per-language floor
// raised it for C#; that floor could never fire once the default is above it, so it was removed rather than left as a tested mechanism that
// reaches nothing. TestTheDefaultMemoryLimitClearsTheLargestRuntime is what stops the default being lowered below that minimum again.
const DefaultWasmtimeMemoryLimitBytes = int64(DefaultMemoryLimitPages) * int64(wasmPageSize)

// DefaultWasmtimeTableElementsLimit bounds indirect-function-table growth
// per wasmtime store.
//
// The 1,048,576-element figure it is derived from came from `tblMinSize` in
// wasmtimeBackend.ExecuteComponent -- the decomposition path, deleted
// 2026-09-01 (IMPROVEMENT-PLAN 3.65). The number is kept rather than
// re-derived: it was measured from real componentize-py bundles, those bundles
// have not changed, and the native Component Model path instantiates the same
// core modules with the same tables. 8x that headroom keeps existing workflows
// working while still capping unbounded/attacker-controlled table growth.
//
// If it ever needs re-deriving, the source is the largest `(table ...)` minimum
// in a componentize-py component's core modules, not anything in this repo.
const DefaultWasmtimeTableElementsLimit = 8 * 1024 * 1024

// DefaultWasmtimeInstancesLimit bounds how many module instances a single
// wasmtime store may create. Component bundles observed in this codebase
// (CPython runtime + adapters) use at most a few dozen instances; 256
// leaves generous headroom while still bounding runaway instantiation.
const DefaultWasmtimeInstancesLimit = 256

// DefaultWasmtimeDeferBudget bounds the cleanup pass the host runs on a guest
// it has just killed (IMPROVEMENT-PLAN 3.35 phase 4).
//
// This is EXTRA execution granted to a workflow the fence has already stopped,
// so it has to be small enough not to undo the bound it is being granted
// against. 5s next to the 30s DefaultWasmtimeExecutionTimeout raises the
// worst case a workflow can hold a worker by about a sixth rather than
// doubling it, and a cleanup pass is a handful of host calls -- releasing a
// lock, refunding a charge -- not a workload.
//
// It is a wall-clock bound and it is the binding one: it stops a defer body
// that loops forever exactly the way the fence stops an entry point that does.
const DefaultWasmtimeDeferBudget = 5 * time.Second

// DefaultWasmtimeInterruptMarginFuel is the fuel reserve at which the soft kill asks the guest to
// stop cleanly instead of letting wasmtime's fuel trap kill the instance mid-borrow (cleat#3922).
//
// Sized from the largest amount of fuel measured between two consecutive handler calls, by
// TestMeasureInterruptMargin (`go test -tags measure ./tests/typescript/`) over the shapes of
// endless loop it keeps: 1,965,040 (tight numeric
// loop), 3,840,041 (a native call in a loop) and 26,241,715 (JSON.parse of a 200k-element payload).
// A fourth, a deliberately exponential regex at 2,116,250, was removed with its workload when code
// scanning flagged the literal; it was never load bearing. 33,554,432 fuel units -- 32 x 1024 x 1024, wasmtime's own count and not
// bytes -- is the largest of those with about a fifth of headroom.
//
// IT IS NOT A CEILING, and the margin is a best-effort reserve rather than a guarantee. QuickJS
// polls its interrupt handler once per ~10,000 BYTECODE operations, while fuel is spent by whatever
// that operation does, so an operation that spends a lot of wasm fuel in one go widens the gap
// without bound: a loop of `"x".repeat(10_000)` did not reach a single poll inside a 400M budget
// (measured 2026-11-17). A guest dominated by that shape still ends on the hard fuel trap, and its
// defers then do not run. That residue is recorded in packages/cleat-ts/DIVERGENCES.md.
//
// It applies only when fuel is metered at all: with --wasm-instruction-limit 0 (the default) there
// is no budget to approach and no soft stop to offer.
const DefaultWasmtimeInterruptMarginFuel = 32 * 1024 * 1024

// DefaultWasmtimeInterruptMarginTime is the epoch arm's reserve: how much of the instance-timeout
// budget must be left for the soft kill to stop the guest cleanly (cleat#3922).
//
// This arm is the one that matters in the default configuration, where --wasm-instruction-limit is
// 0 and the kill is the epoch fence. Sized from the larger of one epoch tick (epochTickInterval,
// 50ms -- a guest stops at the first tick after the deadline, so a reserve smaller than one tick
// can pass the deadline before the handler is asked again) and the longest time measured between
// two consecutive handler calls; see TestMeasureInterruptMargin (built only with `-tags measure`).
//
// The effective reserve is the smaller of this, a tenth of the budget and half of it, with a floor
// of interruptMinMarginTime (two epoch ticks plus a poll gap): a reserve under two ticks can be
// overtaken by the hard trap before any poll sees it, and one above half the budget stops a
// workflow almost at once. On a sub-second budget the floor is what binds, so the kill can arrive up
// to half the budget early.
const DefaultWasmtimeInterruptMarginTime = 250 * time.Millisecond

// interruptUnwindGrant is how much budget the soft kill gives back before the guest unwinds
// (cleat#3922). Measured 2026-11-17, from the handler answering 1 to the host observing the call's
// error, over the workloads in TestMeasureInterruptMargin (`-tags measure`): 0.10-0.46ms in the ordinary shapes and
// 1.33ms for the largest (JSON.parse of a 200k-element payload), spending at most 9.16M fuel.
//
// It has to exceed ONE EPOCH TICK, and that is the real reason it exists rather than the unwind's
// cost. hostBudget.setDeadline arms ticks = remaining/50ms floored at one, and a deadline of N
// ticks expires 50*(N-1) to 50*N ms after it is armed. So the tight case is a budget under 100ms at
// the moment of the stop: that arms ONE tick, whose expiry is a uniform 0-50ms away, and a 0.15ms
// unwind loses that race about once in 330 stops.
//
// WHERE THAT CASE ARISES. With --wasm-instance-timeout at its 30s default the margin is 250ms and
// the bracket arms 5 ticks at the stop, so the guest has 200-250ms to unwind and no race exists.
// A budget under 100ms at the stop needs the reserve to be under two ticks, which the review of this
// PR measured at an 800ms budget -- TestTypeScriptSoftKillsWithASubSecondBudget -- and which lost 7
// drains in 20 before the floor above existed. The floor removes the race there; the grant remains
// for the case where a poll gap or a short ctx deadline still lands the stop inside one tick.
const interruptUnwindGrant = 100 * time.Millisecond

// wasmtimeLimits bundles the resource bounds applied to a wasmtime
// execution. Zero/negative fields mean "use the backend's built-in
// default" (see the Default* constants above) except instructionLimit,
// where 0 legitimately means "fuel metering disabled" — this matches the
// existing --wasm-instruction-limit flag semantics ("0 = no limit").
type wasmtimeLimits struct {
	executionTimeout   time.Duration
	instructionLimit   uint64
	memoryLimitBytes   int64
	tableElementsLimit int64
	instancesLimit     int64
	deferBudget        time.Duration

	// interruptMargin is the fuel reserve the soft kill stops at (cleat#3922). Zero takes
	// DefaultWasmtimeInterruptMarginFuel; it is only consulted when instructionLimit > 0.
	interruptMargin uint64
}

// wasmtimeConfig is everything NewWasmtimeBackend accepts: the resource
// bounds, plus the things that are not bounds.
//
// The options used to be func(*wasmtimeLimits), which was accurate while
// limits were all there was. A logger is not a limit, and putting one in a
// struct called "limits" is the kind of small dishonesty that later gets read
// as a fact about the type.
type wasmtimeConfig struct {
	limits wasmtimeLimits
	logger *slog.Logger

	// moduleCacheMaxEntries bounds the compiled-module cache. 0 means
	// DefaultModuleCacheMaxEntries; see moduleLRU. Not in wasmtimeLimits
	// because it bounds a process-wide CACHE rather than anything one
	// execution may consume, which is what the fields in that struct are.
	moduleCacheMaxEntries int

	// moduleCacheMaxBytes bounds the same cache by estimated resident size.
	// 0 means DefaultModuleCacheMaxBytes. Both bounds apply; see moduleLRU
	// for why a count alone could not say what a hundred entries cost.
	moduleCacheMaxBytes int64

	// componentPrecompile, when non-nil, makes a component compile happen in a short-lived child process (see ComponentPrecompile).
	componentPrecompile *ComponentPrecompile
	// componentFallbackHook is called once for each compile a failed precompile child sent back to the worker (WithWasmtimeComponentPrecompileFallbackHook).
	componentFallbackHook func(reason string)
	compileHeadroom       *compileHeadroom
}

// ComponentPrecompile turns on compiling a Component Model component in a CHILD process instead of in the worker (cleat-internal/component-precompile-design-2026-11-23.md, Option A).
//
// A component compile raises a process's resident memory by ~490 MiB (Python) of Cranelift scratch, most of which the allocator keeps after the compile, and the engine retains ~145 MiB of the
// result for the life of the process. A child that compiles, serializes and exits returns all of it to the OS, and the worker loads the serialized artifact for ~27 MiB. EXPERIMENTAL, off by
// default, and it does not remove the compile PEAK: the child shares the worker's cgroup, so the peak is the worker's baseline plus the child's.
//
// Every failure (the child cannot start, times out, exits nonzero, writes an artifact the worker will not accept) is logged and falls back to the in-process compile; it never fails a workflow.
type ComponentPrecompile struct {
	// Executable is the worker's own binary, resolved once at start-up to an absolute path. It is never looked up on PATH, so the child is the same build and runs the same config code.
	// Empty disables the feature (the in-process compile is used and nothing is logged as a failure).
	Executable string
	// Timeout bounds one child. <= 0 takes DefaultComponentPrecompileTimeout.
	Timeout time.Duration
	// MemoryLimitBytes is the worker's memory limit as the worker read it, 0 if unknown. It is only REPORTED (in a refusal's message, log and metric); nothing here acts on it.
	MemoryLimitBytes uint64
	// RefusalWindow is how long this worker refuses to try a component again after its child was killed: one attempt per component per window, so a run released for retry cannot loop
	// a doomed child. <= 0 takes DefaultComponentPrecompileRefusalWindow.
	RefusalWindow time.Duration
	// ParallelCompilation is wasmtime's parallel_compilation in the CHILD only. false lowers the compile's memory high-water and lengthens it; it does not bind the artifact. This is the Go zero value, so a caller that sets nothing gets the LOW-memory compile; the worker passes the value --component-precompile-parallel resolved (parallel unless its memory limit is under 1 GiB).
	ParallelCompilation bool
}

// DefaultComponentPrecompileRefusalWindow is how long a component whose child was killed is refused on this worker before another child is tried.
const DefaultComponentPrecompileRefusalWindow = 5 * time.Minute

// ErrComponentCompileRefused is what a refused compile matches with errors.Is. It is NOT a failure of the workflow: the guest never ran, so there is no outcome to record. The worker releases the run
// with a back-off (as it does for ErrLimitsUnavailable) so it retries, possibly on a worker with more memory.
var ErrComponentCompileRefused = errors.New("component compile refused")

// ComponentCompileRefusedError says why a component was not compiled, with what an operator needs to act: the memory limit the worker read, the compile mode, and when this worker will try again.
type ComponentCompileRefusedError struct {
	// MemoryLimitBytes is the worker's memory limit as it read it, 0 if unknown.
	MemoryLimitBytes uint64
	// ParallelCompilation is the child's compile mode.
	ParallelCompilation bool
	// RetryAfter is how long until this worker will try this component again. The run is released for at least this long.
	RetryAfter time.Duration
	// Window is the refusal window this worker is configured with: how long after a killed child it will not start another for the component.
	Window time.Duration
	// Repeat is true when the refusal was answered without trying: an earlier attempt within the window was killed.
	Repeat bool
	// Cause is what the child died of, nil for a repeat.
	Cause error
	// Reason is why the compile was refused: ComponentCompileRefusedChildKilled (the empty value means the same) or ComponentCompileRefusedLowHeadroom.
	Reason string
	// HeadroomBytes and NeededBytes are, for ComponentCompileRefusedLowHeadroom, the memory the container had left when the worker looked and what one in-process compile was judged to need.
	HeadroomBytes, NeededBytes uint64
}

// The values of ComponentCompileRefusedError.Reason, which are also the `reason` label of cleat_workflow_releases_total{check="component_compile_refused"}.
const (
	ComponentCompileRefusedChildKilled = "child_killed"
	ComponentCompileRefusedLowHeadroom = "low_headroom"
)

// ReasonLabel is Reason with the empty value read as a killed child, which is the only reason that existed before headroom.
func (e *ComponentCompileRefusedError) ReasonLabel() string {
	if e.Reason == "" {
		return ComponentCompileRefusedChildKilled
	}
	return e.Reason
}

func (e *ComponentCompileRefusedError) Error() string {
	if e.Reason == ComponentCompileRefusedLowHeadroom {
		what := "this worker has %d MiB of memory left, and compiling a component of this size in the worker is judged to need %d MiB (a Python-sized component: about 500 MiB of scratch during the compile and about 300 MiB it keeps; a small C# one far less)"
		if e.Repeat {
			what = "an earlier compile of this component was refused within the last " + e.Window.String() + " (%d MiB left against %d MiB needed)"
		}
		return fmt.Sprintf("%s: "+what+". The compile was not attempted, because the worker would be killed with every run it holds; this run is released and will be retried in %s, possibly on another worker, "+
			"and components already compiled keep running. WHAT A COMPILE KEEPS DOES NOT COME BACK: memory a worker compiled with in its own process stays with that process (the allocator keeps it, and wasm memory never shrinks), so a refusal for want of memory usually lasts until this worker is RESTARTED or its memory limit is RAISED, whichever you do first. "+
			"To fix it: raise the memory limit or restart the worker, and keep --component-precompile on so compiles run in a child that gives its memory back, or lower --resident-memory-percent; lower --component-compile-min-headroom-mb only if you have measured that this component needs less",
			ErrComponentCompileRefused.Error(), e.HeadroomBytes>>20, e.NeededBytes>>20, e.RetryAfter.Round(time.Second))
	}
	limit := "an unknown memory limit"
	if e.MemoryLimitBytes > 0 {
		limit = fmt.Sprintf("a %d MiB memory limit", e.MemoryLimitBytes>>20)
	}
	advice := "raise the memory limit, or compile the component outside the worker"
	if e.ParallelCompilation {
		advice = "set --component-precompile-parallel=false or raise the memory limit"
	}
	what := "the precompile child was killed"
	if e.Cause != nil {
		what += " (" + e.Cause.Error() + ")"
	}
	if e.Repeat {
		what = "an earlier precompile child for this component was killed within the last " + e.Window.String() + ", so none was started"
	}
	return fmt.Sprintf("%s: %s, most likely by %s with parallel_compilation=%t. Compiling the component in the worker would need more memory than the child did, "+
		"so it was not attempted; this run is released and will be retried in %s, possibly on another worker. To fix it: %s",
		ErrComponentCompileRefused.Error(), what, limit, e.ParallelCompilation, e.RetryAfter.Round(time.Second), advice)
}

// Is makes errors.Is(err, ErrComponentCompileRefused) true, so the worker can tell this from a workflow's own failure.
func (e *ComponentCompileRefusedError) Is(target error) bool {
	return target == ErrComponentCompileRefused
}

func (e *ComponentCompileRefusedError) Unwrap() error { return e.Cause }

// compileHeadroom decides, before a component is compiled IN THIS PROCESS, whether the container has the memory left to do it. A compile in the worker costs about 500 MiB of scratch while it
// runs (measured +486 to +512 MiB for a Python component) and keeps up to about 300 MiB of it for good (198 to 308 for each of the first four further components, then 44 to 76) (+185 to +268 raw per further distinct component, 198 to 308 for each of the first four further components and 44 to 76 after, in a 2 GiB container), so at
// a 2 GiB limit a worker survives about six distinct Python components and dies on the seventh (measured 2026-11-23). Serialising compiles removes the simultaneous peak and not that retention.
// Refusing, with the run released and not failed, is what lets the worker degrade (components already compiled keep running) instead of being killed with every run it holds.
//
// WHAT ONE COMPILE NEEDS DEPENDS ON THE COMPONENT, so the need is scaled by the wasm's length (the compile's work, which a guest cannot understate) between a floor and the configured maximum.
// Measured 2026-11-23 on a real worker, arm64 Linux, in a 2 GiB container, the container's own memory.current (peak = highest above the level before the compile, retained = what was still there
// 4 seconds after it), one compile at a time in the worker, precompile off:
//
//	component                         wasm        first compile in the process     each further distinct compile
//	C# (defer_order, 6 variants)     1.0 MiB      peak +118, retained +70         peak +11 to +23, retained +9 to +23
//	C# (conformance, 6 variants)     2.8 MiB      peak +60,  retained +60         peak +33 to +42 (one +3), retained +33 to +41
//	Python (componentize-py)        17.6-19.5 MiB peak +486 to +512 (RSS)         retained +185 to +268 (RSS); 198 to 308 for each of the first four further components and 44 to 76 after, in the 2 GiB container
//
// The C# variants are one set of code stamped with different metadata, so each is a different cache key with the same compile cost, which is what the cache sees and what a fleet of different
// libraries would exceed. The Python figures are the component-memory measurements and the 2 GiB run (anonymous memory after each of eight distinct compiles: 533, 836, 1034, 1277, 1585, 1661,
// 1716, 1760 MiB). The need for a Python-sized component is about 500 MiB of scratch plus about 300 MiB kept, 800; the largest C# peak, which is the first compile of a process and includes the
// engine's own start-up, is 118, doubled to 256 as the floor. Between them the need is compileHeadroomPerWasmByte times the wasm length (about 800 MiB for the 19.5 MiB Python component), so a
// large component of any language is not judged by the C# floor. It is a fit through two measured languages and says so.
type compileHeadroom struct {
	read   func() (available uint64, ok bool) // the container's memory left, ok false if it cannot be read
	need   uint64                             // the need of the LARGEST component: the configured maximum
	window time.Duration
	limit  uint64 // the worker's memory limit as it read it, for the error and its labels
	now    func() time.Time

	mu         sync.Mutex
	until      map[string]time.Time // component key -> when this worker will look again
	refusals   atomic.Int64
	unreadable atomic.Bool // the "cannot read" warning is given once
}

// refusalFor reports whether key was refused within the window, and how long is left.
func (h *compileHeadroom) refusalFor(key string) (time.Duration, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	until, ok := h.until[key]
	if !ok {
		return 0, false
	}
	if left := until.Sub(h.now()); left > 0 {
		return left, true
	}
	delete(h.until, key)
	return 0, false
}

func (h *compileHeadroom) refusal(avail, need uint64, repeat bool, left time.Duration) error {
	h.refusals.Add(1)
	return &ComponentCompileRefusedError{MemoryLimitBytes: h.limit, RetryAfter: left, Window: h.window, Repeat: repeat,
		Reason: ComponentCompileRefusedLowHeadroom, HeadroomBytes: avail, NeededBytes: need}
}

// compileHeadroomFloor is the least memory a component's compile is judged to need, and compileHeadroomPerWasmByte the slope above it: see compileHeadroom for the measurements behind both.
const (
	compileHeadroomFloor       = 256 << 20
	compileHeadroomPerWasmByte = 42
)

// needFor is what compiling a component of wasmLen bytes in this process is judged to need: wasmLen times compileHeadroomPerWasmByte, at least the floor and at most the configured maximum
// (a maximum below the floor is the floor).
func (h *compileHeadroom) needFor(wasmLen int) uint64 {
	n := uint64(wasmLen) * compileHeadroomPerWasmByte
	floor := uint64(compileHeadroomFloor)
	if floor > h.need {
		floor = h.need
	}
	if n < floor {
		n = floor
	}
	if n > h.need {
		n = h.need
	}
	return n
}

// check returns the refusal if the container has less than one compile of a component of wasmLen bytes needs left. A reading it cannot take is no reason to refuse: unreadable is true and the
// caller compiles.
func (h *compileHeadroom) check(key string, wasmLen int) (err error, unreadable bool) {
	avail, ok := h.read()
	if !ok {
		return nil, true
	}
	need := h.needFor(wasmLen)
	if avail >= need {
		return nil, false
	}
	h.mu.Lock()
	h.until[key] = h.now().Add(h.window)
	h.mu.Unlock()
	return h.refusal(avail, need, false, h.window), false
}

// WithWasmtimeCompileHeadroom makes the worker refuse to compile a component IN THIS PROCESS (the fallback after a failed precompile child, and every compile when precompile is off) while
// read reports less than need bytes of memory left. A refused run is released with the same back-off as a killed child. A zero need or a nil read leaves the check off. limitBytes is the worker's
// memory limit as it read it, for the error and its metric labels.
func WithWasmtimeCompileHeadroom(read func() (available uint64, ok bool), need uint64, window time.Duration, limitBytes uint64) WasmtimeOption {
	return func(c *wasmtimeConfig) {
		if read == nil || need == 0 {
			c.compileHeadroom = nil
			return
		}
		if window <= 0 {
			window = DefaultComponentPrecompileRefusalWindow
		}
		c.compileHeadroom = &compileHeadroom{read: read, need: need, window: window, limit: limitBytes, now: time.Now, until: map[string]time.Time{}}
	}
}

// SerializeComponentArg is the argument that makes the worker binary act as the precompile child. It is matched against os.Args[1] BEFORE flag parsing, so no flag, workflow, HTTP route or
// environment can reach it; only an exec of the binary with this exact first argument does. Declared here, untagged, because the worker's main reads it in every build.
const SerializeComponentArg = "--serialize-component"

// DefaultComponentPrecompileTimeout is the bound on one precompile child when none is set: the slowest measured Python compile with parallel_compilation off was 5.7 s on a loaded host, so this is
// generous, and a child that exceeds it is killed and the worker compiles in-process.
const DefaultComponentPrecompileTimeout = 5 * time.Minute

// WithWasmtimeComponentPrecompileFallbackHook sets a function called with a short reason (timeout, child_exit, record, artifact, deserialize, workdir, other) each time a precompile child failed
// for an ordinary reason and the worker compiled the component itself. It is how the worker counts the fallbacks on a metric: every one is a compile in this process, with the memory that
// costs, and a child that fails systematically turns precompile into the in-process case without any other sign. nil, or no precompile, means no calls.
func WithWasmtimeComponentPrecompileFallbackHook(fn func(reason string)) WasmtimeOption {
	return func(c *wasmtimeConfig) { c.componentFallbackHook = fn }
}

// WithWasmtimeComponentPrecompile enables compiling components in a child process. See ComponentPrecompile. A zero Executable keeps the in-process compile.
func WithWasmtimeComponentPrecompile(p ComponentPrecompile) WasmtimeOption {
	return func(c *wasmtimeConfig) {
		if p.Executable == "" {
			c.componentPrecompile = nil
			return
		}
		cp := p
		c.componentPrecompile = &cp
	}
}

// WasmtimeOption configures a wasmtimeBackend, applied at construction time
// (NewWasmtimeBackend). Resource bounds are enforced on every store it creates
// thereafter (see wasmtimeBackend.configureStore).
type WasmtimeOption func(*wasmtimeConfig)

// WithWasmtimeLogger routes the backend's own log records to l.
//
// Without it the backend writes to slog.Default(), which is not where an
// operator who configured a logger is looking. That mattered most on the one
// path where the backend has something to say that nothing else can: whether a
// KILLED workflow's defers ran (§3.35 phase 4). Three records -- the success
// line, the "could not be run" line, and the refuel warning -- all went to the
// default logger, so a worker with a configured handler reported nothing at
// all about the cleanup of a workflow it had just killed.
//
// Found while writing a test that asserted on that line and could not see it.
//
// nil keeps slog.Default().
func WithWasmtimeLogger(l *slog.Logger) WasmtimeOption {
	return func(c *wasmtimeConfig) {
		if l != nil {
			c.logger = l
		}
	}
}

// WithWasmtimeExecutionTimeout bounds a single wasmtime invocation via
// epoch interruption. d <= 0 keeps DefaultWasmtimeExecutionTimeout. A
// per-call context deadline (e.g. from engine.WithWASMInstanceTimeout or
// engine.WithDefaultWorkflowTimeout), when tighter than this, still wins —
// see wasmtimeBackend.configureStore.
func WithWasmtimeExecutionTimeout(d time.Duration) WasmtimeOption {
	return func(c *wasmtimeConfig) { c.limits.executionTimeout = d }
}

// WithWasmtimeInstructionLimit bounds fuel (roughly one unit per WASM
// instruction executed) consumed per invocation. 0 disables fuel
// metering — the wasmtime analogue of the wazero-only
// --wasm-instruction-limit flag defaulting to "no limit".
//
// Note for anyone tempted to raise this as the primary defense: fuel
// exhaustion mid-replay could in principle make replay diverge from the
// original execution if the configured limit changes between the two runs
// (e.g. a flag change), or on hardware where the same WASM instructions
// consume different amounts of fuel due to a wasmtime version skew. Epoch
// interruption (WithWasmtimeExecutionTimeout) does not have this property
// any more than wall-clock time already does for CPU-bound workflows, which
// is why it is the primary, always-on bound and fuel is an optional,
// opt-in secondary one.
func WithWasmtimeInstructionLimit(n uint64) WasmtimeOption {
	return func(c *wasmtimeConfig) { c.limits.instructionLimit = n }
}

// WithWasmtimeMemoryLimits bounds linear memory, table elements, and
// instance counts per store (wasmtime's StoreLimits / ResourceLimiter).
// Values <= 0 keep the backend's built-in default for that dimension
// (see the Default* constants above).
func WithWasmtimeMemoryLimits(memoryBytes, tableElements, instances int64) WasmtimeOption {
	return func(c *wasmtimeConfig) {
		c.limits.memoryLimitBytes = memoryBytes
		c.limits.tableElementsLimit = tableElements
		c.limits.instancesLimit = instances
	}
}

// WithWasmtimeDeferBudget bounds the cleanup pass the host runs on a killed
// guest. d <= 0 keeps DefaultWasmtimeDeferBudget.
//
// Raise it for workflows whose cleanup genuinely needs longer -- a defer that
// makes several slow external calls -- knowing that the worst case a runaway
// workflow can occupy a worker is the execution timeout plus this.
func WithWasmtimeDeferBudget(d time.Duration) WasmtimeOption {
	return func(c *wasmtimeConfig) { c.limits.deferBudget = d }
}

// WithWasmtimeModuleCacheMaxEntries bounds how many compiled modules the
// backend retains, evicting least-recently-used beyond that.
//
// cleat#1563: this cache had no eviction at all, so a long-lived worker
// retained one compiled module per distinct WASM artifact it had ever run,
// across every tenant, for the life of the process.
//
// ENTRIES, NOT BYTES, and the flag help says so rather than implying a memory
// It bounds map and list overhead independently of artifact size, which is why
// it survives alongside the byte bound rather than being replaced by it: a
// deployment with thousands of tiny artifacts is bounded by this one.
//
// n <= 0 uses DefaultModuleCacheMaxEntries.
func WithWasmtimeModuleCacheMaxEntries(n int) WasmtimeOption {
	return func(c *wasmtimeConfig) {
		c.moduleCacheMaxEntries = n
	}
}

// WithWasmtimeModuleCacheMaxBytes bounds the compiled-module cache by
// ESTIMATED resident size -- CompiledSizeEstimate over each entry's wasm.
//
// This is the bound a deployment can actually size against. The entry count
// could not say whether a hundred modules was 8.6 MB or 4.6 GB; measured on
// this repo's own artifacts, both are true depending on the guest language.
// cleat#1907.
//
// n <= 0 uses DefaultModuleCacheMaxBytes.
func WithWasmtimeModuleCacheMaxBytes(n int64) WasmtimeOption {
	return func(c *wasmtimeConfig) {
		c.moduleCacheMaxBytes = n
	}
}

// DefaultModuleCacheMaxEntries bounds the compiled-module cache when the
// operator sets no limit.
//
// 100 matches --wasm-cache-max-entries, the BYTE cache's default, deliberately:
// the two caches are keyed the same way and hold entries for the same
// artifacts, so a worker that can hold 100 distinct WASM binaries has no reason
// to hold a different number of compiled forms of them.
//
// DECLARED HERE, IN AN UNTAGGED FILE, and that placement is load-bearing.
// moduleLRU lives in a //go:build cgo file because it names *wasmtime.Module,
// but cmd/cleat-worker/config.go reads this constant for its flag default and
// is built in every configuration. Putting it beside the type would break the
// CGO-less build of the whole command -- which `go build ./...` does not catch,
// because CGO is on by default and that is what CLAUDE.md tells you to use.
const DefaultModuleCacheMaxEntries = 100
