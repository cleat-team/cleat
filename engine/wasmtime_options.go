package engine

import "time"

// This file has no build constraint (unlike backend_wasmtime.go, which
// requires cgo) so that cmd/cleat-worker and other callers can construct
// WasmtimeOption values unconditionally, regardless of whether the binary
// was built with CGO. When CGO is disabled, NewWasmtimeBackend (the stub in
// backend_wasmtime_stub.go) simply ignores the options and returns an error.

// DefaultWasmtimeExecutionTimeout bounds a single wasmtime invocation (one
// fresh execution or one replay pass) via epoch interruption when the
// caller has not configured a tighter one. This is a required safety net,
// not just a convenience default: wasmtime-go does not observe Go context
// cancellation while a WASM export call is in progress (see the comment on
// wasmtimeBackend.Execute in backend_wasmtime.go), so without a baked-in
// bound here an infinite loop in a workflow hangs the worker permanently
// regardless of any engine- or worker-level context timeout.
//
// 30s matches the wazero backend's hardcoded Runtime.callTimeout default
// (engine/runtime.go), so both backends behave the same way out of the box.
const DefaultWasmtimeExecutionTimeout = 30 * time.Second

// DefaultWasmtimeMemoryLimitBytes bounds linear memory per wasmtime store
// when the caller has not configured a tighter one via
// WithWasmtimeMemoryLimits. It matches DefaultMemoryLimitPages (32 MiB),
// the wazero backend's default (engine/runtime.go), so an operator sees the
// same memory ceiling regardless of which backend happens to run a given
// workflow.
const DefaultWasmtimeMemoryLimitBytes = int64(DefaultMemoryLimitPages) * int64(wasmPageSize)

// DefaultWasmtimeTableElementsLimit bounds indirect-function-table growth
// per wasmtime store.
//
// The 1,048,576-element figure it is derived from came from `tblMinSize` in
// wasmtimeBackend.ExecuteComponent -- the decomposition path, deleted
// 2026-09-01 (IMPROVEMENT-PLAN 3.65). The number is kept rather than
// re-derived: it was measured from real componentize-py bundles, those bundles
// have not changed, and the native Component Model path instantiates the same
// core modules with the same tables. 8x that headroom keeps existing workflows
// working while still capping unbounded/attacker-controlled table growth.
//
// If it ever needs re-deriving, the source is the largest `(table ...)` minimum
// in a componentize-py component's core modules, not anything in this repo.
const DefaultWasmtimeTableElementsLimit = 8 * 1024 * 1024

// DefaultWasmtimeInstancesLimit bounds how many module instances a single
// wasmtime store may create. Component bundles observed in this codebase
// (CPython runtime + adapters) use at most a few dozen instances; 256
// leaves generous headroom while still bounding runaway instantiation.
const DefaultWasmtimeInstancesLimit = 256

// wasmtimeLimits bundles the resource bounds applied to a wasmtime
// execution. Zero/negative fields mean "use the backend's built-in
// default" (see the Default* constants above) except instructionLimit,
// where 0 legitimately means "fuel metering disabled" — this matches the
// existing --wasm-instruction-limit flag semantics ("0 = no limit").
type wasmtimeLimits struct {
	executionTimeout   time.Duration
	instructionLimit   uint64
	memoryLimitBytes   int64
	tableElementsLimit int64
	instancesLimit     int64
}

// WasmtimeOption configures resource limits for a wasmtimeBackend, applied
// at construction time (NewWasmtimeBackend) and enforced on every store it
// creates thereafter (see wasmtimeBackend.configureStore).
type WasmtimeOption func(*wasmtimeLimits)

// WithWasmtimeExecutionTimeout bounds a single wasmtime invocation via
// epoch interruption. d <= 0 keeps DefaultWasmtimeExecutionTimeout. A
// per-call context deadline (e.g. from engine.WithWASMInstanceTimeout or
// engine.WithDefaultWorkflowTimeout), when tighter than this, still wins —
// see wasmtimeBackend.configureStore.
func WithWasmtimeExecutionTimeout(d time.Duration) WasmtimeOption {
	return func(l *wasmtimeLimits) { l.executionTimeout = d }
}

// WithWasmtimeInstructionLimit bounds fuel (roughly one unit per WASM
// instruction executed) consumed per invocation. 0 disables fuel
// metering — the wasmtime analogue of the wazero-only
// --wasm-instruction-limit flag defaulting to "no limit".
//
// Note for anyone tempted to raise this as the primary defense: fuel
// exhaustion mid-replay could in principle make replay diverge from the
// original execution if the configured limit changes between the two runs
// (e.g. a flag change), or on hardware where the same WASM instructions
// consume different amounts of fuel due to a wasmtime version skew. Epoch
// interruption (WithWasmtimeExecutionTimeout) does not have this property
// any more than wall-clock time already does for CPU-bound workflows, which
// is why it is the primary, always-on bound and fuel is an optional,
// opt-in secondary one.
func WithWasmtimeInstructionLimit(n uint64) WasmtimeOption {
	return func(l *wasmtimeLimits) { l.instructionLimit = n }
}

// WithWasmtimeMemoryLimits bounds linear memory, table elements, and
// instance counts per store (wasmtime's StoreLimits / ResourceLimiter).
// Values <= 0 keep the backend's built-in default for that dimension
// (see the Default* constants above).
func WithWasmtimeMemoryLimits(memoryBytes, tableElements, instances int64) WasmtimeOption {
	return func(l *wasmtimeLimits) {
		l.memoryLimitBytes = memoryBytes
		l.tableElementsLimit = tableElements
		l.instancesLimit = instances
	}
}
