package wasm

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/rcownie/durable/internal/analyzer"
)

// paramKind describes how a parameter is passed across the WASM boundary.
type paramKind int

const (
	kindInString  paramKind = iota // input string: (ptr unsafe.Pointer, len uint32)
	kindOutString                  // output string buffer: (outPtr unsafe.Pointer, maxLen uint32)
	kindInt64                      // int64
)

// importDef defines the signature of a single //go:wasmimport stub.
type importDef struct {
	ImportName string      // e.g. "durable_call"
	Params     []paramSpec // input params + output buffers
}

type paramSpec struct {
	Name string
	Kind paramKind
}

// importDefs maps each host function to its low-level WASM import signature.
var importDefs = map[string]importDef{
	"durable_call": {
		ImportName: "durable_call",
		Params: []paramSpec{
			{"service", kindInString},
			{"operation", kindInString},
			{"requestJSON", kindInString},
			{"response", kindOutString},
		},
	},
	"durable_sleep": {
		ImportName: "durable_sleep",
		Params: []paramSpec{
			{"durationMs", kindInt64},
		},
	},
	"durable_await_signals": {
		ImportName: "durable_await_signals",
		Params: []paramSpec{
			{"signalNames", kindInString},
			{"timeoutMs", kindInt64},
			{"signalName", kindOutString},
			{"payload", kindOutString},
		},
	},
	"durable_defer": {
		ImportName: "durable_defer",
		Params: []paramSpec{
			{"description", kindInString},
			{"deferID", kindOutString},
		},
	},
	"durable_log": {
		ImportName: "durable_log",
		Params: []paramSpec{
			{"message", kindInString},
		},
	},
	"durable_poll_cancellation": {
		ImportName: "durable_poll_cancellation",
		Params: []paramSpec{
			{"reason", kindOutString},
		},
	},
	"durable_poll_signal": {
		ImportName: "durable_poll_signal",
		Params: []paramSpec{
			{"signalName", kindInString},
			{"payload", kindOutString},
		},
	},
	"durable_continue_as_new": {
		ImportName: "durable_continue_as_new",
		Params: []paramSpec{
			{"newInputJSON", kindInString},
		},
	},
	"durable_child_workflow": {
		ImportName: "durable_child_workflow",
		Params: []paramSpec{
			{"name", kindInString},
			{"inputJSON", kindInString},
			{"runID", kindOutString},
		},
	},
	"durable_await_child": {
		ImportName: "durable_await_child",
		Params: []paramSpec{
			{"runID", kindInString},
			{"result", kindOutString},
		},
	},
	"durable_call_retry": {
		ImportName: "durable_call_retry",
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
	"durable_await_all_children": {
		ImportName: "durable_await_all_children",
		Params: []paramSpec{
			{"runIDsJSON", kindInString},
			{"results", kindOutString},
		},
	},
	"durable_call_heartbeat": {
		ImportName: "durable_call_heartbeat",
		Params: []paramSpec{
			{"service", kindInString},
			{"operation", kindInString},
			{"requestJSON", kindInString},
			{"heartbeatIntervalMs", kindInt64},
			{"response", kindOutString},
		},
	},
	"durable_version": {
		ImportName: "durable_version",
	},
	"durable_min_version": {
		ImportName: "durable_min_version",
	},
	"set_query_state": {
		ImportName: "set_query_state",
		Params: []paramSpec{
			{"key", kindInString},
			{"val", kindInString},
		},
	},
	"durable_create_promise": {
		ImportName: "durable_create_promise",
		Params: []paramSpec{
			{"name", kindInString},
			{"promise_id_out", kindOutString},
		},
	},
	"durable_await_promise": {
		ImportName: "durable_await_promise",
		Params: []paramSpec{
			{"promise_id", kindInString},
			{"timeout_ms", kindInt64},
			{"result_out", kindOutString},
		},
	},
	"durable_now": {
		ImportName: "durable_now",
	},
	"durable_random": {
		ImportName: "durable_random",
	},
	"durable_register_update_handler": {
		ImportName: "durable_register_update_handler",
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
}

const outBufSize = 65536

// importParamDecl returns a Go parameter declaration for the given spec.
func importParamDecl(spec paramSpec) string {
	switch spec.Kind {
	case kindInString:
		return fmt.Sprintf("%sPtr unsafe.Pointer, %sLen uint32", spec.Name, spec.Name)
	case kindOutString:
		return fmt.Sprintf("out%sPtr unsafe.Pointer, max%sLen uint32", spec.Name, spec.Name)
	case kindInt64:
		return fmt.Sprintf("%s int64", spec.Name)
	}
	return ""
}

// GenerateImports produces the content of gen_wasm_imports.go.
func GenerateImports(pkgName string, usage *UsageInfo) []byte {
	var buf bytes.Buffer

	buf.WriteString("//go:build wasip1\n\n")
	buf.WriteString("// Code generated by durable build. DO NOT EDIT.\n\n")
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
	buf.WriteString("// Code generated by durable build. DO NOT EDIT.\n\n")
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
func BuildOutputs(pkgName string, usage *UsageInfo, result *analyzer.AnalysisResult) *OutputFiles {
	return &OutputFiles{
		Imports: string(GenerateImports(pkgName, usage)),
		Memory:  string(GenerateMemory(pkgName)),
		Adapter: string(GenerateHostAdapter(pkgName, usage)),
		Exports: string(GenerateExports(pkgName, result)),
	}
}
