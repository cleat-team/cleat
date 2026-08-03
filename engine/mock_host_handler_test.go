//go:build cgo

// mockHostHandler is shared test scaffolding for the wasmtime backend tests.
// It lives in its own file (tagged plain "cgo", not wasmtime_component_cgo)
// because it is used both by backend_wasmtime_test.go, which builds under
// plain cgo, and by component_cgo_test.go, which only builds under the
// opt-in wasmtime_component_cgo tag (see component_cgo.go).

package engine

import (
	"context"

	"github.com/tetratelabs/wazero/api"
)

// mockHostHandler is a minimal HostHandler implementation for testing.
// Each method returns the configured ret value.
type mockHostHandler struct {
	ret int64
}

func (h *mockHostHandler) DurableCall(ctx context.Context, m api.Module, service, operation, requestJSON string, responsePtr, responseMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) DurableSleep(ctx context.Context, m api.Module, durationMs int64) int64 {
	return h.ret
}
func (h *mockHostHandler) DurableAwaitSignals(ctx context.Context, m api.Module, signalNames string, timeoutMs int64, sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) DurableDefer(ctx context.Context, m api.Module, description string, deferIDPtr, deferIDMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) DurableLog(ctx context.Context, m api.Module, message string) int64 {
	return h.ret
}
func (h *mockHostHandler) PollCancellation(ctx context.Context, m api.Module, reasonPtr, reasonMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) PollSignal(ctx context.Context, m api.Module, signalName string, payloadPtr, payloadMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) ContinueAsNew(ctx context.Context, m api.Module, newInputJSON string) int64 {
	return h.ret
}
func (h *mockHostHandler) ContinueAsNewWithVersion(ctx context.Context, m api.Module, newInputJSON string, newVersion int) int64 {
	return h.ret
}
func (h *mockHostHandler) ChildWorkflow(ctx context.Context, m api.Module, name, inputJSON string, runIDPtr, runIDMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) ChildWorkflowWithOptions(ctx context.Context, m api.Module, name, inputJSON string, version int64, priority int64, parentClosePolicy string, runIDPtr, runIDMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) ChildWorkflowInSchema(ctx context.Context, m api.Module, targetSchema, name, inputJSON string, version int64, priority int64, parentClosePolicy string, runIDPtr, runIDMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) AwaitChild(ctx context.Context, m api.Module, runID string, resultPtr, resultMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) AwaitAllChildren(ctx context.Context, m api.Module, runIDsJSON string, resultsPtr, resultsMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) PollChild(ctx context.Context, m api.Module, runID string, resultPtr, resultMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) AwaitAnyChild(ctx context.Context, m api.Module, runIDsJSON string, resultPtr, resultMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) DurableCallWithRetry(ctx context.Context, m api.Module, service, operation, requestJSON string, maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs int64, nonRetryableErrorsJSON string, responsePtr, responseMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) DurableCallWithHeartbeat(ctx context.Context, m api.Module, service, operation, requestJSON string, heartbeatIntervalMs int64, responsePtr, responseMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) Version(ctx context.Context) int64    { return h.ret }
func (h *mockHostHandler) MinVersion(ctx context.Context) int64 { return h.ret }
func (h *mockHostHandler) SetQueryState(ctx context.Context, m api.Module, key, value string) int64 {
	return h.ret
}
func (h *mockHostHandler) Now(ctx context.Context) int64    { return h.ret }
func (h *mockHostHandler) Random(ctx context.Context) int64 { return h.ret }
func (h *mockHostHandler) CreatePromise(ctx context.Context, m api.Module, name string, promiseIDPtr, promiseIDMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) AwaitPromise(ctx context.Context, m api.Module, promiseID string, timeoutMs int64, resultPtr, resultMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) PluginCall(ctx context.Context, m api.Module, pluginName, functionName, inputJSON string, responsePtr, responseMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) PluginCallStreaming(ctx context.Context, m api.Module, pluginName, functionName, inputJSON string, responsePtr, responseMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) RegisterUpdateHandler(ctx context.Context, m api.Module, name string) int64 {
	return h.ret
}
func (h *mockHostHandler) SendSignalAndWait(ctx context.Context, m api.Module, targetRunID, signalName, payload string, timeoutMs int64, responsePtr, responseMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) ReplyToSignal(ctx context.Context, m api.Module, correlationID, response string) int64 {
	return h.ret
}
func (h *mockHostHandler) SignalWorkflow(ctx context.Context, m api.Module, targetRunID, signalName, payload string) int64 {
	return h.ret
}
func (h *mockHostHandler) SetScope(ctx context.Context, m api.Module, objectType, instanceKey string, prevScopePtr, prevScopeMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) GetScope(ctx context.Context, m api.Module, objTypePtr, objTypeMaxLen, instKeyPtr, instKeyMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) UUID(ctx context.Context, m api.Module, seed string, uuidPtr, uuidMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) AcquireLock(ctx context.Context, m api.Module, key string, ttlMs int64) int64 {
	return h.ret
}
func (h *mockHostHandler) ReleaseLock(ctx context.Context, m api.Module, key string) int64 {
	return h.ret
}
func (h *mockHostHandler) SideEffect(ctx context.Context, m api.Module, computedResult string, respPtr, respMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) WorkflowID(ctx context.Context, m api.Module, idPtr, idMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) RunID(ctx context.Context, m api.Module, idPtr, idMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) ResolvePromise(ctx context.Context, m api.Module, promiseID, value string) int64 {
	return h.ret
}
func (h *mockHostHandler) RejectPromise(ctx context.Context, m api.Module, promiseID, errMsg string) int64 {
	return h.ret
}
func (h *mockHostHandler) DurableSend(ctx context.Context, m api.Module, service, operation, requestJSON string) int64 {
	return h.ret
}
func (h *mockHostHandler) DurableScheduleInvoke(ctx context.Context, m api.Module, service, operation, requestJSON string, delayMs int64) int64 {
	return h.ret
}
func (h *mockHostHandler) RegisterQueryHandler(ctx context.Context, m api.Module, name string) int64 {
	return h.ret
}
func (h *mockHostHandler) SetState(ctx context.Context, m api.Module, key, value string) int64 {
	return h.ret
}
func (h *mockHostHandler) GetState(ctx context.Context, m api.Module, key string, valuePtr, valueMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) DeleteState(ctx context.Context, m api.Module, key string) int64 {
	return h.ret
}
func (h *mockHostHandler) IncrState(ctx context.Context, m api.Module, key string, delta int64) int64 {
	return h.ret
}
func (h *mockHostHandler) HasState(ctx context.Context, m api.Module, key string) int64 {
	return h.ret
}
func (h *mockHostHandler) ListState(ctx context.Context, m api.Module, prefix string, keysPtr, keysMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) RunDetached(ctx context.Context, m api.Module, name, inputJSON string) int64 {
	return h.ret
}
func (h *mockHostHandler) Fetch(ctx context.Context, m api.Module, method, url, headersJSON, body string, responsePtr, responseMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) JsonParse(ctx context.Context, m api.Module, jsonPtr, jsonLen, outPtr, outMaxLen uint32) int64 {
	return h.ret
}
func (h *mockHostHandler) JsonStringify(ctx context.Context, m api.Module, ptr, len, outPtr, outMaxLen uint32) int64 {
	return h.ret
}
