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
	ChildWorkflow(ctx context.Context, m api.Module, name, inputJSON string, runIDPtr, runIDMaxLen uint32) int64
	AwaitChild(ctx context.Context, m api.Module, runID string, resultPtr, resultMaxLen uint32) int64
	AwaitAllChildren(ctx context.Context, m api.Module, runIDsJSON string, resultsPtr, resultsMaxLen uint32) int64
	DurableCallWithRetry(ctx context.Context, m api.Module, service, operation, requestJSON string, maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs int64, nonRetryableErrorsJSON string, responsePtr, responseMaxLen uint32) int64
	DurableCallWithHeartbeat(ctx context.Context, m api.Module, service, operation, requestJSON string, heartbeatIntervalMs int64, responsePtr, responseMaxLen uint32) int64
	Version(ctx context.Context) int64
	MinVersion(ctx context.Context) int64
	SetQueryState(ctx context.Context, m api.Module, key, value string) int64
	Now(ctx context.Context) int64
	Random(ctx context.Context) int64
}

// registerHostFunctions registers all 15 durable_* imports on the "env" host module.
func registerHostFunctions(builder wazero.HostModuleBuilder) {
	// durable_call: (ptr,len x3, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen, respPtr, respMaxLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		service := readWasmString(mem, svcPtr, svcLen)
		op := readWasmString(mem, opPtr, opLen)
		req := readWasmString(mem, reqPtr, reqLen)
		return uint64(h.DurableCall(ctx, m, service, op, req, respPtr, respMaxLen))
	}).Export("durable_call")

	// durable_sleep: (i64) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, durationMs int64) uint64 {
		return uint64(handlerFromContext(ctx).DurableSleep(ctx, m, durationMs))
	}).Export("durable_sleep")

	// durable_now: () -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context) uint64 {
		return uint64(handlerFromContext(ctx).Now(ctx))
	}).Export("durable_now")

	// durable_random: () -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context) uint64 {
		return uint64(handlerFromContext(ctx).Random(ctx))
	}).Export("durable_random")

	// durable_log: (ptr,len) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, msgPtr, msgLen uint32) uint64 {
		mem := m.Memory()
		msg := readWasmString(mem, msgPtr, msgLen)
		return uint64(handlerFromContext(ctx).DurableLog(ctx, m, msg))
	}).Export("durable_log")

	// durable_version: () -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context) uint64 {
		return uint64(handlerFromContext(ctx).Version(ctx))
	}).Export("durable_version")

	// durable_min_version: () -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context) uint64 {
		return uint64(handlerFromContext(ctx).MinVersion(ctx))
	}).Export("durable_min_version")

	// durable_defer: (ptr,len x2) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		descPtr, descLen, deferIDPtr, deferIDMaxLen uint32) uint64 {
		mem := m.Memory()
		desc := readWasmString(mem, descPtr, descLen)
		return uint64(handlerFromContext(ctx).DurableDefer(ctx, m, desc, deferIDPtr, deferIDMaxLen))
	}).Export("durable_defer")

	// durable_poll_cancellation: (ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		reasonPtr, reasonMaxLen uint32) uint64 {
		return uint64(handlerFromContext(ctx).PollCancellation(ctx, m, reasonPtr, reasonMaxLen))
	}).Export("durable_poll_cancellation")

	// durable_poll_signal: (ptr,len x2) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		namePtr, nameLen, payloadPtr, payloadMaxLen uint32) uint64 {
		mem := m.Memory()
		name := readWasmString(mem, namePtr, nameLen)
		return uint64(handlerFromContext(ctx).PollSignal(ctx, m, name, payloadPtr, payloadMaxLen))
	}).Export("durable_poll_signal")

	// durable_continue_as_new: (ptr,len) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		inputPtr, inputLen uint32) uint64 {
		mem := m.Memory()
		newInput := readWasmString(mem, inputPtr, inputLen)
		return uint64(handlerFromContext(ctx).ContinueAsNew(ctx, m, newInput))
	}).Export("durable_continue_as_new")

	// durable_child_workflow: (ptr,len x3) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		namePtr, nameLen, inputPtr, inputLen, runIDPtr, runIDMaxLen uint32) uint64 {
		mem := m.Memory()
		wfName := readWasmString(mem, namePtr, nameLen)
		wfInput := readWasmString(mem, inputPtr, inputLen)
		return uint64(handlerFromContext(ctx).ChildWorkflow(ctx, m, wfName, wfInput, runIDPtr, runIDMaxLen))
	}).Export("durable_child_workflow")

	// durable_await_child: (ptr,len x2) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		runIDPtr, runIDLen, resultPtr, resultMaxLen uint32) uint64 {
		mem := m.Memory()
		runID := readWasmString(mem, runIDPtr, runIDLen)
		return uint64(handlerFromContext(ctx).AwaitChild(ctx, m, runID, resultPtr, resultMaxLen))
	}).Export("durable_await_child")

	// durable_call_retry: (ptr,len x3, i64 x4, ptr,len, ptr,maxLen) -> i64
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
	}).Export("durable_call_retry")

	// durable_await_signals: (ptr,len, i64, ptr,maxLen x2) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		namesPtr, namesLen uint32, timeoutMs int64,
		sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen uint32) uint64 {
		mem := m.Memory()
		names := readWasmString(mem, namesPtr, namesLen)
		return uint64(handlerFromContext(ctx).DurableAwaitSignals(ctx, m, names, timeoutMs,
			sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen))
	}).Export("durable_await_signals")

	// set_query_state: (ptr,len x2) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		keyPtr, keyLen, valPtr, valLen uint32) uint64 {
		mem := m.Memory()
		key := readWasmString(mem, keyPtr, keyLen)
		val := readWasmString(mem, valPtr, valLen)
		return uint64(handlerFromContext(ctx).SetQueryState(ctx, m, key, val))
	}).Export("set_query_state")

	// durable_call_heartbeat: (ptr,len x3, i64, ptr,maxLen) -> i64
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
	}).Export("durable_call_heartbeat")

	// durable_await_all_children: (ptr,len, ptr,maxLen) -> i64
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module,
		idsPtr, idsLen uint32,
		resultsPtr, resultsMaxLen uint32) uint64 {
		h := handlerFromContext(ctx)
		mem := m.Memory()
		runIDsJSON := readWasmString(mem, idsPtr, idsLen)
		return uint64(h.AwaitAllChildren(ctx, m, runIDsJSON, resultsPtr, resultsMaxLen))
	}).Export("durable_await_all_children")
}

// nowMs is the global time provider, atomically settable for tests.
var nowMs atomic.Int64
