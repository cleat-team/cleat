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
	buf.WriteString("import \"unsafe\"\n\n")

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
		Exports: string(GenerateExports(pkgName, result)),
	}
}
