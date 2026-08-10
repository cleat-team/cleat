package wasm

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/cleat-team/cleat/internal/analyzer"
)

// paramKind describes how a parameter is passed across the WASM boundary.
type paramKind int

const (
	kindInString  paramKind = iota // input string: (ptr unsafe.Pointer, len uint32)
	kindOutString                  // output string buffer: (outPtr unsafe.Pointer, maxLen uint32)
	kindInt64                      // int64
	kindInt32                      // int32
)

// importDef defines the signature of a single //go:wasmimport stub.
type importDef struct {
	ImportName string      // e.g. "cleat_call"
	Params     []paramSpec // input params + output buffers
}

type paramSpec struct {
	Name string
	Kind paramKind
}

// importDefs maps each host function to its low-level WASM import signature.
var importDefs = map[string]importDef{
	"cleat_poll_work": {
		ImportName: "cleat_poll_work",
		Params: []paramSpec{
			{"entryName", kindOutString},
			{"argsJSON", kindOutString},
		},
	},
	"cleat_complete": {
		ImportName: "cleat_complete",
		Params: []paramSpec{
			{"status", kindInt32},
			{"result", kindInString},
		},
	},
	"cleat_call": {
		ImportName: "cleat_call",
		Params: []paramSpec{
			{"service", kindInString},
			{"operation", kindInString},
			{"requestJSON", kindInString},
			{"response", kindOutString},
		},
	},
	"cleat_sleep": {
		ImportName: "cleat_sleep",
		Params: []paramSpec{
			{"durationMs", kindInt64},
		},
	},
	"cleat_await_signals": {
		ImportName: "cleat_await_signals",
		Params: []paramSpec{
			{"signalNames", kindInString},
			{"timeoutMs", kindInt64},
			{"signalName", kindOutString},
			{"payload", kindOutString},
		},
	},
	"cleat_defer": {
		ImportName: "cleat_defer",
		Params: []paramSpec{
			{"description", kindInString},
			{"deferID", kindOutString},
		},
	},
	"cleat_log": {
		ImportName: "cleat_log",
		Params: []paramSpec{
			{"message", kindInString},
		},
	},
	"cleat_poll_cancellation": {
		ImportName: "cleat_poll_cancellation",
		Params: []paramSpec{
			{"reason", kindOutString},
		},
	},
	"cleat_poll_signal": {
		ImportName: "cleat_poll_signal",
		Params: []paramSpec{
			{"signalName", kindInString},
			{"payload", kindOutString},
		},
	},
	"cleat_continue_as_new": {
		ImportName: "cleat_continue_as_new",
		Params: []paramSpec{
			{"newInputJSON", kindInString},
		},
	},
	"cleat_continue_as_new_versioned": {
		ImportName: "cleat_continue_as_new_versioned",
		Params: []paramSpec{
			{"newInputJSON", kindInString},
			{"newVersion", kindInt64},
		},
	},
	"cleat_child_workflow": {
		ImportName: "cleat_child_workflow",
		Params: []paramSpec{
			{"name", kindInString},
			{"inputJSON", kindInString},
			{"runID", kindOutString},
		},
	},
	"cleat_child_workflow_with_options": {
		ImportName: "cleat_child_workflow_with_options",
		Params: []paramSpec{
			{"name", kindInString},
			{"inputJSON", kindInString},
			{"version", kindInt64},
			{"priority", kindInt64},
			{"parentClosePolicy", kindInString},
			{"runID", kindOutString},
		},
	},
	"cleat_await_child": {
		ImportName: "cleat_await_child",
		Params: []paramSpec{
			{"runID", kindInString},
			{"result", kindOutString},
		},
	},
	"cleat_call_retry": {
		ImportName: "cleat_call_retry",
		Params: []paramSpec{
			{"service", kindInString},
			{"operation", kindInString},
			{"requestJSON", kindInString},
			{"maxAttempts", kindInt64},
			{"initialIntervalMs", kindInt64},
			{"backoffCoefficient100x", kindInt64},
			{"maxIntervalMs", kindInt64},
			{"nonRetryableErrorsJSON", kindInString},
			{"response", kindOutString},
		},
	},
	"cleat_await_all_children": {
		ImportName: "cleat_await_all_children",
		Params: []paramSpec{
			{"runIDs", kindInString},
			{"results", kindOutString},
		},
	},
	"cleat_poll_child": {
		ImportName: "cleat_poll_child",
		Params: []paramSpec{
			{"runID", kindInString},
			{"result", kindOutString},
		},
	},
	"cleat_await_any_child": {
		ImportName: "cleat_await_any_child",
		Params: []paramSpec{
			{"runIDs", kindInString},
			{"result", kindOutString},
		},
	},
	"cleat_call_heartbeat": {
		ImportName: "cleat_call_heartbeat",
		Params: []paramSpec{
			{"service", kindInString},
			{"operation", kindInString},
			{"requestJSON", kindInString},
			{"heartbeatIntervalMs", kindInt64},
			{"response", kindOutString},
		},
	},
	"cleat_version": {
		ImportName: "cleat_version",
	},
	"cleat_min_version": {
		ImportName: "cleat_min_version",
	},
	"set_query_state": {
		ImportName: "set_query_state",
		Params: []paramSpec{
			{"key", kindInString},
			{"val", kindInString},
		},
	},
	"cleat_create_promise": {
		ImportName: "cleat_create_promise",
		Params: []paramSpec{
			{"name", kindInString},
			{"promise_id_out", kindOutString},
		},
	},
	"cleat_await_promise": {
		ImportName: "cleat_await_promise",
		Params: []paramSpec{
			{"promise_id", kindInString},
			{"timeout_ms", kindInt64},
			{"result_out", kindOutString},
		},
	},
	"cleat_now": {
		ImportName: "cleat_now",
	},
	"cleat_random": {
		ImportName: "cleat_random",
	},
	"cleat_register_update_handler": {
		ImportName: "cleat_register_update_handler",
		Params: []paramSpec{
			{"name", kindInString},
		},
	},
	"plugin_call": {
		ImportName: "plugin_call",
		Params: []paramSpec{
			{"pluginName", kindInString},
			{"functionName", kindInString},
			{"inputJSON", kindInString},
			{"response", kindOutString},
		},
	},
	"plugin_call_streaming": {
		ImportName: "plugin_call_streaming",
		Params: []paramSpec{
			{"pluginName", kindInString},
			{"functionName", kindInString},
			{"inputJSON", kindInString},
			{"response", kindOutString},
		},
	},
	"cleat_acquire_lock": {
		ImportName: "cleat_acquire_lock",
		Params: []paramSpec{
			{"key", kindInString},
			{"ttl_ms", kindInt64},
		},
	},
	"cleat_side_effect": {
		ImportName: "cleat_side_effect",
		Params: []paramSpec{
			{"result", kindInString},
			{"cachedResult", kindOutString},
		},
	},
	"cleat_release_lock": {
		ImportName: "cleat_release_lock",
		Params: []paramSpec{
			{"key", kindInString},
		},
	},
	"cleat_workflow_id": {
		ImportName: "cleat_workflow_id",
		Params: []paramSpec{
			{"id", kindOutString},
		},
	},
	"cleat_run_id": {
		ImportName: "cleat_run_id",
		Params: []paramSpec{
			{"id", kindOutString},
		},
	},
	"cleat_schedule_cron": {
		ImportName: "cleat_schedule_cron",
		Params: []paramSpec{
			{"workflowName", kindInString},
			{"cronExpr", kindInString},
			{"timezone", kindInString},
			{"inputJSON", kindInString},
			{"scheduleID", kindOutString},
		},
	},
	"cleat_delete_cron": {
		ImportName: "cleat_delete_cron",
		Params: []paramSpec{
			{"scheduleID", kindInString},
		},
	},
	"cleat_list_crons": {
		ImportName: "cleat_list_crons",
		Params: []paramSpec{
			{"result", kindOutString},
		},
	},
	"cleat_json_parse": {
		ImportName: "cleat_json_parse",
		Params: []paramSpec{
			{"json", kindInString},
			{"out", kindOutString},
		},
	},
	"cleat_json_stringify": {
		ImportName: "cleat_json_stringify",
		Params: []paramSpec{
			{"value", kindInString},
			{"out", kindOutString},
		},
	},
}

const outBufSize = 65536

// importParamDecl returns a Go parameter declaration for the given spec.
func importParamDecl(spec paramSpec) string {
	switch spec.Kind {
	case kindInString:
		return fmt.Sprintf("%sPtr unsafe.Pointer, %sLen uint32", spec.Name, spec.Name)
	case kindOutString:
		return fmt.Sprintf("out%sPtr unsafe.Pointer, max%sLen uint32", spec.Name, spec.Name)
	case kindInt32:
		return fmt.Sprintf("%s uint32", spec.Name)
	case kindInt64:
		return fmt.Sprintf("%s int64", spec.Name)
	}
	return ""
}

// GenerateImports produces the content of gen_wasm_imports.go.
func GenerateImports(pkgName string, usage *UsageInfo) []byte {
	var buf bytes.Buffer

	buf.WriteString("//go:build wasip1\n\n")
	buf.WriteString("// Code generated by cleat build. DO NOT EDIT.\n\n")
	fmt.Fprintf(&buf, "package %s\n\n", pkgName)
	buf.WriteString("import \"unsafe\"\n\n")

	seen := make(map[string]bool)
	for _, hf := range usage.Funcs {
		if seen[hf.ImportName] {
			continue
		}
		seen[hf.ImportName] = true

		def, ok := importDefs[hf.ImportName]
		if !ok {
			continue
		}

		var params []string
		for _, p := range def.Params {
			params = append(params, importParamDecl(p))
		}

		fmt.Fprintf(&buf, "//go:wasmimport env %s\n", def.ImportName)
		fmt.Fprintf(&buf, "func %sImport(", goName(def.ImportName))

		if len(params) > 0 {
			buf.WriteString(strings.Join(params, ",\n    "))
		}

		buf.WriteString(") int64\n\n")
	}

	// Always include cleat_complete — the export wrapper calls it
	// to signal workflow completion before the Go WASI runtime exits.
	buf.WriteString(`//go:wasmimport env cleat_complete
func cleatCompleteImport(status uint32, resultPtr unsafe.Pointer, resultLen uint32) int64

//go:wasmimport env cleat_poll_work
func cleatPollWorkImport(entryNamePtr unsafe.Pointer, entryNameMaxLen uint32, argsPtr unsafe.Pointer, argsMaxLen uint32) int64

`)

	return buf.Bytes()
}

// goName converts a snake_case import name to a camelCase Go identifier.
func goName(importName string) string {
	parts := strings.Split(importName, "_")
	for i, p := range parts {
		if i == 0 {
			parts[i] = strings.ToLower(p)
		} else if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

// GenerateMemory produces the content of gen_wasm_memory.go.
func GenerateMemory(pkgName string) []byte {
	var buf bytes.Buffer

	buf.WriteString("//go:build wasip1\n\n")
	buf.WriteString("// Code generated by cleat build. DO NOT EDIT.\n\n")
	fmt.Fprintf(&buf, "package %s\n\n", pkgName)
	buf.WriteString("import (\n\t\"fmt\"\n\t\"unsafe\"\n)\n\n")

	buf.WriteString(`func readString(ptr unsafe.Pointer, length uint32) string {
	if length == 0 || ptr == nil {
		return ""
	}
	return unsafe.String((*byte)(ptr), int(length))
}

func stringPtr(s string) (unsafe.Pointer, uint32) {
	if len(s) == 0 {
		return nil, 0
	}
	return unsafe.Pointer(unsafe.StringData(s)), uint32(len(s))
}

// callErrorMessage builds an error message for a cleat host call failure.
// If the host wrote a response even on error (e.g. replay divergence details),
// the response text is included; otherwise a generic message is used.
//
// The code printed here is the callErrorCode -- the same value that goes into
// CallError.Code -- and not the errCode. The legend below enumerates
// cleat.CallErrorCode values, so pairing it with errCode (the 0-7 bit "did it
// fail" flag, which is 1 for essentially every failure) made the message
// contradict the Code beside it: a refused call reported Code 4 (invalid) and
// then said "error 1", which the legend reads as a timeout. See
// IMPROVEMENT-PLAN.md 2.10.
func callErrorMessage(callName string, responseBuf []byte, responseLen uint32, callErrorCode uint32) string {
	if responseLen > 0 && int(responseLen) <= len(responseBuf) {
		return string(responseBuf[:responseLen])
	}
	return fmt.Sprintf("%s: error %d (0=unknown 1=timeout 2=transient 3=not_found 4=invalid 5=permission_denied)", callName, callErrorCode)
}

// hostErrMessage returns the reason a host call wrote into its output buffer.
//
// It is deliberately not callErrorMessage. That function's fallback prints the
// cleat.CallErrorCode legend, which only applies to calls that pack a
// CallErrorCode into bits 8-39. Calls that return packSimpleResult -- the cron
// schedule calls among them -- have no such field and return 1 for every
// failure, so printing that legend beside it would describe a rejected cron
// expression as a timeout. That is the same mis-signalling as
// IMPROVEMENT-PLAN.md 2.10, which is why it gets its own helper rather than a
// fourth argument.
func hostErrMessage(buf []byte, n uint32) string {
	if n > 0 && int(n) <= len(buf) {
		return string(buf[:n])
	}
	return "no detail reported by the host"
}

`)

	return buf.Bytes()
}

// OutputFiles holds the content of generated files keyed by filename.
type OutputFiles struct {
	Imports string // gen_wasm_imports.go
	Memory  string // gen_wasm_memory.go
	Adapter string // gen_host_adapter.go
	Exports string // gen_wasm_exports.go
}

// BuildOutputs generates all output files for a given analysis result and
// package name. Returns the generated file contents.
func BuildOutputs(pkgName string, usage *UsageInfo, result *analyzer.AnalysisResult, target string) *OutputFiles {
	return &OutputFiles{
		Imports: string(GenerateImports(pkgName, usage)),
		Memory:  string(GenerateMemory(pkgName)),
		Adapter: string(GenerateHostAdapter(pkgName, usage, target)),
		Exports: string(GenerateExports(pkgName, result, target)),
	}
}
