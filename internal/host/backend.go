package host

import (
	"context"
	"encoding/json"
)

// ExecResult holds the result of a WASM function call.
type ExecResult struct {
	Result    string // JSON result string
	Suspended bool   // true if workflow suspended
}

// WasmBackend executes compiled WASM modules.
// Each backend owns compilation, instantiation, and host function wiring.
type WasmBackend interface {
	// Execute runs a WASM module. The session provides the HostHandler for
	// all host function implementations. The backend handles compilation,
	// instantiation, memory management, and export calling.
	Execute(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, session HostHandler) (*ExecResult, error)

	// Close releases all backend resources.
	Close(ctx context.Context) error

	// Name returns a human-readable backend name for diagnostics.
	Name() string
}
