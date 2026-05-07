package host

import (
	"context"
	"sync/atomic"

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
	ChildWorkflowWithOptions(ctx context.Context, m api.Module, name, inputJSON string, version int32, runIDPtr, runIDMaxLen uint32) int64
	AwaitChild(ctx context.Context, m api.Module, runID string, resultPtr, resultMaxLen uint32) int64
	AwaitAllChildren(ctx context.Context, m api.Module, runIDsJSON string, resultsPtr, resultsMaxLen uint32) int64
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
}

// registerHostFunctions registers all 26 cleat_* imports on the "env" host module.
func registerHostFunctions(builder wazero.HostModuleBuilder) {
	// cleat_call: (ptr,len x3, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen, respPtr, respMaxLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		service := readWasmString(mem, svcPtr, svcLen)
		op := readWasmString(mem, opPtr, opLen)
		req := readWasmString(mem, reqPtr, reqLen)
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
		msg := readWasmString(mem, msgPtr, msgLen)
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
		desc := readWasmString(mem, descPtr, descLen)
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
		name := readWasmString(mem, namePtr, nameLen)
		return uint64(handlerFromContext(ctx).PollSignal(ctx, m, name, payloadPtr, payloadMaxLen))
	}).Export("cleat_poll_signal")

	// cleat_continue_as_new: (ptr,len) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		inputPtr, inputLen uint32) uint64 {
		mem := m.Memory()
		newInput := readWasmString(mem, inputPtr, inputLen)
		return uint64(handlerFromContext(ctx).ContinueAsNew(ctx, m, newInput))
	}).Export("cleat_continue_as_new")

	// cleat_continue_as_new_versioned: (ptr,len,i32) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		inputPtr, inputLen, newVersion uint32) uint64 {
		mem := m.Memory()
		newInput := readWasmString(mem, inputPtr, inputLen)
		return uint64(handlerFromContext(ctx).ContinueAsNewWithVersion(ctx, m, newInput, int(newVersion)))
	}).Export("cleat_continue_as_new_versioned")

	// cleat_child_workflow: (ptr,len x3) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		namePtr, nameLen, inputPtr, inputLen, runIDPtr, runIDMaxLen uint32) uint64 {
		mem := m.Memory()
		wfName := readWasmString(mem, namePtr, nameLen)
		wfInput := readWasmString(mem, inputPtr, inputLen)
		return uint64(handlerFromContext(ctx).ChildWorkflow(ctx, m, wfName, wfInput, runIDPtr, runIDMaxLen))
	}).Export("cleat_child_workflow")

	// cleat_await_child: (ptr,len x2) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		runIDPtr, runIDLen, resultPtr, resultMaxLen uint32) uint64 {
		mem := m.Memory()
		runID := readWasmString(mem, runIDPtr, runIDLen)
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
		service := readWasmString(mem, svcPtr, svcLen)
		op := readWasmString(mem, opPtr, opLen)
		req := readWasmString(mem, reqPtr, reqLen)
		nonRetryableErrorsJSON := readWasmString(mem, nonRetryPtr, nonRetryLen)
		return uint64(h.DurableCallWithRetry(ctx, m, service, op, req,
			maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs,
			nonRetryableErrorsJSON, respPtr, respMaxLen))
	}).Export("cleat_call_retry")

	// cleat_await_signals: (ptr,len, i64, ptr,maxLen x2) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		namesPtr, namesLen uint32, timeoutMs int64,
		sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen uint32) uint64 {
		mem := m.Memory()
		names := readWasmString(mem, namesPtr, namesLen)
		return uint64(handlerFromContext(ctx).DurableAwaitSignals(ctx, m, names, timeoutMs,
			sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen))
	}).Export("cleat_await_signals")

	// set_query_state: (ptr,len x2) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		keyPtr, keyLen, valPtr, valLen uint32) uint64 {
		mem := m.Memory()
		key := readWasmString(mem, keyPtr, keyLen)
		val := readWasmString(mem, valPtr, valLen)
		return uint64(handlerFromContext(ctx).SetQueryState(ctx, m, key, val))
	}).Export("set_query_state")

	// cleat_call_heartbeat: (ptr,len x3, i64, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen uint32,
		heartbeatIntervalMs int64,
		respPtr, respMaxLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		service := readWasmString(mem, svcPtr, svcLen)
		op := readWasmString(mem, opPtr, opLen)
		req := readWasmString(mem, reqPtr, reqLen)
		return uint64(h.DurableCallWithHeartbeat(ctx, m, service, op, req, heartbeatIntervalMs, respPtr, respMaxLen))
	}).Export("cleat_call_heartbeat")

	// cleat_await_all_children: (ptr,len, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		idsPtr, idsLen uint32,
		resultsPtr, resultsMaxLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		runIDsJSON := readWasmString(mem, idsPtr, idsLen)
		return uint64(h.AwaitAllChildren(ctx, m, runIDsJSON, resultsPtr, resultsMaxLen))
	}).Export("cleat_await_all_children")

	// plugin_call_streaming: (ptr,len x5) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		pluginNamePtr, pluginNameLen,
		funcNamePtr, funcNameLen,
		inputPtr, inputLen,
		responsePtr, responseMaxLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		pluginName := readWasmString(mem, pluginNamePtr, pluginNameLen)
		funcName := readWasmString(mem, funcNamePtr, funcNameLen)
		inputJSON := readWasmString(mem, inputPtr, inputLen)
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
		pluginName := readWasmString(mem, pluginNamePtr, pluginNameLen)
		funcName := readWasmString(mem, funcNamePtr, funcNameLen)
		inputJSON := readWasmString(mem, inputPtr, inputLen)
		return uint64(h.PluginCall(ctx, m, pluginName, funcName, inputJSON, responsePtr, responseMaxLen))
	}).Export("plugin_call")

	// cleat_register_update_handler: (ptr,len) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		namePtr, nameLen uint32) uint64 {
		mem := m.Memory()
		name := readWasmString(mem, namePtr, nameLen)
		return uint64(handlerFromContext(ctx).RegisterUpdateHandler(ctx, m, name))
	}).Export("cleat_register_update_handler")
	// cleat_create_promise: (ptr,len x2) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		namePtr, nameLen, promiseIDPtr, promiseIDMaxLen uint32) uint64 {
		mem := m.Memory()
		name := readWasmString(mem, namePtr, nameLen)
		return uint64(handlerFromContext(ctx).CreatePromise(ctx, m, name, promiseIDPtr, promiseIDMaxLen))
	}).Export("cleat_create_promise")

	// cleat_await_promise: (ptr,len, i64, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		promiseIDPtr, promiseIDLen uint32, timeoutMs int64,
		resultPtr, resultMaxLen uint32) uint64 {
		mem := m.Memory()
		promiseID := readWasmString(mem, promiseIDPtr, promiseIDLen)
		return uint64(handlerFromContext(ctx).AwaitPromise(ctx, m, promiseID, timeoutMs, resultPtr, resultMaxLen))
	}).Export("cleat_await_promise")

	// cleat_send_signal_and_wait: (ptr,len x3, i64, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		targetPtr, targetLen, sigPtr, sigLen, payloadPtr, payloadLen uint32,
		timeoutMs int64,
		respPtr, respMaxLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		targetRunID := readWasmString(mem, targetPtr, targetLen)
		signalName := readWasmString(mem, sigPtr, sigLen)
		payload := readWasmString(mem, payloadPtr, payloadLen)
		return uint64(h.SendSignalAndWait(ctx, m, targetRunID, signalName, payload, timeoutMs, respPtr, respMaxLen))
	}).Export("cleat_send_signal_and_wait")

	// cleat_reply_to_signal: (ptr,len x2) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		correlationPtr, correlationLen, respPtr, respLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		correlationID := readWasmString(mem, correlationPtr, correlationLen)
		response := readWasmString(mem, respPtr, respLen)
		return uint64(h.ReplyToSignal(ctx, m, correlationID, response))
	}).Export("cleat_reply_to_signal")

	// cleat_signal_workflow: (ptr,len x3) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		targetPtr, targetLen, sigPtr, sigLen, payloadPtr, payloadLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		targetRunID := readWasmString(mem, targetPtr, targetLen)
		signalName := readWasmString(mem, sigPtr, sigLen)
		payload := readWasmString(mem, payloadPtr, payloadLen)
		return uint64(h.SignalWorkflow(ctx, m, targetRunID, signalName, payload))
	}).Export("cleat_signal_workflow")

	// cleat_set_scope: (ptr,len x2, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		objTypePtr, objTypeLen, instKeyPtr, instKeyLen uint32,
		prevScopePtr, prevScopeMaxLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		objType := readWasmString(mem, objTypePtr, objTypeLen)
		instKey := readWasmString(mem, instKeyPtr, instKeyLen)
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
		seed := readWasmString(mem, seedPtr, seedLen)
		return uint64(h.UUID(ctx, m, seed, uuidPtr, uuidMaxLen))
	}).Export("cleat_uuid")
}

// nowMs is the global time provider, atomically settable for tests.
var nowMs atomic.Int64
