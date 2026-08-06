//go:build cgo

package engine

import (
	"context"
	"encoding/binary"
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

	// witDylib holds the wit_dylib stack machine state for component
	// model adapter ABI (push/pop/export_call). Initialized per
	// ExecuteComponent call.
	witDylib *witDylibState

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
	lim := wasmtimeLimits{}
	for _, opt := range opts {
		opt(&lim)
	}
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
		// Try native component model via CGo first.
		result, cgoErr := b.ExecuteComponentCGo(ctx, wasmBytes, entryPoint, []byte(input), OutBufSize)
		if cgoErr == nil {
			return result, nil
		}
		// A guest that exhausted its execution budget must not be handed a
		// second one. Falling through to decomposition here did exactly that:
		// a component with a 2s budget was interrupted on the native path and
		// then started again from scratch on the decomposition path, so the
		// effective bound was a multiple of the configured one -- and the
		// error the caller finally saw was decomposition's, which reads as a
		// guest defect rather than the host stopping a runaway.
		//
		// Only for limit traps. Every other native-path failure is still a
		// reason to try decomposition, which is the whole point of having it.
		if isExecutionLimit(cgoErr) {
			return nil, cgoErr
		}
		// Say why the native path was not taken. This used to be
		// `if result, err := ...; err == nil`, discarding the error entirely,
		// so a native-path failure surfaced only as whatever the fallback below
		// happened to report -- typically an unresolved-import error from
		// decomposition, which reads like "wasmtime cannot run this component"
		// when the real cause was something else.
		//
		// The example that mattered: until 2026-08-05 the native path was
		// compiled out entirely unless the wasmtime_component_cgo tag was set,
		// and no build set it. Every component therefore reached decomposition,
		// and decomposition's failure was read for months as wasmtime's verdict
		// on Component Model guests. It was not; the native path runs them.
		// The remaining known limit is real though: the export lookup resolves
		// only top-level names (componentGetFunc passes a nil parent export
		// index), so a component exporting through an interface instance still
		// reports its export as missing and lands here.
		//
		// Logged rather than returned: the fallback may still succeed, and
		// turning a recoverable miss into a hard failure would change
		// behaviour. The point is only that the reason stops vanishing.
		slog.DebugContext(ctx, "wasmtime native component path unavailable, falling back to decomposition",
			"entry_point", entryPoint, "error", cgoErr)
		// Fall back to manual decomposition + instantiation.
		bundle, bundleErr := wasm.ParseComponentBundle(wasmBytes)
		if bundleErr != nil {
			return nil, fmt.Errorf("host: parse component bundle (native component path first failed: %v): %w", cgoErr, bundleErr)
		}
		res, fallbackErr := b.ExecuteComponent(ctx, wasmBytes, bundle, entryPoint, input, session)
		if fallbackErr != nil {
			return nil, fmt.Errorf("%w (native component path first failed: %v)", fallbackErr, cgoErr)
		}
		return res, nil
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
				return nil, fmt.Errorf("host: export %q failed: %s", entryPoint, guestErrorText(completeErr))
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
					return nil, fmt.Errorf("host: export %q: %w", entryPoint, limitErr)
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
		return nil, fmt.Errorf("host: export %q not found", entryPoint)
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
		return nil, fmt.Errorf("host: export %q failed: %s", entryPoint, completeErr)
	}
	if completeResult == `"__cleat_suspended__"` {
		return &ExecResult{Suspended: true}, nil
	}

	if completeResult != "" {
		return &ExecResult{Result: completeResult, Suspended: false}, nil
	}

	if callErr != nil {
		if limitErr := b.resourceLimitError(callErr, execTimeout); limitErr != nil {
			callErr = limitErr
		}
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

func (b *wasmtimeBackend) ExecuteComponent(ctx context.Context, wasmBytes []byte, bundle *wasm.ComponentBundle, entryPoint string, input json.RawMessage, session HostHandler) (*ExecResult, error) {
	const componentAdapterModule = "__component_adapter__"

	// ---- Step 1: Compile all core modules ----
	compiled := make([]*wasmtime.Module, len(bundle.Modules))
	for i, modBytes := range bundle.Modules {
		patched := wasm.PatchEmptyImportModuleName(modBytes, componentAdapterModule)
		if rewritten, rwErr := wasm.RewriteWitImports(patched); rwErr == nil && rewritten != nil {
			patched = rewritten
		}
		m, err := wasmtime.NewModule(b.engine, patched)
		if err != nil {
			return nil, fmt.Errorf("host: compile core module %d: %w", i, err)
		}
		compiled[i] = m
		defer m.Close()
	}

	// ---- Step 2: Create store with WASI ----
	store := wasmtime.NewStore(b.engine)
	defer store.Close()
	execTimeout, err := b.configureStore(ctx, store)
	if err != nil {
		return nil, err
	}
	wasiConfig := wasmtime.NewWasiConfig()
	wasiConfig.InheritStderr()
	store.SetWasi(wasiConfig)

	// ---- Step 3: Walk instance DAG ----
	instances := make([]*wasmtime.Instance, len(bundle.Instances))

	// Resolve FromExports chains: walk transitively to find the actual
	// instantiated instance that provides exports for each instance index.
	actualProvider := make([]int, len(bundle.Instances))
	for i := range actualProvider {
		actualProvider[i] = i
	}
	// Iterate to closure: follow FromExports chains until we reach an
	// instantiated instance or hit a fixed point.
	for changed := true; changed; {
		changed = false
		for i, inst := range bundle.Instances {
			if inst.ModuleIndex >= 0 {
				continue // has its own module, no need to resolve further
			}
			// Try to resolve through any FromExports entry.
			for _, fe := range inst.FromExports {
				src := fe.SourceInstance
				if src >= 0 && src < len(actualProvider) && actualProvider[src] != i {
					next := actualProvider[src]
					if bundle.Instances[next].ModuleIndex >= 0 && actualProvider[i] != next {
						actualProvider[i] = next
						changed = true
						break
					}
				}
			}
		}
	}

	// Initialize the wit_dylib stack machine for component model ABI.
	b.witDylib = newWitDylibState()

	// Find the CPython runtime instance: the one whose compiled module
	// has the most exports. The component model DAG's FromExports chains
	// for GOT.mem / GOT.func may point to adapter instances that lack
	// the actual CPython symbols; we fall back to this instance for GOT.
	cpythonRuntimeIdx := -1
	maxExports := 0
	for i, inst := range bundle.Instances {
		if inst.ModuleIndex >= 0 && inst.ModuleIndex < len(compiled) {
			if n := len(compiled[inst.ModuleIndex].Exports()); n > maxExports {
				maxExports = n
				cpythonRuntimeIdx = i
			}
		}
	}

	// Multi-pass instantiation: instances are processed in passes.
	// Each pass tries to instantiate any still-pending instance that has
	// a module. If instantiation fails because a dependency isn't ready,
	// it is retried in a later pass. This handles complex FromExports
	// chains and avoids the need for an explicit topological sort.
	pending := make([]bool, len(bundle.Instances))
	pendingCount := 0
	for i, inst := range bundle.Instances {
		if inst.ModuleIndex >= 0 {
			pending[i] = true
			pendingCount++
		}
	}

	maxPasses := len(bundle.Instances) + 5
	for pass := 0; pass < maxPasses && pendingCount > 0; pass++ {
		progress := false
		for i, inst := range bundle.Instances {
			if !pending[i] || inst.ModuleIndex < 0 {
				continue
			}
			cm := compiled[inst.ModuleIndex]

			// Build a map from import module name to source instance index,
			// resolving through FromExports chains to the actual instantiated instance.
			importNameToInstance := make(map[string]int, len(inst.Args))
			for _, arg := range inst.Args {
				resolved := arg.InstanceIndex
				if resolved >= 0 && resolved < len(actualProvider) {
					resolved = actualProvider[resolved]
				}
				importNameToInstance[arg.Name] = resolved
				if arg.Name == "" {
					importNameToInstance[componentAdapterModule] = resolved
				}
			}
			// For GOT.mem / GOT.func imports, override to the CPython
			// runtime instance when available. The component model DAG
			// may route through adapter instances that lack CPython symbols.
			if cpythonRuntimeIdx >= 0 {
				for _, arg := range inst.Args {
					if strings.HasPrefix(arg.Name, "GOT.") {
						importNameToInstance[arg.Name] = cpythonRuntimeIdx
					}
				}
			}

			linker := wasmtime.NewLinker(b.engine)
			// Register host functions (WASI, env stubs, teavm stubs, all cleat_*).
			// Use dummy completeResult/completeErr since component modules don't
			// use the Go dispatcher cleat_complete protocol.
			var completeResult, completeErr string
			if err := b.registerAllImports(linker, &completeResult, &completeErr, true, abortImportType(cm)); err != nil {
				return nil, fmt.Errorf("host: register imports for instance %d: %w", i, err)
			}

			// Per-export routing: resolve GOT / libpython imports
			// from already-instantiated instances before DefineInstance.
			b.perExportRoute(store, cm, linker, instances, bundle, compiled)

			// Wire cross-module imports: for each import the module declares,		}

			// Wire cross-module imports: for each import the module declares,
			// map it to the already-instantiated source instance.
			// Skip WASI 0.2.0 interface names — adapter signatures may not
			// match what the module expects. Traps handle them instead.
			for importName, srcIdx := range importNameToInstance {
				if strings.Contains(importName, ":") && !strings.HasPrefix(importName, "GOT.") {
					continue
				}
				if srcIdx < 0 || srcIdx >= len(instances) || instances[srcIdx] == nil {
					continue
				}
				if err := linker.DefineInstance(store, importName, instances[srcIdx]); err != nil {
					// "defined twice" is OK — some exports (e.g. abort) are
					// defined by both registerEnvStubs and the source instance.
					if !strings.Contains(err.Error(), "defined twice") {
						return nil, fmt.Errorf("host: define instance %d as %q for instance %d: %w", srcIdx, importName, i, err)
					}
				}
			}

			// Some modules import "memory" from "env" (not as a host function).
			// Route it from any already-instantiated instance that exports memory.
			for _, prevInst := range instances {
				if prevInst == nil {
					continue
				}
				if memExp := prevInst.GetExport(store, "memory"); memExp != nil {
					_ = linker.Define(store, "env", "memory", memExp)
					break
				}
			}
			// wit_dylib functions for component model adapter canonical ABI.
			for _, impTy := range cm.Imports() {
				if impTy.Module() != "env" || impTy.Name() == nil ||
					!strings.HasPrefix(*impTy.Name(), "wit_dylib_") {
					continue
				}
				if impTy.Type() == nil || impTy.Type().FuncType() == nil {
					continue
				}
				b.defineWitDylib(store, linker, impTy)
			}
			// Final GOT routing: for GOT.mem/GOT.func imports, route
			// from the CPython runtime with proper mutability handling.
			for _, impTy := range cm.Imports() {
				modName := impTy.Module()
				if modName != "GOT.mem" && modName != "GOT.func" {
					continue
				}
				namePtr := impTy.Name()
				if namePtr == nil {
					continue
				}
				fieldName := *namePtr
				if fieldName == "__memory_base" || fieldName == "__table_base" {
					continue
				}
				extType := impTy.Type()
				if extType == nil || extType.GlobalType() == nil {
					continue
				}
				importGlobalType := extType.GlobalType()
				routed := false
				if cpythonRuntimeIdx >= 0 && instances[cpythonRuntimeIdx] != nil {
					cpythonInst := instances[cpythonRuntimeIdx]
					cpythonModIdx := bundle.Instances[cpythonRuntimeIdx].ModuleIndex
					for _, expTy := range compiled[cpythonModIdx].Exports() {
						if !strings.HasSuffix(expTy.Name(), ":"+fieldName) {
							continue
						}
						candidate := cpythonInst.GetExport(store, expTy.Name())
						if candidate == nil || candidate.Global() == nil {
							continue
						}
						val := candidate.Global().Get(store)
						newGType := wasmtime.NewGlobalType(
							importGlobalType.Content(),
							importGlobalType.Mutable())
						if newG, newErr := wasmtime.NewGlobal(store, newGType, val); newErr == nil {
							_ = linker.Define(store, modName, fieldName, newG)
							routed = true
						}
						break
					}
				}
				if !routed {
					// Create a default mutable global with the import's type.
					gType := wasmtime.NewGlobalType(
						importGlobalType.Content(),
						importGlobalType.Mutable())
					if g, err := wasmtime.NewGlobal(store, gType, wasmtime.ValI32(0)); err == nil {
						_ = linker.Define(store, modName, fieldName, g)
					}
				}
			}

			// Fill unresolved WASI 0.2.0 imports with traps.
			_ = linker.DefineUnknownImportsAsTraps(cm)
			// Define placeholder imports for modules that need them.
			// __indirect_function_table: size from the module's table import
			// (or a generous default if import info is unavailable).
			tblMinSize := uint32(1048576)
			tblHasMax := false
			tblMaxSize := uint32(0)
			for _, impTy := range cm.Imports() {
				if impTy.Module() == "env" && impTy.Name() != nil && *impTy.Name() == "__indirect_function_table" {
					if extType := impTy.Type(); extType != nil {
						if tt := extType.TableType(); tt != nil {
							tblMinSize = tt.Minimum()
							tblHasMax, tblMaxSize = tt.Maximum()
						}
					}
					break
				}
			}
			tblType := wasmtime.NewTableType(wasmtime.NewValType(wasmtime.KindFuncref), tblMinSize, tblHasMax, tblMaxSize)
			if tbl, err := wasmtime.NewTable(store, tblType, wasmtime.ValFuncref(nil)); err == nil {
				_ = linker.Define(store, "env", "__indirect_function_table", tbl)
			}
			i32Mut := wasmtime.NewGlobalType(wasmtime.NewValType(wasmtime.KindI32), true)
			i32Imm := wasmtime.NewGlobalType(wasmtime.NewValType(wasmtime.KindI32), false)
			if sp, err := wasmtime.NewGlobal(store, i32Mut, wasmtime.ValI32(0)); err == nil {
				_ = linker.Define(store, "env", "__stack_pointer", sp)
			}
			if mb, err := wasmtime.NewGlobal(store, i32Imm, wasmtime.ValI32(1024)); err == nil {
				_ = linker.Define(store, "env", "__memory_base", mb)
			}
			if tb, err := wasmtime.NewGlobal(store, i32Imm, wasmtime.ValI32(1024)); err == nil {
				_ = linker.Define(store, "env", "__table_base", tb)
			}
			if gmb, err := wasmtime.NewGlobal(store, i32Imm, wasmtime.ValI32(0)); err == nil {
				_ = linker.Define(store, "GOT.mem", "__memory_base", gmb)
			}
			if gtb, err := wasmtime.NewGlobal(store, i32Imm, wasmtime.ValI32(1)); err == nil {
				_ = linker.Define(store, "GOT.func", "__table_base", gtb)
			}

			modInst, instErr := linker.Instantiate(store, cm)
			if instErr != nil {
				// If the error is a missing import, retry in a later pass
				// (the dependency may not be instantiated yet).
				if strings.Contains(instErr.Error(), "unknown import") ||
					strings.Contains(instErr.Error(), "has not been defined") {
					continue // retry in next pass
				}
				// Element segment / table errors can result from
				// adapter-provided tables conflicting with our
				// placeholders. Retry without cross-module routing.
				if strings.Contains(instErr.Error(), "undefined element") ||
					strings.Contains(instErr.Error(), "out of bounds") {
					linker2 := wasmtime.NewLinker(b.engine)
					var cr2, ce2 string
					b.registerAllImports(linker2, &cr2, &ce2, true, abortImportType(cm))
					// wit_dylib functions for component model adapter canonical ABI (fallback).
					for _, impTy := range cm.Imports() {
						if impTy.Module() != "env" || impTy.Name() == nil ||
							!strings.HasPrefix(*impTy.Name(), "wit_dylib_") {
							continue
						}
						if impTy.Type() == nil || impTy.Type().FuncType() == nil {
							continue
						}
						b.defineWitDylib(store, linker2, impTy)
					}
					_ = linker2.DefineUnknownImportsAsTraps(cm)
					for _, prevInst := range instances {
						if prevInst == nil {
							continue
						}
						if memExp := prevInst.GetExport(store, "memory"); memExp != nil {
							_ = linker2.Define(store, "env", "memory", memExp)
							break
						}
					}
					tblMin2 := uint32(1048576)
					tblType2 := wasmtime.NewTableType(wasmtime.NewValType(wasmtime.KindFuncref), tblMin2, false, 0)
					if tbl2, _ := wasmtime.NewTable(store, tblType2, wasmtime.ValFuncref(nil)); tbl2 != nil {
						_ = linker2.Define(store, "env", "__indirect_function_table", tbl2)
					}
					i32Imm2 := wasmtime.NewGlobalType(wasmtime.NewValType(wasmtime.KindI32), false)
					i32Mut2 := wasmtime.NewGlobalType(wasmtime.NewValType(wasmtime.KindI32), true)
					if sp2, _ := wasmtime.NewGlobal(store, i32Mut2, wasmtime.ValI32(0)); sp2 != nil {
						_ = linker2.Define(store, "env", "__stack_pointer", sp2)
					}
					if mb2, _ := wasmtime.NewGlobal(store, i32Imm2, wasmtime.ValI32(1024)); mb2 != nil {
						_ = linker2.Define(store, "env", "__memory_base", mb2)
					}
					if tb2, _ := wasmtime.NewGlobal(store, i32Imm2, wasmtime.ValI32(1024)); tb2 != nil {
						_ = linker2.Define(store, "env", "__table_base", tb2)
					}
					if gmb2, _ := wasmtime.NewGlobal(store, i32Imm2, wasmtime.ValI32(0)); gmb2 != nil {
						_ = linker2.Define(store, "GOT.mem", "__memory_base", gmb2)
					}
					if gtb2, _ := wasmtime.NewGlobal(store, i32Imm2, wasmtime.ValI32(1)); gtb2 != nil {
						_ = linker2.Define(store, "GOT.func", "__table_base", gtb2)
					}
					// Also run per-export routing for the fresh linker
					// to resolve GOT / libpython global imports.
					b.perExportRoute(store, cm, linker2, instances, bundle, compiled)
					if modInst2, err2 := linker2.Instantiate(store, cm); err2 == nil {
						instances[i] = modInst2
						pending[i] = false
						pendingCount--
						progress = true
						continue
					}
				}
				// Build a list of expected import module names for diagnostics.
				var importMods []string
				for importName := range importNameToInstance {
					importMods = append(importMods, importName)
				}
				return nil, fmt.Errorf("host: instantiate instance %d (module %d, %d args, imports: %v): %w", i, inst.ModuleIndex, len(inst.Args), importMods, instErr)
			}
			instances[i] = modInst
			pending[i] = false
			pendingCount--
			progress = true
		}
		if !progress {
			// No instances could be instantiated in this pass.
			// Build a diagnostic list of whats still pending.
			var pendingList []int
			for idx, p := range pending {
				if p {
					pendingList = append(pendingList, idx)
				}
			}
			return nil, fmt.Errorf("host: could not instantiate %d instances (stuck at pass %d): pending=%v", pendingCount, pass, pendingList)
		}
	}

	if pendingCount > 0 {
		var pendingList []int
		for idx, p := range pending {
			if p {
				pendingList = append(pendingList, idx)
			}
		}
		return nil, fmt.Errorf("host: %d instances still pending after %d passes: %v", pendingCount, maxPasses, pendingList)
	}

	// ---- Step 3b: Call constructors on all core instances ----
	// Modules compiled with Emscripten or componentize-py export
	// __wasm_call_ctors which must be called before the entry point
	// to set up WIT metadata (wit_dylib_initialize) and dispatch tables.
	for i, inst := range instances {
		if inst == nil {
			continue
		}
		if f := inst.GetFunc(store, "__wasm_call_ctors"); f != nil {
			if _, err := f.Call(store); err != nil {
				return nil, fmt.Errorf("host: __wasm_call_ctors instance %d: %w", i, err)
			}
		}
		if f := inst.GetFunc(store, "__wasm_apply_data_relocs"); f != nil {
			if _, err := f.Call(store); err != nil {
				return nil, fmt.Errorf("host: __wasm_apply_data_relocs instance %d: %w", i, err)
			}
		}
	}

	// ---- Step 3c: Scan for wit_dylib metadata blob ----
	// Dump the first 256 u32 values from the adapter instance's memory
	// to find the metadata blob, then call wit_dylib_initialize.
	if b.witDylib != nil {
		for _, inst := range instances {
			if inst == nil {
				continue
			}
			memExp := inst.GetExport(store, "memory")
			if memExp == nil {
				continue
			}
			m := memExp.Memory()
			if m == nil {
				continue
			}
			data := m.UnsafeData(store)
			if len(data) < 256 {
				continue
			}
			// Scan for the metadata signature: 16 small u32 counts
			// followed by type arrays. The counts[14] is export_funcs.
			for ptr := 0; ptr < len(data)-64; ptr += 4 {
				nExportFuncs := binary.LittleEndian.Uint32(data[ptr+56:])
				// Check that the first 13 counts are all < 1000 (reasonable)
				allSmall := true
				for j := 0; j < 13; j++ {
					v := binary.LittleEndian.Uint32(data[ptr+j*4:])
					if v > 1000 {
						allSmall = false
						break
					}
				}
				if allSmall && nExportFuncs >= 1 && nExportFuncs <= 20 {
					if err := b.witDylib.initialize(m, store, int32(ptr)); err == nil {
						break
					}
				}
			}
			break // only check first instance with memory
		}
	}

	// ---- Step 4: Build resolved exports map per instance ----
	type resolvedExp struct {
		exportName string
		inst       *wasmtime.Instance
	}
	resolvedExports := make([]map[string]resolvedExp, len(bundle.Instances))

	for i, inst := range bundle.Instances {
		resolvedExports[i] = make(map[string]resolvedExp)
		if inst.ModuleIndex >= 0 {
			modInst := instances[i]
			if modInst == nil {
				continue
			}
			// Collect function exports by iterating the module's export types.
			cm := compiled[inst.ModuleIndex]
			exports := cm.Exports()
			for _, exp := range exports {
				if exp.Type().FuncType() != nil {
					resolvedExports[i][exp.Name()] = resolvedExp{exportName: exp.Name(), inst: modInst}
				}
			}
		}
		// Apply FromExports aliases.
		for _, fe := range inst.FromExports {
			if fe.SourceInstance >= 0 && fe.SourceInstance < len(resolvedExports) {
				if exp, ok := resolvedExports[fe.SourceInstance][fe.SourceName]; ok {
					resolvedExports[i][fe.Name] = exp
				}
			}
		}
	}

	// ---- Step 5: Resolve entry point ----
	exp, ok := bundle.Exports[entryPoint]
	if !ok {
		return nil, fmt.Errorf("host: component export %q not found", entryPoint)
	}

	var entryInst *wasmtime.Instance
	var entryExportName string

	if exp.InstanceIndex >= 0 && exp.InstanceIndex < len(instances) {
		// Direct instance reference.
		if re, ok2 := resolvedExports[exp.InstanceIndex][exp.Name]; ok2 && re.inst != nil {
			entryInst = re.inst
			entryExportName = re.exportName
		} else if instances[exp.InstanceIndex] != nil {
			entryInst = instances[exp.InstanceIndex]
			entryExportName = exp.Name
		}
	} else {
		// No direct instance reference (e.g. func export without
		// instance sort). Search all instantiated instances.
		for i, inst := range instances {
			if inst == nil {
				continue
			}
			if re, ok2 := resolvedExports[i][exp.Name]; ok2 && re.inst != nil {
				entryInst = re.inst
				entryExportName = re.exportName
				break
			}
			if f := inst.GetFunc(store, exp.Name); f != nil {
				entryInst = inst
				entryExportName = exp.Name
				break
			}
		}
	}

	if entryInst == nil {
		return nil, fmt.Errorf("host: cannot resolve component export %q (instance %d)", entryPoint, exp.InstanceIndex)
	}

	fn := entryInst.GetFunc(store, entryExportName)
	if fn == nil {
		return nil, fmt.Errorf("host: component export %q func %q not found", entryPoint, entryExportName)
	}

	// ---- Step 6: Find memory and set up scratch buffers ----
	memory := entryInst.GetExport(store, "memory")
	if memory == nil {
		// Try other instances for memory.
		for _, inst := range instances {
			if inst == nil {
				continue
			}
			if m := inst.GetExport(store, "memory"); m != nil {
				memory = m
				break
			}
		}
	}
	if memory == nil {
		return nil, fmt.Errorf("host: no exported memory found in component instances")
	}
	mem := memory.Memory()
	if mem == nil {
		return nil, fmt.Errorf("host: memory export is not a memory")
	}

	outBufSz := OutBufSize
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

	// Write input JSON.
	inputBytes := []byte(input)
	if len(inputBytes) > 0 {
		data := mem.UnsafeData(store)
		if uint64(inputOffset)+uint64(len(inputBytes)) > uint64(len(data)) {
			return nil, fmt.Errorf("host: input exceeds memory bounds")
		}
		copy(data[inputOffset:], inputBytes)
	}

	// ---- Step 7: Call the entry point ----
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

	if callErr != nil {
		if limitErr := b.resourceLimitError(callErr, execTimeout); limitErr != nil {
			callErr = limitErr
		}
		return nil, fmt.Errorf("host: component export %q: %w", entryPoint, callErr)
	}
	if results == nil {
		return nil, fmt.Errorf("host: export %q returned no results", entryPoint)
	}

	raw, ok := results.(int64)
	if !ok {
		return nil, fmt.Errorf("host: export %q returned non-int64 result", entryPoint)
	}

	if raw == (1 << 62) {
		return &ExecResult{Suspended: true}, nil
	}

	errCode, actualLen := decodeExportResult(uint64(raw))
	if actualLen > outBufSz {
		return nil, fmt.Errorf("host: export %q: output overflow: wrote %d bytes, buffer is %d bytes", entryPoint, actualLen, outBufSz)
	}

	data := mem.UnsafeData(store)
	outputStr := string(data[outputOffset : outputOffset+actualLen])
	if errCode != 0 {
		return nil, fmt.Errorf("host: export %q: %s", entryPoint, outputStr)
	}

	return &ExecResult{Result: outputStr, Suspended: false}, nil
}

// registerAllImports registers all host function imports on the given linker.
// Extracted so both Execute and ExecuteComponent can share the same setup.
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
	if err := b.registerCleatChildWorkflowInSchema(linker); err != nil {
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

// perExportRoute resolves non-host imports by searching already-instantiated
// instances for matching exports. Exact name match first, then suffix match
// for prefixed exports (e.g. libpython3.14.so:PyExc_AttributeError matches
// imports of PyExc_AttributeError). Handles global mutability mismatches.
func (b *wasmtimeBackend) perExportRoute(store wasmtime.Storelike, cm *wasmtime.Module, linker *wasmtime.Linker, instances []*wasmtime.Instance, bundle *wasm.ComponentBundle, compiled []*wasmtime.Module) {
	for _, impTy := range cm.Imports() {
		modName := impTy.Module()
		namePtr := impTy.Name()
		if namePtr == nil {
			continue
		}
		fieldName := *namePtr

		// Skip WASI and teavm (handled by registerAllImports).
		// "env" imports are NOT skipped — some have prefixed
		// names like libpython3.14.so:memory_base that need
		// suffix matching from other instances.
		if modName == "wasi_snapshot_preview1" || modName == "teavm" ||
			strings.Contains(modName, "wasi:") {
			continue
		}
		if modName == "env" && fieldName != "memory" &&
			fieldName != "__indirect_function_table" &&
			fieldName != "__stack_pointer" &&
			!strings.Contains(fieldName, ":") {
			continue
		}

		extType := impTy.Type()
		if extType == nil {
			continue
		}
		// Search already-instantiated instances — exact then suffix.
		for prevIdx, prevInst := range instances {
			if prevInst == nil {
				continue
			}
			exp := prevInst.GetExport(store, fieldName)
			if exp == nil && prevIdx < len(bundle.Instances) {
				// Suffix match: source module exports ending in
				// ":" + fieldName.
				prevModIdx := bundle.Instances[prevIdx].ModuleIndex
				if prevModIdx >= 0 && prevModIdx < len(compiled) {
					for _, expTy := range compiled[prevModIdx].Exports() {
						en := expTy.Name()
						if !strings.HasSuffix(en, ":"+fieldName) {
							continue
						}
						candidate := prevInst.GetExport(store, en)
						if candidate == nil {
							continue
						}
						// Type check before accepting.
						if (extType.FuncType() != nil && candidate.Func() != nil) ||
							(extType.GlobalType() != nil && candidate.Global() != nil) ||
							(extType.MemoryType() != nil && candidate.Memory() != nil) ||
							(extType.TableType() != nil && candidate.Table() != nil) {
							exp = candidate
							break
						}
					}
				}
			}
			if exp == nil {
				continue
			}
			// Route the export under the import's module name.
			// For globals, handle mutability mismatches.
			if extType.FuncType() != nil && exp.Func() != nil {
				_ = linker.Define(store, modName, fieldName, exp)
			} else if extType.GlobalType() != nil && exp.Global() != nil {
				expGlobal := exp.Global()
				expGlobalType := expGlobal.Type(store)
				importGlobalType := extType.GlobalType()
				if importGlobalType.Mutable() != expGlobalType.Mutable() ||
					importGlobalType.Content().Kind() != expGlobalType.Content().Kind() {
					val := expGlobal.Get(store)
					newGlobalType := wasmtime.NewGlobalType(
						importGlobalType.Content(),
						importGlobalType.Mutable())
					if newGlobal, newErr := wasmtime.NewGlobal(
						store, newGlobalType, val); newErr == nil {
						_ = linker.Define(store, modName, fieldName, newGlobal)
					}
				} else {
					_ = linker.Define(store, modName, fieldName, exp)
				}
			} else if extType.MemoryType() != nil && exp.Memory() != nil {
				_ = linker.Define(store, modName, fieldName, exp)
			} else if extType.TableType() != nil && exp.Table() != nil {
				_ = linker.Define(store, modName, fieldName, exp)
			}
			break
		}
	}
}

// defineWitDylib defines a wit_dylib_* host function for the component model
// adapter (module 10). These functions implement the canonical ABI memory
// read/write operations needed by componentize-py generated modules.
func (b *wasmtimeBackend) defineWitDylib(store *wasmtime.Store, linker *wasmtime.Linker, impTy *wasmtime.ImportType) {
	name := *impTy.Name()
	functype := impTy.Type().FuncType()
	makeNoop := func() *wasmtime.Func {
		return wasmtime.NewFunc(store, functype,
			func(_ *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
				resTypes := functype.Results()
				results := make([]wasmtime.Val, len(resTypes))
				for i, rt := range resTypes {
					switch rt.Kind() {
					case wasmtime.KindI32:
						results[i] = wasmtime.ValI32(0)
					case wasmtime.KindI64:
						results[i] = wasmtime.ValI64(0)
					case wasmtime.KindF32:
						results[i] = wasmtime.ValF32(0)
					case wasmtime.KindF64:
						results[i] = wasmtime.ValF64(0)
					default:
						results[i] = wasmtime.ValI32(0)
					}
				}
				return results, nil
			})
	}

	// Push functions: ctx = args[0].I32()
	if strings.HasPrefix(name, "wit_dylib_push_") {
		kind := name[15:]
		switch {
		case kind == "u32" || kind == "s32" || kind == "u8" || kind == "s8" ||
			kind == "u16" || kind == "s16" || kind == "bool" || kind == "char" ||
			kind == "flags" || kind == "enum":
			fn := wasmtime.NewFunc(store, functype,
				func(_ *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
					if b.witDylib != nil {
						b.witDylib.pushI32(args[0].I32(), args[1].I32())
					}
					return nil, nil
				})
			_ = linker.Define(store, "env", name, fn)
		case kind == "u64" || kind == "s64":
			fn := wasmtime.NewFunc(store, functype,
				func(_ *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
					if b.witDylib != nil {
						b.witDylib.pushI64(args[0].I32(), args[1].I64())
					}
					return nil, nil
				})
			_ = linker.Define(store, "env", name, fn)
		case kind == "f32":
			fn := wasmtime.NewFunc(store, functype,
				func(_ *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
					if b.witDylib != nil {
						b.witDylib.pushF32(args[0].I32(), args[1].F32())
					}
					return nil, nil
				})
			_ = linker.Define(store, "env", name, fn)
		case kind == "f64":
			fn := wasmtime.NewFunc(store, functype,
				func(_ *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
					if b.witDylib != nil {
						b.witDylib.pushF64(args[0].I32(), args[1].F64())
					}
					return nil, nil
				})
			_ = linker.Define(store, "env", name, fn)
		case kind == "string":
			fn := wasmtime.NewFunc(store, functype,
				func(caller *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
					if b.witDylib != nil && len(args) >= 3 {
						exp := caller.GetExport("memory")
						if exp != nil {
							mem := exp.Memory()
							if mem != nil {
								data := mem.UnsafeData(caller)
								ptr := args[1].I32()
								length := args[2].I32()
								if ptr >= 0 && int(ptr)+int(length) <= len(data) {
									strData := make([]byte, length)
									copy(strData, data[ptr:ptr+length])
									b.witDylib.pushString(args[0].I32(), strData)
								}
							}
						}
					}
					return nil, nil
				})
			_ = linker.Define(store, "env", name, fn)
		default:
			fn := wasmtime.NewFunc(store, functype,
				func(_ *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
					if b.witDylib != nil && len(args) >= 2 {
						b.witDylib.pushI32(args[0].I32(), args[1].I32())
					}
					return nil, nil
				})
			_ = linker.Define(store, "env", name, fn)
		}
		return
	}

	// Pop functions: ctx = args[0].I32()
	if strings.HasPrefix(name, "wit_dylib_pop_") {
		kind := name[14:]
		switch {
		case kind == "u32" || kind == "s32" || kind == "u8" || kind == "s8" ||
			kind == "u16" || kind == "s16" || kind == "bool" || kind == "char" ||
			kind == "flags" || kind == "enum":
			fn := wasmtime.NewFunc(store, functype,
				func(_ *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
					var val int32
					if b.witDylib != nil {
						val = b.witDylib.popI32(args[0].I32())
					}
					return []wasmtime.Val{wasmtime.ValI32(val)}, nil
				})
			_ = linker.Define(store, "env", name, fn)
		case kind == "u64" || kind == "s64":
			fn := wasmtime.NewFunc(store, functype,
				func(_ *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
					var val int64
					if b.witDylib != nil {
						val = b.witDylib.popI64(args[0].I32())
					}
					return []wasmtime.Val{wasmtime.ValI64(val)}, nil
				})
			_ = linker.Define(store, "env", name, fn)
		case kind == "f32":
			fn := wasmtime.NewFunc(store, functype,
				func(_ *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
					var val float32
					if b.witDylib != nil {
						val = b.witDylib.popF32(args[0].I32())
					}
					return []wasmtime.Val{wasmtime.ValF32(val)}, nil
				})
			_ = linker.Define(store, "env", name, fn)
		case kind == "f64":
			fn := wasmtime.NewFunc(store, functype,
				func(_ *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
					var val float64
					if b.witDylib != nil {
						val = b.witDylib.popF64(args[0].I32())
					}
					return []wasmtime.Val{wasmtime.ValF64(val)}, nil
				})
			_ = linker.Define(store, "env", name, fn)
		case kind == "string":
			fn := wasmtime.NewFunc(store, functype,
				func(caller *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
					var length int32
					if b.witDylib != nil && len(args) >= 2 {
						exp := caller.GetExport("memory")
						if exp != nil {
							mem := exp.Memory()
							if mem != nil {
								length = b.witDylib.popString(args[0].I32(), mem, caller, args[1].I32())
							}
						}
					}
					return []wasmtime.Val{wasmtime.ValI32(length)}, nil
				})
			_ = linker.Define(store, "env", name, fn)
		default:
			fn := wasmtime.NewFunc(store, functype,
				func(_ *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
					if b.witDylib != nil {
						_ = b.witDylib.popI32(args[0].I32())
					}
					return []wasmtime.Val{wasmtime.ValI32(0)}, nil
				})
			_ = linker.Define(store, "env", name, fn)
		}
		return
	}

	// Export lifecycle and other special functions
	switch name {
	case "wit_dylib_initialize":
		fn := wasmtime.NewFunc(store, functype,
			func(caller *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
				if b.witDylib != nil && len(args) >= 1 {
					exp := caller.GetExport("memory")
					if exp != nil {
						mem := exp.Memory()
						if mem != nil {
							b.witDylib.initialize(mem, caller, args[0].I32())
						}
					}
				}
				resTypes := functype.Results()
				results := make([]wasmtime.Val, len(resTypes))
				return results, nil
			})
		_ = linker.Define(store, "env", name, fn)

	case "wit_dylib_export_start":
		fn := wasmtime.NewFunc(store, functype,
			func(_ *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
				var handle int32
				if b.witDylib != nil && len(args) >= 1 {
					handle = b.witDylib.exportStart(args[0].I32())
				}
				resTypes := functype.Results()
				results := make([]wasmtime.Val, len(resTypes))
				if len(results) > 0 {
					results[0] = wasmtime.ValI32(handle)
				}
				return results, nil
			})
		_ = linker.Define(store, "env", name, fn)

	case "wit_dylib_export_call", "wit_dylib_export_async_callback":
		if name == "wit_dylib_export_call" {
			fn := wasmtime.NewFunc(store, functype,
				func(caller *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
					if len(args) < 2 || b.witDylib == nil {
						return nil, nil
					}
					elemIdx := b.witDylib.getExportElemIndex(args[1].I32())
					if elemIdx < 0 {
						return nil, nil
					}
					tableExp := caller.GetExport("__indirect_function_table")
					if tableExp == nil {
						return nil, nil
					}
					table := tableExp.Table()
					if table == nil {
						return nil, nil
					}
					elem, err := table.Get(caller, uint64(elemIdx))
					if err != nil {
						return nil, nil
					}
					callee := elem.Funcref()
					if callee == nil {
						return nil, nil
					}
					ctx := args[0].I32()
					arg3 := b.witDylib.popI32(ctx)
					arg2 := b.witDylib.popI32(ctx)
					arg1 := b.witDylib.popI32(ctx)
					arg0 := b.witDylib.popI32(ctx)
					callResult, callErr := callee.Call(caller, arg0, arg1, arg2, arg3)
					if callErr != nil {
						return nil, wasmtime.NewTrap(callErr.Error())
					}
					if r, ok := callResult.(int32); ok {
						b.witDylib.pushI32(ctx, r)
					} else if r, ok := callResult.(int64); ok {
						b.witDylib.pushI64(ctx, r)
					}
					return nil, nil
				})
			_ = linker.Define(store, "env", name, fn)
		} else {
			_ = linker.Define(store, "env", name, makeNoop())
		}

	case "wit_dylib_export_async_call":
		fn := wasmtime.NewFunc(store, functype,
			func(_ *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
				resTypes := functype.Results()
				results := make([]wasmtime.Val, len(resTypes))
				if len(results) > 0 {
					results[0] = wasmtime.ValI32(0)
				}
				return results, nil
			})
		_ = linker.Define(store, "env", name, fn)

	case "wit_dylib_export_finish":
		fn := wasmtime.NewFunc(store, functype,
			func(_ *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
				if b.witDylib != nil && len(args) >= 2 {
					b.witDylib.exportFinish(args[0].I32())
				}
				return nil, nil
			})
		_ = linker.Define(store, "env", name, fn)

	case "cabi_realloc":
		fn := wasmtime.NewFunc(store, functype,
			func(caller *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
				oldPtr := args[0].I32()
				oldSize := args[1].I32()
				_ = oldSize
				_ = args[2].I32() // align
				newSize := args[3].I32()
				exp := caller.GetExport("memory")
				if exp == nil {
					return []wasmtime.Val{wasmtime.ValI32(0)}, nil
				}
				mem := exp.Memory()
				if mem == nil {
					return []wasmtime.Val{wasmtime.ValI32(0)}, nil
				}
				if oldPtr == 0 && newSize > 0 {
					data := mem.UnsafeData(caller)
					newPtr := int32(len(data) - int(newSize) - 64)
					if newPtr < 0 {
						newPtr = 0
					}
					return []wasmtime.Val{wasmtime.ValI32(newPtr)}, nil
				}
				return []wasmtime.Val{wasmtime.ValI32(oldPtr)}, nil
			})
		_ = linker.Define(store, "env", name, fn)

	case "wit_dylib_list_append":
		fn := wasmtime.NewFunc(store, functype,
			func(_ *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
				if b.witDylib != nil && len(args) >= 2 {
					b.witDylib.pushI32(args[0].I32(), args[1].I32())
				}
				return nil, nil
			})
		_ = linker.Define(store, "env", name, fn)

	default:
		_ = linker.Define(store, "env", name, makeNoop())
	}
}
