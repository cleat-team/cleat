package wasm

import (
	"bytes"
	"fmt"
	"strings"
)

// adapterDef describes how to generate the closure for a single HostCalls
// method, bridging the clean Go interface to the //go:wasmimport call.
type adapterDef struct {
	FieldName   string          // HostCallsOptions field name
	ReturnType  string          // Go return type for the closure, e.g. "(string, error)"
	Params      []adapterParam  // closure parameter descriptions
	ResultStmts []string        // lines of Go code for result processing
}

type adapterParam struct {
	Name string // parameter name
	Type string // "string", "int64", "[]string"
}

var adapterDefs = map[string]adapterDef{
	"DurableCall": {
		FieldName:  "DurableCall",
		ReturnType: "(string, error)",
		Params: []adapterParam{
			{"service", "string"},
			{"operation", "string"},
			{"requestJSON", "string"},
		},
		ResultStmts: []string{
			"responseLen := uint32(uint64(result) >> 40)",
			"callErrorCode := durable.CallErrorCode((uint64(result) >> 8) & 0xFFFFFFFF)",
			"errCode := uint32(result & 0xFF)",
			"if errCode != 0 {",
			`	return "", &durable.CallError{`,
			`		Service:   service,`,
			`		Operation: operation,`,
			`		Code:      callErrorCode,`,
			`		Message:   fmt.Sprintf("durable_call: error code %d", errCode),`,
			`	}`,
			"}",
			"return unsafe.String(&responseBuf[0], int(responseLen)), nil",
		},
	},
	"DurableSleep": {
		FieldName: "DurableSleep",
		Params: []adapterParam{
			{"durationMs", "int64"},
		},
		ResultStmts: []string{
			"sleepStatus := byte(uint64(result) >> 56)",
			"if sleepStatus == 1 {",
			"	panic(durable.ErrSuspend)",
			"}",
		},
	},
	"DurableSleepMs": {
		FieldName: "DurableSleepMs",
		Params: []adapterParam{
			{"durationMs", "int64"},
		},
		ResultStmts: []string{
			"sleepStatus := byte(uint64(result) >> 56)",
			"if sleepStatus == 1 {",
			"	panic(durable.ErrSuspend)",
			"}",
		},
	},
	"DurableAwaitSignals": {
		FieldName:  "DurableAwaitSignals",
		ReturnType: "(string, string, bool, error)",
		Params: []adapterParam{
			{"signalNames", "[]string"},
			{"timeoutMs", "int64"},
		},
		ResultStmts: []string{
			"signalNameLen := uint32(uint64(result) >> 48)",
			"payloadLen := uint32((uint64(result) >> 32) & 0xFFFF)",
			"timedOut := uint32((uint64(result) >> 16) & 0xFFFF) != 0",
			"errCode := uint32(result & 0xFFFF)",
			"if errCode != 0 {",
			`	return "", "", false, fmt.Errorf("durable_await_signals: error code %d", errCode)`,
			"}",
			"return unsafe.String(&signalNameBuf[0], int(signalNameLen)), unsafe.String(&payloadBuf[0], int(payloadLen)), timedOut, nil",
		},
	},
	"DurableDefer": {
		FieldName:  "DurableDefer",
		ReturnType: "(string, error)",
		Params: []adapterParam{
			{"description", "string"},
		},
		ResultStmts: []string{
			"deferIDLen := uint32(uint64(result) >> 32)",
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`	return "", fmt.Errorf("durable_defer: error code %d", errCode)`,
			"}",
			"return unsafe.String(&deferIDBuf[0], int(deferIDLen)), nil",
		},
	},
	"DurableLog": {
		FieldName: "DurableLog",
		Params: []adapterParam{
			{"message", "string"},
		},
		ResultStmts: []string{
			"_ = result",
		},
	},
	"PollCancellation": {
		FieldName:  "PollCancellation",
		ReturnType: "(bool, string)",
		ResultStmts: []string{
			"reasonLen := uint32(uint64(result) >> 32)",
			"cancelled := uint32(result) != 0",
			"return cancelled, unsafe.String(&reasonBuf[0], int(reasonLen))",
		},
	},
	"PollSignal": {
		FieldName:  "PollSignal",
		ReturnType: "(string, bool, error)",
		Params: []adapterParam{
			{"signalName", "string"},
		},
		ResultStmts: []string{
			"payloadLen := uint32(uint64(result) >> 32)",
			"flags := uint32(result)",
			"errCode := flags & 0xFF",
			"found := (flags >> 8) != 0",
			"if errCode != 0 {",
			`	return "", false, fmt.Errorf("durable_poll_signal: error code %d", errCode)`,
			"}",
			"return unsafe.String(&payloadBuf[0], int(payloadLen)), found, nil",
		},
	},
	"ContinueAsNew": {
		FieldName:  "ContinueAsNew",
		ReturnType: "error",
		Params: []adapterParam{
			{"newInputJSON", "string"},
		},
		ResultStmts: []string{
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`	return fmt.Errorf("durable_continue_as_new: error code %d", errCode)`,
			"}",
			"return nil",
		},
	},
	"ChildWorkflow": {
		FieldName:  "ChildWorkflow",
		ReturnType: "(string, error)",
		Params: []adapterParam{
			{"name", "string"},
			{"inputJSON", "string"},
		},
		ResultStmts: []string{
			"runIDLen := uint32(uint64(result) >> 32)",
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`	return "", fmt.Errorf("durable_child_workflow: error code %d", errCode)`,
			"}",
			"return unsafe.String(&runIDBuf[0], int(runIDLen)), nil",
		},
	},
	"AwaitChild": {
		FieldName:  "AwaitChild",
		ReturnType: "(string, error)",
		Params: []adapterParam{
			{"runID", "string"},
		},
		ResultStmts: []string{
			"suspendSentinel := uint64(result)&(1<<62) != 0",
			"if suspendSentinel {",
			"	panic(durable.ErrSuspend)",
			"}",
			"resultLen := uint32(uint64(result) >> 32)",
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`	return "", fmt.Errorf("durable_await_child: error code %d", errCode)`,
			"}",
			"return unsafe.String(&resultBuf[0], int(resultLen)), nil",
		},
	},
	"AwaitAllChildren": {
		FieldName:  "AwaitAllChildren",
		ReturnType: "([]ChildResult, error)",
		Params: []adapterParam{
			{"runIDs", "[]string"},
		},
		ResultStmts: []string{
			"resultLen := uint32(uint64(result) >> 32)",
			"errCode := uint32(result & 0xFF)",
			"if errCode != 0 {",
			`    return nil, fmt.Errorf("durable_await_all_children: error code %d", errCode)`,
			"}",
			"var outcomes []ChildResult",
			`if err := json.Unmarshal(resultsBuf[:resultLen], &outcomes); err != nil {`,
			`    return nil, fmt.Errorf("durable_await_all_children: bad result: %w", err)`,
			"}",
			"return outcomes, nil",
		},
	},
	"DurableCallWithRetry": {
		FieldName:  "DurableCallWithRetry",
		ReturnType: "(string, error)",
		Params: []adapterParam{
			{"service", "string"},
			{"operation", "string"},
			{"requestJSON", "string"},
			{"maxAttempts", "int64"},
			{"initialIntervalMs", "int64"},
			{"backoffCoefficient100x", "int64"},
			{"maxIntervalMs", "int64"},
			{"nonRetryableErrorsJSON", "string"},
		},
		ResultStmts: []string{
			"responseLen := uint32(uint64(result) >> 40)",
			"callErrorCode := durable.CallErrorCode((uint64(result) >> 8) & 0xFFFFFFFF)",
			"errCode := uint32(result & 0xFF)",
			"if errCode != 0 {",
			`	return "", &durable.CallError{`,
			`		Service:   service,`,
			`		Operation: operation,`,
			`		Code:      callErrorCode,`,
			`		Message:   fmt.Sprintf("durable_call_retry: error code %d", errCode),`,
			`	}`,
			"}",
			"return unsafe.String(&responseBuf[0], int(responseLen)), nil",
		},
	},
	"DurableCallWithHeartbeat": {
		FieldName:  "DurableCallWithHeartbeat",
		ReturnType: "(string, error)",
		Params: []adapterParam{
			{"service", "string"},
			{"operation", "string"},
			{"requestJSON", "string"},
			{"heartbeatInterval", "time.Duration"},
			{"onProgress", "func(string)"},
		},
		ResultStmts: []string{
			"responseLen := uint32(uint64(result) >> 40)",
			"callErrorCode := durable.CallErrorCode((uint64(result) >> 8) & 0xFFFFFFFF)",
			"errCode := uint32(result & 0xFF)",
			"if errCode != 0 {",
			`	return "", &durable.CallError{`,
			`		Service:   service,`,
			`		Operation: operation,`,
			`		Code:      callErrorCode,`,
			`		Message:   fmt.Sprintf("durable_call_heartbeat: error code %d", errCode),`,
			`	}`,
			"}",
			"return unsafe.String(&responseBuf[0], int(responseLen)), nil",
		},
	},
	"Version": {
		FieldName:  "Version",
		ReturnType: "int",
		ResultStmts: []string{
			"return int(uint32(result))",
		},
	},
	"MinVersion": {
		FieldName:  "MinVersion",
		ReturnType: "int",
		ResultStmts: []string{
			"return int(uint32(result))",
		},
	},
	"SetQueryState": {
		FieldName: "SetQueryState",
		Params: []adapterParam{
			{"key", "string"},
			{"val", "string"},
		},
		ResultStmts: []string{
			"_ = result",
		},
	},
	"Now": {
		FieldName:  "Now",
		ReturnType: "int64",
		ResultStmts: []string{
			"return result",
		},
	},
	"NowMs": {
		FieldName:  "NowMs",
		ReturnType: "int64",
		ResultStmts: []string{
			"return result",
		},
	},
	"Random": {
		FieldName:  "Random",
		ReturnType: "int64",
		ResultStmts: []string{
			"return result",
		},
	},
		"CreatePromise": {
			FieldName:  "CreatePromise",
			ReturnType: "(string, error)",
			Params: []adapterParam{
				{"name", "string"},
			},
			ResultStmts: []string{
				"promiseIDLen := uint32(uint64(result) >> 32)",
				"errCode := uint32(result)",
				"if errCode != 0 {",
				"return \"\", fmt.Errorf(\"durable_create_promise: error code %d\", errCode)",
				"}",
				"return unsafe.String(&promiseIDOutBuf[0], int(promiseIDLen)), nil",
			},
		},
		"AwaitPromise": {
			FieldName:  "AwaitPromise",
			ReturnType: "(string, bool, error)",
			Params: []adapterParam{
				{"promiseID", "string"},
				{"timeoutMs", "int64"},
			},
			ResultStmts: []string{
				"resultLen := uint32(uint64(result) >> 32)",
				"timedOut := uint32((uint64(result) >> 16) & 0xFFFF) != 0",
				"errCode := uint32(result & 0xFFFF)",
				"if errCode != 0 {",
				"return \"\", false, fmt.Errorf(\"durable_await_promise: error code %d\", errCode)",
				"}",
				"return unsafe.String(&resultOutBuf[0], int(resultLen)), timedOut, nil",
			},
		},
		"RegisterUpdateHandler": {
			FieldName: "RegisterUpdateHandler",
			Params: []adapterParam{
				{"name", "string"},
			},
			ResultStmts: []string{
				"_ = result",
			},
		},
		"PluginCall": {
			FieldName:  "PluginCall",
			ReturnType: "(string, error)",
			Params: []adapterParam{
				{"pluginName", "string"},
				{"functionName", "string"},
				{"inputJSON", "string"},
			},
			ResultStmts: []string{
				"responseLen := uint32(uint64(result) >> 40)",
				"errCode := uint32(result & 0xFF)",
				"if errCode != 0 {",
				`	return "", fmt.Errorf("plugin_call: error code %d", errCode)`,
				"}",
				"return unsafe.String(&responseBuf[0], int(responseLen)), nil",
			},
		},
		"PluginCallStreaming": {
			FieldName:  "PluginCallStreaming",
			ReturnType: "(<-chan durable.StreamEvent, error)",
			Params: []adapterParam{
				{"pluginName", "string"},
				{"functionName", "string"},
				{"inputJSON", "string"},
			},
			ResultStmts: []string{
				"responseLen := uint32(uint64(result) >> 40)",
				"errCode := uint32(result & 0xFF)",
				"if errCode != 0 {",
				`		return nil, fmt.Errorf("plugin_call_streaming: error code %d", errCode)`,
				"}",
				"var events []durable.StreamEvent",
				`if err := json.Unmarshal(responseBuf[:responseLen], &events); err != nil {`,
				`		return nil, fmt.Errorf("plugin_call_streaming: bad chunk data: %w", err)`,
				"}",
				"ch := make(chan durable.StreamEvent, len(events))",
				"for _, ev := range events {",
				"		ch <- ev",
				"}",
				"close(ch)",
				"return ch, nil",
			},
		},
		}

// needsFmt returns true if any of the used adapter defs use fmt.Errorf or fmt.Sprintf.
func needsFmt(usage *UsageInfo) bool {
	for _, hf := range usage.Funcs {
		adef, ok := adapterDefs[hf.FieldName]
		if !ok {
			continue
		}
		for _, stmt := range adef.ResultStmts {
			if strings.Contains(stmt, "fmt.Errorf") || strings.Contains(stmt, "fmt.Sprintf") {
				return true
			}
		}
	}
	return false
}

// needsJSON returns true if any of the used adapter defs have []string params.
func needsJSON(usage *UsageInfo) bool {
	for _, hf := range usage.Funcs {
		adef, ok := adapterDefs[hf.FieldName]
		if !ok {
			continue
		}
		for _, p := range adef.Params {
			if p.Type == "[]string" {
				return true
			}
		}
		for _, stmt := range adef.ResultStmts {
			if strings.Contains(stmt, "json.Unmarshal") || strings.Contains(stmt, "json.Marshal") {
				return true
			}
		}
	}
	return false
}

// needsUnsafe returns true if any adapter reads from output buffers.
func needsUnsafe(usage *UsageInfo) bool {
	for _, hf := range usage.Funcs {
		adef, ok := adapterDefs[hf.FieldName]
		if !ok {
			continue
		}
		for _, stmt := range adef.ResultStmts {
			if strings.Contains(stmt, "unsafe.String") {
				return true
			}
		}
	}
	return false
}

// needsTime returns true if any of the used adapter defs have a time.Duration param.
func needsTime(usage *UsageInfo) bool {
	for _, hf := range usage.Funcs {
		adef, ok := adapterDefs[hf.FieldName]
		if !ok {
			continue
		}
		for _, p := range adef.Params {
			if p.Type == "time.Duration" {
				return true
			}
		}
	}
	return false
}

// numOutBufs returns the number of kindOutString params in the import def.
func numOutBufs(importName string) int {
	def, ok := importDefs[importName]
	if !ok {
		return 0
	}
	n := 0
	for _, p := range def.Params {
		if p.Kind == kindOutString {
			n++
		}
	}
	return n
}

// outBufNames returns the buffer variable names for output string params.
func outBufNames(importName string) []string {
	def, ok := importDefs[importName]
	if !ok {
		return nil
	}
	var names []string
	for _, p := range def.Params {
		if p.Kind == kindOutString {
			names = append(names, p.Name+"Buf")
		}
	}
	return names
}

// GenerateHostAdapter produces the content of gen_host_adapter.go.
// The generated code creates a durable.HostCalls interface value backed by
// WASM host imports via durable.NewHostCalls.
func GenerateHostAdapter(pkgName string, usage *UsageInfo) []byte {
	var buf bytes.Buffer

	buf.WriteString("//go:build wasip1\n\n")
	buf.WriteString("// Code generated by durable build. DO NOT EDIT.\n\n")
	fmt.Fprintf(&buf, "package %s\n\n", pkgName)

	buf.WriteString("import (\n")
	if needsFmt(usage) {
		buf.WriteString("\t\"fmt\"\n")
	}
	if needsJSON(usage) {
		buf.WriteString("\t\"encoding/json\"\n")
	}
	if needsUnsafe(usage) {
		buf.WriteString("\t\"unsafe\"\n")
	}
	if needsTime(usage) {
		buf.WriteString("\t\"time\"\n")
	}
	buf.WriteString("\n")
	buf.WriteString("\t\"github.com/rcownie/durable/durable\"\n")
	buf.WriteString(")\n\n")

	buf.WriteString(`const _durableOutBufSize = 65536

// makeHostCalls creates a durable.HostCalls backed by WASM host imports.
func makeHostCalls() durable.HostCalls {
	return durable.NewHostCalls(durable.HostCallsOptions{
`)

	for _, hf := range usage.Funcs {
		adef, ok := adapterDefs[hf.FieldName]
		if !ok {
			continue
		}
		generateField(&buf, hf, adef)
	}

	buf.WriteString("\t})\n")
	buf.WriteString("}\n")

	return buf.Bytes()
}

// generateField writes a single HostCallsOptions field closure.
func generateField(buf *bytes.Buffer, hf HostFunction, adef adapterDef) {
	importName := hf.ImportName

	fmt.Fprintf(buf, "\t\t%s: func(", adef.FieldName)

	// Closure parameter list.
	var closureParams []string
	for _, p := range adef.Params {
		switch p.Type {
		case "string":
			closureParams = append(closureParams, p.Name+" string")
		case "[]string":
			closureParams = append(closureParams, p.Name+" []string")
		case "int64":
			closureParams = append(closureParams, p.Name+" int64")
		case "time.Duration":
			closureParams = append(closureParams, p.Name+" time.Duration")
		case "func(string)":
			closureParams = append(closureParams, p.Name+" func(string)")
		}
	}
	buf.WriteString(strings.Join(closureParams, ", "))
	buf.WriteString(")")

	// Return type.
	if adef.ReturnType != "" {
		buf.WriteString(" ")
		buf.WriteString(adef.ReturnType)
	}

	buf.WriteString(" {\n")

	// Allocate output buffers.
	for _, name := range outBufNames(importName) {
		fmt.Fprintf(buf, "\t\t\t%s := make([]byte, _durableOutBufSize)\n", name)
	}

	// Argument setup (stringPtr for input strings, json.Marshal for []string).
	for _, p := range adef.Params {
		switch p.Type {
		case "string":
			fmt.Fprintf(buf, "\t\t\t%sPtr, %sLen := stringPtr(%s)\n", p.Name, p.Name, p.Name)
		case "[]string":
			fmt.Fprintf(buf, "\t\t\t%sJSON, err := json.Marshal(%s)\n", p.Name, p.Name)
			fmt.Fprintf(buf, "\t\t\tif err != nil { panic(\"json.Marshal for %s: \" + err.Error()) }\n", p.Name)
			fmt.Fprintf(buf, "\t\t\t%sPtr, %sLen := stringPtr(string(%sJSON))\n", p.Name, p.Name, p.Name)
		}
	}

	// Convert time.Duration params to int64 milliseconds for WASM.
	for _, p := range adef.Params {
		if p.Type == "time.Duration" {
			fmt.Fprintf(buf, "\t\t\t%sMs := %s.Milliseconds()\n", p.Name, p.Name)
		}
	}

	// Build the import call. All imports return int64.
	hasResultUse := len(adef.ResultStmts) > 0
	if hasResultUse {
		buf.WriteString("\t\t\tresult := ")
	} else {
		buf.WriteString("\t\t\t")
	}
	buf.WriteString(goName(importName))
	buf.WriteString("Import(")

	def := importDefs[importName]
	var args []string
	for _, dp := range def.Params {
		switch dp.Kind {
		case kindInString:
			args = append(args, fmt.Sprintf("%sPtr, %sLen", dp.Name, dp.Name))
		case kindOutString:
			args = append(args, fmt.Sprintf("unsafe.Pointer(unsafe.SliceData(%sBuf)), uint32(len(%sBuf))", dp.Name, dp.Name))
		case kindInt64:
			args = append(args, dp.Name)
		}
	}
	buf.WriteString(strings.Join(args, ", "))
	buf.WriteString(")\n")

	// Result processing.
	for _, stmt := range adef.ResultStmts {
		buf.WriteString("\t\t\t" + stmt + "\n")
	}

	buf.WriteString("\t\t},\n")
}
