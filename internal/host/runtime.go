package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)


// cleatComplete stores the workflow result delivered via cleat_complete host import.
// This decouples workflow completion from Go WASI runtime shutdown behavior.
type cleatComplete struct {
	Result *string // JSON result (status=0)
	Error  *string // error message (status=1)
}

var cleatCompleteKey struct{}

var wazeroInitOnce sync.Once
// Runtime wraps a wazero runtime with pre-registered host function imports.
type Runtime struct {
	wazeroRuntime wazero.Runtime
	stdout        bytes.Buffer
	stderr        bytes.Buffer
	callTimeout   time.Duration // per-call WASM execution timeout (0 = none)

		// Fields for cleat_complete host function (see imports.go).
		// When the WASM export calls cleat_complete before returning,
		// the result is stored here so CallExportWithSuspend can
		// return it even if the Go WASI runtime subsequently calls
		// proc_exit (which would otherwise overwrite the result).
		completeMu     sync.Mutex
		completeResult *string
		completeErr    *string
}

// Stdout returns captured stdout output from the most recent module.
func (r *Runtime) Stdout() string { return r.stdout.String() }

// Stderr returns captured stderr output from the most recent module.
func (r *Runtime) Stderr() string { return r.stderr.String() }

// NewRuntime creates a Runtime with all cleat_* host functions and the plugin_call
// host function registered on the "env" module. WASI preview1 is also instantiated
// for Go wasip1 support. Plugin host functions are registered via the Engine's
// PluginRegistry — not through NewRuntime.
func NewRuntime(ctx context.Context) (*Runtime, error) {
	wazeroInitOnce.Do(func() {
		dummy := wazero.NewRuntime(context.Background())
		dummy.Close(context.Background())
	})

	rtCfg := wazero.NewRuntimeConfigCompiler().
		WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExtendedConst)
	rt := wazero.NewRuntimeWithConfig(ctx, rtCfg)

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
	if _, err := wasiBuilder.Instantiate(ctx); err != nil {
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

	registerHostFunctions(envBuilder)
	if _, err := envBuilder.Instantiate(ctx); err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("host: instantiating env module: %w", err)
	}

	return &Runtime{wazeroRuntime: rt, callTimeout: 30 * time.Second}, nil
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
	go func() {
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
		case <-time.After(delay):
		}

		// Liveness check: memory is allocated during instantiation, but
		// if _start panicked the module may have been torn down, so verify
		// memory is still accessible.
		if mem := mod.Memory(); mem != nil && mem.Size() > 0 {
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

	// Reserve scratch space. Use high offsets to avoid conflicts with the
	// Go runtime's own stack/heap (which grows from low addresses).
	scratchBase := uint32(10 * 1024 * 1024) // 10MB offset
	inputOffset := scratchBase
	outputOffset := scratchBase + outBufSize

	// Grow memory to fit our scratch region.
	needed := outputOffset + outBufSize
	currentSize := mem.Size()
	if currentSize < needed {
		pagesNeeded := (needed - currentSize + 65535) / 65536
		if _, ok := mem.Grow(pagesNeeded); !ok {
			if _, ok := mem.Grow(1); !ok {
				return "", false, fmt.Errorf("host: memory grow failed: needed %d bytes (%d pages), current %d bytes", needed, pagesNeeded, currentSize)
			}
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
		uint64(outBufSize),
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
	if actualLen > outBufSize {
		return "", false, fmt.Errorf("host: %s: output overflow: wrote %d bytes, buffer is %d bytes", exportName, actualLen, outBufSize)
	}

	if errCode != 0 {
		errMsg := readWasmString(mem, outputOffset, minU32(actualLen, outBufSize))
		return "", false, fmt.Errorf("host: %s: %s", exportName, errMsg)
	}

	response := readWasmString(mem, outputOffset, minU32(actualLen, outBufSize))
	return response, false, nil
}
