package wasm

import (
	"bytes"
	"fmt"
	"strings"
)

// adapterDef describes how to generate the closure for a single HostCalls
// method, bridging the clean Go interface to the //go:wasmimport call.
type adapterDef struct {
	FieldName   string         // HostCallsOptions field name
	ReturnType  string         // Go return type for the closure, e.g. "(string, error)"
	Params      []adapterParam // closure parameter descriptions
	ResultStmts []string       // lines of Go code for result processing
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
			"callErrorCode := cleat.CallErrorCode((uint64(result) >> 8) & 0xFFFFFFFF)",
			"errCode := uint32(result & 0xFF)",
			"if errCode != 0 {",
			`	return "", &cleat.CallError{`,
			`		Service:   service,`,
			`		Operation: operation,`,
			`		Code:      callErrorCode,`,
			`		Message:   callErrorMessage("cleat_call", responseBuf, responseLen, errCode),`,
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
			"	panic(cleat.ErrSuspend)",
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
			"	panic(cleat.ErrSuspend)",
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
			`	return "", "", false, fmt.Errorf("cleat_await_signals: error %d (0=unknown 1=timeout 2=transient 3=not_found 4=invalid 5=permission_denied)", errCode)`,
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
			`	return "", fmt.Errorf("cleat_defer: error %d (0=unknown 1=timeout 2=transient 3=not_found 4=invalid 5=permission_denied)", errCode)`,
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
			`	return "", false, fmt.Errorf("cleat_poll_signal: error %d (0=unknown 1=timeout 2=transient 3=not_found 4=invalid 5=permission_denied)", errCode)`,
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
			`	return fmt.Errorf("cleat_continue_as_new: error %d (0=unknown 1=timeout 2=transient 3=not_found 4=invalid 5=permission_denied)", errCode)`,
			"}",
			"return nil",
		},
	},
	"ContinueAsNewWithVersion": {
		FieldName:  "ContinueAsNewWithVersion",
		ReturnType: "error",
		Params: []adapterParam{
			{"newInputJSON", "string"},
			{"newVersion", "int64"},
		},
		ResultStmts: []string{
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`	return fmt.Errorf("cleat_continue_as_new_versioned: error %d (0=unknown 1=timeout 2=transient 3=not_found 4=invalid 5=permission_denied)", errCode)`,
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
			`	return "", fmt.Errorf("cleat_child_workflow: error %d (0=unknown 1=timeout 2=transient 3=not_found 4=invalid 5=permission_denied)", errCode)`,
			"}",
			"return unsafe.String(&runIDBuf[0], int(runIDLen)), nil",
		},
	},
	"ChildWorkflowWithOptions": {
		FieldName:  "ChildWorkflowWithOptions",
		ReturnType: "(string, error)",
		Params: []adapterParam{
			{"name", "string"},
			{"inputJSON", "string"},
			{"version", "int"},
			{"parentClosePolicy", "string"},
			{"priority", "int"},
		},
		ResultStmts: []string{
			"runIDLen := uint32(uint64(result) >> 32)",
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`	return "", fmt.Errorf("cleat_child_workflow_with_options: error %d (0=unknown 1=timeout 2=transient 3=not_found 4=invalid 5=permission_denied)", errCode)`,
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
			"	panic(cleat.ErrSuspend)",
			"}",
			"resultLen := uint32(uint64(result) >> 32)",
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`	return "", fmt.Errorf("cleat_await_child: error %d (0=unknown 1=timeout 2=transient 3=not_found 4=invalid 5=permission_denied)", errCode)`,
			"}",
			"return unsafe.String(&resultBuf[0], int(resultLen)), nil",
		},
	},
	"AwaitAllChildren": {
		FieldName:  "AwaitAllChildren",
		ReturnType: "([]cleat.ChildResult, error)",
		Params: []adapterParam{
			{"runIDs", "[]string"},
		},
		ResultStmts: []string{
			"resultLen := uint32(uint64(result) >> 32)",
			"errCode := uint32(result & 0xFF)",
			"if errCode != 0 {",
			`    return nil, fmt.Errorf("cleat_await_all_children: error %d (0=unknown 1=timeout 2=transient 3=not_found 4=invalid 5=permission_denied)", errCode)`,
			"}",
			"outcomes := parseChildResultArray(unsafe.String(&resultsBuf[0], int(resultLen)))",
			"return outcomes, nil",
		},
	},
	"PollChild": {
		FieldName:  "PollChild",
		ReturnType: "(string, string, error)",
		Params: []adapterParam{
			{"runID", "string"},
		},
		ResultStmts: []string{
			"resultLen := uint32(uint64(result) >> 32)",
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`	return "", "", fmt.Errorf("cleat_poll_child: error %d (0=unknown 1=timeout 2=transient 3=not_found 4=invalid 5=permission_denied)", errCode)`,
			"}",
			"prStatus, prResult, prErr := parseSimpleResult(unsafe.String(&resultBuf[0], int(resultLen)), \"result\")",
			"if prErr != \"\" {",
				"	return prStatus, prResult, fmt.Errorf(\"%s\", prErr)",
				"}",
				"return prStatus, prResult, nil",
		},
	},
	"AwaitAnyChild": {
		FieldName:  "AwaitAnyChild",
		ReturnType: "(string, string, error)",
		Params: []adapterParam{
			{"runIDs", "[]string"},
		},
		ResultStmts: []string{
			"suspendSentinel := uint64(result)&(1<<62) != 0",
			"if suspendSentinel {",
			"	panic(cleat.ErrSuspend)",
			"}",
			"resultLen := uint32(uint64(result) >> 32)",
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`	return "", "", fmt.Errorf("cleat_await_any_child: error %d (0=unknown 1=timeout 2=transient 3=not_found 4=invalid 5=permission_denied)", errCode)`,
			"}",
			"outRunID, outResult, outErr := parseSimpleResult(unsafe.String(&resultBuf[0], int(resultLen)), \"result\")",
			"if outErr != \"\" {",
				"	return outRunID, \"\", fmt.Errorf(\"%s\", outErr)",
				"}",
				"return outRunID, outResult, nil",
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
			"callErrorCode := cleat.CallErrorCode((uint64(result) >> 8) & 0xFFFFFFFF)",
			"errCode := uint32(result & 0xFF)",
			"if errCode != 0 {",
			`	return "", &cleat.CallError{`,
			`		Service:   service,`,
			`		Operation: operation,`,
			`		Code:      callErrorCode,`,
			`		Message:   callErrorMessage("cleat_call_retry", responseBuf, responseLen, errCode),`,
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
			"callErrorCode := cleat.CallErrorCode((uint64(result) >> 8) & 0xFFFFFFFF)",
			"errCode := uint32(result & 0xFF)",
			"if errCode != 0 {",
			`	return "", &cleat.CallError{`,
			`		Service:   service,`,
			`		Operation: operation,`,
			`		Code:      callErrorCode,`,
			`		Message:   callErrorMessage("cleat_call_heartbeat", responseBuf, responseLen, errCode),`,
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
	"WorkflowID": {
		FieldName:  "WorkflowID",
		ReturnType: "string",
		ResultStmts: []string{
			"idLen := uint32(result)",
			"return unsafe.String(&idBuf[0], int(idLen))",
		},
	},
	"RunID": {
		FieldName:  "RunID",
		ReturnType: "string",
		ResultStmts: []string{
			"idLen := uint32(result)",
			"return unsafe.String(&idBuf[0], int(idLen))",
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
			"return \"\", fmt.Errorf(\"cleat_create_promise: error %d (0=unknown 1=timeout 2=transient 3=not_found 4=invalid 5=permission_denied)\", errCode)",
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
			"return \"\", false, fmt.Errorf(\"cleat_await_promise: error %d (0=unknown 1=timeout 2=transient 3=not_found 4=invalid 5=permission_denied)\", errCode)",
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
			`	return "", fmt.Errorf("plugin_call: error %d (0=unknown 1=timeout 2=transient 3=not_found 4=invalid 5=permission_denied)", errCode)`,
			"}",
			"return unsafe.String(&responseBuf[0], int(responseLen)), nil",
		},
	},
	"AcquireLock": {
		FieldName:  "AcquireLock",
		ReturnType: "(bool, error)",
		Params: []adapterParam{
			{"key", "string"},
			{"ttlMs", "int64"},
		},
		ResultStmts: []string{
			"errCode := uint32(result & 0xFF)",
			"acquired := uint32((uint64(result) >> 8) & 0x1) != 0",
			"if errCode != 0 {",
			`    return false, fmt.Errorf("cleat_acquire_lock: error %d (0=unknown 1=timeout 2=transient 3=not_found 4=invalid 5=permission_denied)", errCode)`,
			"}",
			"return acquired, nil",
		},
	},
	"AcquireLockMs": {
		FieldName:  "AcquireLockMs",
		ReturnType: "(bool, error)",
		Params: []adapterParam{
			{"key", "string"},
			{"ttlMs", "int64"},
		},
		ResultStmts: []string{
			"errCode := uint32(result & 0xFF)",
			"acquired := uint32((uint64(result) >> 8) & 0x1) != 0",
			"if errCode != 0 {",
			`    return false, fmt.Errorf("cleat_acquire_lock: error %d (0=unknown 1=timeout 2=transient 3=not_found 4=invalid 5=permission_denied)", errCode)`,
			"}",
			"return acquired, nil",
		},
	},
	"ReleaseLock": {
		FieldName:  "ReleaseLock",
		ReturnType: "error",
		Params: []adapterParam{
			{"key", "string"},
		},
		ResultStmts: []string{
			"errCode := uint32(result & 0xFF)",
			"if errCode != 0 {",
			`    return fmt.Errorf("cleat_release_lock: error %d (0=unknown 1=timeout 2=transient 3=not_found 4=invalid 5=permission_denied)", errCode)`,
			"}",
			"return nil",
		},
	},
	"SideEffect": {
		FieldName:  "SideEffect",
		ReturnType: "(string, error)",
		Params: []adapterParam{
			{"fn", "func() (string, error)"},
		},
		ResultStmts: []string{
			"cachedResultLen := uint32(uint64(result) >> 32)",
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`    return "", fmt.Errorf("cleat_side_effect: error %d (0=unknown 1=timeout 2=transient 3=not_found 4=invalid 5=permission_denied)", errCode)`,
			"}",
			"return unsafe.String(&cachedResultBuf[0], int(cachedResultLen)), nil",
		},
	},
	"PluginCallStreaming": {
		FieldName:  "PluginCallStreaming",
		ReturnType: "(<-chan cleat.StreamEvent, error)",
		Params: []adapterParam{
			{"pluginName", "string"},
			{"functionName", "string"},
			{"inputJSON", "string"},
		},
		ResultStmts: []string{
			"responseLen := uint32(uint64(result) >> 40)",
			"errCode := uint32(result & 0xFF)",
			"if errCode != 0 {",
			`		return nil, fmt.Errorf("plugin_call_streaming: error %d (0=unknown 1=timeout 2=transient 3=not_found 4=invalid 5=permission_denied)", errCode)`,
			"}",
			"var events []cleat.StreamEvent",
			`if err := json.Unmarshal(responseBuf[:responseLen], &events); err != nil {`,
			`		return nil, fmt.Errorf("plugin_call_streaming: bad chunk data: %w", err)`,
			"}",
			"ch := make(chan cleat.StreamEvent, len(events))",
			"for _, ev := range events {",
			"		ch <- ev",
			"}",
			"close(ch)",
			"return ch, nil",
		},
	},
}

// hostWrapperDef describes how to generate a host_ wrapper function for a
// higher-level HostCalls method that isn't a direct WASM import wrapper.
// The body should call core host_* functions rather than going through h.
type hostWrapperDef struct {
	ReturnType string
	Params     []adapterParam
	Body       []string // Go statements implementing the wrapper
}

// hostWrapperDefs contains definitions for wrapper methods that call
// core host_* functions. These are methods on HostCalls that eventually
// delegate to a direct WASM import (e.g., DurableCallJSON → DurableCall).
var hostWrapperDefs = map[string]hostWrapperDef{
	"DurableCallJSON": {
		ReturnType: "error",
		Params: []adapterParam{
			{"service", "string"},
			{"operation", "string"},
			{"requestJSON", "string"},
			{"result", "interface{}"},
		},
		Body: []string{
			"resp, err := host_DurableCall(service, operation, requestJSON)",
			"if err != nil { return err }",
			"if result == nil { return nil }",
			`return json.Unmarshal([]byte(resp), result)`,
		},
	},
	"DurableCallTyped": {
		ReturnType: "error",
		Params: []adapterParam{
			{"service", "string"},
			{"operation", "string"},
			{"request", "interface{}"},
			{"result", "interface{}"},
		},
		Body: []string{
			"reqBytes, err := json.Marshal(request)",
			`if err != nil { return fmt.Errorf("durable: marshaling request for %s.%s: %%w", service, operation, err) }`,
			"return host_DurableCallJSON(service, operation, string(reqBytes), result)",
		},
	},
	"DurableCallTypedWithOptions": {
		ReturnType: "error",
		Params: []adapterParam{
			{"opts", "cleat.CallOptions"},
			{"service", "string"},
			{"operation", "string"},
			{"request", "interface{}"},
			{"result", "interface{}"},
		},
		Body: []string{
			"reqBytes, err := json.Marshal(request)",
			`if err != nil { return fmt.Errorf("durable: marshaling request for %s.%s: %%w", service, operation, err) }`,
			"return host_DurableCallJSONWithOptions(opts, service, operation, string(reqBytes), result)",
		},
	},
	"DurableCallWithOptions": {
		ReturnType: "(string, error)",
		Params: []adapterParam{
			{"opts", "cleat.CallOptions"},
			{"service", "string"},
			{"operation", "string"},
			{"requestJSON", "string"},
		},
		Body: []string{
			"_ = opts",
			"return host_DurableCall(service, operation, requestJSON)",
		},
	},
	"DurableCallJSONWithOptions": {
		ReturnType: "error",
		Params: []adapterParam{
			{"opts", "cleat.CallOptions"},
			{"service", "string"},
			{"operation", "string"},
			{"requestJSON", "string"},
			{"result", "interface{}"},
		},
		Body: []string{
			"resp, err := host_DurableCallWithOptions(opts, service, operation, requestJSON)",
			"if err != nil { return err }",
			"if result == nil { return nil }",
			`return json.Unmarshal([]byte(resp), result)`,
		},
	},
	"AwaitSignals": {
		ReturnType: "cleat.SignalResult",
		Params: []adapterParam{
			{"signalNames", "[]string"},
			{"timeout", "time.Duration"},
		},
		Body: []string{
			"name, payload, timedOut, err := host_DurableAwaitSignals(signalNames, timeout.Milliseconds())",
			"return cleat.SignalResult{Name: name, Payload: payload, TimedOut: timedOut, Err: err}",
		},
	},
	"Now": {
		ReturnType: "time.Time",
		Body: []string{
			"ms := host_NowMs()",
			"return time.Unix(ms/1000, (ms%1000)*1_000_000)",
		},
	},
	"DurableSleep": {
		Params: []adapterParam{
			{"d", "time.Duration"},
		},
		Body: []string{
			"host_DurableSleepMs(d.Milliseconds())",
		},
	},
	"AcquireLock": {
		ReturnType: "(bool, error)",
		Params: []adapterParam{
			{"key", "string"},
			{"ttl", "time.Duration"},
		},
		Body: []string{
			"return host_AcquireLockMs(key, ttl.Milliseconds())",
		},
	},
	"ChildWorkflowTyped": {
		ReturnType: "(string, error)",
		Params: []adapterParam{
			{"name", "string"},
			{"request", "interface{}"},
		},
		Body: []string{
			"reqJSON, err := json.Marshal(request)",
			`if err != nil { return "", fmt.Errorf("durable: marshaling child workflow input for %%s: %%w", name, err) }`,
			"return host_ChildWorkflow(name, string(reqJSON))",
		},
	},
	"AwaitChildTyped": {
		ReturnType: "error",
		Params: []adapterParam{
			{"runID", "string"},
			{"result", "interface{}"},
		},
		Body: []string{
			"resp, err := host_AwaitChild(runID)",
			"if err != nil { return err }",
			`return json.Unmarshal([]byte(resp), result)`,
		},
	},
	"DurableCallTypedWithHeartbeat": {
		ReturnType: "error",
		Params: []adapterParam{
			{"service", "string"},
			{"operation", "string"},
			{"request", "interface{}"},
			{"result", "interface{}"},
			{"heartbeatInterval", "time.Duration"},
			{"onProgress", "func(string)"},
		},
		Body: []string{
			"reqJSON, err := json.Marshal(request)",
			`if err != nil { return fmt.Errorf("durable: marshaling request for %s.%s: %%w", service, operation, err) }`,
			"resp, err := host_DurableCallWithHeartbeat(service, operation, string(reqJSON), heartbeatInterval, onProgress)",
			"if err != nil { return err }",
			"if result == nil { return nil }",
			`return json.Unmarshal([]byte(resp), result)`,
		},
	},
}

// needsFmt returns true if any of the used adapter or wrapper defs use fmt.
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

// needsManualJSON returns true if any of the used adapter defs need the
// hand-written JSON helpers (buildJSONStringArray, parseSimpleResult,
// parseChildResultArray). These replace encoding/json to avoid TinyGo
// reflection bugs and keep WASM binary size small for both targets.
func needsManualJSON(usage *UsageInfo) bool {
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
			if strings.Contains(stmt, "parseSimpleResult") || strings.Contains(stmt, "parseChildResultArray") {
				return true
			}
		}
	}
	return false
}

// needsJSON returns true if any of the used adapter defs reference encoding/json.
func needsJSON(usage *UsageInfo) bool {
	for _, hf := range usage.Funcs {
		adef, ok := adapterDefs[hf.FieldName]
		if !ok {
			continue
		}
		for _, stmt := range adef.ResultStmts {
			if strings.Contains(stmt, "json.") {
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

// needsTime returns true if any of the used adapter defs use time types.
// Only checks adapterDefs (not hostWrapperDefs) because this determines
// imports for gen_host_adapter.go which only uses adapterDefs.
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
// It generates closure-based HostCallsOptions fields that call through
// WASM imports. The workflow code calls h.MethodName(...) through the
// HostCalls interface.
func GenerateHostAdapter(pkgName string, usage *UsageInfo, target string) []byte {
	var buf bytes.Buffer

	buf.WriteString("//go:build wasip1\n\n")
	buf.WriteString("// Code generated by cleat build. DO NOT EDIT.\n\n")
	fmt.Fprintf(&buf, "package %s\n\n", pkgName)

	hasManualJSON := needsManualJSON(usage)

	buf.WriteString("import (\n")
	if needsFmt(usage) {
		buf.WriteString("\t\"fmt\"\n")
	}
	if needsUnsafe(usage) {
		buf.WriteString("\t\"unsafe\"\n")
	}
	if needsTime(usage) {
		buf.WriteString("\t\"time\"\n")
	}
	if needsJSON(usage) {
		buf.WriteString("\t\"encoding/json\"\n")
	}
	buf.WriteString("\n")
	buf.WriteString("\t\"github.com/cleat-team/cleat/cleat\"\n")
	buf.WriteString(")\n\n")

	buf.WriteString("const _cleatOutBufSize = 65536\n\n")

	buf.WriteString("func makeHostCalls() cleat.HostCalls {\n")
	buf.WriteString("\treturn cleat.NewHostCalls(cleat.HostCallsOptions{\n")
	for _, hf := range usage.Funcs {
		adef, ok := adapterDefs[hf.FieldName]
		if !ok {
			continue
		}
		generateField(&buf, hf, adef)
	}
	buf.WriteString("\t})\n")
	buf.WriteString("}\n")

	if hasManualJSON {
		writeManualJSONHelpers(&buf)
	}

	return buf.Bytes()
}

// writeManualJSONHelpers emits hand-written JSON encoding/decoding helpers
// that avoid importing encoding/json. TinyGo's reflection-based JSON
// implementation can corrupt WASM function tables, causing "invalid table
// access" or "unreachable" panics. These helpers handle the specific types
// used by the generated adapter (string arrays, simple result objects,
// arrays of ChildResult).
//
// These helpers are used for both --target tinygo and --target go to keep
// generated code consistent and avoid the 1-2 MB binary size increase from
// importing encoding/json in WASM builds. Standard Go's encoding/json would
// work correctly here but is not needed for the limited patterns the adapter
// generates.
func writeManualJSONHelpers(buf *bytes.Buffer) {
	buf.WriteString(`
// buildJSONStringArray builds a JSON array of strings without reflection.
// e.g. ["abc","def"]
func buildJSONStringArray(strs []string) string {
	if len(strs) == 0 {
		return "[]"
	}
	n := 2 + (len(strs) - 1) // brackets + commas
	for _, s := range strs {
		n += 2 + len(s) // quotes + content
	}
	b := make([]byte, 0, n)
	b = append(b, '[')
	for i, s := range strs {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '"')
		b = append(b, s...)
		b = append(b, '"')
	}
	b = append(b, ']')
	return string(b)
}

// parseSimpleResult parses a JSON object like {"run_id":"x","result":"y","error":"z"}
// or {"status":"ok","result":"y","error":"z"} without reflection.
// The secondKey controls which field is returned as the second value
// (usually "result" for child outcomes, "run_id" for AwaitAnyChild).
// Returns (firstVal, secondVal, errVal) where firstVal is the value of
// "run_id" or "status", secondVal is the value of secondKey, and errVal
// is the value of "error".
func parseSimpleResult(json string, secondKey string) (first, second, errStr string) {
	first = extractJSONString(json, "run_id")
	if first == "" {
		first = extractJSONString(json, "status")
	}
	second = extractJSONString(json, secondKey)
	errStr = extractJSONString(json, "error")
	return
}

// parseChildResultArray parses a JSON array of {"run_id":"...","status":"...","result":"...","error":"..."} objects.
// Returns []cleat.ChildResult.
func parseChildResultArray(json string) []cleat.ChildResult {
	var results []cleat.ChildResult
	// Find each object between { and }
	i := 0
	for i < len(json) {
		open := strings.Index(json[i:], "{")
		if open < 0 {
			break
		}
		close := strings.Index(json[i+open:], "}")
		if close < 0 {
			break
		}
		obj := json[i+open : i+open+close+1]
		results = append(results, cleat.ChildResult{
			RunID:  extractJSONString(obj, "run_id"),
			Result: extractJSONString(obj, "result"),
			Error:  extractJSONString(obj, "error"),
		})
		i = i + open + close + 1
	}
	return results
}

`)
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
		case "int":
			closureParams = append(closureParams, p.Name+" int")
		case "time.Duration":
			closureParams = append(closureParams, p.Name+" time.Duration")
		case "func(string)":
			closureParams = append(closureParams, p.Name+" func(string)")
		case "func() (string, error)":
			closureParams = append(closureParams, p.Name+" func() (string, error)")
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
		fmt.Fprintf(buf, "\t\t\t%s := make([]byte, _cleatOutBufSize)\n", name)
	}

	// Build adapter param type lookup for import arg conversion.
	adapterParamType := make(map[string]string)
	for _, p := range adef.Params {
		adapterParamType[p.Name] = p.Type
	}

	// Argument setup (stringPtr for input strings, json.Marshal for []string).
	for _, p := range adef.Params {
		switch p.Type {
		case "string":
			fmt.Fprintf(buf, "\t\t\t%sPtr, %sLen := stringPtr(%s)\n", p.Name, p.Name, p.Name)
		case "[]string":
			fmt.Fprintf(buf, "\t\t\t%sJSON := buildJSONStringArray(%s)\n", p.Name, p.Name)
			fmt.Fprintf(buf, "\t\t\t%sPtr, %sLen := stringPtr(%sJSON)\n", p.Name, p.Name, p.Name)
		case "func() (string, error)":
			fmt.Fprintf(buf, "\t\t\t_computedResult, _sideEffectErr := %s()\n", p.Name)
			fmt.Fprintf(buf, "\t\t\tif _sideEffectErr != nil { return \"\", _sideEffectErr }\n")
			fmt.Fprintf(buf, "\t\t\tresultPtr, resultLen := stringPtr(_computedResult)\n")
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
			if adapterParamType[dp.Name] == "int" {
				args = append(args, fmt.Sprintf("int64(%s)", dp.Name))
			} else {
				args = append(args, dp.Name)
			}
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

// generateHostFunc writes a standalone package-level function that makes a
// direct WASM import call. The transformer rewrites h.FieldName(...) calls
// to host_FieldName(...) calls, avoiding all function pointer indirection.
func generateHostFunc(buf *bytes.Buffer, hf HostFunction, adef adapterDef) {
	importName := hf.ImportName
	funcName := "host_" + adef.FieldName

	fmt.Fprintf(buf, "// %s is the direct-call version of h.%s.\n", funcName, adef.FieldName)
	fmt.Fprintf(buf, "// Called by workflow code after source transformation.\n")
	fmt.Fprintf(buf, "func %s(", funcName)

	// Parameter list.
	var params []string
	for _, p := range adef.Params {
		switch p.Type {
		case "string":
			params = append(params, p.Name+" string")
		case "[]string":
			params = append(params, p.Name+" []string")
		case "int64":
			params = append(params, p.Name+" int64")
		case "int":
			params = append(params, p.Name+" int")
		case "time.Duration":
			params = append(params, p.Name+" time.Duration")
		case "func(string)":
			params = append(params, p.Name+" func(string)")
		case "func() (string, error)":
			params = append(params, p.Name+" func() (string, error)")
		}
	}
	buf.WriteString(strings.Join(params, ", "))
	buf.WriteString(")")

	if adef.ReturnType != "" {
		buf.WriteString(" ")
		buf.WriteString(adef.ReturnType)
	}

	buf.WriteString(" {\n")

	// Allocate output buffers.
	for _, name := range outBufNames(importName) {
		fmt.Fprintf(buf, "\t%s := make([]byte, _cleatOutBufSize)\n", name)
	}

	// Build adapter param type lookup for import arg conversion.
	adapterParamType := make(map[string]string)
	for _, p := range adef.Params {
		adapterParamType[p.Name] = p.Type
	}

	// Argument setup.
	for _, p := range adef.Params {
		switch p.Type {
		case "string":
			fmt.Fprintf(buf, "\t%sPtr, %sLen := stringPtr(%s)\n", p.Name, p.Name, p.Name)
		case "[]string":
			fmt.Fprintf(buf, "\t%sJSON, err := json.Marshal(%s)\n", p.Name, p.Name)
			fmt.Fprintf(buf, "\tif err != nil { panic(\"json.Marshal for %s: \" + err.Error()) }\n", p.Name)
			fmt.Fprintf(buf, "\t%sPtr, %sLen := stringPtr(string(%sJSON))\n", p.Name, p.Name, p.Name)
		case "func() (string, error)":
			fmt.Fprintf(buf, "\t_computedResult, _sideEffectErr := %s()\n", p.Name)
			fmt.Fprintf(buf, "\tif _sideEffectErr != nil { return \"\", _sideEffectErr }\n")
			fmt.Fprintf(buf, "\tresultPtr, resultLen := stringPtr(_computedResult)\n")
		}
	}

	// Convert time.Duration params to int64 milliseconds for WASM.
	for _, p := range adef.Params {
		if p.Type == "time.Duration" {
			fmt.Fprintf(buf, "\t%sMs := %s.Milliseconds()\n", p.Name, p.Name)
		}
	}

	// Build the import call.
	hasResultUse := len(adef.ResultStmts) > 0
	if hasResultUse {
		buf.WriteString("\tresult := ")
	} else {
		buf.WriteString("\t")
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
			if adapterParamType[dp.Name] == "int" {
				args = append(args, fmt.Sprintf("int64(%s)", dp.Name))
			} else {
				args = append(args, dp.Name)
			}
		}
	}
	buf.WriteString(strings.Join(args, ", "))
	buf.WriteString(")\n")

	// Result processing.
	for _, stmt := range adef.ResultStmts {
		buf.WriteString("\t" + stmt + "\n")
	}

	buf.WriteString("}\n\n")
}

// generateHostWrapperFunc writes a standalone host_ wrapper function for a
// higher-level HostCalls method that delegates to core host_* functions.
func generateHostWrapperFunc(buf *bytes.Buffer, fieldName string, wdef hostWrapperDef) {
	funcName := "host_" + fieldName

	fmt.Fprintf(buf, "// %s is the direct-call version of h.%s (wrapper).\n", funcName, fieldName)
	fmt.Fprintf(buf, "func %s(", funcName)

	var params []string
	for _, p := range wdef.Params {
		switch p.Type {
		case "string":
			params = append(params, p.Name+" string")
		case "[]string":
			params = append(params, p.Name+" []string")
		case "int64":
			params = append(params, p.Name+" int64")
		case "int":
			params = append(params, p.Name+" int")
		case "time.Duration":
			params = append(params, p.Name+" time.Duration")
		case "func(string)":
			params = append(params, p.Name+" func(string)")
		case "func() (string, error)":
			params = append(params, p.Name+" func() (string, error)")
		case "interface{}":
			params = append(params, p.Name+" interface{}")
		case "cleat.CallOptions":
			params = append(params, p.Name+" cleat.CallOptions")
		}
	}
	buf.WriteString(strings.Join(params, ", "))
	buf.WriteString(")")

	if wdef.ReturnType != "" {
		buf.WriteString(" ")
		buf.WriteString(wdef.ReturnType)
	}

	buf.WriteString(" {\n")
	for _, stmt := range wdef.Body {
		buf.WriteString("\t" + stmt + "\n")
	}
	buf.WriteString("}\n\n")
}
