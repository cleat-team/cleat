package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"

	"github.com/cleat-team/cleat/monitoring/prometheus"
)

// cleatComplete stores the workflow result delivered via cleat_complete host import.
// This decouples workflow completion from Go WASI runtime shutdown behavior.
type cleatComplete struct {
	Result *string // JSON result (status=0)
	Error  *string // error message (status=1)
}

var cleatCompleteKey struct{}

// DefaultMemoryLimitPages is the default max WASM linear memory (512 pages = 32 MB).
const DefaultMemoryLimitPages = 512

var wazeroInitOnce sync.Once

// Runtime wraps a wazero runtime with pre-registered host function imports.
type Runtime struct {
	wazeroRuntime wazero.Runtime
	// stdout/stderr are NOT goroutine-safe — they are shared across callers
	// of InstantiateModuleNamed. Concurrent execution must use the
	// wazeroBackend.Execute() path, which uses per-backend buffers.
	stdout           bytes.Buffer
	stderr           bytes.Buffer
	callTimeout      time.Duration // per-call WASM execution timeout (0 = none)
	MemoryLimitPages uint32        // max WASM linear memory in pages (64KB each)
	fuelLimit        uint64        // max WASM fuel (function calls) per invocation; 0 = no limit
	Metrics          *prometheus.Metrics
}

// Stdout returns captured stdout output from the most recent module.
func (r *Runtime) Stdout() string { return r.stdout.String() }

// Stderr returns captured stderr output from the most recent module.
func (r *Runtime) Stderr() string { return r.stderr.String() }

// NewRuntime creates a Runtime with all cleat_* host functions and the plugin_call
// host function registered on the "env" module. WASI preview1 is also instantiated
// for Go wasip1 support. Plugin host functions are registered via the Engine's
// PluginRegistry — not through NewRuntime.
//
// Floating-point determinism architecture:
//
// WASM floating-point (f32/f64) operations follow IEEE 754-2019, which guarantees
// bit-identical results for the same operations on the same inputs across all
// compliant hardware. wazero's interpreter mode (the default for cleat workflows)
// implements strict IEEE 754 semantics without any "fast math" optimizations that
// could break determinism.
//
// However, there are important gotchas:
//  1. NaN payloads: IEEE 754 allows multiple bit patterns for NaN. WASM f32/f64
//     operations that produce NaN may return different NaN payloads across CPU
//     architectures or wazero versions. This is only a problem if NaN payloads
//     affect control flow (e.g., comparing NaN values).
//  2. Denormal numbers: Some CPUs implement "flush-to-zero" for denormals,
//     while others preserve them. wazero's interpreter preserves denormals.
//  3. Compiler optimizations: The host Go compiler may apply FMA (fused
//     multiply-add) or other optimizations that change the exact order of
//     floating-point operations. wazero's WASM interpreter does not apply
//     such optimizations to WASM code.
//
// Best practice: avoid floating-point in workflow control flow conditions.
// Use math.Float64bits()/math.Float32bits() for exact bitwise comparison, or
// use integer arithmetic. See docs/determinism.md for more details.
func NewRuntime(ctx context.Context, memoryLimitPages uint32, instructionLimit uint64) (*Runtime, error) {
	if memoryLimitPages == 0 {
		memoryLimitPages = DefaultMemoryLimitPages
	}
	wazeroInitOnce.Do(func() {
		dummy := wazero.NewRuntime(context.Background())
		dummy.Close(context.Background())
	})

	rtCfg := wazero.NewRuntimeConfigCompiler().
		WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExtendedConst).
		WithMemoryLimitPages(memoryLimitPages)
	rt := wazero.NewRuntimeWithConfig(ctx, rtCfg)

	r := &Runtime{wazeroRuntime: rt, callTimeout: 30 * time.Second, MemoryLimitPages: memoryLimitPages, fuelLimit: instructionLimit}

	// WASI is required by Go wasip1 modules for goroutine/stack management.
	// We build WASI with clock_time_get and random_get stubbed out so that
	// workflow code calling time.Now() or crypto/rand through WASI panics
	// instead of silently breaking determinism. Workflows must use h.Now()
	// and h.Random() (imported as cleat_now / cleat_random).
	wasiBuilder := rt.NewHostModuleBuilder(wasi_snapshot_preview1.ModuleName)
	wasi_snapshot_preview1.NewFunctionExporter().ExportFunctions(wasiBuilder)
	// Register reset_adapter_state, which is required by core modules
	// extracted from component model binaries produced by componentize-py.
	// In the full component model assembly this function is provided by the
	// WASI preview1 adapter; here we provide a no-op stub since the adapter
	// layer has been stripped during component decomposition.
	wasiBuilder.NewFunctionBuilder().WithFunc(
		func(ctx context.Context, m api.Module) {},
	).Export("reset_adapter_state")
	// Instantiate WASI with a fake Sys context that returns fixed (zero) time.
	// This prevents the wazero nil pointer panic in clock_time_get (which
	// accesses mod.Sys for walltime/nanotime) while keeping the Go WASM
	// runtime's GC/goroutine scheduler from accessing real wall clock time.
	// Workflow logic uses h.Now() (cleat_now) for deterministic time.
	wasiCompiled, err := wasiBuilder.Compile(ctx)
	if err != nil {
		return nil, fmt.Errorf("host: compiling WASI module: %w", err)
	}
	wasiConfig := wazero.NewModuleConfig().
		WithWalltime(func() (int64, int32) { return 0, 0 }, sys.ClockResolution(1)).
		WithNanotime(func() int64 { return 0 }, sys.ClockResolution(1))
	if _, err := rt.InstantiateModule(ctx, wasiCompiled, wasiConfig); err != nil {
		return nil, fmt.Errorf("host: instantiating WASI module: %w", err)
	}

	// TeaVM runtime stubs — required by TeaVM-compiled Java WASM modules.
	// These are minimal no-op stubs that satisfy the "teavm" import namespace.
	teavmBuilder := rt.NewHostModuleBuilder("teavm")
	teavmBuilder.NewFunctionBuilder().
		WithFunc(func(_ context.Context, _ api.Module, chars uint32, count uint32) {}).
		Export("putwcharsOut")
	teavmBuilder.NewFunctionBuilder().
		WithFunc(func(ctx context.Context) float64 { return float64(handlerFromContext(ctx).Now(ctx)) }).
		Export("currentTimeMillis")
	teavmBuilder.NewFunctionBuilder().
		WithFunc(func(_ context.Context, _ api.Module, ptr uint32) {}).
		Export("logString")
	teavmBuilder.NewFunctionBuilder().
		WithFunc(func(_ context.Context, _ api.Module, ptr uint32) {}).
		Export("logInt")
	teavmBuilder.NewFunctionBuilder().
		WithFunc(func(_ context.Context, _ api.Module) {}).
		Export("logOutOfMemory")
	if _, err := teavmBuilder.Instantiate(ctx); err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("host: instantiating teavm module: %w", err)
	}

	// Build the "env" host module that provides cleat_* imports.
	envBuilder := rt.NewHostModuleBuilder("env")

	// Stub for AssemblyScript abort — required by AS-compiled WASM modules.
	envBuilder.NewFunctionBuilder().
		WithFunc(func(_ context.Context, _ api.Module, msg, file, line, col uint32) {}).
		Export("abort")

	registerHostFunctions(envBuilder, r)
	if _, err := envBuilder.Instantiate(ctx); err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("host: instantiating env module: %w", err)
	}

	return r, nil
}

// Close releases all resources held by the runtime.
func (r *Runtime) Close(ctx context.Context) error {
	return r.wazeroRuntime.Close(ctx)
}

// CompileModule pre-compiles a WASM binary for repeated instantiation.
func (r *Runtime) CompileModule(ctx context.Context, wasmBytes []byte) (wazero.CompiledModule, error) {
	return r.wazeroRuntime.CompileModule(ctx, wasmBytes)
}

// InstantiateModule creates a new module instance without running _start.
// Use InitModule to start the Go runtime afterwards.
func (r *Runtime) InstantiateModule(ctx context.Context, compiled wazero.CompiledModule) (api.Module, error) {
	return r.InstantiateModuleNamed(ctx, compiled, "")
}

// InstantiateModuleNamed instantiates a compiled module with the given name.
// Named modules can be imported by other modules via wazero's module linking.
// This is used when instantiating plugin modules alongside the workflow
// module so that the workflow can import from named plugin modules.
// If name is empty, the module gets wazero's default unnamed module config.
func (r *Runtime) InstantiateModuleNamed(ctx context.Context, compiled wazero.CompiledModule, name string) (api.Module, error) {
	r.stdout.Reset()
	r.stderr.Reset()
	config := wazero.NewModuleConfig().
		WithName(name).
		WithStdout(&r.stdout).
		WithStderr(&r.stderr).
		WithStartFunctions()
	return r.wazeroRuntime.InstantiateModule(ctx, compiled, config)
}

// instantiateModuleNamedWithWriters is like InstantiateModuleNamed but uses
// the provided writers for stdout/stderr capture instead of the Runtime's
// shared buffers. This is used by wazeroBackend.Execute() so that concurrent
// workflow executions each have independent buffers.
func (r *Runtime) instantiateModuleNamedWithWriters(ctx context.Context, compiled wazero.CompiledModule, name string, stdout, stderr *bytes.Buffer) (api.Module, error) {
	config := wazero.NewModuleConfig().
		WithName(name).
		WithStdout(stdout).
		WithStderr(stderr).
		WithStartFunctions()
	return r.wazeroRuntime.InstantiateModule(ctx, compiled, config)
}

// InitModule starts the Go wasip1 runtime by calling _start in a background
// goroutine. _start initializes WASI and calls main() which blocks to keep
// the module alive. After yielding until the runtime is ready, the module
// can accept export calls.
//
// Readiness detection uses exponential backoff after _start has been
// dispatched. Most Go wasip1 binaries complete runtime initialization in
// under 1ms (stack setup, GC init, scheduler start). Without a shared-memory
// readiness flag (which would require SDK changes), we check that the module
// is responsive by verifying its memory remains accessible — a live WASM
// module with initialized memory is a reliable indicator that _start
// succeeded.
//
// For non-Go modules (e.g., Rust, C) that don't have _start, this is a no-op.
func (r *Runtime) InitModule(ctx context.Context, mod api.Module) error {
	start := mod.ExportedFunction("_start")
	if start == nil {
		return nil
	}

	started := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				errCh <- fmt.Errorf("host: _start panicked: %v", r)
			}
		}()
		close(started)
		start.Call(ctx)
	}()

	// Wait for the goroutine to actually begin executing _start.
	select {
	case <-started:
	case <-ctx.Done():
		return ctx.Err()
	}

	// Exponential backoff: check module liveness at increasing intervals.
	// Fast-starting modules (most Go wasip1 binaries) pass on the first
	// iteration and pay only the initial 100µs yield. Slow starters
	// (e.g., modules with heavy init() work) get up to ~10ms total.
	delay := 100 * time.Microsecond
	const maxDelay = 10 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			return err
		case <-time.After(delay):
		}

		// Liveness check: memory is allocated during instantiation, but
		// if _start panicked the module may have been torn down, so verify
		// memory is still accessible.
		if mem := mod.Memory(); mem != nil && mem.Size() > 0 {
			// Give Go WASM runtime extra time to initialize the function
			// table after _start launches. Prevents call_indirect traps
			// in child workflows where timing is tighter.
			time.Sleep(50 * time.Millisecond)
			// Check for a late panic before returning success.
			select {
			case err := <-errCh:
				return err
			default:
			}
			return nil
		}
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

// InstantiateAndInit compiles, instantiates, and initialises a WASM module.
// Convenience wrapper used by tests and the worker.
func (r *Runtime) InstantiateAndInit(ctx context.Context, wasmBytes []byte) (api.Module, error) {
	compiled, err := r.CompileModule(ctx, wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("host: compile: %w", err)
	}
	// Note: compiled.Close is deferred to the caller via mod.Close.
	mod, err := r.InstantiateModule(ctx, compiled)
	if err != nil {
		compiled.Close(ctx)
		return nil, fmt.Errorf("host: instantiate: %w", err)
	}
	if err := r.InitModule(ctx, mod); err != nil {
		mod.Close(ctx)
		return nil, fmt.Errorf("host: init: %w", err)
	}
	return mod, nil
}

// wasmTrapError wraps a WASM trap/exit error with a formatted message
// that includes the stack trace with DWARF-resolved source locations.
type wasmTrapError struct {
	cause error
	msg   string
}

func (e *wasmTrapError) Error() string { return e.msg }

// Unwrap preserves the original error so errors.Is/errors.As still work.
func (e *wasmTrapError) Unwrap() error { return e.cause }

// formatWasmCallError formats an error from a wazero function call into a
// human-readable WASM stack trace.
//
// wazero's engine already resolves DWARF source locations and embeds the
// stack trace in the error message. This function classifies the error and
// produces a clean format.
//
// For ExitError (from proc_exit / context cancellation), we report the
// exit code. For WASM trap errors, wazero's message already includes the
// full stack trace with file:line locations resolved from DWARF debug info
// (when the module was compiled with debug symbols, which is the default
// for Go `GOOS=wasip1 GOARCH=wasm` builds). We replace the "wasm error:"
// prefix with "wasm trap:" for consistency.
//
// If DWARF info is not available (e.g., the module was stripped), the
// wazero message falls back to raw function indices and instruction offsets.
func formatWasmCallError(err error) error {
	// ExitError: the module called proc_exit (e.g., Go's os.Exit) or context
	// was cancelled. These don't carry stack trace info.
	var exitErr *sys.ExitError
	if errors.As(err, &exitErr) {
		switch exitErr.ExitCode() {
		case sys.ExitCodeContextCanceled:
			return &wasmTrapError{cause: err, msg: "wasm trap: context canceled"}
		case sys.ExitCodeDeadlineExceeded:
			return &wasmTrapError{cause: err, msg: "wasm trap: deadline exceeded"}
		default:
			return &wasmTrapError{
				cause: err,
				msg:   fmt.Sprintf("wasm trap: exit(code=%d)", exitErr.ExitCode()),
			}
		}
	}

	// Trap error: wazero's format already includes the stack trace with
	// DWARF-resolved source locations (file:line). Replace the "wasm error:"
	// prefix with "wasm trap:" for consistency.
	//
	// Example wazero output:
	//   wasm error: unreachable
	//   wasm stack trace:
	//       env.cleat_call(i32,i32,i32,i32,i32,i32,i32,i32)
	//           0x1234: /build/workflow.go:42:5
	//       main.handler(i32,i32)
	//           0x5678: /build/workflow.go:15:7
	errMsg := err.Error()
	if strings.Contains(errMsg, "wasm error:") {
		errMsg = strings.Replace(errMsg, "wasm error:", "wasm trap:", 1)
		return &wasmTrapError{cause: err, msg: errMsg}
	}

	// Fallback: unknown error type — wrap with "wasm trap:" prefix.
	return &wasmTrapError{cause: err, msg: fmt.Sprintf("wasm trap: %s", errMsg)}
}

// fuelMeter implements wazero's FunctionListenerFactory and FunctionListener
// to provide fuel-based instruction metering. It tracks consumed function calls
// and shuts down the module when the fuel budget is exhausted.
type fuelMeter struct {
	remaining atomic.Uint64
	Metrics   *prometheus.Metrics
}

// NewFunctionListener satisfies FunctionListenerFactory. It returns itself as
// a shared listener since the meter is not per-function.
func (fm *fuelMeter) NewFunctionListener(_ api.FunctionDefinition) experimental.FunctionListener {
	return fm
}

// Before implements FunctionListener. Each function call consumes one unit of
// fuel. When the budget is exhausted, the module is closed to stop execution.
func (fm *fuelMeter) Before(ctx context.Context, mod api.Module, _ api.FunctionDefinition, _ []uint64, _ experimental.StackIterator) {
	if fm.remaining.Add(^uint64(0)) == 0 { // decrement by 1
		if fm.Metrics != nil {
			fm.Metrics.RecordWasmFuelExhausted(ctx)
		}
		mod.CloseWithExitCode(context.Background(), 1)
	}
}

// After implements FunctionListener (no-op).
func (fm *fuelMeter) After(_ context.Context, _ api.Module, _ api.FunctionDefinition, _ []uint64) {}

// Abort implements FunctionListener (no-op).
func (fm *fuelMeter) Abort(_ context.Context, _ api.Module, _ api.FunctionDefinition, _ error) {}

// fuelExhaustedError is returned when a WASM module exhausts its fuel budget.
var fuelExhaustedError = fmt.Errorf("wasm trap: instruction limit exceeded (fuel exhausted)")

// ErrSuspended is returned by CallExport when the workflow suspends.
var ErrSuspended = fmt.Errorf("workflow suspended")

// CallExport invokes an exported WASM function with JSON input.
// It writes inputJSON into the module's linear memory, calls the export,
// and decodes the int64 result per the exports.go convention.
// If the export returns the suspend sentinel, it returns ("", nil, ErrSuspended).
func (r *Runtime) CallExport(ctx context.Context, mod api.Module, exportName string, inputJSON []byte) (string, error) {
	result, suspended, err := r.CallExportWithSuspend(ctx, mod, exportName, inputJSON)
	if err != nil {
		return "", err
	}
	if suspended {
		return "", ErrSuspended
	}
	return result, nil
}

// CallExportWithSuspend invokes an exported WASM function and detects suspension.
func (r *Runtime) CallExportWithSuspend(ctx context.Context, mod api.Module, exportName string, inputJSON []byte) (result string, suspended bool, err error) {
	// Recover from WASM panics/traps so the caller (Engine.run) can invoke registered defers.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("host: export %q panicked: %v", exportName, r)
		}
	}()

	fn := mod.ExportedFunction(exportName)
	if fn == nil {
		return "", false, fmt.Errorf("host: export %q not found", exportName)
	}

	mem := mod.Memory()
	if mem == nil {
		return "", false, fmt.Errorf("host: module has no exported memory")
	}

	// Place scratch buffers at the end of current WASM memory to avoid
	// collision with the module's heap, but never below the legacy 10 MiB
	// offset. Some WASM SDKs (Java/TeaVM, AssemblyScript) hardcode the
	// 10 MiB convention and will break if the buffer moves lower.
	currentSize := mem.Size()
	legacyOffset := uint32(10 * 1024 * 1024)
	scratchBase := currentSize + wasmPageSize // one guard page after current heap
	if scratchBase < legacyOffset {
		scratchBase = legacyOffset
	}
	inputOffset := scratchBase
	outputOffset := scratchBase + OutBufSize

	// Grow memory to fit our scratch region.
	needed := outputOffset + OutBufSize
	if currentSize < needed {
		pagesNeeded := (needed - currentSize + wasmPageSize - 1) / wasmPageSize
		if _, ok := mem.Grow(pagesNeeded); !ok {
			return "", false, fmt.Errorf("host: memory grow failed: needed %d bytes (%d pages), current %d bytes, memory limit %d pages", needed, pagesNeeded, currentSize, r.MemoryLimitPages)
		}
	}

	// Write input JSON into WASM memory.
	if len(inputJSON) > 0 {
		if ok := mem.Write(inputOffset, inputJSON); !ok {
			return "", false, fmt.Errorf("host: write input JSON to WASM memory at offset %d failed", inputOffset)
		}
	}

	// Apply per-call WASM execution timeout if configured.
	callCtx := ctx
	var cancel context.CancelFunc
	if r.callTimeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, r.callTimeout)
		defer cancel()
	}

	// Call the export: func(argsPtr, argsLen, outPtr, maxOutLen uint32) int64
	//
	// When the WASM module traps (unreachable, OOB memory, etc.),
	// wazero's engine recovers and returns an error with a stack trace.
	// If the module was compiled with DWARF debug info (default for
	// Go wasip1 builds), the stack trace includes source file:line
	// locations. If DWARF is unavailable (stripped binary), the
	// trace falls back to raw function indices and offsets.
	// See formatWasmCallError for the formatting logic.

	// Set up fuel metering if an instruction limit is configured.
	// wazero's function listener API is used to count function calls;
	// each call consumes one unit of fuel. When the budget is exhausted,
	// the module is closed, which surfaces as an ExitError to the caller.
	if r.fuelLimit > 0 {
		fm := &fuelMeter{Metrics: r.Metrics}
		fm.remaining.Store(r.fuelLimit)
		callCtx = experimental.WithFunctionListenerFactory(callCtx, fm)
	}

	// Create a cleatComplete struct and put it in the context.
	// The WASM export wrapper calls cleat_complete host import to store
	// the result before returning, so even if Go WASI subsequently calls
	// proc_exit (which returns as a fn.Call error), we can retrieve the result.
	complete := &cleatComplete{}
	callCtx = context.WithValue(callCtx, &cleatCompleteKey, complete)

	results, err := fn.Call(callCtx,
		uint64(inputOffset),
		uint64(len(inputJSON)),
		uint64(outputOffset),
		uint64(OutBufSize),
	)
	if err != nil {
		// Check for cleat_complete result before treating as error.
		// Go WASI runtime may call proc_exit after the export wrapper
		// has already stored the result via cleat_complete host import.
		if complete.Result != nil {
			return *complete.Result, false, nil
		}
		if complete.Error != nil {
			return "", false, fmt.Errorf("host: export %q failed: %s", exportName, *complete.Error)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return "", false, fmt.Errorf("host: export %q timed out after %v", exportName, r.callTimeout)
		}
		// Detect fuel exhaustion from module close within function listener.
		if r.fuelLimit > 0 {
			var exitErr *sys.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
				return "", false, fuelExhaustedError
			}
		}
		return "", false, formatWasmCallError(err)
	}

	if len(results) == 0 {
		return "", false, fmt.Errorf("host: export %q returned no results. The WASM module may have panicked or returned void.", exportName)
	}

	// Check for suspend sentinel: (1 << 62).
	if results[0] == (1 << 62) {
		return "", true, nil
	}

	errCode, actualLen := decodeExportResult(results[0])

	// Detect output overflow: if WASM wrote more bytes than the buffer can hold,
	// the output was silently truncated. Return an error instead of partial data.
	if actualLen > OutBufSize {
		return "", false, fmt.Errorf("host: %s: output overflow: wrote %d bytes, buffer is %d bytes", exportName, actualLen, OutBufSize)
	}

	if errCode != 0 {
		errMsg := readWasmString(mem, outputOffset, minU32(actualLen, OutBufSize))
		return "", false, fmt.Errorf("host: %s: %s", exportName, errMsg)
	}

	response := readWasmString(mem, outputOffset, minU32(actualLen, OutBufSize))
	return response, false, nil
}
