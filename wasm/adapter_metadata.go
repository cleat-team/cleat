package wasm

import "sort"

// adapterDef describes how to generate the closure for a single HostCalls
// method, bridging the clean Go interface to the //go:wasmimport call.
type adapterDef struct {
	FieldName   string         // HostCallsOptions field name
	ReturnType  string         // Go return type for the closure, e.g. "(string, error)"
	Params      []adapterParam // closure parameter descriptions
	ResultStmts []string       // lines of Go code for result processing

	// PreStmts are emitted before the import arguments are set up. They exist
	// for a method whose import takes an argument the caller does not supply:
	// DurableDeferFunc takes only a closure, but cleat_defer wants a
	// description, so the description is synthesised here.
	PreStmts []string
}

type adapterParam struct {
	Name string // parameter name
	Type string // "string", "int64", "[]string"
}

// suspendSentinelStmts is the guest-side half of callSuspendSentinel
// (engine/memory.go), and it must run before any field of the result is
// decoded.
//
// The sentinel is bit 31, which is free in all six result layouts a host call
// that starts fresh work can return. "Free" means the host cannot produce it,
// not that these decoders would otherwise ignore it: in the await-signals
// layout bit 31 lands inside the timed-out field, read below as
// `(r>>16)&0xFFFF != 0`, so checking fields first would turn a stop into an
// ordinary timeout and the guest would run on. Order is the contract.
//
// See IMPROVEMENT-PLAN 3.84 and ABI.md.
var suspendSentinelStmts = []string{
	"if uint64(result)&(1<<31) != 0 {",
	"	panic(cleat.ErrSuspend)",
	"}",
}

// withSuspendCheck prefixes the sentinel test onto a decoder's statements. Every
// host call the host can refuse mid-segment goes through it, so the check
// cannot be forgotten by a decoder that is added later.
func withSuspendCheck(stmts ...string) []string {
	out := make([]string, 0, len(suspendSentinelStmts)+len(stmts))
	out = append(out, suspendSentinelStmts...)
	return append(out, stmts...)
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
		ResultStmts: withSuspendCheck(
			"responseLen := uint32(uint64(result) >> 40)",
			"callErrorCode := cleat.CallErrorCode((uint64(result) >> 8) & 0xFFFFFFFF)",
			"errCode := uint32(result & 0xFF)",
			"if errCode != 0 {",
			`	return "", &cleat.CallError{`,
			`		Service:   service,`,
			`		Operation: operation,`,
			`		Code:      callErrorCode,`,
			`		Message:   callErrorMessage("cleat_call", responseBuf, responseLen, uint32(callErrorCode)),`,
			`	}`,
			"}",
			"return unsafe.String(&responseBuf[0], int(responseLen)), nil",
		),
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
		ResultStmts: withSuspendCheck(
			"signalNameLen := uint32(uint64(result) >> 48)",
			"payloadLen := uint32((uint64(result) >> 32) & 0xFFFF)",
			"timedOut := uint32((uint64(result) >> 16) & 0xFFFF) != 0",
			"errCode := uint32(result & 0xFFFF)",
			"if errCode != 0 {",
			`	return "", "", false, fmt.Errorf("cleat_await_signals: %s", hostErrMessage(signalNameBuf[:], signalNameLen))`,
			"}",
			"return unsafe.String(&signalNameBuf[0], int(signalNameLen)), unsafe.String(&payloadBuf[0], int(payloadLen)), timedOut, nil",
		),
	},
	// The scope pair. ClearScope deliberately has NO entry here: it is
	// SetScope("", "") -- the documented empty-pair call -- so it is a Go-side
	// wrapper like DurableFetch over DurableCall, and its hostFunctions row
	// records which import it ultimately reaches rather than asking for an
	// adapter of its own.
	//
	// cleat_set_scope packs prevLen in the high 32 bits (packSimpleResult) and
	// errCode in the low bits. That length was returned as a bare 0 on every
	// success path until #1043, so an adapter written against the old encoding
	// would have compiled, run, and handed back "" forever.
	"SetScope": {
		FieldName:  "SetScope",
		ReturnType: "(string, error)",
		Params: []adapterParam{
			{"objectType", "string"},
			{"instanceKey", "string"},
		},
		ResultStmts: []string{
			"prevLen := uint32(uint64(result) >> 32)",
			"errCode := uint32(result & 0xFFFF)",
			"if errCode != 0 {",
			`	return "", fmt.Errorf("cleat_set_scope: %s", hostErrMessage(prevScopeBuf[:], prevLen))`,
			"}",
			"return unsafe.String(&prevScopeBuf[0], int(prevLen)), nil",
		},
	},
	"GetScope": {
		FieldName:  "GetScope",
		ReturnType: "(string, string, error)",
		ResultStmts: []string{
			"objTypeLen := uint32(uint64(result) >> 32)",
			"instKeyLen := uint32(result)",
			"return unsafe.String(&objectTypeBuf[0], int(objTypeLen)), " +
				"unsafe.String(&instanceKeyBuf[0], int(instKeyLen)), nil",
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
			`	return "", fmt.Errorf("cleat_defer: %s", hostErrMessage(deferIDBuf[:], deferIDLen))`,
			"}",
			"return unsafe.String(&deferIDBuf[0], int(deferIDLen)), nil",
		},
	},
	// DurableDeferFunc registers a closure, so unlike DurableDefer it has a
	// body the guest can actually run. The ID the host mints is the key: it is
	// what the workflow gets back, and what _cleatRunDeferred looks up.
	//
	// The host cannot run this body. It invokes defers by export name from a
	// fresh instance, and a closure lives in the memory of the instance that
	// registered it -- so a body registered at runtime is unreachable from
	// outside. The guest runs its own defers instead, on the path where the
	// entry point finished. IMPROVEMENT-PLAN 3.35, 3.70.
	"DurableDeferFunc": {
		FieldName:  "DurableDeferFunc",
		ReturnType: "(string, error)",
		Params: []adapterParam{
			{"fn", "func()"},
		},
		PreStmts: []string{
			// Before the host call, not after: a check that ran after
			// cleat_defer would leave the durable event behind, which is the
			// whole defect. IMPROVEMENT-PLAN 3.35 phase 4.
			"if _cleatInDeferPhase {",
			`	return "", _cleatErrInDeferPhase("DurableDeferFunc")`,
			"}",
			`description := "deferred function"`,
			"descriptionPtr, descriptionLen := stringPtr(description)",
		},
		ResultStmts: []string{
			"deferIDLen := uint32(uint64(result) >> 32)",
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`	return "", fmt.Errorf("cleat_defer: %s", hostErrMessage(deferIDBuf[:], deferIDLen))`,
			"}",
			// Copy rather than alias: unsafe.String would pin the whole 64 KiB
			// output buffer for as long as the table holds the key.
			"deferID := string(unsafe.String(&deferIDBuf[0], int(deferIDLen)))",
			"_cleatRegisterDefer(deferID, fn)",
			"return deferID, nil",
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
	// PollUpdate returns a JSON envelope {"name","payload","request_id"} in one
	// buffer rather than three out-params: three lengths plus a found flag do
	// not fit an i64 alongside each other. The SDK decodes it.
	//
	// withSuspendCheck because delivering an update runs guest code that can
	// start new work, so a defer segment must refuse it. See
	// engine/updater.go and stopSurfaces["DurablePollUpdate"].
	"PollUpdate": {
		FieldName:  "PollUpdate",
		ReturnType: "(string, bool, error)",
		ResultStmts: withSuspendCheck(
			"envelopeLen := uint32(uint64(result) >> 32)",
			"flags := uint32(result)",
			"errCode := flags & 0xFF",
			"found := (flags >> 8) != 0",
			"if errCode != 0 {",
			`	return "", false, fmt.Errorf("cleat_poll_update: %s", hostErrMessage(envelopeBuf[:], envelopeLen))`,
			"}",
			"if !found {",
			`	return "", false, nil`,
			"}",
			"return unsafe.String(&envelopeBuf[0], int(envelopeLen)), true, nil",
		),
	},
	// CompleteUpdate: three strings in, nothing out but an error code.
	"CompleteUpdate": {
		FieldName:  "CompleteUpdate",
		ReturnType: "error",
		Params: []adapterParam{
			{"requestID", "string"},
			// Not "result": the generated ResultStmts below refer to a
			// variable of that name holding the host call's return value, and a
			// parameter would shadow it.
			{"resultJSON", "string"},
			{"errMsg", "string"},
		},
		ResultStmts: withSuspendCheck(
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`	return fmt.Errorf("cleat_complete_update: error %d", errCode)`,
			"}",
			"return nil",
		),
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
			`	return "", false, fmt.Errorf("cleat_poll_signal: %s", hostErrMessage(payloadBuf[:], payloadLen))`,
			"}",
			"return unsafe.String(&payloadBuf[0], int(payloadLen)), found, nil",
		},
	},
	// DurableSend: three strings in, nothing out. IMPROVEMENT-PLAN 3.226.
	// ScheduleInvoke below is the same call with a delay; both get only an
	// error code back, since there is no response buffer.
	"DurableSend": {
		FieldName:  "DurableSend",
		ReturnType: "error",
		Params: []adapterParam{
			{"service", "string"},
			{"operation", "string"},
			{"requestJSON", "string"},
		},
		ResultStmts: []string{
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`	return fmt.Errorf("cleat_send: error %d", errCode)`,
			"}",
			"return nil",
		},
	},
	// ResolvePromise: two strings in, nothing out. IMPROVEMENT-PLAN 3.226.
	"ResolvePromise": {
		FieldName:  "ResolvePromise",
		ReturnType: "error",
		Params: []adapterParam{
			{"id", "string"},
			{"value", "string"},
		},
		ResultStmts: []string{
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`	return fmt.Errorf("cleat_resolve_promise: error %d", errCode)`,
			"}",
			"return nil",
		},
	},
	// RejectPromise: two strings in, nothing out. The mirror of ResolvePromise.
	// IMPROVEMENT-PLAN 3.226.
	"RejectPromise": {
		FieldName:  "RejectPromise",
		ReturnType: "error",
		Params: []adapterParam{
			{"id", "string"},
			{"errMsg", "string"},
		},
		ResultStmts: []string{
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`	return fmt.Errorf("cleat_reject_promise: error %d", errCode)`,
			"}",
			"return nil",
		},
	},
	// ScheduleInvoke: three strings and a delay in, nothing out.
	// IMPROVEMENT-PLAN 3.224. Same shape as SignalWorkflow below: no response
	// buffer, so the error code is all the guest gets.
	"ScheduleInvoke": {
		FieldName:  "ScheduleInvoke",
		ReturnType: "error",
		Params: []adapterParam{
			{"service", "string"},
			{"operation", "string"},
			{"requestJSON", "string"},
			{"delayMs", "int64"},
		},
		ResultStmts: []string{
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`	return fmt.Errorf("cleat_schedule_invoke: error %d", errCode)`,
			"}",
			"return nil",
		},
	},
	// SignalWorkflow: three strings in, nothing out. IMPROVEMENT-PLAN 3.224.
	//
	// errCode is the full 64-bit result rather than a packed field: the engine
	// returns 0 on success and a non-zero sentinel otherwise -- including
	// errSignalAuthRequiredInt when signal authorization refuses the target --
	// and there is no response buffer to carry a message, so the code is all
	// the guest gets.
	"SignalWorkflow": {
		FieldName:  "SignalWorkflow",
		ReturnType: "error",
		Params: []adapterParam{
			{"targetRunID", "string"},
			{"signalName", "string"},
			{"payload", "string"},
		},
		ResultStmts: []string{
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`	return fmt.Errorf("cleat_signal_workflow: error %d", errCode)`,
			"}",
			"return nil",
		},
	},
	"ContinueAsNew": {
		FieldName:  "ContinueAsNew",
		ReturnType: "error",
		Params: []adapterParam{
			{"newInputJSON", "string"},
		},
		PreStmts: []string{
			// IMPROVEMENT-PLAN 3.35 phase 4. Before the host call: a check
			// after cleat_continue_as_new would leave the event in the
			// history, and the worker stores 'done' anyway because the
			// wrapper reports the already-decided result -- so the
			// continuation is recorded and silently never taken.
			"if _cleatInDeferPhase {",
			`	return _cleatErrInDeferPhase("ContinueAsNew")`,
			"}",
		},
		ResultStmts: []string{
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`	return fmt.Errorf("cleat_continue_as_new: error %d", errCode)`,
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
		PreStmts: []string{
			// IMPROVEMENT-PLAN 3.35 phase 4. Before the host call: a check
			// after cleat_continue_as_new would leave the event in the
			// history, and the worker stores 'done' anyway because the
			// wrapper reports the already-decided result -- so the
			// continuation is recorded and silently never taken.
			"if _cleatInDeferPhase {",
			`	return _cleatErrInDeferPhase("ContinueAsNewWithVersion")`,
			"}",
		},
		ResultStmts: []string{
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`	return fmt.Errorf("cleat_continue_as_new_versioned: error %d", errCode)`,
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
		ResultStmts: withSuspendCheck(
			"runIDLen := uint32(uint64(result) >> 32)",
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`	return "", fmt.Errorf("cleat_child_workflow: %s", hostErrMessage(runIDBuf[:], runIDLen))`,
			"}",
			"return unsafe.String(&runIDBuf[0], int(runIDLen)), nil",
		),
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
		ResultStmts: withSuspendCheck(
			"runIDLen := uint32(uint64(result) >> 32)",
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`	return "", fmt.Errorf("cleat_child_workflow_with_options: %s", hostErrMessage(runIDBuf[:], runIDLen))`,
			"}",
			"return unsafe.String(&runIDBuf[0], int(runIDLen)), nil",
		),
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
			`	return "", fmt.Errorf("cleat_await_child: %s", hostErrMessage(resultBuf[:], resultLen))`,
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
			`    return nil, fmt.Errorf("cleat_await_all_children: %s", hostErrMessage(resultsBuf[:], resultLen))`,
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
			`	return "", "", fmt.Errorf("cleat_poll_child: %s", hostErrMessage(resultBuf[:], resultLen))`,
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
			`	return "", "", fmt.Errorf("cleat_await_any_child: %s", hostErrMessage(resultBuf[:], resultLen))`,
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
		ResultStmts: withSuspendCheck(
			"responseLen := uint32(uint64(result) >> 40)",
			"callErrorCode := cleat.CallErrorCode((uint64(result) >> 8) & 0xFFFFFFFF)",
			"errCode := uint32(result & 0xFF)",
			"if errCode != 0 {",
			`	return "", &cleat.CallError{`,
			`		Service:   service,`,
			`		Operation: operation,`,
			`		Code:      callErrorCode,`,
			`		Message:   callErrorMessage("cleat_call_retry", responseBuf, responseLen, uint32(callErrorCode)),`,
			`	}`,
			"}",
			"return unsafe.String(&responseBuf[0], int(responseLen)), nil",
		),
	},
	"DurableCallWithHeartbeat": {
		FieldName:  "DurableCallWithHeartbeat",
		ReturnType: "(string, error)",
		Params: []adapterParam{
			{"service", "string"},
			{"operation", "string"},
			{"requestJSON", "string"},
			{"heartbeatInterval", "time.Duration"},
		},
		ResultStmts: withSuspendCheck(
			"responseLen := uint32(uint64(result) >> 40)",
			"callErrorCode := cleat.CallErrorCode((uint64(result) >> 8) & 0xFFFFFFFF)",
			"errCode := uint32(result & 0xFF)",
			"if errCode != 0 {",
			`	return "", &cleat.CallError{`,
			`		Service:   service,`,
			`		Operation: operation,`,
			`		Code:      callErrorCode,`,
			`		Message:   callErrorMessage("cleat_call_heartbeat", responseBuf, responseLen, uint32(callErrorCode)),`,
			`	}`,
			"}",
			"return unsafe.String(&responseBuf[0], int(responseLen)), nil",
		),
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
			// High 32 bits, matching packSimpleResult and every other
			// length-returning entry here. This read the low half until
			// IMPROVEMENT-PLAN §2.19 -- i.e. the error code, always 0 on
			// success, so WorkflowID() returned "".
			"idLen := uint32(uint64(result) >> 32)",
			"return unsafe.String(&idBuf[0], int(idLen))",
		},
	},
	"RunID": {
		FieldName:  "RunID",
		ReturnType: "string",
		ResultStmts: []string{
			// See WorkflowID above -- same defect, same fix (§2.19).
			"idLen := uint32(uint64(result) >> 32)",
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
	"RunDetached": {
		FieldName:  "RunDetached",
		ReturnType: "error",
		Params: []adapterParam{
			{"name", "string"},
			{"inputJSON", "string"},
		},
		ResultStmts: []string{
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`	return fmt.Errorf("cleat_run_detached: error %d", errCode)`,
			"}",
			"return nil",
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
			"return \"\", fmt.Errorf(\"cleat_create_promise: %s\", hostErrMessage(promiseIDOutBuf[:], promiseIDLen))",
			"}",
			"return unsafe.String(&promiseIDOutBuf[0], int(promiseIDLen)), nil",
		},
	},
	"AwaitPromise": {
		FieldName:  "AwaitPromise",
		ReturnType: "(string, bool, error)",
		Params: []adapterParam{
			{"promiseID", "string"},
			// time.Duration, not int64: cleat.HostCallsOptions.AwaitPromise is
			// func(promiseID string, timeout time.Duration). The generator
			// emits `timeoutMs := timeout.Milliseconds()` for a Duration param,
			// which is also what the import's timeoutMs argument needs.
			{"timeout", "time.Duration"},
		},
		ResultStmts: []string{
			"resultLen := uint32(uint64(result) >> 32)",
			"timedOut := uint32((uint64(result) >> 16) & 0xFFFF) != 0",
			"errCode := uint32(result & 0xFFFF)",
			"if errCode != 0 {",
			"return \"\", false, fmt.Errorf(\"cleat_await_promise: %s\", hostErrMessage(resultOutBuf[:], resultLen))",
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
		ResultStmts: withSuspendCheck(
			"responseLen := uint32(uint64(result) >> 40)",
			"callErrorCode := uint32((uint64(result) >> 8) & 0xFFFFFFFF)",
			"errCode := uint32(result & 0xFF)",
			"if errCode != 0 {",
			`	return "", fmt.Errorf("%s", callErrorMessage("plugin_call", responseBuf, responseLen, callErrorCode))`,
			"}",
			"return unsafe.String(&responseBuf[0], int(responseLen)), nil",
		),
	},
	"AcquireLock": {
		FieldName:  "AcquireLock",
		ReturnType: "(bool, error)",
		Params: []adapterParam{
			{"key", "string"},
			{"ttlMs", "int64"},
		},
		ResultStmts: withSuspendCheck(
			"errCode := uint32(result & 0xFF)",
			"acquired := uint32((uint64(result) >> 8) & 0x1) != 0",
			"if errCode != 0 {",
			`    return false, fmt.Errorf("cleat_acquire_lock: error %d", errCode)`,
			"}",
			"return acquired, nil",
		),
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
			`    return fmt.Errorf("cleat_release_lock: error %d", errCode)`,
			"}",
			"return nil",
		},
	},
	"SideEffect": {
		FieldName:  "SideEffect",
		ReturnType: "(string, error)",
		Params: []adapterParam{
			// A string, not the func. cleat.HostCallsImpl.SideEffect takes the
			// closure, calls it, and passes the computed string to
			// HostCallsOptions.SideEffect -- which is
			// func(computedResult string) (string, error). Emitting the func
			// signature here produced a closure the struct literal would not
			// accept, so no workflow calling SideEffect could compile.
			{"computedResult", "string"},
		},
		ResultStmts: withSuspendCheck(
			"cachedResultLen := uint32(uint64(result) >> 32)",
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`    return "", fmt.Errorf("cleat_side_effect: %s", hostErrMessage(cachedResultBuf[:], cachedResultLen))`,
			"}",
			"return unsafe.String(&cachedResultBuf[0], int(cachedResultLen)), nil",
		),
	},
	"ScheduleCron": {
		FieldName:  "ScheduleCron",
		ReturnType: "(string, error)",
		Params: []adapterParam{
			{"workflowName", "string"},
			{"cronExpr", "string"},
			{"timezone", "string"},
			{"inputJSON", "string"},
		},
		ResultStmts: withSuspendCheck(
			"scheduleIDLen := uint32(uint64(result) >> 32)",
			"errCode := uint32(result)",
			"if errCode != 0 {",
			// The host writes the reason into the same buffer, and it is the
			// only useful thing here: cleat_schedule_cron returns 1 for every
			// failure, so the CallErrorCode legend the older adapters print
			// would report a bad cron expression as a timeout.
			`	return "", fmt.Errorf("cleat_schedule_cron: %s", hostErrMessage(scheduleIDBuf[:], scheduleIDLen))`,
			"}",
			"return unsafe.String(&scheduleIDBuf[0], int(scheduleIDLen)), nil",
		),
	},
	"DeleteCron": {
		FieldName:  "DeleteCron",
		ReturnType: "error",
		Params: []adapterParam{
			{"scheduleID", "string"},
		},
		ResultStmts: []string{
			"errCode := uint32(result)",
			"if errCode != 0 {",
			// No out buffer in this call's ABI, so there is no message to
			// report -- and the CallErrorCode legend does not apply, since
			// the host returns 1 for every failure.
			`	return fmt.Errorf("cleat_delete_cron failed (code %d)", errCode)`,
			"}",
			"return nil",
		},
	},
	"ListCrons": {
		FieldName:  "ListCrons",
		ReturnType: "(string, error)",
		ResultStmts: []string{
			"resultLen := uint32(uint64(result) >> 32)",
			"errCode := uint32(result)",
			"if errCode != 0 {",
			`	return "", fmt.Errorf("cleat_list_crons: %s", hostErrMessage(resultBuf[:], resultLen))`,
			"}",
			"return unsafe.String(&resultBuf[0], int(resultLen)), nil",
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
		ResultStmts: withSuspendCheck(
			"responseLen := uint32(uint64(result) >> 40)",
			"callErrorCode := uint32((uint64(result) >> 8) & 0xFFFFFFFF)",
			"errCode := uint32(result & 0xFF)",
			"if errCode != 0 {",
			`		return nil, fmt.Errorf("%s", callErrorMessage("plugin_call_streaming", responseBuf, responseLen, callErrorCode))`,
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
		),
	},
}

// AdapterFieldNames returns every cleat.HostCallsOptions field the host-adapter
// generator can emit, sorted.
//
// Exported for cmd/cleat's compile test, which needs to assert that its fixture
// exercises the whole table -- a host call the fixture never calls is a host
// call nobody compiles. See IMPROVEMENT-PLAN.md 3.204.
func AdapterFieldNames() []string {
	names := make([]string, 0, len(adapterDefs))
	for _, def := range adapterDefs {
		names = append(names, def.FieldName)
	}
	sort.Strings(names)
	return names
}
