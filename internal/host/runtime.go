package host

import (
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
}

// NewRuntime creates a Runtime with all 14 durable_* host functions registered
// on the "env" module. WASI preview1 is also instantiated for Go wasip1 support.
func NewRuntime(ctx context.Context) (*Runtime, error) {
	rt := wazero.NewRuntime(ctx)

	// WASI is required by Go wasip1 modules for goroutine/stack management.
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	// Build the "env" host module that provides durable_* imports.
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

// InstantiateModule creates a new module instance from a compiled module.
// The Go wasip1 runtime is NOT initialized yet — call InitModule after this
// to start the runtime (calls _start which initializes WASI and then calls
// main() which blocks to keep the module alive).
func (r *Runtime) InstantiateModule(ctx context.Context, compiled wazero.CompiledModule) (api.Module, error) {
	config := wazero.NewModuleConfig().WithName("").WithStartFunctions()
	return r.wazeroRuntime.InstantiateModule(ctx, compiled, config)
}

// InitModule starts the Go wasip1 runtime by calling _start in a background
// goroutine. _start initializes WASI, then calls main() which blocks via
// <-make(chan struct{}). After this returns, the module is ready for export
// calls. The caller should allow a brief scheduling window for initialization.
func (r *Runtime) InitModule(ctx context.Context, mod api.Module) error {
	start := mod.ExportedFunction("_start")
	if start == nil {
		return fmt.Errorf("host: _start export not found")
	}
	go func() {
		start.Call(ctx)
	}()
	runtimeInitPause()
	return nil
}

// runtimeInitPause gives the _start goroutine time to initialize the Go
// wasip1 runtime before the caller attempts export calls. 100ms is generous.
// TODO: replace with proper synchronization once the module exports a ready signal.
func runtimeInitPause() { time.Sleep(200 * time.Millisecond) }

// CallExport invokes an exported WASM function with JSON input.
//
// It writes inputJSON into the module's linear memory at a scratch offset,
// calls the export with (argsPtr, argsLen, outPtr, maxOutLen), and decodes
// the int64 result per exports.go convention:
//
//	bits 0-31  = errCode (0 = success)
//	bits 32-63 = output length
func (r *Runtime) CallExport(ctx context.Context, mod api.Module, exportName string, inputJSON []byte) (string, error) {
	fn := mod.ExportedFunction(exportName)
	if fn == nil {
		return "", fmt.Errorf("host: export %q not found", exportName)
	}

	mem := mod.Memory()
	if mem == nil {
		return "", fmt.Errorf("host: module has no exported memory")
	}

	// Reserve scratch space for input and output buffers.
	scratchBase := uint32(outBufSize * 2) // start at 128KB
	inputOffset := scratchBase
	outputOffset := scratchBase + outBufSize
	needed := outputOffset + outBufSize

	// Grow memory if needed.
	currentSize := mem.Size()
	if currentSize < needed {
		pagesNeeded := (needed - currentSize + 65535) / 65536
		mem.Grow(pagesNeeded)
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
		return "", fmt.Errorf("host: export %q call failed: %w", exportName, err)
	}

	if len(results) == 0 {
		return "", fmt.Errorf("host: export %q returned no results", exportName)
	}

	// Decode the int64 result.
	errCode, actualLen := decodeExportResult(results[0])

	if errCode != 0 {
		errMsg := readWasmString(mem, outputOffset, minU32(actualLen, outBufSize))
		return "", fmt.Errorf("host: %s: %s", exportName, errMsg)
	}

	response := readWasmString(mem, outputOffset, minU32(actualLen, outBufSize))
	return response, nil
}
