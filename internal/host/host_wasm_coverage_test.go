package host

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rcownie/cleat/internal/plugin"
)

// ---------------------------------------------------------------------------
// RunDefer / RunDeferCompiled tests
// ---------------------------------------------------------------------------

func TestRunDefer_InvalidWasm(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	engine := NewEngine(rt, &mockCaller{})

	// Invalid WASM bytes should fail at CompileModule.
	_, err = engine.RunDefer(ctx, []byte{0x00, 0x00, 0x00, 0x00}, "cleanup", nil)
	if err == nil {
		t.Fatal("expected error for invalid WASM, got nil")
	}
	t.Logf("Got expected error: %v", err)
}

func TestRunDeferCompiled_MissingExport(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	engine := NewEngine(rt, &mockCaller{})

	// Compile a minimal valid module that exports memory but no functions.
	compiled, err := rt.CompileModule(ctx, wasmWithMemory())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	// Call a non-existent defer name — should fail with "not found".
	_, err = engine.RunDeferCompiled(ctx, compiled, "nonexistent_cleanup", nil)
	if err == nil {
		t.Fatal("expected error for missing export, got nil")
	}
	t.Logf("Got expected error: %v", err)
}

func TestRunDeferCompiled_MissingImport(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	engine := NewEngine(rt, &mockCaller{})

	// Module with missing import — compilation succeeds but instantiation fails.
	compiled, err := rt.CompileModule(ctx, wasmWithMissingImport())
	if err != nil {
		t.Fatalf("CompileModule should succeed: %v", err)
	}
	defer compiled.Close(ctx)

	// Instantiation fails because "missing_func" is not provided by the host.
	_, err = engine.RunDeferCompiled(ctx, compiled, "cleanup", nil)
	if err == nil {
		t.Fatal("expected error for module with unresolved imports, got nil")
	}
	t.Logf("Got expected error: %v", err)
}

// ---------------------------------------------------------------------------
// PluginCall fresh path tests
// ---------------------------------------------------------------------------

func TestPluginCall_Fresh_NoRegistry(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:  &Engine{caller: &mockCaller{}},
		history: make([]EventRecord, 0),
	}

	// No plugin registry configured — should return an error.
	result := session.PluginCall(ctx, mod, "myplugin", "myfunc", `{}`, 0, 4096)
	_, _, errCode := decodeCallResult(result)
	if errCode == 0 {
		t.Error("expected error code for missing plugin registry")
	}
}

func TestPluginCall_Fresh_PluginNotFound(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	reg := NewPluginRegistry()
	session := &execSession{
		engine:  &Engine{caller: &mockCaller{}, pluginRegistry: reg},
		history: make([]EventRecord, 0),
	}

	// Plugin function not registered — should return an error.
	result := session.PluginCall(ctx, mod, "myplugin", "nonexistent", `{}`, 0, 4096)
	_, _, errCode := decodeCallResult(result)
	if errCode == 0 {
		t.Error("expected error code for unregistered plugin function")
	}
}

func TestPluginCall_Fresh_Success(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	reg := NewPluginRegistry()
	reg.Register("myplugin", "myfunc", func(_ context.Context, inputJSON string) (string, error) {
		return `{"result":"ok","input":` + inputJSON + `}`, nil
	})

	session := &execSession{
		engine:  &Engine{caller: &mockCaller{}, pluginRegistry: reg},
		history: make([]EventRecord, 0),
	}

	result := session.PluginCall(ctx, mod, "myplugin", "myfunc", `{"x":1}`, 0, 4096)
	respLen, callErrCode, errCode := decodeCallResult(result)
	if errCode != 0 || callErrCode != 0 {
		t.Fatalf("expected success, got errCode=%d callErrCode=%d", errCode, callErrCode)
	}
	if respLen == 0 {
		t.Fatal("expected response written to memory")
	}

	// Verify event was recorded.
	if len(session.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(session.history))
	}
	if session.history[0].PluginName != "myplugin" {
		t.Errorf("expected PluginName=myplugin, got %q", session.history[0].PluginName)
	}
	if session.history[0].PluginOutput != `{"result":"ok","input":{"x":1}}` {
		t.Errorf("unexpected output: %q", session.history[0].PluginOutput)
	}
}

func TestPluginCall_Fresh_Error(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	reg := NewPluginRegistry()
	reg.Register("myplugin", "myfunc", func(_ context.Context, _ string) (string, error) {
		return "", _testPluginErr("something went wrong")
	})

	session := &execSession{
		engine:  &Engine{caller: &mockCaller{}, pluginRegistry: reg},
		history: make([]EventRecord, 0),
	}

	result := session.PluginCall(ctx, mod, "myplugin", "myfunc", `{}`, 0, 4096)
	_, _, errCode := decodeCallResult(result)
	if errCode == 0 {
		t.Error("expected error code for plugin function error")
	}

	// Verify error was recorded in history.
	if len(session.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(session.history))
	}
	if session.history[0].PluginError != "something went wrong" {
		t.Errorf("expected PluginError='something went wrong', got %q", session.history[0].PluginError)
	}
}

// ---------------------------------------------------------------------------
// PluginCallStreaming fresh path tests
// ---------------------------------------------------------------------------

func TestPluginCallStreaming_Fresh_NoRegistry(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:  &Engine{caller: &mockCaller{}},
		history: make([]EventRecord, 0),
	}

	// No stream registry configured.
	result := session.PluginCallStreaming(ctx, mod, "p", "f", `{}`, 0, 4096)
	_, _, errCode := decodeCallResult(result)
	if errCode == 0 {
		t.Error("expected error code for missing stream registry")
	}
}

func TestPluginCallStreaming_Fresh_NotFound(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	psr := NewPluginStreamRegistry()
	session := &execSession{
		engine:  &Engine{caller: &mockCaller{}, pluginStreamRegistry: psr},
		history: make([]EventRecord, 0),
	}

	// Stream function not registered.
	result := session.PluginCallStreaming(ctx, mod, "p", "nonexistent", `{}`, 0, 4096)
	_, _, errCode := decodeCallResult(result)
	if errCode == 0 {
		t.Error("expected error code for unregistered stream function")
	}
}

func TestPluginCallStreaming_Fresh_Success(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	psr := NewPluginStreamRegistry()
	psr.Register("p", "f", func(_ context.Context, _ string) (<-chan plugin.StreamEvent, error) {
		ch := make(chan plugin.StreamEvent, 2)
		ch <- plugin.StreamEvent{Index: 0, Content: `{"chunk":1}`, Finish: false}
		ch <- plugin.StreamEvent{Index: 1, Content: `{"chunk":2}`, Finish: true}
		close(ch)
		return ch, nil
	})

	session := &execSession{
		engine:  &Engine{caller: &mockCaller{}, pluginStreamRegistry: psr},
		history: make([]EventRecord, 0),
	}

	result := session.PluginCallStreaming(ctx, mod, "p", "f", `{}`, 0, 4096)
	_, _, errCode := decodeCallResult(result)
	if errCode != 0 {
		t.Fatalf("expected success, got errCode=%d", errCode)
	}

	// Verify both chunks were recorded in history.
	if len(session.history) != 2 {
		t.Fatalf("expected 2 history entries (2 chunks), got %d", len(session.history))
	}
	if session.history[0].PluginOutput != `{"chunk":1}` {
		t.Errorf("expected chunk 1, got %q", session.history[0].PluginOutput)
	}
	if session.history[1].PluginOutput != `{"chunk":2}` {
		t.Errorf("expected chunk 2, got %q", session.history[1].PluginOutput)
	}
	if !session.history[1].StreamFinish {
		t.Error("expected last chunk to have StreamFinish=true")
	}
}

func TestPluginCallStreaming_Fresh_StreamError(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	psr := NewPluginStreamRegistry()
	psr.Register("p", "f", func(_ context.Context, _ string) (<-chan plugin.StreamEvent, error) {
		return nil, _testPluginErr("stream initialization failed")
	})

	session := &execSession{
		engine:  &Engine{caller: &mockCaller{}, pluginStreamRegistry: psr},
		history: make([]EventRecord, 0),
	}

	result := session.PluginCallStreaming(ctx, mod, "p", "f", `{}`, 0, 4096)
	_, _, errCode := decodeCallResult(result)
	if errCode == 0 {
		t.Error("expected error code for stream initialization failure")
	}
}

// ---------------------------------------------------------------------------
// PollSignal tests
// ---------------------------------------------------------------------------

func TestPollSignal_NoStore(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine: &Engine{caller: &mockCaller{}},
	}

	// No signal store — always returns 0.
	result := session.PollSignal(ctx, mod, "test-signal", 0, 4096)
	if result != 0 {
		t.Errorf("expected 0 (not found) with no signal store, got %d", result)
	}
}

func TestPollSignal_NotFound(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	ss := &mockSignalStore{signals: make(map[string]string)}
	session := &execSession{
		engine: &Engine{caller: &mockCaller{}, signalStore: ss},
	}

	// Signal not delivered — returns 0.
	result := session.PollSignal(ctx, mod, "not-delivered", 0, 4096)
	if result != 0 {
		t.Errorf("expected 0 (not found), got %d", result)
	}
}

func TestPollSignal_Found(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	ss := &mockSignalStore{signals: map[string]string{"approval": `{"approved":true}`}}
	session := &execSession{
		engine: &Engine{caller: &mockCaller{}, signalStore: ss},
	}

	result := session.PollSignal(ctx, mod, "approval", 0, 4096)
	if result == 0 {
		t.Fatal("expected non-zero result for found signal")
	}

	// Decode: upper 32 bits = payload length, bits 8-15 = flags (0x0100 = found)
	written := uint32((result >> 32) & 0xFFFFFFFF)
	flags := uint32((result >> 8) & 0xFF)
	if written == 0 {
		t.Error("expected payload written to memory")
	}
	if flags != 0x01 {
		t.Errorf("expected found flag 0x01, got 0x%02x", flags)
	}

	mem := mod.Memory()
	data, ok := mem.Read(0, written)
	if !ok || string(data) != `{"approved":true}` {
		t.Errorf("expected signal payload in memory, got %q", string(data))
	}
}

// ---------------------------------------------------------------------------
// ChildWorkflowWithOptions tests
// ---------------------------------------------------------------------------

func TestChildWorkflowWithOptions_Replay(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history: []EventRecord{
			{Step: 0, EventType: EventTypeChildWorkflow, ChildName: "child-wf", RunID: "child-run-001"},
		},
		isReplay: true,
	}

	result := session.ChildWorkflowWithOptions(ctx, mod, "child-wf", `{"x":1}`, 2, "abandon", 0, 4096)
	written := uint32((result >> 32) & 0xFFFFFFFF)
	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Fatalf("expected success, got errCode=%d", errCode)
	}
	if written == 0 {
		t.Error("expected RunID written to memory")
	}
	if session.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", session.stepCount)
	}
}

func TestChildWorkflowWithOptions_Fresh(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  make([]EventRecord, 0),
		workflowID: "parent-wf",
	}

	result := session.ChildWorkflowWithOptions(ctx, mod, "child-wf", `{"x":1}`, 2, "abandon", 0, 4096)
	written := uint32((result >> 32) & 0xFFFFFFFF)
	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Fatalf("expected success, got errCode=%d", errCode)
	}
	if written == 0 {
		t.Error("expected RunID written to memory")
	}

	// Verify event recorded.
	if len(session.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(session.history))
	}
	if session.history[0].EventType != EventTypeChildWorkflow {
		t.Errorf("expected ChildWorkflow event, got %s", session.history[0].EventType)
	}
	if session.history[0].ChildName != "child-wf" {
		t.Errorf("expected ChildName=child-wf, got %q", session.history[0].ChildName)
	}
	if session.history[0].ParentClosePolicy != "abandon" {
		t.Errorf("expected ParentClosePolicy=abandon, got %q", session.history[0].ParentClosePolicy)
	}
}

// ---------------------------------------------------------------------------
// AwaitAllChildren tests
// ---------------------------------------------------------------------------

func TestAwaitAllChildren_Replay(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history: []EventRecord{
			{Step: 0, EventType: EventTypeAwaitAllChildren, Request: `["c1","c2"]`, Response: `[{"run_id":"c1","result":"ok"},{"run_id":"c2","result":"ok"}]`},
		},
		isReplay: true,
	}

	result := session.AwaitAllChildren(ctx, mod, `["c1","c2"]`, 0, 4096)
	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected success, got errCode=%d", errCode)
	}
	if session.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", session.stepCount)
	}
}

func TestAwaitAllChildren_Fresh_NoStore(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:  &Engine{caller: &mockCaller{}},
		history: make([]EventRecord, 0),
	}

	// No child workflow store — each child reports "no child workflow store".
	result := session.AwaitAllChildren(ctx, mod, `["c1","c2"]`, 0, 4096)
	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected success (no store = mock results), got errCode=%d", errCode)
	}

	// Verify event was recorded.
	if len(session.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(session.history))
	}
	if session.history[0].EventType != EventTypeAwaitAllChildren {
		t.Errorf("expected AwaitAllChildren event, got %s", session.history[0].EventType)
	}

	// Verify the response JSON includes the "no child workflow store" error.
	var outcomes []map[string]string
	if err := json.Unmarshal([]byte(session.history[0].Response), &outcomes); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(outcomes) != 2 {
		t.Fatalf("expected 2 outcomes, got %d", len(outcomes))
	}
	if outcomes[0]["error"] != "no child workflow store" {
		t.Errorf("expected 'no child workflow store' error, got %q", outcomes[0]["error"])
	}
}

// ---------------------------------------------------------------------------
// DurableCallWithRetry tests
// ---------------------------------------------------------------------------

func TestDurableCallWithRetry_Replay(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history: []EventRecord{
			{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op1", Response: `{"ok":true}`},
		},
		isReplay: true,
	}

	// Replay path — delegates to replayCall.
	result := session.DurableCallWithRetry(ctx, mod, "svc", "op1", `{}`, 3, 1, 200, 1000, `[]`, 0, 4096)
	_, callErrCode, errCode := decodeCallResult(result)
	if errCode != 0 || callErrCode != 0 {
		t.Fatalf("expected success, got errCode=%d callErrCode=%d", errCode, callErrCode)
	}
}

func TestDurableCallWithRetry_Fresh_Success(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	caller := &mockCaller{}
	session := &execSession{
		engine:  &Engine{caller: caller},
		history: make([]EventRecord, 0),
	}

	// Fresh call with retry, succeeds on first attempt.
	result := session.DurableCallWithRetry(ctx, mod, "payments", "Charge", `{"amt":100}`, 3, 1, 200, 1000, `[]`, 0, 4096)
	_, callErrCode, errCode := decodeCallResult(result)
	if errCode != 0 || callErrCode != 0 {
		t.Fatalf("expected success, got errCode=%d callErrCode=%d", errCode, callErrCode)
	}

	// Verify event recorded.
	if len(session.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(session.history))
	}
	if session.history[0].EventType != EventTypeCall {
		t.Errorf("expected Call event, got %s", session.history[0].EventType)
	}
}

// ---------------------------------------------------------------------------
// DurableCallWithHeartbeat tests
// ---------------------------------------------------------------------------

func TestDurableCallWithHeartbeat_Replay(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	// Replay path — delegates to replayCallWithHeartbeat.
	// History: heartbeat events followed by a call event.
	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history: []EventRecord{
			{Step: 0, EventType: EventTypeHeartbeat, Service: "svc", Op: "long-op"},
			{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "long-op", Response: `{"done":true}`},
		},
		isReplay: true,
	}

	result := session.DurableCallWithHeartbeat(ctx, mod, "svc", "long-op", `{}`, 5000, 0, 4096)
	_, callErrCode, errCode := decodeCallResult(result)
	if errCode != 0 || callErrCode != 0 {
		t.Fatalf("expected success, got errCode=%d callErrCode=%d", errCode, callErrCode)
	}
	if session.stepCount != 2 {
		t.Errorf("expected stepCount=2 (1 heartbeat + 1 call), got %d", session.stepCount)
	}
}

func TestDurableCallWithHeartbeat_Fresh(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	caller := &mockCaller{}
	session := &execSession{
		engine:  &Engine{caller: caller},
		history: make([]EventRecord, 0),
	}

	// Fresh path: mock caller returns immediately, long heartbeat interval
	// so the ticker never fires before the call completes.
	result := session.DurableCallWithHeartbeat(ctx, mod, "payments", "Charge", `{"amt":100}`, 60000, 0, 4096)
	_, callErrCode, errCode := decodeCallResult(result)
	if errCode != 0 || callErrCode != 0 {
		t.Fatalf("expected success, got errCode=%d callErrCode=%d", errCode, callErrCode)
	}

	// Verify the call event was recorded (no heartbeat events).
	if len(session.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(session.history))
	}
	if session.history[0].EventType != EventTypeCall {
		t.Errorf("expected Call event, got %s", session.history[0].EventType)
	}
	if session.history[0].Service != "payments" || session.history[0].Op != "Charge" {
		t.Errorf("unexpected service/op: %s/%s", session.history[0].Service, session.history[0].Op)
	}
}

func TestDurableCallWithHeartbeat_Fresh_Error(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	// A caller that returns an error.
	errCaller := &heartbeatErrCaller{msg: "connection refused"}
	session := &execSession{
		engine:  &Engine{caller: errCaller},
		history: make([]EventRecord, 0),
	}

	result := session.DurableCallWithHeartbeat(ctx, mod, "svc", "fail", `{}`, 60000, 0, 4096)
	_, _, errCode := decodeCallResult(result)
	if errCode == 0 {
		t.Error("expected error code for failed call")
	}

	// Verify the error was recorded.
	if len(session.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(session.history))
	}
	if session.history[0].Err != "connection refused" {
		t.Errorf("expected error 'connection refused', got %q", session.history[0].Err)
	}
}

// heartbeatErrCaller returns a fixed error for any call, used for heartbeat tests.
type heartbeatErrCaller struct {
	msg string
}

func (e *heartbeatErrCaller) Call(_ context.Context, _, _, _ string) (string, error) {
	return "", _testPluginErr(e.msg)
}

// ---------------------------------------------------------------------------
// SetScope / ClearScope / GetScope tests
// ---------------------------------------------------------------------------

func TestSetScope_Entry_Fresh(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	cks := newMockConcurrencyKeyStore()
	session := &execSession{
		engine: &Engine{
			caller:             &mockCaller{},
			concurrencyKeyStore: cks,
		},
		history:    make([]EventRecord, 0),
		heldScopes: make([]string, 0),
		workflowID: "wf-set-scope",
	}

	// Fresh SetScope via the entry point should acquire a concurrency key.
	result := session.SetScope(ctx, mod, "order", "123", 0, 4096)
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if !session.scopeSet {
		t.Error("expected scope to be set")
	}
	if session.scopePrefix != "vo:order:123:" {
		t.Errorf("expected scope prefix 'vo:order:123:', got %q", session.scopePrefix)
	}
	if len(session.heldScopes) != 1 {
		t.Errorf("expected 1 held scope, got %d", len(session.heldScopes))
	}

	// Verify event recorded.
	if len(session.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(session.history))
	}
	if session.history[0].EventType != EventTypeScopeAcquired {
		t.Errorf("expected ScopeAcquired event, got %s", session.history[0].EventType)
	}
}

func TestSetScope_Entry_ClearViaEmpty(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	cks := newMockConcurrencyKeyStore()
	session := &execSession{
		engine: &Engine{
			caller:             &mockCaller{},
			concurrencyKeyStore: cks,
		},
		history:     make([]EventRecord, 0),
		heldScopes:  make([]string, 0),
		workflowID:  "wf-clear-scope",
		scopeSet:    true,
		scopePrefix: "vo:order:123:",
		scopeObjType: "order",
		scopeInstKey: "123",
	}

	// SetScope with empty objectType and instanceKey — should clear scope.
	result := session.SetScope(ctx, mod, "", "", 0, 4096)
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if session.scopeSet {
		t.Error("expected scope to be cleared")
	}
}

func TestSetScope_Replay(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:     &Engine{caller: &mockCaller{}},
		history: []EventRecord{
			{Step: 0, EventType: EventTypeScopeAcquired, ScopeKey: "vo:order:123"},
		},
		heldScopes: make([]string, 0),
		isReplay:   true,
	}

	result := session.SetScope(ctx, mod, "order", "123", 0, 4096)
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if !session.scopeSet {
		t.Error("expected scope to be set after replay")
	}
}

func TestClearScope_ReleasesKey(t *testing.T) {
	ctx := context.Background()

	cks := newMockConcurrencyKeyStore()

	// First, acquire the key through the store so it's actually held.
	acquired, err := cks.AcquireConcurrencyKey(ctx, "vo:order:123", "wf-holder", 30*time.Minute)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if !acquired {
		t.Fatal("should acquire key initially")
	}

	session := &execSession{
		engine: &Engine{
			caller:             &mockCaller{},
			concurrencyKeyStore: cks,
		},
		scopeSet:     true,
		scopePrefix:  "vo:order:123:",
		scopeObjType: "order",
		scopeInstKey: "123",
		heldScopes:   []string{"vo:order:123"},
	}

	// Clear the scope — should release the key via the store.
	session.ClearScope(ctx)

	if session.scopeSet {
		t.Error("expected scopeSet=false after ClearScope")
	}
	if session.scopePrefix != "" {
		t.Errorf("expected empty scope prefix, got %q", session.scopePrefix)
	}

	// After clearing, another workflow should be able to acquire the key.
	acquired, err = cks.AcquireConcurrencyKey(ctx, "vo:order:123", "wf-new", 30*time.Minute)
	if err != nil {
		t.Fatalf("re-acquire after clear: %v", err)
	}
	if !acquired {
		t.Error("key should be acquirable after ClearScope released it")
	}
}

func TestClearScope_NoKey(t *testing.T) {
	ctx := context.Background()
	// ClearScope with no held scopes should not panic or error.
	session := &execSession{
		engine: &Engine{caller: &mockCaller{}},
	}

	// Should not panic.
	session.ClearScope(ctx)
	if session.scopeSet {
		t.Error("expected scopeSet=false")
	}
}

func TestGetScope_ReturnsScope(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:     &Engine{caller: &mockCaller{}},
		scopeSet:   true,
		scopeObjType: "order",
		scopeInstKey: "123",
	}

	result := session.GetScope(ctx, mod, 0, 4096, 4096, 4096)
	objTypeLen := uint32((result >> 32) & 0xFFFFFFFF)
	instKeyLen := uint32(result & 0xFFFFFFFF)
	if objTypeLen == 0 {
		t.Error("expected non-zero objTypeLen")
	}
	if instKeyLen == 0 {
		t.Error("expected non-zero instKeyLen")
	}
}

func TestGetScope_NoScope(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine: &Engine{caller: &mockCaller{}},
	}

	result := session.GetScope(ctx, mod, 0, 4096, 4096, 4096)
	objTypeLen := uint32((result >> 32) & 0xFFFFFFFF)
	instKeyLen := uint32(result & 0xFFFFFFFF)
	if objTypeLen != 0 {
		t.Errorf("expected objTypeLen=0 when no scope, got %d", objTypeLen)
	}
	if instKeyLen != 0 {
		t.Errorf("expected instKeyLen=0 when no scope, got %d", instKeyLen)
	}
}

// ---------------------------------------------------------------------------
// AcquireLock / ReleaseLock entry point tests
// ---------------------------------------------------------------------------

func TestAcquireLock_Entry_Fresh(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	cks := newMockConcurrencyKeyStore()
	session := &execSession{
		engine: &Engine{
			caller:             &mockCaller{},
			concurrencyKeyStore: cks,
		},
		history:    make([]EventRecord, 0),
		workflowID: "wf-acquire-entry",
	}

	// AcquireLock entry point (not in replay).
	result := session.AcquireLock(ctx, mod, "entry-key", 30000)
	acquired := ((result >> 8) & 0xFF) != 0
	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Fatalf("expected success, got errCode=%d", errCode)
	}
	if !acquired {
		t.Error("expected lock acquired")
	}
	if len(session.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(session.history))
	}
	if session.history[0].EventType != EventTypeAcquireLock {
		t.Errorf("expected AcquireLock event, got %s", session.history[0].EventType)
	}
}

func TestAcquireLock_Entry_Replay(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:  &Engine{caller: &mockCaller{}},
		history: []EventRecord{
			{Step: 0, EventType: EventTypeAcquireLock, LockKey: "entry-key", LockAcquired: 1},
		},
		isReplay: true,
	}

	result := session.AcquireLock(ctx, mod, "entry-key", 30000)
	acquired := ((result >> 8) & 0xFF) != 0
	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Fatalf("expected success, got errCode=%d", errCode)
	}
	if !acquired {
		t.Error("expected lock acquired from replay")
	}
}

func TestReleaseLock_Entry_Fresh(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	cks := newMockConcurrencyKeyStore()
	session := &execSession{
		engine: &Engine{
			caller:             &mockCaller{},
			concurrencyKeyStore: cks,
		},
		history:    make([]EventRecord, 0),
		workflowID: "wf-release-entry",
	}

	// First acquire the lock.
	_ = session.freshAcquireLock(ctx, mod, "release-key", 30000)

	// ReleaseLock entry point (not in replay).
	result := session.ReleaseLock(ctx, mod, "release-key")
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}

	// Verify the release event was recorded.
	if len(session.history) < 2 {
		t.Fatalf("expected at least 2 history entries, got %d", len(session.history))
	}
	if session.history[1].EventType != EventTypeReleaseLock {
		t.Errorf("expected ReleaseLock event, got %s", session.history[1].EventType)
	}
}

func TestReleaseLock_Entry_Replay(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:  &Engine{caller: &mockCaller{}},
		history: []EventRecord{
			{Step: 0, EventType: EventTypeReleaseLock, LockKey: "release-key"},
		},
		isReplay: true,
	}

	result := session.ReleaseLock(ctx, mod, "release-key")
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
}

// ---------------------------------------------------------------------------
// Helper: _testPluginErr returns a simple error value for plugin tests.
// ---------------------------------------------------------------------------

type _testPluginErr string

func (e _testPluginErr) Error() string { return string(e) }
