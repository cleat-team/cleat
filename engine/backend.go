package engine

import (
	"context"
	"encoding/json"
	"time"
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

	// PerExecution returns a new backend instance that shares the underlying
	// compilation engine but has its own per-execution mutable state (handler,
	// work data). This is required by the engine to prevent data races when
	// Execute is called concurrently from multiple goroutines.
	//
	// tenantInstanceTimeout is this tenant's own bound on guest EXECUTION, or
	// 0 when the tenant set none. It is the tenant's RAW value, deliberately
	// unclamped: the operator's ceiling lives on the backend
	// (wasmtimeBackend.limits.executionTimeout, from --wasm-instance-timeout)
	// and the engine cannot see it, so the clamp happens in the one place both
	// numbers exist. A backend that ignores this argument is choosing to run
	// every tenant at the operator's value, which is what the pre-3.94
	// behaviour was and is safe -- the risk of the argument is a tenant
	// RAISING its bound, and only the clamp site can permit that.
	PerExecution(tenantInstanceTimeout time.Duration) WasmBackend
}
