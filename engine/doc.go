// Package engine provides the core workflow execution engine for cleat.
//
// It manages WASM module execution via pluggable backends (wasmtime, wazero),
// event-sourced workflow state, HTTP service integration, and plugin lifecycle.
// The App type orchestrates a runtime, store, service caller, and HTTP mux.
//
// Key types:
//   - App — top-level application orchestrator
//   - Runtime — WASM runtime lifecycle manager
//   - WasmBackend — interface for WASM execution backends
//   - StoreFactory / WorkflowStore — event-sourced workflow persistence
//   - HostHandler — host function wiring for WASM modules
package engine
