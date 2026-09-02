//go:build cgo

// mockHostHandler is shared test scaffolding for the wasmtime backend tests.
// It lives in its own file (tagged plain "cgo", not wasmtime_component_cgo)
// because it is used both by backend_wasmtime_test.go, which builds under
// plain cgo, and by component_cgo_test.go, which only builds under the
// opt-in wasmtime_component_cgo tag (see component_cgo.go).

package engine

import (
	"context"
	"sync"

	"github.com/tetratelabs/wazero/api"
)

// mockCall is one host-handler invocation as the handler actually saw it:
// the method name, and the string arguments the wasmtime wrapper decoded out
// of guest memory and passed in.
type mockCall struct {
	method string
	args   []string
}

// mockHostHandler is a minimal HostHandler implementation for testing.
// Each method records the call and returns the configured ret value.
//
// The recording exists because returning `ret` is not, on its own, an
// observable a test can assert anything useful about. Every method returns the
// same canned value whatever happens, so a test that checks only the return
// value passes identically against a correct wrapper, a wrapper that decodes
// its arguments wrongly, and a wrapper that never reaches the handler at all.
// That is IMPROVEMENT-PLAN §2.16, and the same blind spot produced real
// defects in §2.14 (a nil dereference on every call), §2.18 (18 wrappers
// writing zero bytes back to the guest) and §2.22.
//
// What the wrapper is *for* is transporting arguments across the guest/host
// boundary. Recording them makes that property assertable; see
// closureSetup.expectCall.
type mockHostHandler struct {
	ret int64

	// Guarded because wasmtime may invoke a handler from any goroutine, and
	// the tests run under -race.
	mu    sync.Mutex
	calls []mockCall
}

func (h *mockHostHandler) record(method string, args ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, mockCall{method: method, args: args})
}

// recorded returns a copy of the calls seen so far.
func (h *mockHostHandler) recorded() []mockCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]mockCall(nil), h.calls...)
}

// reset drops the recorded calls, so table-driven subtests can each assert
// against their own invocation rather than the accumulated total.
func (h *mockHostHandler) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = nil
}

func (h *mockHostHandler) DurableCall(ctx context.Context, m api.Module, service, operation, requestJSON string, responsePtr, responseMaxLen uint32) int64 {
	h.record("DurableCall", service, operation, requestJSON)
	return h.ret
}
func (h *mockHostHandler) DurableSleep(ctx context.Context, m api.Module, durationMs int64) int64 {
	h.record("DurableSleep")
	return h.ret
}
func (h *mockHostHandler) DurableAwaitSignals(ctx context.Context, m api.Module, signalNames string, timeoutMs int64, sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen uint32) int64 {
	h.record("DurableAwaitSignals", signalNames)
	return h.ret
}
func (h *mockHostHandler) DurableDefer(ctx context.Context, m api.Module, description string, deferIDPtr, deferIDMaxLen uint32) int64 {
	h.record("DurableDefer", description)
	return h.ret
}
func (h *mockHostHandler) DurableLog(ctx context.Context, m api.Module, message string) int64 {
	h.record("DurableLog", message)
	return h.ret
}
func (h *mockHostHandler) PollCancellation(ctx context.Context, m api.Module, reasonPtr, reasonMaxLen uint32) int64 {
	h.record("PollCancellation")
	return h.ret
}
func (h *mockHostHandler) PollSignal(ctx context.Context, m api.Module, signalName string, payloadPtr, payloadMaxLen uint32) int64 {
	h.record("PollSignal", signalName)
	return h.ret
}
func (h *mockHostHandler) ContinueAsNew(ctx context.Context, m api.Module, newInputJSON string) int64 {
	h.record("ContinueAsNew", newInputJSON)
	return h.ret
}
func (h *mockHostHandler) ContinueAsNewWithVersion(ctx context.Context, m api.Module, newInputJSON string, newVersion int) int64 {
	h.record("ContinueAsNewWithVersion", newInputJSON)
	return h.ret
}
func (h *mockHostHandler) ChildWorkflow(ctx context.Context, m api.Module, name, inputJSON string, runIDPtr, runIDMaxLen uint32) int64 {
	h.record("ChildWorkflow", name, inputJSON)
	return h.ret
}
func (h *mockHostHandler) ChildWorkflowWithOptions(ctx context.Context, m api.Module, name, inputJSON string, version int64, priority int64, parentClosePolicy string, runIDPtr, runIDMaxLen uint32) int64 {
	h.record("ChildWorkflowWithOptions", name, inputJSON, parentClosePolicy)
	return h.ret
}
func (h *mockHostHandler) AwaitChild(ctx context.Context, m api.Module, runID string, resultPtr, resultMaxLen uint32) int64 {
	h.record("AwaitChild", runID)
	return h.ret
}
func (h *mockHostHandler) AwaitAllChildren(ctx context.Context, m api.Module, runIDsJSON string, resultsPtr, resultsMaxLen uint32) int64 {
	h.record("AwaitAllChildren", runIDsJSON)
	return h.ret
}
func (h *mockHostHandler) PollChild(ctx context.Context, m api.Module, runID string, resultPtr, resultMaxLen uint32) int64 {
	h.record("PollChild", runID)
	return h.ret
}
func (h *mockHostHandler) AwaitAnyChild(ctx context.Context, m api.Module, runIDsJSON string, resultPtr, resultMaxLen uint32) int64 {
	h.record("AwaitAnyChild", runIDsJSON)
	return h.ret
}
func (h *mockHostHandler) DurableCallWithRetry(ctx context.Context, m api.Module, service, operation, requestJSON string, maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs int64, nonRetryableErrorsJSON string, responsePtr, responseMaxLen uint32) int64 {
	h.record("DurableCallWithRetry", service, operation, requestJSON, nonRetryableErrorsJSON)
	return h.ret
}
func (h *mockHostHandler) DurableCallWithHeartbeat(ctx context.Context, m api.Module, service, operation, requestJSON string, heartbeatIntervalMs int64, responsePtr, responseMaxLen uint32) int64 {
	h.record("DurableCallWithHeartbeat", service, operation, requestJSON)
	return h.ret
}
func (h *mockHostHandler) Version(ctx context.Context) int64 {
	h.record("Version")
	return h.ret
}
func (h *mockHostHandler) MinVersion(ctx context.Context) int64 {
	h.record("MinVersion")
	return h.ret
}
func (h *mockHostHandler) SetQueryState(ctx context.Context, m api.Module, key, value string) int64 {
	h.record("SetQueryState", key, value)
	return h.ret
}
func (h *mockHostHandler) Now(ctx context.Context) int64 {
	h.record("Now")
	return h.ret
}
func (h *mockHostHandler) Random(ctx context.Context) int64 {
	h.record("Random")
	return h.ret
}
func (h *mockHostHandler) CreatePromise(ctx context.Context, m api.Module, name string, promiseIDPtr, promiseIDMaxLen uint32) int64 {
	h.record("CreatePromise", name)
	return h.ret
}
func (h *mockHostHandler) AwaitPromise(ctx context.Context, m api.Module, promiseID string, timeoutMs int64, resultPtr, resultMaxLen uint32) int64 {
	h.record("AwaitPromise", promiseID)
	return h.ret
}
func (h *mockHostHandler) PluginCall(ctx context.Context, m api.Module, pluginName, functionName, inputJSON string, responsePtr, responseMaxLen uint32) int64 {
	h.record("PluginCall", pluginName, functionName, inputJSON)
	return h.ret
}
func (h *mockHostHandler) PluginCallStreaming(ctx context.Context, m api.Module, pluginName, functionName, inputJSON string, responsePtr, responseMaxLen uint32) int64 {
	h.record("PluginCallStreaming", pluginName, functionName, inputJSON)
	return h.ret
}
func (h *mockHostHandler) RegisterUpdateHandler(ctx context.Context, m api.Module, name string) int64 {
	h.record("RegisterUpdateHandler", name)
	return h.ret
}
func (h *mockHostHandler) SendSignalAndWait(ctx context.Context, m api.Module, targetRunID, signalName, payload string, timeoutMs int64, responsePtr, responseMaxLen uint32) int64 {
	h.record("SendSignalAndWait", targetRunID, signalName, payload)
	return h.ret
}
func (h *mockHostHandler) ReplyToSignal(ctx context.Context, m api.Module, correlationID, response string) int64 {
	h.record("ReplyToSignal", correlationID, response)
	return h.ret
}
func (h *mockHostHandler) SignalWorkflow(ctx context.Context, m api.Module, targetRunID, signalName, payload string) int64 {
	h.record("SignalWorkflow", targetRunID, signalName, payload)
	return h.ret
}
func (h *mockHostHandler) SetScope(ctx context.Context, m api.Module, objectType, instanceKey string, prevScopePtr, prevScopeMaxLen uint32) int64 {
	h.record("SetScope", objectType, instanceKey)
	return h.ret
}
func (h *mockHostHandler) GetScope(ctx context.Context, m api.Module, objTypePtr, objTypeMaxLen, instKeyPtr, instKeyMaxLen uint32) int64 {
	h.record("GetScope")
	return h.ret
}
func (h *mockHostHandler) UUID(ctx context.Context, m api.Module, seed string, uuidPtr, uuidMaxLen uint32) int64 {
	h.record("UUID", seed)
	return h.ret
}
func (h *mockHostHandler) AcquireLock(ctx context.Context, m api.Module, key string, ttlMs int64) int64 {
	h.record("AcquireLock", key)
	return h.ret
}
func (h *mockHostHandler) ReleaseLock(ctx context.Context, m api.Module, key string) int64 {
	h.record("ReleaseLock", key)
	return h.ret
}
func (h *mockHostHandler) SideEffect(ctx context.Context, m api.Module, computedResult string, respPtr, respMaxLen uint32) int64 {
	h.record("SideEffect", computedResult)
	return h.ret
}
func (h *mockHostHandler) WorkflowID(ctx context.Context, m api.Module, idPtr, idMaxLen uint32) int64 {
	h.record("WorkflowID")
	return h.ret
}
func (h *mockHostHandler) RunID(ctx context.Context, m api.Module, idPtr, idMaxLen uint32) int64 {
	h.record("RunID")
	return h.ret
}
func (h *mockHostHandler) ResolvePromise(ctx context.Context, m api.Module, promiseID, value string) int64 {
	h.record("ResolvePromise", promiseID, value)
	return h.ret
}
func (h *mockHostHandler) RejectPromise(ctx context.Context, m api.Module, promiseID, errMsg string) int64 {
	h.record("RejectPromise", promiseID, errMsg)
	return h.ret
}
func (h *mockHostHandler) DurableSend(ctx context.Context, m api.Module, service, operation, requestJSON string) int64 {
	h.record("DurableSend", service, operation, requestJSON)
	return h.ret
}
func (h *mockHostHandler) DurableScheduleInvoke(ctx context.Context, m api.Module, service, operation, requestJSON string, delayMs int64) int64 {
	h.record("DurableScheduleInvoke", service, operation, requestJSON)
	return h.ret
}
func (h *mockHostHandler) RegisterQueryHandler(ctx context.Context, m api.Module, name string) int64 {
	h.record("RegisterQueryHandler", name)
	return h.ret
}
func (h *mockHostHandler) SetState(ctx context.Context, m api.Module, key, value string) int64 {
	h.record("SetState", key, value)
	return h.ret
}
func (h *mockHostHandler) GetState(ctx context.Context, m api.Module, key string, valuePtr, valueMaxLen uint32) int64 {
	h.record("GetState", key)
	return h.ret
}
func (h *mockHostHandler) DeleteState(ctx context.Context, m api.Module, key string) int64 {
	h.record("DeleteState", key)
	return h.ret
}
func (h *mockHostHandler) IncrState(ctx context.Context, m api.Module, key string, delta int64) int64 {
	h.record("IncrState", key)
	return h.ret
}
func (h *mockHostHandler) HasState(ctx context.Context, m api.Module, key string) int64 {
	h.record("HasState", key)
	return h.ret
}
func (h *mockHostHandler) ListState(ctx context.Context, m api.Module, prefix string, keysPtr, keysMaxLen uint32) int64 {
	h.record("ListState", prefix)
	return h.ret
}
func (h *mockHostHandler) RunDetached(ctx context.Context, m api.Module, name, inputJSON string) int64 {
	h.record("RunDetached", name, inputJSON)
	return h.ret
}
func (h *mockHostHandler) Fetch(ctx context.Context, m api.Module, method, url, headersJSON, body string, responsePtr, responseMaxLen uint32) int64 {
	h.record("Fetch", method, url, headersJSON, body)
	return h.ret
}
func (h *mockHostHandler) ScheduleCron(ctx context.Context, m api.Module, workflowName, cronExpr, timezone, inputJSON string, idPtr, idMaxLen uint32) int64 {
	h.record("ScheduleCron", workflowName, cronExpr, timezone, inputJSON)
	return h.ret
}
func (h *mockHostHandler) DeleteCron(ctx context.Context, m api.Module, scheduleID string) int64 {
	h.record("DeleteCron", scheduleID)
	return h.ret
}
func (h *mockHostHandler) ListCrons(ctx context.Context, m api.Module, outPtr, outMaxLen uint32) int64 {
	h.record("ListCrons")
	return h.ret
}
func (h *mockHostHandler) JsonParse(ctx context.Context, m api.Module, input string, outPtr, outMaxLen uint32) int64 {
	h.record("JsonParse", input)
	return h.ret
}
func (h *mockHostHandler) JsonStringify(ctx context.Context, m api.Module, input string, outPtr, outMaxLen uint32) int64 {
	h.record("JsonStringify", input)
	return h.ret
}
