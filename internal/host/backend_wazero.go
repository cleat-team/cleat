package host

import (
	"context"
	"encoding/json"
	"fmt"
)

// Compile-time check: wazeroBackend implements WasmBackend.
var _ WasmBackend = (*wazeroBackend)(nil)

// wazeroBackend implements WasmBackend using the wazero WASM runtime.
// It wraps the existing Runtime type and handles compilation,
// instantiation, execution, and teardown of WASM modules.
type wazeroBackend struct {
	rt *Runtime
}

// NewWazeroBackend creates a new wazeroBackend with a fresh Runtime.
func NewWazeroBackend(ctx context.Context) (*wazeroBackend, error) {
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		return nil, err
	}
	return &wazeroBackend{rt: rt}, nil
}

// Name returns a human-readable backend name for diagnostics.
func (b *wazeroBackend) Name() string {
	return "wazero"
}

// Close releases all resources held by the underlying wazero Runtime.
func (b *wazeroBackend) Close(ctx context.Context) error {
	return b.rt.Close(ctx)
}

// Runtime returns the underlying Runtime for direct access.
// Used by the Engine for backward compatibility with ExecuteCompiled
// and other methods that need direct Runtime access.
func (b *wazeroBackend) Runtime() *Runtime {
	return b.rt
}

// Execute compiles, instantiates, initialises, and runs a WASM module.
//
// The session provides the HostHandler for all host function calls
// (cleat_call, cleat_sleep, etc.). The backend wraps the context with
// the session so that host functions registered in imports.go can find
// it via handlerFromContext.
//
// Cleanup of the compiled module and instance is handled by deferred
// Close calls within this method.
func (b *wazeroBackend) Execute(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, session HostHandler) (*ExecResult, error) {
	// Step 1: Compile the WASM binary into a reusable compiled module.
	compiled, err := b.rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("host: compile module: %w", err)
	}
	defer compiled.Close(ctx)

	// Step 2: Instantiate the compiled module to create a runnable instance.
	mod, err := b.rt.InstantiateModule(ctx, compiled)
	if err != nil {
		return nil, fmt.Errorf("host: instantiate module: %w", err)
	}
	defer mod.Close(ctx)

	// Step 3: Initialize the module runtime.
	// For Go wasip1 modules, this calls _start in a background goroutine
	// to initialise the Go runtime. For non-Go modules (Rust, AssemblyScript,
	// etc.) that don't export _start, this is a no-op.
	if err := b.rt.InitModule(ctx, mod); err != nil {
		return nil, fmt.Errorf("host: init module: %w", err)
	}

	// Step 4: Wrap the context with the session HostHandler so that host
	// functions registered in imports.go (cleat_call, cleat_sleep, etc.)
	// can find the session via handlerFromContext(ctx).
	ctx = withHandler(ctx, session)

	// Step 5: Call the exported function with suspend detection.
	// CallExportWithSuspend handles writing input to WASM linear memory,
	// applying the per-call execution timeout, detecting the suspend
	// sentinel, and decoding the int64 result.
	result, suspended, err := b.rt.CallExportWithSuspend(ctx, mod, entryPoint, input)
	if err != nil {
		return nil, err
	}

	// Step 6: Return the result with suspend status.
	return &ExecResult{Result: result, Suspended: suspended}, nil
}
