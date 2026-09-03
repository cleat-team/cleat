//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cespare/xxhash/v2"

	"github.com/bytecodealliance/wasmtime-go/v44"

	"github.com/cleat-team/cleat/wasm"
)

// epochTickInterval controls how often the background goroutine started by
// NewWasmtimeBackend advances the shared wasmtime engine's epoch (see
// startEpochTicker). Epoch interruption overhead is a fixed, tiny check
// inserted at loop back-edges and function entries regardless of tick
// rate, so a fine-grained tick mostly costs ticker wakeups, not steady
// -state execution overhead; 50ms bounds worst-case timeout overrun (the
// gap between "deadline passed" and "next epoch increment observed") to
// +50ms, which is negligible next to the multi-second timeouts this
// backend is configured with.
const epochTickInterval = 50 * time.Millisecond

// Compile-time check: wasmtimeBackend implements WasmBackend.
var _ WasmBackend = (*wasmtimeBackend)(nil)

// errBadParamInt64 is the int64 equivalent of errBadParam for return values
// from FuncWrap closures (which must return int64, not uint64).
const errBadParamInt64 int64 = -4294967295

// wasmMeta bundles cached per-WASM-binary metadata. All fields are
// computed once from the WASM import section and cached by xxhash key.
type wasmMeta struct {
	envNeeded map[string]bool // "env" imports the module needs
	hasWasi   bool            // imports from wasi_snapshot_preview1
	language  string          // detected source language ("go", "python", etc.)
}

// wasmtimeBackend implements WasmBackend using the wasmtime WASM runtime.
// It loads core WASM modules (post-decompose Component Model binaries).
//
// Build constraint: this file requires CGO because wasmtime-go wraps the
// wasmtime Rust runtime via CGo.
type wasmtimeBackend struct {
	engine  *wasmtime.Engine
	handler HostHandler // current execution session

	// moduleCache holds compiled wasmtime Modules keyed by xxhash of wasmBytes.
	// Shared across PerExecution instances to avoid serialized recompilation.
	moduleCache  *sync.Map
	compileLocks *sync.Map // per-key *sync.Mutex to serialize compilation
	metaCache    *sync.Map // per-key *wasmMeta (envNeeded, hasWasi, language)

	// envNeeded is the set of "env" module imports the WASM module requests.
	// nil means "register everything" (conservative fallback on parse error).
	envNeeded map[string]bool

	// Work data for the Go dispatcher (cleat_poll_work).
	workEntryPoint string
	workInput      []byte

	// limits bounds every store this backend creates: wall-clock time
	// (epoch interruption), optional instruction count (fuel), and linear
	// memory / table / instance ceilings (StoreLimits). See
	// wasmtimeLimits and configureStore. Shared by value across
	// PerExecution copies (copied, not pointed to, so each execution's
	// store sees a consistent snapshot even if this were ever mutated
	// concurrently, which it isn't after construction).
	limits wasmtimeLimits

	// logger receives this backend's own records. nil means slog.Default();
	// see log() and WithWasmtimeLogger for why that default is a trap.
	logger *slog.Logger

	// deferPhase marks this execution as a defer segment: the workflow is
	// being replayed for the sole purpose of running its outstanding defers,
	// not to make progress. See runGuestDefersAfterSuspend.
	//
	// Set per-execution by the engine (executor.go), so it lives on the
	// PerExecution copy rather than the root, and is deliberately NOT copied
	// by PerExecution -- a root backend is never in a defer phase, and
	// inheriting the flag would make every subsequent execution one.
	deferPhase bool

	// epochStop, when non-nil, stops the background epoch-ticker goroutine
	// on Close. Only set on the backend returned directly by
	// NewWasmtimeBackend ("the root") — PerExecution copies share the same
	// underlying *wasmtime.Engine and must not each start their own
	// ticker or race to close it.
	epochStop chan struct{}
	closeOnce sync.Once // guards closing epochStop so Close is idempotent

	// epochDone is closed by the ticker goroutine as it returns. Close must
	// wait on it before calling engine.Close(): signalling epochStop only
	// asks the goroutine to stop, it does not mean it has. A goroutine
	// already inside the ticker branch goes on to call IncrementEpoch on a
	// freed engine, which panics with "object has been closed already".
	// Observed in CI on 2026-08-03 during TestEngineExecute.
	epochDone chan struct{}
}

// NewWasmtimeBackend creates a new wasmtimeBackend with a fresh engine
// configured to bound WASM execution: epoch interruption is always enabled
// (see epochTickInterval / DefaultWasmtimeExecutionTimeout below) so a
// runaway workflow cannot hang the worker permanently, which is the bug
// this backend previously had with a bare wasmtime.NewEngine() and no
// Config at all. Fuel-based instruction metering is enabled additionally
// when WithWasmtimeInstructionLimit(n) is passed with n > 0.
func NewWasmtimeBackend(ctx context.Context, opts ...WasmtimeOption) (*wasmtimeBackend, error) {
	bcfg := wasmtimeConfig{}
	for _, opt := range opts {
		opt(&bcfg)
	}
	lim := bcfg.limits
	if lim.executionTimeout <= 0 {
		lim.executionTimeout = DefaultWasmtimeExecutionTimeout
	}
	if lim.memoryLimitBytes <= 0 {
		lim.memoryLimitBytes = DefaultWasmtimeMemoryLimitBytes
	}
	if lim.tableElementsLimit <= 0 {
		lim.tableElementsLimit = DefaultWasmtimeTableElementsLimit
	}
	if lim.instancesLimit <= 0 {
		lim.instancesLimit = DefaultWasmtimeInstancesLimit
	}

	cfg := wasmtime.NewConfig()
	cfg.SetEpochInterruption(true)
	if lim.instructionLimit > 0 {
		cfg.SetConsumeFuel(true)
	}
	eng := wasmtime.NewEngineWithConfig(cfg)

	b := &wasmtimeBackend{
		engine:       eng,
		moduleCache:  new(sync.Map),
		compileLocks: new(sync.Map),
		metaCache:    new(sync.Map),
		limits:       lim,
		logger:       bcfg.logger,
		epochStop:    make(chan struct{}),
		epochDone:    make(chan struct{}),
	}
	b.startEpochTicker()
	return b, nil
}

// startEpochTicker launches a background goroutine that advances the
// shared wasmtime engine's epoch every epochTickInterval. Engine.IncrementEpoch
// is documented as safe to call from any goroutine. Combined with the
// per-store relative deadline set in configureStore, this turns wall-clock
// time into a hard interrupt of running WASM code — including tight loops
// that never call back into the host — which context cancellation alone
// cannot do for wasmtime (wasmtime-go does not observe ctx.Done() while a
// WASM export call is in progress).
func (b *wasmtimeBackend) startEpochTicker() {
	ticker := time.NewTicker(epochTickInterval)
	stop := b.epochStop
	done := b.epochDone
	eng := b.engine
	go func() {
		defer ticker.Stop()
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				eng.IncrementEpoch()
			}
		}
	}()
}

// log returns the configured logger, or slog.Default() when none was set.
//
// Mirrors Engine.log(). The nil case is deliberately still slog.Default()
// rather than a discard: a backend built without a logger by a caller that
// never had one -- cmd/cleat-worker's verify_backend.go, and every test --
// should still say something.
func (b *wasmtimeBackend) log() *slog.Logger {
	if b.logger != nil {
		return b.logger
	}
	return slog.Default()
}

// Name returns "wasmtime" for diagnostics.
func (b *wasmtimeBackend) Name() string { return "wasmtime" }

// Close releases the wasmtime engine resources, including stopping the
// background epoch ticker started by NewWasmtimeBackend. Only meaningful
// on the root backend (epochStop is nil on PerExecution copies). Idempotent:
// closeOnce guards the channel close so a second Close (already a documented
// requirement — see engine.Close's own doc comment) does not panic.
func (b *wasmtimeBackend) Close(ctx context.Context) error {
	if b.epochStop != nil {
		b.closeOnce.Do(func() { close(b.epochStop) })
		// Wait for the goroutine to actually be gone. Closing epochStop is a
		// request, not an acknowledgement -- without this join, a ticker tick
		// that has already been selected races engine.Close() and panics.
		<-b.epochDone
	}
	b.engine.Close()
	return nil
}

// PerExecution returns a new backend that shares the wasmtime Engine
// but has its own per-execution handler and work data, eliminating
// the data race when Execute is called concurrently. The resource limits
// configured on the root backend are copied so every execution enforces
// the same bounds; epochStop is deliberately left nil (see its doc).
func (b *wasmtimeBackend) PerExecution() WasmBackend {
	return &wasmtimeBackend{
		engine:       b.engine,
		moduleCache:  b.moduleCache,
		compileLocks: b.compileLocks,
		metaCache:    b.metaCache,
		limits:       b.limits,
		// Copied, like limits. A PerExecution copy that dropped the logger
		// would send every record from the path that actually executes
		// workflows to slog.Default(), which is the bug this field exists to
		// fix -- and it would do so while the root backend looked correct.
		logger: b.logger,
	}
}

// configureStore applies this backend's resource bounds to a freshly
// created store, before it is used to instantiate or run any WASM code:
//
//   - Wall-clock timeout via epoch interruption (store.SetEpochDeadline).
//     Prefers ctx's deadline when it is tighter than the backend's
//     configured executionTimeout, so the engine-level timeouts
//     (engine.WithWASMInstanceTimeout, engine.WithDefaultWorkflowTimeout —
//     both wired into ctx by executor.go) still take priority when set
//     tighter than the worker's --wasm-instance-timeout default. Never
//     widens past either: the tighter of the two always wins.
//   - Fuel-based instruction budget (store.SetFuel), only when the backend
//     was constructed with WithWasmtimeInstructionLimit(n > 0) — Config
//     must have SetConsumeFuel(true) called on it beforehand, which
//     NewWasmtimeBackend does whenever a nonzero instruction limit is
//     configured.
//   - Linear memory / table element / instance ceilings (store.Limiter),
//     so a workflow cannot exhaust host memory by growing memory (or
//     tables, or spawning instances) without bound.
//
// It returns the wall-clock timeout it actually applied (after reconciling
// with ctx's deadline) so callers can pass it to resourceLimitError for a
// precise error message.
func (b *wasmtimeBackend) configureStore(ctx context.Context, store *wasmtime.Store) (time.Duration, error) {
	timeout := b.limits.executionTimeout
	if timeout <= 0 {
		timeout = DefaultWasmtimeExecutionTimeout
	}
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			if remaining <= 0 {
				// ctx is already past its deadline; still apply a bound
				// (rather than skip epoch interruption entirely) so the
				// module is interrupted at the next tick instead of
				// running unbounded.
				remaining = time.Millisecond
			}
			timeout = remaining
		}
	}
	ticks := uint64(timeout / epochTickInterval)
	if ticks == 0 {
		ticks = 1
	}
	store.SetEpochDeadline(ticks)

	if b.limits.instructionLimit > 0 {
		if err := store.SetFuel(b.limits.instructionLimit); err != nil {
			return timeout, fmt.Errorf("host: enable wasm instruction budget: %w", err)
		}
	}

	memLimit := int64(-1)
	if b.limits.memoryLimitBytes > 0 {
		memLimit = b.limits.memoryLimitBytes
	}
	tblLimit := int64(-1)
	if b.limits.tableElementsLimit > 0 {
		tblLimit = b.limits.tableElementsLimit
	}
	instLimit := int64(-1)
	if b.limits.instancesLimit > 0 {
		instLimit = b.limits.instancesLimit
	}
	store.Limiter(memLimit, tblLimit, instLimit, -1, -1)
	return timeout, nil
}

// runGuestDefersAfterKill runs the defers of a workflow the host just stopped.
//
// IMPROVEMENT-PLAN 3.35 phase 4. A guest killed by the fence, the instruction
// limit, or an unrecoverable runtime failure never reaches the entry point
// wrapper that normally drains its defer table (3.70), so its cleanup -- the
// released lock, the refunded charge -- simply did not happen. #544 and #548
// measured that the instance is nonetheless still usable and its closures
// intact after all three; this is the call that uses that.
//
// Best-effort by construction. It is called immediately before returning the
// error that says the workflow was killed, and it must not change that error:
// the workflow failed, and it failed for the reason the caller already has.
// A cleanup that itself fails is logged and nothing more.
//
// The budget refresh is not uniform, and the shape of it was measured rather
// than assumed (2026-09-02, probes over testdata/fencereentry):
//
//   - Wall clock is always refreshed. SetEpochDeadline is relative to the
//     current epoch, so without it the call is interrupted immediately.
//   - Fuel is refreshed only when metering is on, and it is REQUIRED there:
//     without SetFuel the runner traps instantly, ran=0. The wall-clock budget
//     above stays the binding bound, so this can be generous.
//   - The memory ceiling is deliberately NOT raised. It does not need to be:
//     the export takes no arguments, so unlike an entry point it needs no
//     scratch buffers, and an OOM-killed guest ran its defer with the ceiling
//     left exactly where it was (ran=1, the defer reached the host). Raising
//     it would hand more memory to a guest that has just proved it cannot be
//     trusted with what it had.
//
// setDeferPhase marks this per-execution backend as running a defer segment.
//
// Unexported and reached through an interface assertion in executor.go rather
// than added to WasmBackend: a defer segment is a wasmtime-path concept today,
// and widening the backend interface for it would oblige every implementation
// to have an opinion about a phase it cannot enter.
func (b *wasmtimeBackend) setDeferPhase(on bool) { b.deferPhase = on }

// runGuestDefersAfterSuspend drains the defer table of a workflow that
// suspended during a defer segment.
//
// IMPROVEMENT-PLAN 3.35 phase 5 / 3.81. A defer segment replays a workflow
// whose terminal outcome is already decided, purely to run its outstanding
// cleanup. The common case -- a workflow worth terminating is usually one that
// is waiting -- is that replay re-suspends on the recorded sleep or await it
// was sitting in, and never reaches the end of history at all.
//
// That suspension is what makes this work, rather than a problem to route
// around. The guest's own drain is gated on `if !__susSuspended`
// (wasm/exports.go, writeRunDeferred), so a suspended guest deliberately does
// NOT drain: the defer table is still populated and the closures are still in
// the instance's memory. The host calls the drain export itself, here, with
// ordinary host-call semantics in force.
//
// Contrast with runGuestDefersAfterKill, which this deliberately mirrors but
// does not share code with. That one runs after a trap and must not disturb
// the error it is about to return; this one runs after a clean suspension,
// where the calls the defer bodies make are the segment's real work and their
// events belong in the history. The budget handling is identical and is
// explained there.
//
// 3.81's measurement is why this is not simply "refuse the fresh call and let
// the guest drain": _cleatRunDeferred takes the whole defer table before
// running anything, so a refusal that also refuses the defer bodies' calls
// consumes the cleanup rather than performing it.
func (b *wasmtimeBackend) runGuestDefersAfterSuspend(
	store *wasmtime.Store, instance *wasmtime.Instance, entryPoint string,
) {
	fn := instance.GetFunc(store, deferRunnerExport)
	if fn == nil {
		return
	}

	budget := b.limits.deferBudget
	if budget <= 0 {
		budget = DefaultWasmtimeDeferBudget
	}
	ticks := uint64(budget / epochTickInterval)
	if ticks == 0 {
		ticks = 1
	}
	store.SetEpochDeadline(ticks)
	if b.limits.instructionLimit > 0 {
		if err := store.SetFuel(b.limits.instructionLimit); err != nil {
			b.log().Warn("could not refuel the guest to run its defers on a defer segment",
				"entry_point", entryPoint, "error", err)
			return
		}
	}

	// Bracket the drain so the defer bodies' own durable calls are permitted
	// while the workflow body's are stopped. Without this the segment refuses
	// the cleanup calls too -- and because _cleatRunDeferred takes the whole
	// defer table before running anything, that CONSUMES the cleanup rather
	// than skipping it: the lock is not released, the charge is not refunded,
	// and the registrations are gone. Measured in IMPROVEMENT-PLAN 3.81.
	//
	// Asserted rather than required: a handler that does not implement it is a
	// backend running without an engine session, which has no calls to stop.
	if d, ok := b.handler.(interface{ setDeferDrain(bool) }); ok {
		d.setDeferDrain(true)
		defer d.setDeferDrain(false)
	}

	var ran int64
	var callErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				callErr = fmt.Errorf("wasmtime panic: %v", r)
			}
		}()
		res, err := fn.Call(store)
		if err != nil {
			callErr = err
			return
		}
		ran, _ = res.(int64)
	}()

	if callErr != nil {
		b.log().Warn("a defer segment's defers could not be run",
			"entry_point", entryPoint, "error", callErr)
		return
	}
	b.log().Info("ran a defer segment's defers",
		"entry_point", entryPoint, "defers_run", ran)
}

func (b *wasmtimeBackend) runGuestDefersAfterKill(
	store *wasmtime.Store, instance *wasmtime.Instance, entryPoint string, cause error,
) {
	fn := instance.GetFunc(store, deferRunnerExport)
	if fn == nil {
		// Not an error worth logging on its own: a guest built before this
		// export existed, or one with no entry points, simply has nothing to
		// drain here.
		return
	}

	budget := b.limits.deferBudget
	if budget <= 0 {
		budget = DefaultWasmtimeDeferBudget
	}
	ticks := uint64(budget / epochTickInterval)
	if ticks == 0 {
		ticks = 1
	}
	store.SetEpochDeadline(ticks)
	if b.limits.instructionLimit > 0 {
		if err := store.SetFuel(b.limits.instructionLimit); err != nil {
			b.log().Warn("could not refuel the guest to run its defers",
				"entry_point", entryPoint, "error", err)
			return
		}
	}

	var ran int64
	var callErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				callErr = fmt.Errorf("wasmtime panic: %v", r)
			}
		}()
		res, err := fn.Call(store)
		if err != nil {
			callErr = err
			return
		}
		ran, _ = res.(int64)
	}()

	switch {
	case callErr != nil:
		b.log().Warn("a killed workflow's defers could not be run",
			"entry_point", entryPoint, "defer_budget", budget,
			"error", callErr, "killed_by", cause)
	case ran > 0:
		b.log().Info("ran the defers of a killed workflow",
			"entry_point", entryPoint, "defers_run", ran, "killed_by", cause)
	}
}

// executionLimitError marks an error as "the host stopped this guest", as
// opposed to "this guest failed".
//
// It exists because that question has to be answerable by a caller that did
// not do the classifying. backend_wasmtime.go's Execute falls back from the
// native component path to decomposition on failure, which is right for a
// guest fault and wrong for an exhausted budget: re-running a runaway guest on
// a second path hands it a second budget, so the effective bound becomes a
// multiple of the configured one. Answering by re-inspecting the error at the
// fallback site would mean duplicating the classification, which is how the
// two copies in IMPROVEMENT-PLAN 2.72 came to disagree.
type executionLimitError struct{ err error }

func (e *executionLimitError) Error() string { return e.err.Error() }
func (e *executionLimitError) Unwrap() error { return e.err }

// isExecutionLimit reports whether err is one the host raised by enforcing a
// bound, at any depth of wrapping.
func isExecutionLimit(err error) bool {
	var limitErr *executionLimitError
	return errors.As(err, &limitErr)
}

// resourceLimitError recognizes traps caused by the resource bounds
// configureStore applies (epoch interruption, fuel exhaustion) and turns
// them into a clear, actionable message naming the limit that was hit and
// its configured value. Returns nil for any other error (including other
// kinds of traps), so the caller should fall back to its normal error
// wrapping in that case.
//
// Two detection routes, because the two execution paths report the same trap
// differently:
//
//   - Core modules and decomposition go through wasmtime-go, which surfaces a
//     *wasmtime.Trap carrying a machine-readable code. Preferred wherever it
//     is available.
//   - The native component path goes through the Component Model C API, whose
//     wasmtime_error_t exposes only a rendered message, an exit status and a
//     wasm trace -- there is no trap-code accessor on it (see
//     wasmtimeinc/wasmtime/error.h). So that path is matched on wasmtime's own
//     rendering of the two trap codes.
//
// The string match is deliberately anchored on the full canonical phrases
// rather than a fragment like "fuel" or "interrupt", which is the distinction
// 2.26 was about: matching a short token inside a longer message is how the
// SQL Server classifier matched error numbers as substrings.
func (b *wasmtimeBackend) resourceLimitError(err error, timeout time.Duration) error {
	if err == nil {
		return nil
	}
	timeLimit := func() error {
		return &executionLimitError{fmt.Errorf("execution time limit exceeded (%s wall-clock budget; configure with --wasm-instance-timeout): %w", timeout, err)}
	}
	fuelLimit := func() error {
		return &executionLimitError{fmt.Errorf("instruction limit exceeded (%d fuel units; configure with --wasm-instruction-limit): %w", b.limits.instructionLimit, err)}
	}

	var trap *wasmtime.Trap
	if errors.As(err, &trap) {
		if code := trap.Code(); code != nil {
			switch *code {
			case wasmtime.Interrupt:
				return timeLimit()
			case wasmtime.OutOfFuel:
				return fuelLimit()
			}
		}
		return nil
	}

	switch msg := err.Error(); {
	case strings.Contains(msg, wasmtimeInterruptTrapText):
		return timeLimit()
	case strings.Contains(msg, wasmtimeOutOfFuelTrapText):
		return fuelLimit()
	}
	return nil
}

// wasmtime's rendering of the two trap codes configureStore can produce. These
// are the strings wasmtime_error_message writes for WASMTIME_TRAP_CODE_INTERRUPT
// and WASMTIME_TRAP_CODE_OUT_OF_FUEL; they are asserted against a real
// interrupt in TestComponentPathResourceLimitClassification rather than taken
// on trust, because they are upstream's wording and not part of any API
// contract.
const (
	wasmtimeInterruptTrapText = "wasm trap: interrupt"
	wasmtimeOutOfFuelTrapText = "all fuel consumed by WebAssembly"
)

// Execute compiles, instantiates, and runs a core WASM module via wasmtime.
//
// The session provides the HostHandler for all host function calls. The
// wasmtime backend registers flat "env" module imports matching the names
// produced by component decomposition (e.g. cleat_call, cleat_sleep).
//
// Like the wazero backend, it reserves scratch space in linear memory at a
// high offset (10 MB) for input/output buffers and uses the same encoding
// conventions for export return values.
func (b *wasmtimeBackend) Execute(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, session HostHandler) (*ExecResult, error) {
	t0 := time.Now()

	// Create per-execution store with WASI configuration.
	store := wasmtime.NewStore(b.engine)
	defer store.Close()
	execTimeout, err := b.configureStore(ctx, store)
	if err != nil {
		return nil, err
	}
	t1 := time.Now()

	// Configure WASI for Go wasip1 module support.
	// The module may need WASI for stack/goroutine management even though we
	// override time/random functions for determinism.
	// Skip for modules that don't import from WASI (AS, Rust cdylib, TeaVM)
	// to avoid wasmtime-go v44 nil pointer dereference during fn.Call.
	// Look up or populate cached per-WASM metadata (envNeeded, hasWasi, language).
	wHash := xxhash.Sum64(wasmBytes)
	wKey := strconv.FormatUint(wHash, 16)
	var meta *wasmMeta
	if cached, ok := b.metaCache.Load(wKey); ok {
		meta = cached.(*wasmMeta)
	} else {
		meta = &wasmMeta{
			envNeeded: wasm.NeededEnvImports(wasmBytes),
			hasWasi:   wasm.HasWasiImports(wasmBytes),
			language:  wasm.DetectLanguage(wasmBytes),
		}
		b.metaCache.Store(wKey, meta)
	}
	b.envNeeded = meta.envNeeded
	needsWasi := meta.hasWasi
	if needsWasi {
		wasiConfig := wasmtime.NewWasiConfig()
		wasiConfig.InheritStderr()
		store.SetWasi(wasiConfig)
	}

	// Wrap context so host functions can find the session.
	ctx = withHandler(ctx, session)
	b.handler = session

	// Detect Component Model binaries and dispatch to the component execution path.
	if isComponentWasm(wasmBytes) {
		// The native Component Model path is the only one. There used to be a
		// hand-rolled decomposition fallback here -- ~620 lines of
		// shared-everything dynamic linking, GOT.mem/GOT.func routing,
		// placeholder tables, an "instance with the most exports is the CPython
		// runtime" heuristic, and a multi-pass instantiation loop with an
		// `undefined element` retry. It never once executed a workflow.
		//
		// Measured 2026-09-01 against the only Component Model binary in the
		// repo, a 19.3 MB componentize-py build, with the native path as the
		// control:
		//
		//	native (this path)        reached CPython, ran guest code, and
		//	                          returned the guest's own type error
		//	wasmtime decomposition    failed at instance 81 of 85:
		//	                          "incompatible import type for env::cleat_call"
		//	wazero decomposition      failed at instance 8:
		//	                          "memory is not exported in module env"
		//
		// tiers.yaml already parked decomposition at tier 3 -- "not built, not
		// shipped, not claimed". See IMPROVEMENT-PLAN 3.65.
		//
		// The consequence for the caller is that a native-path failure is now
		// the answer rather than a prelude to a second, worse error. That is
		// the improvement, not a regression: the fallback's failure was what
		// callers actually saw, and it described decomposition's problems with
		// the module rather than the real cause.
		return b.ExecuteComponentCGo(ctx, wasmBytes, entryPoint, []byte(input), OutBufSize)
	}

	// Compile the WASM module (cached by xxhash key, computed above).
	compileStart := time.Now()

	var module *wasmtime.Module

	// Fast path: module already cached.
	if cached, ok := b.moduleCache.Load(wKey); ok {
		module = cached.(*wasmtime.Module)
		if DebugTiming {
			fmt.Fprintf(os.Stderr, "TIMING: wasmtime compile CACHE HIT elapsed=%dms\n", time.Since(compileStart).Milliseconds())
		}
	} else {
		// Slow path: serialize compilation per unique WASM binary.
		muI, _ := b.compileLocks.LoadOrStore(wKey, new(sync.Mutex))
		mu := muI.(*sync.Mutex)
		mu.Lock()
		// Double-check: another goroutine may have compiled while we waited.
		if cached, ok := b.moduleCache.Load(wKey); ok {
			module = cached.(*wasmtime.Module)
			if DebugTiming {
				fmt.Fprintf(os.Stderr, "TIMING: wasmtime compile WAIT THEN HIT elapsed=%dms\n", time.Since(compileStart).Milliseconds())
			}
		} else {
			var err error
			module, err = wasmtime.NewModule(b.engine, wasmBytes)
			if DebugTiming {
				fmt.Fprintf(os.Stderr, "TIMING: wasmtime compile CACHE MISS elapsed=%dms\n", time.Since(compileStart).Milliseconds())
			}
			if err != nil {
				mu.Unlock()
				return nil, fmt.Errorf("host: compile: %w", err)
			}
			b.moduleCache.Store(wKey, module)
		}
		mu.Unlock()
	}
	// Do NOT close the module — it's cached and shared.

	// Create linker and register host functions.
	linker := wasmtime.NewLinker(b.engine)

	// Register cleat_* host functions. We use a closure-based approach:
	// each function captures a result/error holder so that cleat_complete
	// can store the workflow result and the Execute method can retrieve
	// it even when the module subsequently traps (e.g. via proc_exit).
	var completeResult, completeErr string
	if err := b.registerAllImports(linker, &completeResult, &completeErr, needsWasi, abortImportType(module)); err != nil {
		return nil, fmt.Errorf("host: register imports: %w", err)
	}

	t2 := time.Now()

	// Instantiate the module.
	instance, err := linker.Instantiate(store, module)
	if err != nil {
		return nil, fmt.Errorf("host: instantiate: %w", err)
	}
	t3 := time.Now()

	// Get exported memory.
	memory := instance.GetExport(store, "memory")
	if memory == nil {
		return nil, fmt.Errorf("host: module has no exported memory")
	}
	mem := memory.Memory()
	if mem == nil {
		return nil, fmt.Errorf("host: memory export is not a memory")
	}

	lang := meta.language

	// If the module exports _start, call it first. Go wasip1 modules use
	// the cleat_poll_work dispatcher protocol to route work. Java/TeaVM
	// modules need _start for runtime init (shadow stack, fiber state)
	// before the entry point can be called directly.
	if lang == "go" {
		if startFn := instance.GetFunc(store, "_start"); startFn != nil {
			b.workEntryPoint = entryPoint
			// Wrap in DispatchWrapper format that gen_wasm_exports.go expects:
			// {"inputJSON":"<escaped inner JSON>"}
			escaped, _ := json.Marshal(string(input))
			b.workInput = []byte(fmt.Sprintf(`{"inputJSON":%s}`, string(escaped)))

			// Write work data to a fixed WASM memory location (offset 1024)
			// so main() can read it without calling cleat_poll_work.
			// This works around a Go 1.25 pointer-passing bug for modules
			// with specific import counts.
			b.writeWorkToFixedMemory(mem, store, entryPoint, []byte(input))

			var startErr error
			func() {
				defer func() {
					if r := recover(); r != nil {
						if completeResult == "" && completeErr == "" {
							completeErr = fmt.Sprintf("wasm _start panic: %v", r)
						}
					}
				}()
				_, startErr = startFn.Call(store)
			}()

			if completeResult == `"__cleat_suspended__"` {
				if b.deferPhase {
					b.runGuestDefersAfterSuspend(store, instance, entryPoint)
				}
				return &ExecResult{Suspended: true}, nil
			}

			// completeErr is checked before completeResult, and returned as an
			// error rather than as a result.
			//
			// A failing Go guest reports itself twice. cleatDispatch's error
			// branch calls cleat_complete(1, err) (wasm/exports.go) and *then*
			// returns the same error as its []byte result; the generated
			// main() re-reports that return value with cleat_complete(0, ...)
			// unconditionally, having no way to tell that the dispatch failed
			// (wasm/build.go). Both reports arrive here, in the two variables
			// registerCleatComplete correctly keeps them apart in -- and
			// preferring completeResult threw the status bit away. Every Go
			// workflow that returned an error was handed back as a success, so
			// the worker took the success path and stored status='done' with
			// the error text sitting in the result column.
			//
			// The order is not a new convention: it is what the two paths that
			// already get this right do. The direct-export branch below (every
			// non-Go guest) and the wazero backend (runtime.go) both surface
			// completeErr as a Go error. This branch -- Go on wasmtime, the
			// primary language on the primary backend -- was the one that did
			// not.
			//
			// Traps were never affected: fuel and epoch exhaustion reach the
			// resource-limit check below, because a trapped guest never gets to
			// call cleat_complete at all. It was specifically the guest that
			// stopped cleanly and *said* it had failed that was not believed.
			//
			// See IMPROVEMENT-PLAN.md 3.22.
			if completeErr != "" {
				// Marked, not just formatted: the guest stopped cleanly and
				// said it had failed. Without the marker the executor cannot
				// tell this from a trap and labels it one (3.23).
				return nil, &GuestReturnedError{
					Err: fmt.Errorf("host: export %q failed: %s", entryPoint, guestErrorText(completeErr)),
				}
			}
			if completeResult != "" {
				return &ExecResult{Result: completeResult, Suspended: false}, nil
			}
			// Neither cleat_complete outcome was recorded. Normally that
			// means the module hasn't reached cleat_complete yet but is
			// about to exit cleanly (Go's wasip1 runtime traps via
			// proc_exit when main() returns, which surfaces as a non-nil
			// startErr even on success — the historical reason this
			// branch used to ignore startErr entirely and assume "ok").
			// But if startErr is a resource-limit trap (epoch
			// interruption / fuel exhaustion from configureStore above),
			// the module was killed mid-execution — most likely stuck in
			// an infinite loop — and never got the chance to call
			// cleat_complete. That must surface as a real error, not a
			// silent "ok".
			if startErr != nil {
				if limitErr := b.resourceLimitError(startErr, execTimeout); limitErr != nil {
					b.runGuestDefersAfterKill(store, instance, entryPoint, limitErr)
					return nil, fmt.Errorf("host: export %q: %w", entryPoint, limitErr)
				}
				// A NON-ZERO WASI exit is the guest dying, not finishing.
				//
				// The check above only recognises the two resource-limit trap
				// codes. Everything else reaching here used to fall through to
				// the `"ok"` below, and the case that matters is not
				// hypothetical: when the Go runtime cannot grow the heap past
				// the configured memory limit it does not trap at all. It
				// prints a goroutine dump and calls proc_exit(2) from its fatal
				// path. That is not a *wasmtime.Trap, so resourceLimitError
				// returns nil, so an out-of-memory workflow was returned as
				// Result: `"ok"` with a nil error — and the worker stored
				// status='done'. Every step after the allocation silently never
				// happened, with no error text anywhere to find it by.
				//
				// Measured 2026-09-02 through Engine.Execute against
				// testdata/fencereentry's allocate_forever under a 64 MB limit:
				// result="ok" err=<nil>. See IMPROVEMENT-PLAN §3.71.
				//
				// Only non-zero is a failure. proc_exit(0) is how EVERY healthy
				// Go guest leaves — main() returns and the wasip1 runtime exits
				// — which is the whole reason startErr was ignored here.
				//
				// Non-Go guests never had this hole: the direct-export path
				// below returns its callErr unconditionally.
				//
				// Deliberately NOT widened to "any startErr is a failure". A
				// non-resource trap that is not a proc_exit still falls through
				// to `"ok"`, which is the same shape of hole. It is left alone
				// because nothing has demonstrated a Go guest reaching it: Go
				// recovers panics into cleat_complete, and its unrecoverable
				// failures leave through proc_exit, which the check above now
				// catches. Widening on the strength of an argument rather than
				// a measurement is how the exit-0 path -- which every healthy
				// guest depends on -- would get broken.
				var wasmErr *wasmtime.Error
				if errors.As(startErr, &wasmErr) {
					if code, ok := wasmErr.ExitStatus(); ok && code != 0 {
						b.runGuestDefersAfterKill(store, instance, entryPoint, startErr)
						return nil, fmt.Errorf(
							"host: export %q: the guest exited with status %d without "+
								"reporting a result; it was killed rather than finishing "+
								"(a Go guest exits this way on an unrecoverable runtime "+
								"failure such as out-of-memory or stack exhaustion): %w",
							entryPoint, code, startErr)
					}
				}
			}
			return &ExecResult{Result: `"ok"`, Suspended: false}, nil
		}
	} else if lang == "java" {
		// TeaVM modules need _start to initialize the runtime (shadow
		// stack, fiber system, thread-local globals) before any export
		// can be called. Call it synchronously and wait for return.
		if startFn := instance.GetFunc(store, "_start"); startFn != nil {
			if _, err := startFn.Call(store); err != nil {
				if limitErr := b.resourceLimitError(err, execTimeout); limitErr != nil {
					err = limitErr
				}
				return nil, fmt.Errorf("host: teaVM _start failed: %w", err)
			}
		}
	}

	// Set up scratch buffers for the direct export call (non-Go path).
	outBufSz := OutBufSize // 1 MB default, configurable

	currentSize := uint64(mem.DataSize(store))
	scratchBase, scratchErr := scratchBaseFor(currentSize, outBufSz)
	if scratchErr != nil {
		return nil, scratchErr
	}
	inputOffset := scratchBase
	outputOffset := scratchBase + outBufSz

	needed := uint64(outputOffset + outBufSz)
	if currentSize < needed {
		pagesNeeded := (needed - currentSize + wasmPageSize - 1) / wasmPageSize
		if _, err := mem.Grow(store, pagesNeeded); err != nil {
			return nil, fmt.Errorf("host: grow memory: exceeded configured wasm memory limit (%d bytes; configure with --wasm-memory-max-mb): %w", b.limits.memoryLimitBytes, err)
		}
	}

	inputBytes := []byte(input)
	if len(inputBytes) > 0 {
		data := mem.UnsafeData(store)
		if uint64(inputOffset)+uint64(len(inputBytes)) > uint64(len(data)) {
			return nil, fmt.Errorf("host: input exceeds memory bounds")
		}
		copy(data[inputOffset:], inputBytes)
	}

	// Call the export directly (non-Go modules, or Go modules without _start).
	fn := instance.GetFunc(store, entryPoint)
	if fn == nil {
		return nil, fmt.Errorf("host: export %q not found: %w", entryPoint, ErrExportNotFound)
	}

	t4 := time.Now()

	// Call the entry point. The return value is a single i64 encoding
	// the error code (low 32 bits) and output length (high 32 bits).
	// Wrap in recover to handle wasmtime-go internal panics (e.g., from
	// modules with unexpected import/export configurations).
	var results any
	var callErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				callErr = fmt.Errorf("host: wasmtime panic in %q: %v", entryPoint, r)
			}
		}()
		results, callErr = fn.Call(store, int32(inputOffset), int32(len(inputBytes)), int32(outputOffset), int32(outBufSz))
	}()

	callElapsed := time.Since(t4)
	if DebugTiming {
		fmt.Fprintf(os.Stderr, "TIMING-WASMTIME: lang=%s preStart=%d call=%d total=%d ms (store=%d compileLink=%d instantiate=%d)\n",
			meta.language,
			t3.Sub(t0).Milliseconds(),
			callElapsed.Milliseconds(),
			time.Since(t0).Milliseconds(),
			t1.Sub(t0).Milliseconds(),
			t2.Sub(t1).Milliseconds(),
			t3.Sub(t2).Milliseconds())
	}

	// Check for a result delivered via cleat_complete before treating
	// a trap/proc_exit as an error.
	if completeErr != "" {
		// Same marking as the Go-on-wasmtime branch above; this is the
		// direct-export path taken by every non-Go guest.
		return nil, &GuestReturnedError{
			Err: fmt.Errorf("host: export %q failed: %s", entryPoint, completeErr),
		}
	}
	if completeResult == `"__cleat_suspended__"` {
		if b.deferPhase {
			b.runGuestDefersAfterSuspend(store, instance, entryPoint)
		}
		return &ExecResult{Suspended: true}, nil
	}

	if completeResult != "" {
		return &ExecResult{Result: completeResult, Suspended: false}, nil
	}

	if callErr != nil {
		if limitErr := b.resourceLimitError(callErr, execTimeout); limitErr != nil {
			callErr = limitErr
		}
		// Run the guest's outstanding defers before giving up on it.
		// IMPROVEMENT-PLAN §3.35 phase 4.
		//
		// This is the non-Go path -- Rust, Java, AssemblyScript, and any Go
		// module without _start -- and until this call it had no defer pass at
		// all. The Go-on-wasmtime branch above got one in #550; #553, #557 and
		// #558 then gave Rust, AssemblyScript and Java a __cleat_run_deferred
		// export for the host to call, and nothing called it. "The guest
		// exports it" and "the host calls it" are two different facts.
		//
		// Measured 2026-09-02 before the fix, AssemblyScript spin_forever under
		// a 2s fence: the workflow was killed, its defer did not run, and the
		// engine's fallback pass logged `defer execution failed ...
		// export=cleat_defer_defer-0 ... not found` -- a message about an
		// export naming convention no guest in any language has ever had, for
		// cleanup that simply never happened.
		//
		// Only reached when the guest did NOT come out through its own wrapper:
		// the completeErr and completeResult branches above return first, and
		// a guest that reached either has already drained its own defer table
		// (§3.73). This call is idempotent regardless -- every SDK's runner
		// drains the table before running the first body, so a second call
		// runs nothing and returns 0.
		b.runGuestDefersAfterKill(store, instance, entryPoint, callErr)
		return nil, fmt.Errorf("host: export %q: %w", entryPoint, callErr)
	}

	if results == nil {
		return nil, fmt.Errorf("host: export %q returned no results", entryPoint)
	}

	// Decode the packed int64 result.
	raw, ok := results.(int64)
	if !ok {
		return nil, fmt.Errorf("host: export %q returned non-int64 result", entryPoint)
	}

	// Check for the suspend sentinel: (1 << 62).
	if raw == (1 << 62) {
		if b.deferPhase {
			b.runGuestDefersAfterSuspend(store, instance, entryPoint)
		}
		return &ExecResult{Suspended: true}, nil
	}

	errCode, actualLen := decodeExportResult(uint64(raw))

	// Read output from linear memory.
	data := mem.UnsafeData(store)
	if actualLen > outBufSz {
		return nil, fmt.Errorf("host: export %q: output overflow: wrote %d bytes, buffer is %d bytes", entryPoint, actualLen, outBufSz)
	}
	outputStr := string(data[outputOffset : outputOffset+actualLen])

	if errCode != 0 {
		return nil, fmt.Errorf("host: export %q: %s", entryPoint, outputStr)
	}

	return &ExecResult{Result: outputStr, Suspended: false}, nil
}

// ---------------------------------------------------------------------------
// Shared-memory dispatcher protocol for Go WASM modules
// ---------------------------------------------------------------------------

// dispatcher memory layout (matches gen_main_stub.go for --target go):
//
//	Offset  Size   Field
//	0       1      command byte (0=idle, 1=execute, 2=done, 3=error)
//	1       4      entry point name length (uint32 LE)
//	5       4      input JSON length (uint32 LE)
//	9       4      output JSON length (uint32 LE)
//	13      256    entry point name buffer
//	269     65536  input JSON buffer
//	65837   65536  output JSON buffer
const (
	_dispatcherBase      = 10 * 1024 * 1024 // 10 MiB
	_dispatcherCmd       = _dispatcherBase + 0
	_dispatcherNameLen   = _dispatcherBase + 1
	_dispatcherInputLen  = _dispatcherBase + 5
	_dispatcherOutputLen = _dispatcherBase + 9
	_dispatcherNameBuf   = _dispatcherBase + 13
	_dispatcherInputBuf  = _dispatcherBase + 269
	_dispatcherOutputBuf = _dispatcherBase + 65837
	_dispatcherNameMax   = 256
	_dispatcherInputMax  = 65536
	_dispatcherOutputMax = 65536
	_dispatcherInterval  = 10 * time.Millisecond
	_dispatcherTimeout   = 30 * time.Second
)

// registerAllImports registers all host function imports on the given linker.
// Extracted so the core-module and native-component paths share the same setup.
func (b *wasmtimeBackend) registerAllImports(linker *wasmtime.Linker, completeResult, completeErr *string, needsWasi bool, abortTy *wasmtime.FuncType) error {
	if needsWasi {
		if err := b.registerWasiStubs(linker); err != nil {
			return err
		}
	}
	if err := b.registerEnvStubs(linker, abortTy); err != nil {
		return err
	}
	if err := b.registerTeavmStubs(linker); err != nil {
		return err
	}
	if err := b.registerCleatCall(linker, completeResult, completeErr); err != nil {
		return err
	}
	if err := b.registerCleatSleep(linker); err != nil {
		return err
	}
	if err := b.registerCleatNow(linker); err != nil {
		return err
	}
	if err := b.registerCleatRandom(linker); err != nil {
		return err
	}
	if err := b.registerCleatLog(linker); err != nil {
		return err
	}
	if err := b.registerCleatVersion(linker); err != nil {
		return err
	}
	if err := b.registerCleatMinVersion(linker); err != nil {
		return err
	}
	if err := b.registerCleatDefer(linker); err != nil {
		return err
	}
	if err := b.registerCleatPollCancellation(linker); err != nil {
		return err
	}
	if err := b.registerCleatPollSignal(linker); err != nil {
		return err
	}
	if err := b.registerCleatContinueAsNew(linker); err != nil {
		return err
	}
	if err := b.registerCleatContinueAsNewVersioned(linker); err != nil {
		return err
	}
	if err := b.registerCleatChildWorkflow(linker); err != nil {
		return err
	}
	if err := b.registerCleatChildWorkflowWithOptions(linker); err != nil {
		return err
	}
	if err := b.registerCleatAwaitChild(linker); err != nil {
		return err
	}
	if err := b.registerCleatAwaitAllChildren(linker); err != nil {
		return err
	}
	if err := b.registerCleatPollChild(linker); err != nil {
		return err
	}
	if err := b.registerCleatAwaitAnyChild(linker); err != nil {
		return err
	}
	if err := b.registerCleatCallRetry(linker); err != nil {
		return err
	}
	if err := b.registerCleatAwaitSignals(linker); err != nil {
		return err
	}
	if err := b.registerCleatSetQueryState(linker); err != nil {
		return err
	}
	if err := b.registerCleatCallHeartbeat(linker); err != nil {
		return err
	}
	if err := b.registerCleatPluginCall(linker); err != nil {
		return err
	}
	if err := b.registerCleatPluginCallStreaming(linker); err != nil {
		return err
	}
	if err := b.registerCleatRegisterUpdateHandler(linker); err != nil {
		return err
	}
	if err := b.registerCleatCreatePromise(linker); err != nil {
		return err
	}
	if err := b.registerCleatAwaitPromise(linker); err != nil {
		return err
	}
	if err := b.registerCleatSendSignalAndWait(linker); err != nil {
		return err
	}
	if err := b.registerCleatReplyToSignal(linker); err != nil {
		return err
	}
	if err := b.registerCleatSignalWorkflow(linker); err != nil {
		return err
	}
	if err := b.registerCleatSetScope(linker); err != nil {
		return err
	}
	if err := b.registerCleatGetScope(linker); err != nil {
		return err
	}
	if err := b.registerCleatUUID(linker); err != nil {
		return err
	}
	if err := b.registerCleatAcquireLock(linker); err != nil {
		return err
	}
	if err := b.registerCleatReleaseLock(linker); err != nil {
		return err
	}
	if err := b.registerCleatSideEffect(linker); err != nil {
		return err
	}
	if err := b.registerCleatWorkflowID(linker); err != nil {
		return err
	}
	if err := b.registerCleatRunID(linker); err != nil {
		return err
	}
	if err := b.registerCleatResolvePromise(linker); err != nil {
		return err
	}
	if err := b.registerCleatRejectPromise(linker); err != nil {
		return err
	}
	if err := b.registerCleatSend(linker); err != nil {
		return err
	}
	if err := b.registerCleatScheduleInvoke(linker); err != nil {
		return err
	}
	if err := b.registerCleatRegisterQueryHandler(linker); err != nil {
		return err
	}
	if err := b.registerCleatRunDetached(linker); err != nil {
		return err
	}
	if err := b.registerCleatSetState(linker); err != nil {
		return err
	}
	if err := b.registerCleatGetState(linker); err != nil {
		return err
	}
	if err := b.registerCleatDeleteState(linker); err != nil {
		return err
	}
	if err := b.registerCleatIncrState(linker); err != nil {
		return err
	}
	if err := b.registerCleatHasState(linker); err != nil {
		return err
	}
	if err := b.registerCleatListState(linker); err != nil {
		return err
	}
	if err := b.registerCleatFetch(linker); err != nil {
		return err
	}
	if err := b.registerCleatScheduleCron(linker); err != nil {
		return err
	}
	if err := b.registerCleatDeleteCron(linker); err != nil {
		return err
	}
	if err := b.registerCleatListCrons(linker); err != nil {
		return err
	}
	if err := b.registerCleatJsonParse(linker); err != nil {
		return err
	}
	if err := b.registerCleatJsonStringify(linker); err != nil {
		return err
	}
	if err := b.registerCleatComplete(linker, completeResult, completeErr); err != nil {
		return err
	}
	if err := b.registerCleatPollWork(linker); err != nil {
		return err
	}
	return nil
}

// writeWorkToFixedMemory writes the entry point name and input JSON to a
// fixed location in WASM linear memory (offset 1024). Go WASM modules built
// with the fixed-memory main stub read from this location instead of calling
// cleat_poll_work. This avoids a Go 1.25 compiler bug where unsafe.Pointer
// parameters passed through //go:wasmimport can carry garbage values for
// modules with specific import table configurations.
const fixedWorkOffset = 1024
const fixedWorkMaxEntry = 256
const fixedWorkMaxInput = 65536

func (b *wasmtimeBackend) writeWorkToFixedMemory(mem *wasmtime.Memory, store wasmtime.Storelike, entryPoint string, input []byte) {
	data := mem.UnsafeData(store)
	if len(data) < fixedWorkOffset+8 {
		return
	}
	entryLen := len(entryPoint)
	if entryLen > fixedWorkMaxEntry {
		entryLen = fixedWorkMaxEntry
	}
	inputLen := len(input)
	if inputLen > fixedWorkMaxInput {
		inputLen = fixedWorkMaxInput
	}

	// Layout: [entryLen:4][inputLen:4][entryBytes...][inputBytes...]
	putU32LE(data[fixedWorkOffset:fixedWorkOffset+4], uint32(entryLen))
	putU32LE(data[fixedWorkOffset+4:fixedWorkOffset+8], uint32(inputLen))
	if entryLen > 0 {
		copy(data[fixedWorkOffset+8:fixedWorkOffset+8+entryLen], entryPoint[:entryLen])
	}
	if inputLen > 0 {
		copy(data[fixedWorkOffset+8+entryLen:fixedWorkOffset+8+entryLen+inputLen], input[:inputLen])
	}
}
