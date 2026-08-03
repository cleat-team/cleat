package engine

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// handlerContextKey is the context key for the per-execution HostHandler.
type handlerContextKey struct{}

func withHandler(ctx context.Context, h HostHandler) context.Context {
	return context.WithValue(ctx, handlerContextKey{}, h)
}

func handlerFromContext(ctx context.Context) HostHandler {
	return ctx.Value(handlerContextKey{}).(HostHandler)
}

// HostHandler is the per-execution session interface. Each method corresponds
// to one host function import from //go:wasmimport env <name>.
type HostHandler interface {
	DurableCall(ctx context.Context, m api.Module, service, operation, requestJSON string, responsePtr, responseMaxLen uint32) int64
	DurableSleep(ctx context.Context, m api.Module, durationMs int64) int64
	DurableAwaitSignals(ctx context.Context, m api.Module, signalNames string, timeoutMs int64, sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen uint32) int64
	DurableDefer(ctx context.Context, m api.Module, description string, deferIDPtr, deferIDMaxLen uint32) int64
	DurableLog(ctx context.Context, m api.Module, message string) int64
	PollCancellation(ctx context.Context, m api.Module, reasonPtr, reasonMaxLen uint32) int64
	PollSignal(ctx context.Context, m api.Module, signalName string, payloadPtr, payloadMaxLen uint32) int64
	ContinueAsNew(ctx context.Context, m api.Module, newInputJSON string) int64
	ContinueAsNewWithVersion(ctx context.Context, m api.Module, newInputJSON string, newVersion int) int64
	ChildWorkflow(ctx context.Context, m api.Module, name, inputJSON string, runIDPtr, runIDMaxLen uint32) int64
	ChildWorkflowWithOptions(ctx context.Context, m api.Module, name, inputJSON string, version int64, priority int64, parentClosePolicy string, runIDPtr, runIDMaxLen uint32) int64
	ChildWorkflowInSchema(ctx context.Context, m api.Module, targetSchema, name, inputJSON string, version int64, priority int64, parentClosePolicy string, runIDPtr, runIDMaxLen uint32) int64
	AwaitChild(ctx context.Context, m api.Module, runID string, resultPtr, resultMaxLen uint32) int64
	AwaitAllChildren(ctx context.Context, m api.Module, runIDsJSON string, resultsPtr, resultsMaxLen uint32) int64
	PollChild(ctx context.Context, m api.Module, runID string, resultPtr, resultMaxLen uint32) int64
	AwaitAnyChild(ctx context.Context, m api.Module, runIDsJSON string, resultPtr, resultMaxLen uint32) int64
	DurableCallWithRetry(ctx context.Context, m api.Module, service, operation, requestJSON string, maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs int64, nonRetryableErrorsJSON string, responsePtr, responseMaxLen uint32) int64
	DurableCallWithHeartbeat(ctx context.Context, m api.Module, service, operation, requestJSON string, heartbeatIntervalMs int64, responsePtr, responseMaxLen uint32) int64
	Version(ctx context.Context) int64
	MinVersion(ctx context.Context) int64
	SetQueryState(ctx context.Context, m api.Module, key, value string) int64
	Now(ctx context.Context) int64
	Random(ctx context.Context) int64
	CreatePromise(ctx context.Context, m api.Module, name string, promiseIDPtr, promiseIDMaxLen uint32) int64
	AwaitPromise(ctx context.Context, m api.Module, promiseID string, timeoutMs int64, resultPtr, resultMaxLen uint32) int64
	PluginCall(ctx context.Context, m api.Module, pluginName, functionName, inputJSON string, responsePtr, responseMaxLen uint32) int64
	PluginCallStreaming(ctx context.Context, m api.Module, pluginName, functionName, inputJSON string, responsePtr, responseMaxLen uint32) int64
	RegisterUpdateHandler(ctx context.Context, m api.Module, name string) int64

	// Signal correlation (ABI 2.23-2.25)
	SendSignalAndWait(ctx context.Context, m api.Module, targetRunID, signalName, payload string, timeoutMs int64, responsePtr, responseMaxLen uint32) int64
	ReplyToSignal(ctx context.Context, m api.Module, correlationID, response string) int64
	SignalWorkflow(ctx context.Context, m api.Module, targetRunID, signalName, payload string) int64

	// Scoped state / virtual objects (ABI 2.26-2.28)
	SetScope(ctx context.Context, m api.Module, objectType, instanceKey string, prevScopePtr, prevScopeMaxLen uint32) int64
	GetScope(ctx context.Context, m api.Module, objTypePtr, objTypeMaxLen, instKeyPtr, instKeyMaxLen uint32) int64
	UUID(ctx context.Context, m api.Module, seed string, uuidPtr, uuidMaxLen uint32) int64

	// Lock/concurrency key operations.
	AcquireLock(ctx context.Context, m api.Module, key string, ttlMs int64) int64
	ReleaseLock(ctx context.Context, m api.Module, key string) int64

	// SideEffect records non-deterministic computation result in event
	// history on first execution and returns cached result on replay.
	SideEffect(ctx context.Context, m api.Module, computedResult string, respPtr, respMaxLen uint32) int64

	// WorkflowID returns the current workflow's unique identifier.
	WorkflowID(ctx context.Context, m api.Module, idPtr, idMaxLen uint32) int64

	// RunID returns the current workflow run's unique identifier.
	RunID(ctx context.Context, m api.Module, idPtr, idMaxLen uint32) int64

	// ResolvePromise resolves a durable promise with a value.
	ResolvePromise(ctx context.Context, m api.Module, promiseID, value string) int64

	// RejectPromise rejects a durable promise with an error message.
	RejectPromise(ctx context.Context, m api.Module, promiseID, errMsg string) int64

	// DurableSend sends a fire-and-forget request to an external service.
	DurableSend(ctx context.Context, m api.Module, service, operation, requestJSON string) int64

	// DurableScheduleInvoke schedules a delayed one-shot invocation.
	DurableScheduleInvoke(ctx context.Context, m api.Module, service, operation, requestJSON string, delayMs int64) int64

	// RegisterQueryHandler registers a read-only query handler.
	RegisterQueryHandler(ctx context.Context, m api.Module, name string) int64

	// State operations (Stream R)
	SetState(ctx context.Context, m api.Module, key, value string) int64
	GetState(ctx context.Context, m api.Module, key string, valuePtr, valueMaxLen uint32) int64
	DeleteState(ctx context.Context, m api.Module, key string) int64
	IncrState(ctx context.Context, m api.Module, key string, delta int64) int64
	HasState(ctx context.Context, m api.Module, key string) int64
	ListState(ctx context.Context, m api.Module, prefix string, keysPtr, keysMaxLen uint32) int64

	// Detached execution (Stream R)
	RunDetached(ctx context.Context, m api.Module, name, inputJSON string) int64

	// HTTP fetch (Stream R)
	Fetch(ctx context.Context, m api.Module, method, url, headersJSON, body string, responsePtr, responseMaxLen uint32) int64

	// JsonParse validates and normalizes a JSON string via the host's encoding/json.
	JsonParse(ctx context.Context, m api.Module, input string, outPtr, outMaxLen uint32) int64

	// JsonStringify validates and re-serializes a JSON string via the host's encoding/json.
	JsonStringify(ctx context.Context, m api.Module, input string, outPtr, outMaxLen uint32) int64
}

// registerHostFunctions registers all cleat_* imports on the "env" host module.
func registerHostFunctions(builder wazero.HostModuleBuilder, rt *Runtime) {
	// cleat_call: (ptr,len x3, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen, respPtr, respMaxLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		service, ok := readServiceName(mem, svcPtr, svcLen)
		if !ok {
			return uint64(badParamDurableCall)
		}
		op, ok := readServiceName(mem, opPtr, opLen)
		if !ok {
			return uint64(badParamDurableCall)
		}
		// readWasmPayload, not readWasmStringValidated: a durable call that
		// takes no arguments passes "" here and must be allowed.
		req, ok := readWasmPayload(mem, reqPtr, reqLen, MaxWasmStringLen)
		if !ok {
			return uint64(badParamDurableCall)
		}
		return uint64(h.DurableCall(ctx, m, service, op, req, respPtr, respMaxLen))
	}).Export("cleat_call")

	// cleat_sleep: (i64) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, durationMs int64) uint64 {
		return uint64(handlerFromContext(ctx).DurableSleep(ctx, m, durationMs))
	}).Export("cleat_sleep")

	// cleat_now: () -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context) uint64 {
		return uint64(handlerFromContext(ctx).Now(ctx))
	}).Export("cleat_now")

	// cleat_random: () -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context) uint64 {
		return uint64(handlerFromContext(ctx).Random(ctx))
	}).Export("cleat_random")

	// cleat_log: (ptr,len) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, msgPtr, msgLen uint32) uint64 {
		mem := m.Memory()
		msg, ok := readWasmStringValidated(mem, msgPtr, msgLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		return uint64(handlerFromContext(ctx).DurableLog(ctx, m, msg))
	}).Export("cleat_log")

	// cleat_version: () -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context) uint64 {
		return uint64(handlerFromContext(ctx).Version(ctx))
	}).Export("cleat_version")

	// cleat_min_version: () -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context) uint64 {
		return uint64(handlerFromContext(ctx).MinVersion(ctx))
	}).Export("cleat_min_version")

	// cleat_defer: (ptr,len x2) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		descPtr, descLen, deferIDPtr, deferIDMaxLen uint32) uint64 {
		mem := m.Memory()
		desc, ok := readWasmStringValidated(mem, descPtr, descLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		return uint64(handlerFromContext(ctx).DurableDefer(ctx, m, desc, deferIDPtr, deferIDMaxLen))
	}).Export("cleat_defer")

	// cleat_poll_cancellation: (ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		reasonPtr, reasonMaxLen uint32) uint64 {
		return uint64(handlerFromContext(ctx).PollCancellation(ctx, m, reasonPtr, reasonMaxLen))
	}).Export("cleat_poll_cancellation")

	// cleat_poll_signal: (ptr,len x2) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		namePtr, nameLen, payloadPtr, payloadMaxLen uint32) uint64 {
		mem := m.Memory()
		name, ok := readServiceName(mem, namePtr, nameLen)
		if !ok {
			return errBadParam
		}
		return uint64(handlerFromContext(ctx).PollSignal(ctx, m, name, payloadPtr, payloadMaxLen))
	}).Export("cleat_poll_signal")

	// cleat_continue_as_new: (ptr,len) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		inputPtr, inputLen uint32) uint64 {
		mem := m.Memory()
		newInput, ok := readWasmStringValidated(mem, inputPtr, inputLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		return uint64(handlerFromContext(ctx).ContinueAsNew(ctx, m, newInput))
	}).Export("cleat_continue_as_new")

	// cleat_continue_as_new_versioned: (ptr,len,i32) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		inputPtr, inputLen, newVersion uint32) uint64 {
		mem := m.Memory()
		newInput, ok := readWasmStringValidated(mem, inputPtr, inputLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		return uint64(handlerFromContext(ctx).ContinueAsNewWithVersion(ctx, m, newInput, int(newVersion)))
	}).Export("cleat_continue_as_new_versioned")

	// cleat_child_workflow: (ptr,len x3) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		namePtr, nameLen, inputPtr, inputLen, runIDPtr, runIDMaxLen uint32) uint64 {
		mem := m.Memory()
		wfName, ok := readServiceName(mem, namePtr, nameLen)
		if !ok {
			return errBadParam
		}
		wfInput, ok := readWasmStringValidated(mem, inputPtr, inputLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		return uint64(handlerFromContext(ctx).ChildWorkflow(ctx, m, wfName, wfInput, runIDPtr, runIDMaxLen))
	}).Export("cleat_child_workflow")

	// cleat_child_workflow_with_options: (ptr,len x3, i64, i64, ptr,len, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		namePtr, nameLen, inputPtr, inputLen uint32, version int64, priority int64,
		policyPtr, policyLen, runIDPtr, runIDMaxLen uint32) uint64 {
		mem := m.Memory()
		wfName, ok := readServiceName(mem, namePtr, nameLen)
		if !ok {
			return errBadParam
		}
		wfInput, ok := readWasmStringValidated(mem, inputPtr, inputLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		parentClosePolicy := ""
		if policyLen > 0 {
			var ok bool
			parentClosePolicy, ok = readServiceName(mem, policyPtr, policyLen)
			if !ok {
				return errBadParam
			}
		}
		return uint64(handlerFromContext(ctx).ChildWorkflowWithOptions(ctx, m, wfName, wfInput, version, priority, parentClosePolicy, runIDPtr, runIDMaxLen))
	}).Export("cleat_child_workflow_with_options")

	// cleat_child_workflow_in_schema: (ptr,len x4, i64, i64, ptr,len, ptr,maxLen) -> i64
	// Creates a child workflow in a different PostgreSQL schema for cross-instance cooperation.
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		schemaPtr, schemaLen, namePtr, nameLen, inputPtr, inputLen uint32, version int64, priority int64,
		policyPtr, policyLen, runIDPtr, runIDMaxLen uint32) uint64 {
		mem := m.Memory()
		targetSchema, ok := readServiceName(mem, schemaPtr, schemaLen)
		if !ok {
			return errBadParam
		}
		wfName, ok := readServiceName(mem, namePtr, nameLen)
		if !ok {
			return errBadParam
		}
		wfInput, ok := readWasmStringValidated(mem, inputPtr, inputLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		parentClosePolicy := ""
		if policyLen > 0 {
			var ok bool
			parentClosePolicy, ok = readServiceName(mem, policyPtr, policyLen)
			if !ok {
				return errBadParam
			}
		}
		return uint64(handlerFromContext(ctx).ChildWorkflowInSchema(ctx, m, targetSchema, wfName, wfInput, version, priority, parentClosePolicy, runIDPtr, runIDMaxLen))
	}).Export("cleat_child_workflow_in_schema")

	// cleat_await_child: (ptr,len x2) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		runIDPtr, runIDLen, resultPtr, resultMaxLen uint32) uint64 {
		mem := m.Memory()
		runID, ok := readServiceName(mem, runIDPtr, runIDLen)
		if !ok {
			return errBadParam
		}
		return uint64(handlerFromContext(ctx).AwaitChild(ctx, m, runID, resultPtr, resultMaxLen))
	}).Export("cleat_await_child")

	// cleat_call_retry: (ptr,len x3, i64 x4, ptr,len, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen uint32,
		maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs int64,
		nonRetryPtr, nonRetryLen uint32,
		respPtr, respMaxLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		service, ok := readServiceName(mem, svcPtr, svcLen)
		if !ok {
			return uint64(badParamDurableCall)
		}
		op, ok := readServiceName(mem, opPtr, opLen)
		if !ok {
			return uint64(badParamDurableCall)
		}
		req, ok := readWasmPayload(mem, reqPtr, reqLen, MaxWasmStringLen)
		if !ok {
			return uint64(badParamDurableCall)
		}
		// An empty non-retryable-errors list is the common case: most calls
		// name no non-retryable errors at all.
		nonRetryableErrorsJSON, ok := readWasmPayload(mem, nonRetryPtr, nonRetryLen, MaxWasmStringLen)
		if !ok {
			return uint64(badParamDurableCall)
		}
		return uint64(h.DurableCallWithRetry(ctx, m, service, op, req,
			maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs,
			nonRetryableErrorsJSON, respPtr, respMaxLen))
	}).Export("cleat_call_retry")

	// cleat_await_signals: (ptr,len, i64, ptr,maxLen x2) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		namesPtr, namesLen uint32, timeoutMs int64,
		sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen uint32) uint64 {
		mem := m.Memory()
		names, ok := readWasmStringValidated(mem, namesPtr, namesLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		return uint64(handlerFromContext(ctx).DurableAwaitSignals(ctx, m, names, timeoutMs,
			sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen))
	}).Export("cleat_await_signals")

	// set_query_state: (ptr,len x2) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		keyPtr, keyLen, valPtr, valLen uint32) uint64 {
		mem := m.Memory()
		key, ok := readServiceName(mem, keyPtr, keyLen)
		if !ok {
			return errBadParam
		}
		val, ok := readWasmStringValidated(mem, valPtr, valLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		return uint64(handlerFromContext(ctx).SetQueryState(ctx, m, key, val))
	}).Export("set_query_state")

	// cleat_call_heartbeat: (ptr,len x3, i64, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen uint32,
		heartbeatIntervalMs int64,
		respPtr, respMaxLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		service, ok := readServiceName(mem, svcPtr, svcLen)
		if !ok {
			return uint64(badParamDurableCall)
		}
		op, ok := readServiceName(mem, opPtr, opLen)
		if !ok {
			return uint64(badParamDurableCall)
		}
		req, ok := readWasmPayload(mem, reqPtr, reqLen, MaxWasmStringLen)
		if !ok {
			return uint64(badParamDurableCall)
		}
		return uint64(h.DurableCallWithHeartbeat(ctx, m, service, op, req, heartbeatIntervalMs, respPtr, respMaxLen))
	}).Export("cleat_call_heartbeat")

	// cleat_await_all_children: (ptr,len, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		idsPtr, idsLen uint32,
		resultsPtr, resultsMaxLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		runIDsJSON, ok := readWasmStringValidated(mem, idsPtr, idsLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		return uint64(h.AwaitAllChildren(ctx, m, runIDsJSON, resultsPtr, resultsMaxLen))
	}).Export("cleat_await_all_children")

	// cleat_poll_child: (ptr,len, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		runIDPtr, runIDLen, resultPtr, resultMaxLen uint32) uint64 {
		mem := m.Memory()
		runID, ok := readServiceName(mem, runIDPtr, runIDLen)
		if !ok {
			return errBadParam
		}
		return uint64(handlerFromContext(ctx).PollChild(ctx, m, runID, resultPtr, resultMaxLen))
	}).Export("cleat_poll_child")

	// cleat_await_any_child: (ptr,len, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		idsPtr, idsLen, resultPtr, resultMaxLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		runIDsJSON, ok := readWasmStringValidated(mem, idsPtr, idsLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		return uint64(h.AwaitAnyChild(ctx, m, runIDsJSON, resultPtr, resultMaxLen))
	}).Export("cleat_await_any_child")

	// plugin_call_streaming: (ptr,len x5) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		pluginNamePtr, pluginNameLen,
		funcNamePtr, funcNameLen,
		inputPtr, inputLen,
		responsePtr, responseMaxLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		pluginName, ok := readServiceName(mem, pluginNamePtr, pluginNameLen)
		if !ok {
			return uint64(badParamDurableCall)
		}
		funcName, ok := readServiceName(mem, funcNamePtr, funcNameLen)
		if !ok {
			return uint64(badParamDurableCall)
		}
		// Empty input is legitimate: plenty of plugin functions take none.
		inputJSON, ok := readWasmPayload(mem, inputPtr, inputLen, MaxWasmStringLen)
		if !ok {
			return uint64(badParamDurableCall)
		}
		return uint64(h.PluginCallStreaming(ctx, m, pluginName, funcName, inputJSON, responsePtr, responseMaxLen))
	}).Export("plugin_call_streaming")

	// plugin_call: (ptr,len x5) -> i64 — routes through PluginRegistry for event history recording.
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		pluginNamePtr, pluginNameLen,
		funcNamePtr, funcNameLen,
		inputPtr, inputLen,
		responsePtr, responseMaxLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		pluginName, ok := readServiceName(mem, pluginNamePtr, pluginNameLen)
		if !ok {
			return uint64(badParamDurableCall)
		}
		funcName, ok := readServiceName(mem, funcNamePtr, funcNameLen)
		if !ok {
			return uint64(badParamDurableCall)
		}
		// Empty input is legitimate: plenty of plugin functions take none.
		inputJSON, ok := readWasmPayload(mem, inputPtr, inputLen, MaxWasmStringLen)
		if !ok {
			return uint64(badParamDurableCall)
		}
		return uint64(h.PluginCall(ctx, m, pluginName, funcName, inputJSON, responsePtr, responseMaxLen))
	}).Export("plugin_call")

	// cleat_register_update_handler: (ptr,len) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		namePtr, nameLen uint32) uint64 {
		mem := m.Memory()
		name, ok := readServiceName(mem, namePtr, nameLen)
		if !ok {
			return errBadParam
		}
		return uint64(handlerFromContext(ctx).RegisterUpdateHandler(ctx, m, name))
	}).Export("cleat_register_update_handler")
	// cleat_create_promise: (ptr,len x2) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		namePtr, nameLen, promiseIDPtr, promiseIDMaxLen uint32) uint64 {
		mem := m.Memory()
		name, ok := readServiceName(mem, namePtr, nameLen)
		if !ok {
			return errBadParam
		}
		return uint64(handlerFromContext(ctx).CreatePromise(ctx, m, name, promiseIDPtr, promiseIDMaxLen))
	}).Export("cleat_create_promise")

	// cleat_await_promise: (ptr,len, i64, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		promiseIDPtr, promiseIDLen uint32, timeoutMs int64,
		resultPtr, resultMaxLen uint32) uint64 {
		mem := m.Memory()
		promiseID, ok := readServiceName(mem, promiseIDPtr, promiseIDLen)
		if !ok {
			return errBadParam
		}
		return uint64(handlerFromContext(ctx).AwaitPromise(ctx, m, promiseID, timeoutMs, resultPtr, resultMaxLen))
	}).Export("cleat_await_promise")

	// cleat_send_signal_and_wait: (ptr,len x3, i64, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		targetPtr, targetLen, sigPtr, sigLen, payloadPtr, payloadLen uint32,
		timeoutMs int64,
		respPtr, respMaxLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		targetRunID, ok := readServiceName(mem, targetPtr, targetLen)
		if !ok {
			return errBadParam
		}
		signalName, ok := readServiceName(mem, sigPtr, sigLen)
		if !ok {
			return errBadParam
		}
		payload, ok := readWasmStringValidated(mem, payloadPtr, payloadLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		return uint64(h.SendSignalAndWait(ctx, m, targetRunID, signalName, payload, timeoutMs, respPtr, respMaxLen))
	}).Export("cleat_send_signal_and_wait")

	// cleat_reply_to_signal: (ptr,len x2) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		correlationPtr, correlationLen, respPtr, respLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		correlationID, ok := readServiceName(mem, correlationPtr, correlationLen)
		if !ok {
			return errBadParam
		}
		response, ok := readWasmStringValidated(mem, respPtr, respLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		return uint64(h.ReplyToSignal(ctx, m, correlationID, response))
	}).Export("cleat_reply_to_signal")

	// cleat_signal_workflow: (ptr,len x3) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		targetPtr, targetLen, sigPtr, sigLen, payloadPtr, payloadLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		targetRunID, ok := readServiceName(mem, targetPtr, targetLen)
		if !ok {
			return errBadParam
		}
		signalName, ok := readServiceName(mem, sigPtr, sigLen)
		if !ok {
			return errBadParam
		}
		payload, ok := readWasmStringValidated(mem, payloadPtr, payloadLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		return uint64(h.SignalWorkflow(ctx, m, targetRunID, signalName, payload))
	}).Export("cleat_signal_workflow")

	// cleat_set_scope: (ptr,len x2, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		objTypePtr, objTypeLen, instKeyPtr, instKeyLen uint32,
		prevScopePtr, prevScopeMaxLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		objType, ok := readServiceName(mem, objTypePtr, objTypeLen)
		if !ok {
			return errBadParam
		}
		instKey, ok := readServiceName(mem, instKeyPtr, instKeyLen)
		if !ok {
			return errBadParam
		}
		return uint64(h.SetScope(ctx, m, objType, instKey, prevScopePtr, prevScopeMaxLen))
	}).Export("cleat_set_scope")

	// cleat_get_scope: (ptr,maxLen x2) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		objTypePtr, objTypeMaxLen, instKeyPtr, instKeyMaxLen uint32) uint64 {
		h := handlerFromContext(ctx)
		return uint64(h.GetScope(ctx, m, objTypePtr, objTypeMaxLen, instKeyPtr, instKeyMaxLen))
	}).Export("cleat_get_scope")

	// cleat_uuid: (ptr,len, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		seedPtr, seedLen, uuidPtr, uuidMaxLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		seed, ok := readWasmStringValidated(mem, seedPtr, seedLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		return uint64(h.UUID(ctx, m, seed, uuidPtr, uuidMaxLen))
	}).Export("cleat_uuid")

	// cleat_acquire_lock: (ptr,len, i64) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		keyPtr, keyLen uint32, ttlMs int64) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		key, ok := readServiceName(mem, keyPtr, keyLen)
		if !ok {
			return errBadParam
		}
		return uint64(h.AcquireLock(ctx, m, key, ttlMs))
	}).Export("cleat_acquire_lock")

	// cleat_release_lock: (ptr,len) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		keyPtr, keyLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		key, ok := readServiceName(mem, keyPtr, keyLen)
		if !ok {
			return errBadParam
		}
		return uint64(h.ReleaseLock(ctx, m, key))
	}).Export("cleat_release_lock")

	// cleat_side_effect: (ptr,len, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		resultPtr, resultLen, outPtr, outMaxLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		result, ok := readWasmStringValidated(mem, resultPtr, resultLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		return uint64(h.SideEffect(ctx, m, result, outPtr, outMaxLen))
	}).Export("cleat_side_effect")

	// cleat_workflow_id: (ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		idPtr, idMaxLen uint32) uint64 {
		return uint64(handlerFromContext(ctx).WorkflowID(ctx, m, idPtr, idMaxLen))
	}).Export("cleat_workflow_id")

	// cleat_run_id: (ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		idPtr, idMaxLen uint32) uint64 {
		return uint64(handlerFromContext(ctx).RunID(ctx, m, idPtr, idMaxLen))
	}).Export("cleat_run_id")

	// cleat_resolve_promise: (ptr,len x2) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		idPtr, idLen, valPtr, valLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		promiseID, ok := readServiceName(mem, idPtr, idLen)
		if !ok {
			return errBadParam
		}
		value, ok := readWasmStringValidated(mem, valPtr, valLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		return uint64(h.ResolvePromise(ctx, m, promiseID, value))
	}).Export("cleat_resolve_promise")

	// cleat_reject_promise: (ptr,len x2) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		idPtr, idLen, errPtr, errLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		promiseID, ok := readServiceName(mem, idPtr, idLen)
		if !ok {
			return errBadParam
		}
		errMsg, ok := readWasmStringValidated(mem, errPtr, errLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		return uint64(h.RejectPromise(ctx, m, promiseID, errMsg))
	}).Export("cleat_reject_promise")

	// cleat_send: (ptr,len x3) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		service, ok := readServiceName(mem, svcPtr, svcLen)
		if !ok {
			return errBadParam
		}
		op, ok := readServiceName(mem, opPtr, opLen)
		if !ok {
			return errBadParam
		}
		req, ok := readWasmStringValidated(mem, reqPtr, reqLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		return uint64(h.DurableSend(ctx, m, service, op, req))
	}).Export("cleat_send")

	// cleat_schedule_invoke: (ptr,len x3, i64) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen uint32, delayMs int64) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		service, ok := readServiceName(mem, svcPtr, svcLen)
		if !ok {
			return errBadParam
		}
		op, ok := readServiceName(mem, opPtr, opLen)
		if !ok {
			return errBadParam
		}
		req, ok := readWasmStringValidated(mem, reqPtr, reqLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		return uint64(h.DurableScheduleInvoke(ctx, m, service, op, req, delayMs))
	}).Export("cleat_schedule_invoke")

	// cleat_register_query_handler: (ptr,len) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		namePtr, nameLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		name, ok := readServiceName(mem, namePtr, nameLen)
		if !ok {
			return errBadParam
		}
		return uint64(h.RegisterQueryHandler(ctx, m, name))
	}).Export("cleat_register_query_handler")

	// cleat_run_detached: (ptr,len x2) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		namePtr, nameLen, inputPtr, inputLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		name, ok := readServiceName(mem, namePtr, nameLen)
		if !ok {
			return errBadParam
		}
		inputJSON, ok := readWasmStringValidated(mem, inputPtr, inputLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		return uint64(h.RunDetached(ctx, m, name, inputJSON))
	}).Export("cleat_run_detached")

	// cleat_set_state: (ptr,len x2) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		keyPtr, keyLen, valPtr, valLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		key, ok := readServiceName(mem, keyPtr, keyLen)
		if !ok {
			return errBadParam
		}
		value, ok := readWasmStringValidated(mem, valPtr, valLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		return uint64(h.SetState(ctx, m, key, value))
	}).Export("cleat_set_state")

	// cleat_get_state: (ptr,len, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		keyPtr, keyLen, valuePtr, valueMaxLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		key, ok := readServiceName(mem, keyPtr, keyLen)
		if !ok {
			return errBadParam
		}
		return uint64(h.GetState(ctx, m, key, valuePtr, valueMaxLen))
	}).Export("cleat_get_state")

	// cleat_delete_state: (ptr,len) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		keyPtr, keyLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		key, ok := readServiceName(mem, keyPtr, keyLen)
		if !ok {
			return errBadParam
		}
		return uint64(h.DeleteState(ctx, m, key))
	}).Export("cleat_delete_state")

	// cleat_incr_state: (ptr,len, i64) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		keyPtr, keyLen uint32, delta int64) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		key, ok := readServiceName(mem, keyPtr, keyLen)
		if !ok {
			return errBadParam
		}
		return uint64(h.IncrState(ctx, m, key, delta))
	}).Export("cleat_incr_state")

	// cleat_has_state: (ptr,len) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		keyPtr, keyLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		key, ok := readServiceName(mem, keyPtr, keyLen)
		if !ok {
			return errBadParam
		}
		return uint64(h.HasState(ctx, m, key))
	}).Export("cleat_has_state")

	// cleat_list_state: (ptr,len, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		prefixPtr, prefixLen, keysPtr, keysMaxLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		prefix, ok := readWasmStringValidated(mem, prefixPtr, prefixLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		return uint64(h.ListState(ctx, m, prefix, keysPtr, keysMaxLen))
	}).Export("cleat_list_state")

	// cleat_fetch: (ptr,len x4, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		methodPtr, methodLen, urlPtr, urlLen, headersPtr, headersLen, bodyPtr, bodyLen uint32,
		responsePtr, responseMaxLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		method, ok := readServiceName(mem, methodPtr, methodLen)
		if !ok {
			return errBadParam
		}
		url, ok := readWasmStringValidated(mem, urlPtr, urlLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		headersJSON, ok := readWasmStringValidated(mem, headersPtr, headersLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		body, ok := readWasmStringValidated(mem, bodyPtr, bodyLen, MaxWasmStringLen)
		if !ok {
			return errBadParam
		}
		return uint64(h.Fetch(ctx, m, method, url, headersJSON, body, responsePtr, responseMaxLen))
	}).Export("cleat_fetch")

	// cleat_json_parse: (ptr,len, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		jsonPtr, jsonLen, outPtr, outMaxLen uint32) uint64 {
		// The wrapper reads the input, as every other host function's does.
		// packSimpleResult(1) rather than errBadParam: these two report
		// failure as "not valid JSON", which is what an unreadable argument
		// amounts to here, and it is what the guest decodes.
		input, ok := readWasmStringValidated(m.Memory(), jsonPtr, jsonLen, MaxWasmStringLen)
		if !ok {
			return uint64(packSimpleResult(1))
		}
		return uint64(handlerFromContext(ctx).JsonParse(ctx, m, input, outPtr, outMaxLen))
	}).Export("cleat_json_parse")

	// cleat_json_stringify: (ptr,len, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		ptr, len, outPtr, outMaxLen uint32) uint64 {
		input, ok := readWasmStringValidated(m.Memory(), ptr, len, MaxWasmStringLen)
		if !ok {
			return uint64(packSimpleResult(1))
		}
		return uint64(handlerFromContext(ctx).JsonStringify(ctx, m, input, outPtr, outMaxLen))
	}).Export("cleat_json_stringify")

	// cleat_poll_work supplies entry point + input to Go wasip1
	// modules via their _start/main path. Normal Go WASM builds call this
	// from main() to receive work before dispatching to the entry point.
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		entryPtr, entryMaxLen, argsPtr, argsMaxLen uint32) uint64 {
		return 0
	}).Export("cleat_poll_work")

	// cleat_complete signals workflow completion with a result or error.
	// This is called by the WASM export wrapper BEFORE returning, so the
	// worker can capture the result even if the Go WASI runtime subsequently
	// calls proc_exit (which would overwrite the normal return value).
	// status=0 means success (result is JSON), status=1 means error (result is error message).
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		status uint32, resultPtr uint32, resultLen uint32) uint64 {
		mem := m.Memory()
		result := readWasmString(mem, resultPtr, resultLen)
		// Store in context for CallExportWithSuspend to retrieve.
		r := ctx.Value(&cleatCompleteKey)
		if r != nil {
			c := r.(*cleatComplete)
			if status == 0 {
				c.Result = &result
			} else {
				c.Error = &result
			}
		}
		return 0
	}).Export("cleat_complete")
}

// nowMs is the global time provider, atomically settable for tests.
var nowMs atomic.Int64

// UpdateNowMs sets the global Now() seed to the current wall-clock time.
// Call this at worker startup and periodically (e.g., on each poll cycle)
// so that fresh workflow execution sessions see a reasonable wall-clock time.
// During replay, the seed is overridden from event history timestamps.
func UpdateNowMs() {
	nowMs.Store(time.Now().UnixMilli())
}
