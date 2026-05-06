// Package wasm generates WASM import/export stubs and host adapter code
// for the durable workflow transformer.
package wasm

import (
	"go/ast"

	"github.com/rcownie/durable/internal/analyzer"
	"github.com/rcownie/durable/internal/closure"
)

// HostFunction identifies a host function that can be imported from the
// WASM host environment (e.g., "durable_call", "durable_sleep").
type HostFunction struct {
	ImportName string // snake_case name used in //go:wasmimport
	FieldName  string // the HostCallsOptions field name (e.g., "DurableCall")
}

// All host functions that map to HostCalls interface methods.
// Higher-level wrapper methods (e.g. AwaitSignals, DurableCallTyped)
// map to the same import as their underlying low-level method.
var hostFunctions = []HostFunction{
	// Core call methods
	{"durable_call", "DurableCall"},
	{"durable_call", "DurableCallTyped"},
	{"durable_call", "DurableCallJSON"},
	{"durable_call", "DurableCallWithOptions"},
	{"durable_call", "DurableCallJSONWithOptions"},
	{"durable_call_heartbeat", "DurableCallWithHeartbeat"},
	// Sleep
	{"durable_sleep", "DurableSleep"},
	{"durable_sleep", "DurableSleepMs"},
	// Signals
	{"durable_await_signals", "DurableAwaitSignals"},
	{"durable_await_signals", "AwaitSignals"},
	// Defer
	{"durable_defer", "DurableDefer"},
	// Logging
	{"durable_log", "DurableLog"},
	{"durable_log", "LogKV"},
	// Cancellation / signals
	{"durable_poll_cancellation", "PollCancellation"},
	{"durable_poll_signal", "PollSignal"},
	// Lifecycle
	{"durable_continue_as_new", "ContinueAsNew"},
	{"durable_child_workflow", "ChildWorkflow"},
	{"durable_await_child", "AwaitChild"},
	{"durable_call_retry", "DurableCallWithRetry"},
	// Versioning
	{"durable_version", "Version"},
	{"durable_version", "MinVersion"},
	// State
	{"set_query_state", "SetQueryState"},
	// Time
	{"durable_now", "Now"},
	{"durable_now", "NowMs"},
	// Random
	{"durable_random", "Random"},
}

// UsageInfo records which host functions are actually called by the
// durable closure and provides lookup helpers for code generation.
type UsageInfo struct {
	Used map[string]bool // keyed by ImportName

	// Funcs lists the HostFunction descriptors that are actually used,
	// in a stable order (by ImportName).
	Funcs []HostFunction
}

// AnalyzeUsage scans every function in the durable closure and returns
// which HostCalls methods are called.
func AnalyzeUsage(result *analyzer.AnalysisResult, cr *closure.Result) *UsageInfo {
	info := &UsageInfo{
		Used: make(map[string]bool),
	}

	// Build the set of functions in the durable closure.
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
		if importName, ok := fieldToImport[fieldName]; ok {
			info.Used[importName] = true
		}
		return true
	})
}

// Count returns the number of used host functions.
func (u *UsageInfo) Count() int {
	return len(u.Funcs)
}
