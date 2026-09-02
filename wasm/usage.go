// Package wasm generates WASM import/export stubs and host adapter code
// for the cleat workflow transformer.
//
// Supported wasm compilation targets:
//   - "go"           — Standard Go wasip1 (go build with GOOS=wasip1 GOARCH=wasm)
//   - "rust"         — Rust via cargo + wasm32-wasip1
//   - "java"         — Java via Gradle + TeaVM
//   - "assemblyscript" — AssemblyScript via asc
//   - "python"       — Python via componentize-py
package wasm

import (
	"go/ast"
	"go/token"
	"strings"

	"github.com/cleat-team/cleat/internal/analyzer"
	"github.com/cleat-team/cleat/internal/closure"
)

// GoTarget identifies the standard Go WASM compilation target.
// Uses GOOS=wasip1 GOARCH=wasm go build.
const GoTarget = "go"

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
	{"cleat_defer", "DurableDeferFunc"},
	// Logging
	{"cleat_log", "DurableLog"},
	{"cleat_log", "LogKV"},
	// Cancellation / signals
	{"cleat_poll_cancellation", "PollCancellation"},
	{"cleat_poll_signal", "PollSignal"},
	// Lifecycle
	{"cleat_continue_as_new", "ContinueAsNew"},
	{"cleat_continue_as_new_versioned", "ContinueAsNewWithVersion"},
	{"cleat_child_workflow", "ChildWorkflow"},
	{"cleat_child_workflow_with_options", "ChildWorkflowWithOptions"},
	{"cleat_child_workflow", "ChildWorkflowTyped"},
	{"cleat_await_child", "AwaitChild"},
	{"cleat_await_all_children", "AwaitAllChildren"},
	{"cleat_await_any_child", "AwaitAnyChild"},
	{"cleat_poll_child", "PollChild"},
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
	// Random
	{"cleat_random", "Random"},
	// Lock/concurrency key operations
	{"cleat_acquire_lock", "AcquireLock"},
	{"cleat_acquire_lock", "AcquireLockMs"},
	{"cleat_release_lock", "ReleaseLock"},
	// SideEffect
	{"cleat_side_effect", "SideEffect"},
	// Identity
	{"cleat_workflow_id", "WorkflowID"},
	{"cleat_run_id", "RunID"},
	// Cron schedules
	{"cleat_schedule_cron", "ScheduleCron"},
	{"cleat_delete_cron", "DeleteCron"},
	{"cleat_list_crons", "ListCrons"},
}

// UsageInfo records which host functions are actually called by the
// cleat closure and provides lookup helpers for code generation.
type UsageInfo struct {
	Used map[string]bool // keyed by ImportName

	// Funcs lists the HostFunction descriptors that are actually used,
	// in a stable order (by ImportName).
	Funcs []HostFunction

	// Children records child workflow names detected in the AST.
	// Keys are the first argument string literals of h.ChildWorkflow(name, ...),
	// h.ChildWorkflowWithOptions(name, ...), and h.ChildWorkflowTyped(name, ...).
	Children map[string]bool
}

// AnalyzeUsage scans every function in the cleat closure and returns
// which HostCalls methods are called.
func AnalyzeUsage(result *analyzer.AnalysisResult, cr *closure.Result) *UsageInfo {
	info := &UsageInfo{
		Used:     make(map[string]bool),
		Children: make(map[string]bool),
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

	// Incorporate //cleat:require directives from source comments.
	collectRequirements(result, info)

	// Build the stable ordered list of used functions.
	for _, hf := range hostFunctions {
		if info.Used[hf.ImportName] {
			info.Funcs = append(info.Funcs, hf)
		}
	}

	return info
}

// collectRequirements scans the target package's source files for
// //cleat:require directives and adds the listed host functions to info.Used.
//
// Directive format:
//
//	//cleat:require ChildWorkflowWithOptions,AwaitAnyChild
//
// The directive names HostCallsOptions field names (not import names). They
// are resolved to import names via the hostFunctions table.
func collectRequirements(result *analyzer.AnalysisResult, info *UsageInfo) {
	fieldToImport := make(map[string]string)
	for _, hf := range hostFunctions {
		fieldToImport[hf.FieldName] = hf.ImportName
	}

	for _, file := range result.TargetPkg.Files {
		for _, cg := range file.Comments {
			for _, c := range cg.List {
				text := c.Text
				const prefix = "//cleat:require "
				if !strings.HasPrefix(text, prefix) {
					continue
				}
				rest := text[len(prefix):]
				for _, field := range strings.Split(rest, ",") {
					field = strings.TrimSpace(field)
					if importName, ok := fieldToImport[field]; ok {
						info.Used[importName] = true
					}
				}
			}
		}
	}
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
			// Check for PluginCaller methods — map to plugin_call imports.
			if analyzer.PluginCallerMethod(sel) {
				info.Used["plugin_call"] = true
				info.Used["plugin_call_streaming"] = true
			}
			return true
		}
		fieldName := selExpr.Sel.Name
		if importName, ok := fieldToImport[fieldName]; ok && importName != "" {
			info.Used[importName] = true
		}

		// Extract child workflow name from the first string literal argument.
		if fieldName == "ChildWorkflow" || fieldName == "ChildWorkflowWithOptions" || fieldName == "ChildWorkflowTyped" {
			if len(call.Args) > 0 {
				if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					name := strings.Trim(lit.Value, `"`)
					info.Children[name] = true
				}
			}
		}
		return true
	})
}

// Count returns the number of used host functions.
func (u *UsageInfo) Count() int {
	return len(u.Funcs)
}
