// Package wasm generates WASM import/export stubs and host adapter code
// for the cleat workflow transformer.
//
// Supported wasm compilation targets:
//   - "go" (default) — standard Go wasip1/wasm
//   - "tinygo"       — TinyGo wasip1 (for smaller binary size)
//   - "rust"         — Rust via cargo + wasm32-wasip1
//   - "java"         — Java via Gradle + TeaVM
//   - "assemblyscript" — AssemblyScript via asc
//   - "python"       — Python via componentize-py
package wasm

import (
	"go/ast"

	"github.com/rcownie/cleat/internal/analyzer"
	"github.com/rcownie/cleat/internal/closure"
)

// PythonTarget identifies the Python WASM compilation target.
// Used by the Go build system to dispatch to the componentize-py pipeline.
const PythonTarget = "python"

// HostFunction identifies a host function that can be imported from the
// WASM host environment (e.g., "cleat_call", "cleat_sleep").
type HostFunction struct {
	ImportName string // snake_case name used in //go:wasmimport
	FieldName  string // the HostCallsOptions field name (e.g., "DurableCall")
}

// All host functions that map to HostCalls interface methods.
// Higher-level wrapper methods (e.g. AwaitSignals, DurableCallTyped)
// map to the same import as their underlying low-level method.
var hostFunctions = []HostFunction{
	// Core call methods
	{"cleat_call", "DurableCall"},
	{"cleat_call", "DurableCallTyped"},
	{"cleat_call", "DurableCallJSON"},
	{"cleat_call", "DurableCallWithOptions"},
	{"cleat_call", "DurableCallJSONWithOptions"},
	{"cleat_call_heartbeat", "DurableCallWithHeartbeat"},
	// Sleep
	{"cleat_sleep", "DurableSleep"},
	{"cleat_sleep", "DurableSleepMs"},
	// Signals
	{"cleat_await_signals", "DurableAwaitSignals"},
	{"cleat_await_signals", "AwaitSignals"},
	// Defer
	{"cleat_defer", "DurableDefer"},
	// Logging
	{"cleat_log", "DurableLog"},
	{"cleat_log", "LogKV"},
	// Cancellation / signals
	{"cleat_poll_cancellation", "PollCancellation"},
	{"cleat_poll_signal", "PollSignal"},
	// Lifecycle
	{"cleat_continue_as_new", "ContinueAsNew"},
	{"cleat_child_workflow", "ChildWorkflow"},
	{"cleat_child_workflow", "ChildWorkflowTyped"},
	{"cleat_await_child", "AwaitChild"},
	{"cleat_await_all_children", "AwaitAllChildren"},
	{"cleat_await_child", "AwaitChildTyped"},
	{"cleat_call_retry", "DurableCallWithRetry"},
	// Versioning
	{"cleat_version", "Version"},
	{"cleat_min_version", "MinVersion"},
	// State
	{"set_query_state", "SetQueryState"},
	// State mutation methods (all map to set_query_state import)
	{"set_query_state", "SetState"},
	{"set_query_state", "DeleteState"},
	{"set_query_state", "IncrState"},
	// Promises
	{"cleat_create_promise", "CreatePromise"},
	{"cleat_await_promise", "AwaitPromise"},
	// Update handlers
	{"cleat_register_update_handler", "RegisterUpdateHandler"},
	{"plugin_call", "PluginCall"},
	{"plugin_call_streaming", "PluginCallStreaming"},
	// Fetch / HTTP methods (all map to durable_call import)
	{"cleat_call", "DurableFetch"},
	{"cleat_call", "DurableFetchJSON"},
	{"cleat_call", "FetchGet"},
	{"cleat_call", "FetchGetJSON"},
	// Detached execution (no WASM import needed, but tracked so it's not silently ignored)
	{"", "RunDetached"},
	// Heartbeat variants
	{"cleat_call_heartbeat", "DurableCallTypedWithHeartbeat"},
	// Time
	{"cleat_now", "Now"},
	{"cleat_now", "NowMs"},
	// Random
	{"cleat_random", "Random"},
}

// UsageInfo records which host functions are actually called by the
// cleat closure and provides lookup helpers for code generation.
type UsageInfo struct {
	Used map[string]bool // keyed by ImportName

	// Funcs lists the HostFunction descriptors that are actually used,
	// in a stable order (by ImportName).
	Funcs []HostFunction
}

// AnalyzeUsage scans every function in the cleat closure and returns
// which HostCalls methods are called.
func AnalyzeUsage(result *analyzer.AnalysisResult, cr *closure.Result) *UsageInfo {
	info := &UsageInfo{
		Used: make(map[string]bool),
	}

	// Build the set of functions in the cleat closure.
	durableSet := make(map[string]bool)
	for name := range cr.DurableLeaves {
		durableSet[name] = true
	}
	for name := range cr.DurableClosure {
		durableSet[name] = true
	}

	// Scan each durable function for HostCalls field calls.
	for _, fd := range result.Funcs {
		if !durableSet[fd.FullyQualifiedName()] {
			continue
		}
		collectHostCallsCalls(fd, info)
	}

	// Build the stable ordered list of used functions.
	for _, hf := range hostFunctions {
		if info.Used[hf.ImportName] {
			info.Funcs = append(info.Funcs, hf)
		}
	}

	return info
}

// collectHostCallsCalls walks a function body and records which HostCalls
// methods are called.
func collectHostCallsCalls(fd *analyzer.FuncDecl, info *UsageInfo) {
	if fd.Ast.Body == nil || fd.Pkg.Info == nil {
		return
	}

	// Build a map from field name to import name for quick lookup.
	fieldToImport := make(map[string]string)
	for _, hf := range hostFunctions {
		fieldToImport[hf.FieldName] = hf.ImportName
	}

	ast.Inspect(fd.Ast.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		selExpr, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		sel, ok := fd.Pkg.Info.Selections[selExpr]
		if !ok {
			return true
		}
		if !analyzer.HostCallsMethod(sel) {
			return true
		}
		fieldName := selExpr.Sel.Name
		if importName, ok := fieldToImport[fieldName]; ok && importName != "" {
			info.Used[importName] = true
		}
		return true
	})
}

// Count returns the number of used host functions.
func (u *UsageInfo) Count() int {
	return len(u.Funcs)
}
