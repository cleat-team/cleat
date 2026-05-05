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

// InstantiateModule creates a new module instance without running _start.
// Use InitModule to start the Go runtime afterwards.
func (r *Runtime) InstantiateModule(ctx context.Context, compiled wazero.CompiledModule) (api.Module, error) {
	r.stdout.Reset()
	r.stderr.Reset()
	config := wazero.NewModuleConfig().
		WithName("").
		WithStdout(&r.stdout).
		WithStderr(&r.stderr).
		WithStartFunctions()
	return r.wazeroRuntime.InstantiateModule(ctx, compiled, config)
}

// InitModule starts the Go wasip1 runtime by calling _start in a background
// goroutine. _start initializes WASI and calls main() which blocks to keep
// the module alive. After a brief pause for initialization, the module is
// ready for export calls.
func (r *Runtime) InitModule(ctx context.Context, mod api.Module) error {
	start := mod.ExportedFunction("_start")
	if start == nil {
		return fmt.Errorf("host: _start export not found")
	}
	go func() {
		start.Call(ctx)
	}()
	// Give Go runtime time to initialize WASI before main() enters its loop.
	time.Sleep(200 * time.Millisecond)
	return nil
}

// CallExport invokes an exported WASM function with JSON input.
// It writes inputJSON into the module's linear memory, calls the export,
// and decodes the int64 result per the exports.go convention.
func (r *Runtime) CallExport(ctx context.Context, mod api.Module, exportName string, inputJSON []byte) (string, error) {
	fn := mod.ExportedFunction(exportName)
	if fn == nil {
		return "", fmt.Errorf("host: export %q not found", exportName)
	}

	mem := mod.Memory()
	if mem == nil {
		return "", fmt.Errorf("host: module has no exported memory")
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
			// Grow failed — try once more with a smaller amount.
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
		return "", fmt.Errorf("host: export %q call failed: %w", exportName, err)
	}

	if len(results) == 0 {
		return "", fmt.Errorf("host: export %q returned no results", exportName)
	}

	errCode, actualLen := decodeExportResult(results[0])

	if errCode != 0 {
		errMsg := readWasmString(mem, outputOffset, minU32(actualLen, outBufSize))
		return "", fmt.Errorf("host: %s: %s", exportName, errMsg)
	}

	response := readWasmString(mem, outputOffset, minU32(actualLen, outBufSize))
	return response, nil
}
