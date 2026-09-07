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
	// ...and cleat_call_retry, because these are the ergonomic forms that carry
	// a RetryPolicy. Without these two lines the import is never wired for
	// them, so cleat/runtime.go's host-retry branch is unreachable and every
	// Go retry policy silently becomes an SDK-level loop that suspends once per
	// backoff -- N segments where Rust's identical policy is one, and not
	// dead-letterable where Rust's is. IMPROVEMENT-PLAN 3.88.
	//
	// A guest that uses these WITHOUT a RetryPolicy pays one unused import
	// entry; the host registers only what a module asks for
	// (wasmtimeBackend.skipIfNotNeeded), so the cost is the import list, not a
	// host function that runs.
	{"cleat_call_retry", "DurableCallWithOptions"},
	{"cleat_call_retry", "DurableCallJSONWithOptions"},
	{"cleat_call_heartbeat", "DurableCallWithHeartbeat"},
	// Sleep
	{"cleat_sleep", "DurableSleep"},
	{"cleat_sleep", "DurableSleepMs"},
	// Signals
	{"cleat_await_signals", "DurableAwaitSignals"},
	// IMPROVEMENT-PLAN 3.224: this row was missing, so a Go workflow calling
	// h.SignalWorkflow(...) compiled with no cleat_signal_workflow import at all.
	// Rust, Java and AssemblyScript all bound it; Go alone did not. The engine
	// half has always worked -- SignalWorkflow is the one signalling path that
	// does call DeliverSignal (engine/signaller.go) -- so this was the engine
	// able to deliver a signal between workflows and no Go guest able to ask.
	{"cleat_signal_workflow", "SignalWorkflow"},
	// IMPROVEMENT-PLAN 3.224, same omission: a Go workflow calling
	// h.ScheduleInvoke(...) compiled with no cleat_schedule_invoke import.
	// The three methods of IMPROVEMENT-PLAN 3.226: a public Go method and a
	// host export with no path between them. Every other SDK -- Python, Rust,
	// Java, AssemblyScript -- exposes all three; Go was the only one that did
	// not, which is what settled whether they were meant to be callable from a
	// workflow at all.
	{"cleat_send", "DurableSend"},
	{"cleat_resolve_promise", "ResolvePromise"},
	{"cleat_reject_promise", "RejectPromise"},
	{"cleat_schedule_invoke", "ScheduleInvoke"},
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
	// Promises
	{"cleat_create_promise", "CreatePromise"},
	{"cleat_await_promise", "AwaitPromise"},
	// Update handlers
	{"cleat_register_update_handler", "RegisterUpdateHandler"},
	{"cleat_poll_update", "PollUpdate"},
	{"cleat_complete_update", "CompleteUpdate"},
	{"plugin_call", "PluginCall"},
	{"plugin_call_streaming", "PluginCallStreaming"},
	// Fetch / HTTP methods (all map to durable_call import)
	{"cleat_call", "DurableFetch"},
	{"cleat_call", "DurableFetchJSON"},
	{"cleat_call", "FetchGet"},
	{"cleat_call", "FetchGetJSON"},
	// Detached execution. This row carried an EMPTY import name until the Go
	// signature changed -- tracked deliberately, because it took a closure and
	// a closure cannot cross the ABI. The consequence was that RunDetached
	// worked under localdev and cleattest, which populate the field directly,
	// and silently did nothing in every compiled workflow: the unwired branch
	// returned nil. A test double succeeding where production is a no-op is the
	// same shape as the signal defects in 0b/0c. The signature now matches the
	// host call and every other SDK, so the import is real.
	//
	// The empty-name row is described rather than quoted on purpose:
	// sdk_import_names_test.go scans this whole FILE for row literals, so a
	// literal in a comment is counted as a row and disagrees with the row count
	// taken from the slice itself.
	{"cleat_run_detached", "RunDetached"},
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

// compositeRequires maps an SDK wrapper method to the imports its
// implementation needs.
//
// It is deliberately NOT part of hostFunctions. That table is bidirectional:
// info.Funcs is built from it, and every entry emits an adapter FIELD named
// FieldName implemented by ImportName's body. Adding a wrapper there invents a
// field -- {"cleat_call", "DurableCallWithHeartbeat"} emitted a
// DurableCallWithHeartbeat closure carrying a heartbeatIntervalMs parameter
// that cleat_call's body never reads, and the generated guest failed to compile
// with "declared and not used". Wrappers are SDK-level; they need the inner
// IMPORT wired and no field of their own.
//
// Why any of this is needed: AnalyzeUsage scans the user's AST for
// h.<Method>(...) and does not follow into the SDK, so a wrapper implemented in
// terms of another host call contributes no import. The inner adapter field
// then stays nil and HostCallsImpl's nil branch returns a zero value -- in a
// compiled workflow, with no build or deploy error. h.NewUUID() returned
// 00000000-0000-4000-8000-000000000000 in every workflow, and a body of
// h.Log(...) plus h.Call(...) compiled with no host calls wired at all. See #775.
//
// TestEveryCompositeHostCallHasAnImportRow derives this from the SDK source and
// fails when a wrapper is added without an entry.
var compositeRequires = map[string][]string{
	// NowMs is not a composite in the h.Method(...) sense -- it invokes the
	// `now` CLOSURE FIELD directly, exactly as Now() does -- which is why
	// TestEveryCompositeHostCallHasAnImportRow does not see it: that scan looks
	// for h.<Uppercase>(, and this calls h.now(). Without a row here a workflow
	// whose only host call is h.NowMs() compiled with ZERO host functions and
	// the method returned 0, an epoch timestamp, silently.
	//
	// cleat_now is enough: info.Funcs is hostFunctions filtered by Used, so
	// marking the import used pulls in {"cleat_now", "Now"} and the emitted Now
	// field is what populates h.now. IMPROVEMENT-PLAN 3.234.
	"NowMs":                  {"cleat_now"},
	"NewUUID":                {"cleat_random"},
	"NewUUIDv7":              {"cleat_random", "cleat_now"},
	"UUID":                   {"cleat_workflow_id"},
	"Log":                    {"cleat_log"},
	"Call":                   {"cleat_call"},
	"AwaitCondition":         {"cleat_await_signals", "cleat_now", "cleat_complete_update", "cleat_log", "cleat_poll_update"},
	"AwaitSignalsWithQuorum": {"cleat_await_signals", "cleat_poll_update", "cleat_complete_update", "cleat_log"},
	// Delegates to AwaitPromise, which is the dispatch point, so it reaches the
	// update imports too. Merged into the existing row rather than added as a
	// second one -- a duplicate map key does not override, it fails to compile,
	// which is how this was caught.
	"AwaitPromiseMs": {"cleat_await_promise", "cleat_poll_update", "cleat_complete_update", "cleat_log"},

	// SendSignalAndWait and ReplyToSignal are composites, not host calls:
	// the reply channel is a promise and its ID is the correlation ID, so
	// request/reply needs no ABI of its own (IMPROVEMENT-PLAN 3.220). Each
	// row is the transitive set its method actually reaches -- send, create
	// the reply promise, await it; reply by resolving it.
	"SendSignalAndWait": {"cleat_create_promise", "cleat_signal_workflow", "cleat_await_promise", "cleat_poll_update", "cleat_complete_update", "cleat_log"},
	// DispatchUpdates polls, runs the handler, and completes -- plus DurableLog
	// on the two paths where the host hands back something this SDK cannot use.
	"AwaitChildTyped": {"cleat_complete_update", "cleat_log", "cleat_poll_update"},
	"DispatchUpdates": {"cleat_poll_update", "cleat_complete_update", "cleat_log"},
	// EVERY SUSPENSION POINT IS A DISPATCH POINT, so each one transitively
	// needs the update imports. This is not bookkeeping: without these rows the
	// closure analysis omits cleat_poll_update from a workflow whose only
	// suspension is a sleep, and the call the SDK makes there fails to link.
	//
	// The cost is real and worth naming -- a workflow that never uses updates
	// still imports both calls, because the dispatch point is unconditional.
	// See cleat.HostCallsImpl.DispatchUpdates for why it has to be.
	"DurableSleep":                  {"cleat_poll_update", "cleat_complete_update", "cleat_log"},
	"DurableSleepMs":                {"cleat_poll_update", "cleat_complete_update", "cleat_log"},
	"AwaitSignals":                  {"cleat_poll_update", "cleat_complete_update", "cleat_log"},
	"AwaitPromise":                  {"cleat_poll_update", "cleat_complete_update", "cleat_log"},
	"AwaitChild":                    {"cleat_poll_update", "cleat_complete_update", "cleat_log"},
	"AwaitAllChildren":              {"cleat_poll_update", "cleat_complete_update", "cleat_log"},
	"AwaitAnyChild":                 {"cleat_poll_update", "cleat_complete_update", "cleat_log"},
	"ReplyToSignal":                 {"cleat_resolve_promise"},
	"PollSignals":                   {"cleat_poll_signal"},
	"ChildWorkflowWithOptions":      {"cleat_child_workflow"},
	"DurableCallWithHeartbeat":      {"cleat_call"},
	"DurableCallTypedWithHeartbeat": {"cleat_call"},
	// DurableCallWithOptions sleeps between retry attempts, so a workflow that
	// sets a RetryPolicy and never calls DurableSleep itself would compile with
	// cleat_sleep unwired and back off for no time at all.
	"DurableCallWithOptions":      {"cleat_sleep", "cleat_poll_update", "cleat_complete_update", "cleat_log"},
	"DurableCallTypedWithOptions": {"cleat_call", "cleat_call_retry", "cleat_sleep", "cleat_complete_update", "cleat_log", "cleat_poll_update"},
	"DurableCallJSONWithOptions":  {"cleat_sleep", "cleat_complete_update", "cleat_log", "cleat_poll_update"},
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
		// Wrappers implemented over another host call. See compositeRequires.
		for _, importName := range compositeRequires[fieldName] {
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
