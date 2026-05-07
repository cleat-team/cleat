package host

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)


// Runtime wraps a wazero runtime with pre-registered host function imports.
type Runtime struct {
	wazeroRuntime wazero.Runtime
	stdout        bytes.Buffer
	stderr        bytes.Buffer
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
	rt := wazero.NewRuntime(ctx)

	// WASI is required by Go wasip1 modules for goroutine/stack management.
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	// Build the "env" host module that provides cleat_* imports.
	envBuilder := rt.NewHostModuleBuilder("env")
	registerHostFunctions(envBuilder)


	if _, err := envBuilder.Instantiate(ctx); err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("host: instantiating env module: %w", err)
	}

	return &Runtime{wazeroRuntime: rt}, nil
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
// the module alive. After a brief pause for initialization, the module is
// ready for export calls.
//
// For non-Go modules (e.g., Rust, C) that don't have _start, this is a no-op.
func (r *Runtime) InitModule(ctx context.Context, mod api.Module) error {
	start := mod.ExportedFunction("_start")
	if start == nil {
		return nil // Non-Go modules don't need _start.
	}
	go func() {
		start.Call(ctx)
	}()
	// Give Go runtime time to initialize WASI before main() enters its loop.
	time.Sleep(200 * time.Millisecond)
	return nil
}

// InstantiateAndInit compiles, instantiates, and initialises a WASM module.
// Convenience wrapper used by tests and the worker.
func (r *Runtime) InstantiateAndInit(ctx context.Context, wasmBytes []byte) (api.Module, error) {
	compiled, err := r.CompileModule(ctx, wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("compile: %w", err)
	}
	// Note: compiled.Close is deferred to the caller via mod.Close.
	mod, err := r.InstantiateModule(ctx, compiled)
	if err != nil {
		compiled.Close(ctx)
		return nil, fmt.Errorf("instantiate: %w", err)
	}
	if err := r.InitModule(ctx, mod); err != nil {
		mod.Close(ctx)
		return nil, fmt.Errorf("init: %w", err)
	}
	return mod, nil
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
			mem.Grow(1)
		}
	}

	// Write input JSON into WASM memory.
	if len(inputJSON) > 0 {
		mem.Write(inputOffset, inputJSON)
	}

	// Call the export: func(argsPtr, argsLen, outPtr, maxOutLen uint32) int64
	results, err := fn.Call(ctx,
		uint64(inputOffset),
		uint64(len(inputJSON)),
		uint64(outputOffset),
		uint64(outBufSize),
	)
	if err != nil {
		return "", false, fmt.Errorf("host: export %q call failed: %w", exportName, err)
	}

	if len(results) == 0 {
		return "", false, fmt.Errorf("host: export %q returned no results", exportName)
	}

	// Check for suspend sentinel: (1 << 62).
	if results[0] == (1 << 62) {
		return "", true, nil
	}

	errCode, actualLen := decodeExportResult(results[0])

	if errCode != 0 {
		errMsg := readWasmString(mem, outputOffset, minU32(actualLen, outBufSize))
		return "", false, fmt.Errorf("host: %s: %s", exportName, errMsg)
	}

	response := readWasmString(mem, outputOffset, minU32(actualLen, outBufSize))
	return response, false, nil
}
