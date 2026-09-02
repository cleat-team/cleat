package engine

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/plugin"
)

// ---------------------------------------------------------------------------
// EngineOption / With* tests
// ---------------------------------------------------------------------------

type stubWorkflowState struct {
	version    int
	minVersion int
	childVer   map[string]int
	priority   int
}

func (s *stubWorkflowState) Version() int    { return s.version }
func (s *stubWorkflowState) MinVersion() int { return s.minVersion }
func (s *stubWorkflowState) ChildVersion(name string) (int, bool) {
	if s.childVer == nil {
		return 0, false
	}
	v, ok := s.childVer[name]
	return v, ok
}
func (s *stubWorkflowState) Priority() int { return s.priority }

type stubFetcher struct{}

func (f *stubFetcher) Fetch(ctx context.Context, method, url, headersJSON, body string) (string, error) {
	return "", nil
}

// errorFetcher returns a configurable error on every Fetch call.
type errorFetcher struct {
	errMsg string
}

func (f *errorFetcher) Fetch(_ context.Context, _, _, _, _ string) (string, error) {
	return "", fmt.Errorf("%s", f.errMsg)
}

type stubWasmBackend struct{}

func (b *stubWasmBackend) Execute(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, session HostHandler) (*ExecResult, error) {
	return nil, nil
}
func (b *stubWasmBackend) Close(ctx context.Context) error { return nil }
func (b *stubWasmBackend) Name() string                    { return "stub" }
func (b *stubWasmBackend) PerExecution() WasmBackend       { return &stubWasmBackend{} }

func TestWithWorkflowState(t *testing.T) {
	ws := &stubWorkflowState{version: 3}
	opt := WithWorkflowState(ws)
	e := NewEngine(nil, nil, opt)
	if e.state != ws {
		t.Error("WithWorkflowState did not set state")
	}
}

func TestWithTraceID(t *testing.T) {
	want := "trace-abc-123"
	opt := WithTraceID(want)
	e := NewEngine(nil, nil, opt)
	if e.traceID != want {
		t.Errorf("WithTraceID: got %q, want %q", e.traceID, want)
	}
}

func TestWithFetcher(t *testing.T) {
	f := &stubFetcher{}
	opt := WithFetcher(f)
	e := NewEngine(nil, nil, opt)
	if e.fetcher != f {
		t.Error("WithFetcher did not set fetcher")
	}
}

func TestWithDefName(t *testing.T) {
	want := "my-workflow"
	opt := WithDefName(want)
	e := NewEngine(nil, nil, opt)
	if e.defName != want {
		t.Errorf("WithDefName: got %q, want %q", e.defName, want)
	}
}

func TestWithDefVersion(t *testing.T) {
	want := 42
	opt := WithDefVersion(want)
	e := NewEngine(nil, nil, opt)
	if e.defVersion != want {
		t.Errorf("WithDefVersion: got %d, want %d", e.defVersion, want)
	}
}

func TestWithUpdateHandler(t *testing.T) {
	called := false
	fn := func(name, payload string) (string, error) {
		called = true
		return "ok-" + name, nil
	}
	opt := WithUpdateHandler(fn)
	e := NewEngine(nil, nil, opt)
	if e.updateHandler == nil {
		t.Fatal("WithUpdateHandler did not set updateHandler")
	}
	res, err := e.updateHandler("test", "body")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != "ok-test" {
		t.Errorf("got %q, want %q", res, "ok-test")
	}
	if !called {
		t.Error("handler was not called")
	}
}

func TestWithPluginCallObserver(t *testing.T) {
	o := func(pluginName, functionName string, d time.Duration, err error) {
	}
	opt := WithPluginCallObserver(o)
	e := NewEngine(nil, nil, opt)
	if e.pluginCallObserver == nil {
		t.Fatal("WithPluginCallObserver did not set pluginCallObserver")
	}
	e.pluginCallObserver("p", "f", time.Second, nil)
}

func TestWithSchema(t *testing.T) {
	want := "cleat_schema"
	opt := WithSchema(want)
	e := NewEngine(nil, nil, opt)
	if e.schema != want {
		t.Errorf("WithSchema: got %q, want %q", e.schema, want)
	}
}

func TestWithWorkflowStore(t *testing.T) {
	store := &stubWorkflowStore{}
	opt := WithWorkflowStore(store)
	e := NewEngine(nil, nil, opt)
	if e.workflowStore != store {
		t.Error("WithWorkflowStore did not set workflowStore")
	}
}

func TestWithRequireSignalAuth(t *testing.T) {
	opt := WithRequireSignalAuth(true)
	e := NewEngine(nil, nil, opt)
	if !e.requireSignalAuth {
		t.Error("WithRequireSignalAuth(true) did not set requireSignalAuth")
	}
}

func TestWithRequireSignalAuth_False(t *testing.T) {
	e := NewEngine(nil, nil)
	if e.requireSignalAuth {
		t.Error("requireSignalAuth should default to false")
	}
}

func TestWithSignalAuthCheck(t *testing.T) {
	fn := func(ctx context.Context, targetWorkflowID, callerDefName string) error {
		return errors.New("denied")
	}
	opt := WithSignalAuthCheck(fn)
	e := NewEngine(nil, nil, opt)
	if e.signalAuthCheck == nil {
		t.Fatal("WithSignalAuthCheck did not set signalAuthCheck")
	}
	if err := e.signalAuthCheck(context.Background(), "w1", "caller"); err == nil {
		t.Error("expected error from signalAuthCheck")
	}
}

func TestWithEncryption(t *testing.T) {
	enc := &PayloadEncryption{}
	opt := WithEncryption(enc, true)
	e := NewEngine(nil, nil, opt)
	if e.encryption != enc {
		t.Error("WithEncryption did not set encryption")
	}
	if !e.encryptSensitivePayloads {
		t.Error("WithEncryption did not set encryptSensitivePayloads")
	}
}

func TestWithEncryption_Disabled(t *testing.T) {
	enc := &PayloadEncryption{}
	opt := WithEncryption(enc, false)
	e := NewEngine(nil, nil, opt)
	if e.encryption != enc {
		t.Error("WithEncryption did not set encryption")
	}
	if e.encryptSensitivePayloads {
		t.Error("encryptSensitivePayloads should be false when disabled")
	}
}

func TestWithMaxQuotaEvents(t *testing.T) {
	opt := WithMaxQuotaEvents(500)
	e := NewEngine(nil, nil, opt)
	if e.maxQuotaEvents != 500 {
		t.Errorf("maxQuotaEvents: got %d, want 500", e.maxQuotaEvents)
	}
	if e.maxEventsPerWorkflow != 500 {
		t.Errorf("maxEventsPerWorkflow: got %d, want 500", e.maxEventsPerWorkflow)
	}
}

func TestWithMaxQuotaEvents_Zero(t *testing.T) {
	e := NewEngine(nil, nil)
	if e.maxQuotaEvents != 0 {
		t.Errorf("maxQuotaEvents should default to 0, got %d", e.maxQuotaEvents)
	}
}

func TestWithMaxQuotaChildren(t *testing.T) {
	opt := WithMaxQuotaChildren(10)
	e := NewEngine(nil, nil, opt)
	if e.maxQuotaChildren != 10 {
		t.Errorf("maxQuotaChildren: got %d, want 10", e.maxQuotaChildren)
	}
}

func TestWithMaxQuotaConcurrencyKeys(t *testing.T) {
	opt := WithMaxQuotaConcurrencyKeys(5)
	e := NewEngine(nil, nil, opt)
	if e.maxQuotaConcurrencyKeys != 5 {
		t.Errorf("maxQuotaConcurrencyKeys: got %d, want 5", e.maxQuotaConcurrencyKeys)
	}
}

func TestWithMaxRetryAttempts(t *testing.T) {
	opt := WithMaxRetryAttempts(3)
	e := NewEngine(nil, nil, opt)
	if e.maxRetries != 3 {
		t.Errorf("maxRetries: got %d, want 3", e.maxRetries)
	}
}

func TestWithInitialEventCount(t *testing.T) {
	opt := WithInitialEventCount(42)
	e := NewEngine(nil, nil, opt)
	if e.initialEventCount != 42 {
		t.Errorf("initialEventCount: got %d, want 42", e.initialEventCount)
	}
}

func TestWithVersionValidation(t *testing.T) {
	fn := func() error { return errors.New("version mismatch") }
	opt := WithVersionValidation(fn)
	e := NewEngine(nil, nil, opt)
	if e.versionValidateFn == nil {
		t.Fatal("WithVersionValidation did not set versionValidateFn")
	}
	if err := e.versionValidateFn(); err == nil {
		t.Error("expected error from versionValidateFn")
	}
}

func TestWithVersionValidation_Nil(t *testing.T) {
	opt := WithVersionValidation(func() error { return nil })
	e := NewEngine(nil, nil, opt)
	if err := e.versionValidateFn(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWithAllowVersionMismatch(t *testing.T) {
	opt := WithAllowVersionMismatch(true)
	e := NewEngine(nil, nil, opt)
	if !e.allowVersionMismatch {
		t.Error("WithAllowVersionMismatch(true) did not set allowVersionMismatch")
	}
}

func TestWithWorkflowEventVerifier(t *testing.T) {
	fn := func(ctx context.Context, workflowID string) error { return nil }
	opt := WithWorkflowEventVerifier(fn, true)
	e := NewEngine(nil, nil, opt)
	if e.workflowEventVerifier == nil {
		t.Error("WithWorkflowEventVerifier did not set verifier")
	}
	if !e.failOnChecksumMismatch {
		t.Error("failOnChecksumMismatch should be true")
	}
}

func TestWithWorkflowEventVerifier_NoFail(t *testing.T) {
	fn := func(ctx context.Context, workflowID string) error { return nil }
	opt := WithWorkflowEventVerifier(fn, false)
	e := NewEngine(nil, nil, opt)
	if e.failOnChecksumMismatch {
		t.Error("failOnChecksumMismatch should be false")
	}
}

func TestWithWorkerID(t *testing.T) {
	want := "worker-1"
	opt := WithWorkerID(want)
	e := NewEngine(nil, nil, opt)
	if e.workerID != want {
		t.Errorf("WithWorkerID: got %q, want %q", e.workerID, want)
	}
}

func TestWithWASMInstanceTimeout(t *testing.T) {
	d := 30 * time.Second
	opt := WithWASMInstanceTimeout(d)
	e := NewEngine(nil, nil, opt)
	if e.wasmInstanceTimeout != d {
		t.Errorf("wasmInstanceTimeout: got %v, want %v", e.wasmInstanceTimeout, d)
	}
}

func TestWithWASMInstanceTimeout_Zero(t *testing.T) {
	e := NewEngine(nil, nil)
	if e.wasmInstanceTimeout != 0 {
		t.Errorf("wasmInstanceTimeout should default to 0, got %v", e.wasmInstanceTimeout)
	}
}

func TestWithDefaultWorkflowTimeout(t *testing.T) {
	d := 5 * time.Minute
	opt := WithDefaultWorkflowTimeout(d)
	e := NewEngine(nil, nil, opt)
	if e.defaultWorkflowTimeout != d {
		t.Errorf("defaultWorkflowTimeout: got %v, want %v", e.defaultWorkflowTimeout, d)
	}
}

func TestWithContinueAsNewHandler(t *testing.T) {
	fn := func(ctx context.Context, currentRunID, workerID string, generation int64, defName string, defVersion int, newInput string, newEvents []EventRecord, result string, queryState map[string]string, priority int) (string, error) {
		return "new-run-id", nil
	}
	opt := WithContinueAsNewHandler(fn)
	e := NewEngine(nil, nil, opt)
	if e.continueAsNewHandler == nil {
		t.Fatal("WithContinueAsNewHandler did not set handler")
	}
	runID, err := e.continueAsNewHandler(context.Background(), "old", "w1", 1, "wf", 1, "{}", nil, "", nil, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runID != "new-run-id" {
		t.Errorf("got %q, want %q", runID, "new-run-id")
	}
}

func TestWithBackend(t *testing.T) {
	backend := &stubWasmBackend{}
	opt := WithBackend("python", backend)
	e := NewEngine(nil, nil, opt)
	if e.backends == nil {
		t.Fatal("WithBackend did not initialize backends map")
	}
	if e.backends["python"] != backend {
		t.Error("WithBackend did not set backend for python")
	}
}

func TestWithBackend_MultipleLanguages(t *testing.T) {
	goBackend := &stubWasmBackend{}
	pyBackend := &stubWasmBackend{}
	e := NewEngine(nil, nil,
		WithBackend("go", goBackend),
		WithBackend("python", pyBackend),
	)
	if e.backends["go"] != goBackend {
		t.Error("go backend not set")
	}
	if e.backends["python"] != pyBackend {
		t.Error("python backend not set")
	}
}

func TestWithBackend_NilMapInit(t *testing.T) {
	e := &Engine{}
	opt := WithBackend("rust", &stubWasmBackend{})
	opt(e)
	if e.backends == nil {
		t.Fatal("WithBackend should initialize nil backends map")
	}
}

func TestWithReplayStepCallback(t *testing.T) {
	called := false
	cb := func(step int, event *EventRecord, queryState map[string]string) ReplayStepAction {
		called = true
		return ReplayNext
	}
	opt := WithReplayStepCallback(cb)
	e := NewEngine(nil, nil, opt)
	if e.stepCallback == nil {
		t.Fatal("WithReplayStepCallback did not set callback")
	}
	action := e.stepCallback(0, nil, nil)
	if action != ReplayNext {
		t.Errorf("expected ReplayNext, got %v", action)
	}
	if !called {
		t.Error("callback was not called")
	}
}

func TestWithReplayStepCallback_Quit(t *testing.T) {
	cb := func(step int, event *EventRecord, queryState map[string]string) ReplayStepAction {
		return ReplayQuit
	}
	opt := WithReplayStepCallback(cb)
	e := NewEngine(nil, nil, opt)
	action := e.stepCallback(0, nil, nil)
	if action != ReplayQuit {
		t.Errorf("expected ReplayQuit, got %v", action)
	}
}

func TestWithLogger(t *testing.T) {
	l := slog.New(slog.DiscardHandler)
	opt := WithLogger(l)
	e := NewEngine(nil, nil, opt)
	if e.logger != l {
		t.Error("WithLogger did not set logger")
	}
}

func TestEngine_log_WithLogger(t *testing.T) {
	l := slog.New(slog.DiscardHandler)
	e := NewEngine(nil, nil, WithLogger(l))
	if e.log() != l {
		t.Error("log() should return configured logger")
	}
}

func TestEngine_log_Default(t *testing.T) {
	e := NewEngine(nil, nil)
	if e.log() == nil {
		t.Error("log() should fall back to default logger, not nil")
	}
}

func TestNewEngine_Defaults(t *testing.T) {
	e := NewEngine(nil, nil)
	if e.backends == nil {
		t.Error("NewEngine should initialize backends map")
	}
	if e.defaultBackend != "go" {
		t.Errorf("defaultBackend: got %q, want \"go\"", e.defaultBackend)
	}
	if e.logger == nil {
		t.Error("NewEngine should set default logger")
	}
}

func TestNewEngine_OptionChaining(t *testing.T) {
	e := NewEngine(nil, nil,
		WithWorkflowID("wf-1"),
		WithTenantID("tenant-1"),
		WithDefName("my-workflow"),
		WithDefVersion(3),
		WithSchema("s"),
		WithWorkerID("w-1"),
	)
	if e.workflowID != "wf-1" {
		t.Errorf("workflowID: got %q", e.workflowID)
	}
	if e.tenantID != "tenant-1" {
		t.Errorf("tenantID: got %q", e.tenantID)
	}
	if e.defName != "my-workflow" {
		t.Errorf("defName: got %q", e.defName)
	}
	if e.defVersion != 3 {
		t.Errorf("defVersion: got %d", e.defVersion)
	}
	if e.schema != "s" {
		t.Errorf("schema: got %q", e.schema)
	}
	if e.workerID != "w-1" {
		t.Errorf("workerID: got %q", e.workerID)
	}
}

// ---------------------------------------------------------------------------
// PluginRegistry health tracking
// ---------------------------------------------------------------------------

func TestPluginRegistry_Has_NotRegistered(t *testing.T) {
	pr := NewPluginRegistry()
	if pr.Has("p", "f") {
		t.Error("Has should return false for unregistered function")
	}
}

func TestPluginRegistry_Has_Registered(t *testing.T) {
	pr := NewPluginRegistry()
	pr.Register("p", "f", func(ctx context.Context, inputJSON string) (string, error) {
		return "ok", nil
	})
	if !pr.Has("p", "f") {
		t.Error("Has should return true for registered function")
	}
}

func TestPluginRegistry_SetHealthTracker(t *testing.T) {
	tracker := plugin.NewPluginHealthTracker()
	pr := NewPluginRegistry()
	pr.SetHealthTracker(tracker)
	tracker.MarkUnhealthy("bad-plugin", errors.New("boom"))
	if pr.IsPluginHealthy("bad-plugin") {
		t.Error("plugin should be unhealthy after tracker marked it")
	}
}

func TestPluginRegistry_SetHealthTracker_Shared(t *testing.T) {
	tracker := plugin.NewPluginHealthTracker()
	pr1 := NewPluginRegistry()
	pr2 := NewPluginRegistry()
	pr1.SetHealthTracker(tracker)
	pr2.SetHealthTracker(tracker)
	pr1.MarkPluginUnhealthy("bad", errors.New("crash"))
	if pr2.IsPluginHealthy("bad") {
		t.Error("shared tracker: pr2 should see plugin as unhealthy")
	}
}

func TestPluginRegistry_IsPluginHealthy(t *testing.T) {
	pr := NewPluginRegistry()
	if !pr.IsPluginHealthy("any") {
		t.Error("new registry: all plugins should be healthy")
	}
}

func TestPluginRegistry_MarkPluginUnhealthy(t *testing.T) {
	pr := NewPluginRegistry()
	pr.MarkPluginUnhealthy("p", errors.New("panic"))
	if pr.IsPluginHealthy("p") {
		t.Error("plugin should be unhealthy after MarkPluginUnhealthy")
	}
}

func TestPluginRegistry_PluginHealthStatus(t *testing.T) {
	pr := NewPluginRegistry()
	status := pr.PluginHealthStatus()
	if len(status) != 0 {
		t.Errorf("expected empty status, got %d entries", len(status))
	}
	pr.MarkPluginUnhealthy("p", errors.New("boom"))
	status = pr.PluginHealthStatus()
	if len(status) != 1 {
		t.Fatalf("expected 1 unhealthy, got %d", len(status))
	}
	if status[0].Name != "p" {
		t.Errorf("plugin name: got %q, want %q", status[0].Name, "p")
	}
}

func TestPluginRegistry_UnhealthyError(t *testing.T) {
	pr := NewPluginRegistry()
	if err := pr.UnhealthyError("p"); err != nil {
		t.Errorf("healthy plugin should return nil error, got %v", err)
	}
	boom := errors.New("boom")
	pr.MarkPluginUnhealthy("p", boom)
	if err := pr.UnhealthyError("p"); err == nil {
		t.Error("unhealthy plugin should return error")
	}
}

// ---------------------------------------------------------------------------
// PluginStreamRegistry tests
// ---------------------------------------------------------------------------

func TestPluginStreamRegistry_Has_NotRegistered(t *testing.T) {
	psr := NewPluginStreamRegistry()
	if psr.Has("p", "f") {
		t.Error("Has should return false for unregistered function")
	}
}

func TestPluginStreamRegistry_Has_Registered(t *testing.T) {
	psr := NewPluginStreamRegistry()
	psr.Register("p", "f", plugin.PluginStreamFunc(func(ctx context.Context, inputJSON string) (<-chan plugin.StreamEvent, error) {
		return nil, nil
	}))
	if !psr.Has("p", "f") {
		t.Error("Has should return true for registered function")
	}
}

func TestPluginStreamRegistry_SetHealthTracker(t *testing.T) {
	tracker := plugin.NewPluginHealthTracker()
	psr := NewPluginStreamRegistry()
	psr.SetHealthTracker(tracker)
	tracker.MarkUnhealthy("bad", errors.New("boom"))
	if psr.IsPluginHealthy("bad") {
		t.Error("plugin should be unhealthy after tracker marked it")
	}
}

func TestPluginStreamRegistry_IsPluginHealthy(t *testing.T) {
	psr := NewPluginStreamRegistry()
	if !psr.IsPluginHealthy("any") {
		t.Error("new registry: all plugins should be healthy")
	}
}

func TestPluginStreamRegistry_MarkPluginUnhealthy(t *testing.T) {
	psr := NewPluginStreamRegistry()
	psr.MarkPluginUnhealthy("p", errors.New("panic"))
	if psr.IsPluginHealthy("p") {
		t.Error("plugin should be unhealthy after MarkPluginUnhealthy")
	}
}

func TestPluginStreamRegistry_PluginHealthStatus(t *testing.T) {
	psr := NewPluginStreamRegistry()
	psr.MarkPluginUnhealthy("p", errors.New("boom"))
	status := psr.PluginHealthStatus()
	if len(status) != 1 {
		t.Fatalf("expected 1 unhealthy, got %d", len(status))
	}
}

func TestPluginStreamRegistry_UnhealthyError(t *testing.T) {
	psr := NewPluginStreamRegistry()
	if err := psr.UnhealthyError("p"); err != nil {
		t.Errorf("healthy plugin should return nil error, got %v", err)
	}
	boom := errors.New("boom")
	psr.MarkPluginUnhealthy("p", boom)
	if err := psr.UnhealthyError("p"); err == nil {
		t.Error("unhealthy plugin should return error")
	}
}

func TestPluginStreamRegistry_RegisterStream_Interface(t *testing.T) {
	psr := NewPluginStreamRegistry()
	opts := plugin.FuncOptions{Name: "f"}
	err := psr.RegisterStream("p", opts, plugin.PluginStreamFunc(func(ctx context.Context, inputJSON string) (<-chan plugin.StreamEvent, error) {
		return nil, nil
	}))
	if err != nil {
		t.Fatalf("RegisterStream failed: %v", err)
	}
	if !psr.Has("p", "f") {
		t.Error("RegisterStream should register the function")
	}
}

func TestPluginStreamRegistry_RegisterStream_Duplicate(t *testing.T) {
	psr := NewPluginStreamRegistry()
	opts := plugin.FuncOptions{Name: "f"}
	fn := plugin.PluginStreamFunc(func(ctx context.Context, inputJSON string) (<-chan plugin.StreamEvent, error) {
		return nil, nil
	})
	psr.RegisterStream("p", opts, fn)
	err := psr.RegisterStream("p", opts, fn)
	if err == nil {
		t.Error("expected error for duplicate registration")
	}
}

// ---------------------------------------------------------------------------
// isDefinitelyNonRetryable tests
// ---------------------------------------------------------------------------

type retryableError struct{ retryable bool }

func (e *retryableError) Error() string   { return "retryable error" }
func (e *retryableError) Retryable() bool { return e.retryable }

func TestIsDefinitelyNonRetryable_RetryableFalse(t *testing.T) {
	err := &retryableError{retryable: false}
	if !isDefinitelyNonRetryable(err, nil) {
		t.Error("should be non-retryable when Retryable() returns false")
	}
}

func TestIsDefinitelyNonRetryable_RetryableTrue(t *testing.T) {
	err := &retryableError{retryable: true}
	if isDefinitelyNonRetryable(err, nil) {
		t.Error("should be retryable when Retryable() returns true and no patterns")
	}
}

func TestIsDefinitelyNonRetryable_PatternMatch(t *testing.T) {
	err := errors.New("something went wrong with connection refused")
	if !isDefinitelyNonRetryable(err, []string{"connection refused"}) {
		t.Error("should be non-retryable when error matches pattern")
	}
}

func TestIsDefinitelyNonRetryable_PatternNoMatch(t *testing.T) {
	err := errors.New("timeout")
	if isDefinitelyNonRetryable(err, []string{"connection refused"}) {
		t.Error("should be retryable when error does not match any pattern")
	}
}

func TestIsDefinitelyNonRetryable_NoInterface(t *testing.T) {
	err := errors.New("plain error")
	if isDefinitelyNonRetryable(err, nil) {
		t.Error("plain error should be retryable (no Retryable interface)")
	}
}

func TestIsDefinitelyNonRetryable_NoInterface_PatternMatch(t *testing.T) {
	err := errors.New("plain error with fatal signal")
	if !isDefinitelyNonRetryable(err, []string{"fatal"}) {
		t.Error("plain error should be non-retryable when pattern matches")
	}
}

func TestIsDefinitelyNonRetryable_BothRetryableAndPattern(t *testing.T) {
	err := &retryableError{retryable: true}
	if !isDefinitelyNonRetryable(err, []string{"retryable error"}) {
		t.Error("pattern match should make it non-retryable even though Retryable()=true")
	}
}

func TestIsDefinitelyNonRetryable_NilError(t *testing.T) {
	if isDefinitelyNonRetryable(nil, nil) {
		t.Error("nil error should not be non-retryable")
	}
}

func TestIsDefinitelyNonRetryable_RetryableFalseWithPatterns(t *testing.T) {
	err := &retryableError{retryable: false}
	if !isDefinitelyNonRetryable(err, []string{"anything"}) {
		t.Error("Retryable()=false should return true regardless of patterns")
	}
}

// ---------------------------------------------------------------------------
// parseDeferStepNo tests
// ---------------------------------------------------------------------------

func TestParseDeferStepNo_Valid(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"defer-0", 0},
		{"defer-1", 1},
		{"defer-42", 42},
		{"defer-999", 999},
		{"defer-123456789", 123456789},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := parseDeferStepNo(tc.input)
			if got != tc.want {
				t.Errorf("parseDeferStepNo(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

func TestParseDeferStepNo_Invalid(t *testing.T) {
	tests := []string{
		"",
		"not-defer",
		"defer-",
		"defer-abc",
		"defer",
		"DEFER-1",
	}
	for _, input := range tests {
		t.Run("invalid_"+input, func(t *testing.T) {
			got := parseDeferStepNo(input)
			if got != -1 {
				t.Errorf("parseDeferStepNo(%q) = %d, want -1", input, got)
			}
		})
	}
}

func TestParseDeferStepNo_Negative(t *testing.T) {
	got := parseDeferStepNo("defer--1")
	if got != -1 {
		t.Errorf("parseDeferStepNo(%q) = %d, want -1", "defer--1", got)
	}
}

// ---------------------------------------------------------------------------
// stripCompactedEvents tests
// ---------------------------------------------------------------------------

func TestStripCompactedEvents_Zero(t *testing.T) {
	history := []EventRecord{{Step: 0}, {Step: 1}, {Step: 2}}
	result := stripCompactedEvents(history, 0)
	if len(result) != 3 {
		t.Errorf("step=0: expected unchanged, got len %d", len(result))
	}
}

func TestStripCompactedEvents_Negative(t *testing.T) {
	history := []EventRecord{{Step: 0}, {Step: 1}}
	result := stripCompactedEvents(history, -1)
	if len(result) != 2 {
		t.Errorf("step=-1: expected unchanged, got len %d", len(result))
	}
}

func TestStripCompactedEvents_BeyondLen(t *testing.T) {
	history := []EventRecord{{Step: 0}}
	result := stripCompactedEvents(history, 5)
	if len(result) != 1 {
		t.Errorf("step=5 >= len=1: expected unchanged, got len %d", len(result))
	}
}

func TestStripCompactedEvents_Normal(t *testing.T) {
	history := []EventRecord{
		{Step: 0}, {Step: 1}, {Step: 2}, {Step: 3}, {Step: 4},
	}
	result := stripCompactedEvents(history, 2)
	if len(result) != 3 {
		t.Fatalf("expected 3 events after stripping 2, got %d", len(result))
	}
	if result[0].Step != 2 {
		t.Errorf("first remaining step: got %d, want 2", result[0].Step)
	}
}

func TestStripCompactedEvents_CopySemantics(t *testing.T) {
	history := []EventRecord{{Step: 0}, {Step: 1}, {Step: 2}}
	result := stripCompactedEvents(history, 1)
	result[0].Step = 999
	if history[1].Step != 1 {
		t.Error("modifying result should not affect original")
	}
}

func TestStripCompactedEvents_EmptyHistory(t *testing.T) {
	result := stripCompactedEvents(nil, 0)
	if result != nil {
		t.Error("nil history should return nil")
	}
}

func TestStripCompactedEvents_EqualLen(t *testing.T) {
	history := []EventRecord{{Step: 0}, {Step: 1}}
	result := stripCompactedEvents(history, 2)
	if len(result) != 2 {
		t.Errorf("step=2 == len=2: expected unchanged, got len %d", len(result))
	}
}

// ---------------------------------------------------------------------------
// Constants and types
// ---------------------------------------------------------------------------

func TestEventTypeConstants(t *testing.T) {
	if EventTypeCall != "call" {
		t.Errorf("EventTypeCall = %q, want %q", EventTypeCall, "call")
	}
	if EventTypeAwaitSignals != "await_signals" {
		t.Errorf("EventTypeAwaitSignals = %q", EventTypeAwaitSignals)
	}
	if EventTypeSignalReceived != "signal_received" {
		t.Errorf("EventTypeSignalReceived = %q", EventTypeSignalReceived)
	}
}

func TestReplayStepAction_Values(t *testing.T) {
	if ReplayNext != 0 {
		t.Errorf("ReplayNext = %d, want 0", ReplayNext)
	}
	if ReplayQuit != 1 {
		t.Errorf("ReplayQuit = %d, want 1", ReplayQuit)
	}
}

func TestMaxRetryAttempts_Value(t *testing.T) {
	if MaxRetryAttempts != 100 {
		t.Errorf("MaxRetryAttempts = %d, want 100", MaxRetryAttempts)
	}
}

// ---------------------------------------------------------------------------
// truncateWithHash edge cases
// ---------------------------------------------------------------------------

func TestTruncateWithHash_ShortString(t *testing.T) {
	got := truncateWithHash("hello", 100)
	if got != "hello" {
		t.Errorf("short string should not be truncated: got %q", got)
	}
}

func TestTruncateWithHash_ExactFit(t *testing.T) {
	got := truncateWithHash("hello", 5)
	if got != "hello" {
		t.Errorf("exact fit should not be truncated: got %q", got)
	}
}

func TestTruncateWithHash_LongString(t *testing.T) {
	got := truncateWithHash("hello world", 5)
	if len(got) <= 5 {
		t.Error("truncated string should have hash appended")
	}
}

// ---------------------------------------------------------------------------
// tryDecodeBase64 edge cases
// ---------------------------------------------------------------------------

func TestTryDecodeBase64_NonBase64String(t *testing.T) {
	got := tryDecodeBase64("not-valid-base64!!!")
	if got != "not-valid-base64!!!" {
		t.Errorf("non-base64 string should be returned as-is: got %q", got)
	}
}

// ---------------------------------------------------------------------------
// tryEncodeBase64 tests
// ---------------------------------------------------------------------------

func TestTryEncodeBase64_Empty(t *testing.T) {
	got := tryEncodeBase64("")
	if got != "" {
		t.Errorf("empty string: got %q, want empty", got)
	}
}

func TestTryEncodeBase64_Normal(t *testing.T) {
	input := "hello world"
	got := tryEncodeBase64(input)
	want := base64.StdEncoding.EncodeToString([]byte(input))
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTryEncodeBase64_RoundTrip(t *testing.T) {
	tests := []string{
		"hello",
		"hello world with spaces",
		`{"key": "value"}`,
		"unicode: hello",
		"special chars: !@#$%^&*()",
	}
	for _, original := range tests {
		t.Run(original[:min(20, len(original))], func(t *testing.T) {
			encoded := tryEncodeBase64(original)
			decoded := tryDecodeBase64(encoded)
			if decoded != original {
				t.Errorf("round-trip failed: %q -> %q -> %q", original, encoded, decoded)
			}
		})
	}
}

func TestTryEncodeBase64_RoundTrip_Empty(t *testing.T) {
	encoded := tryEncodeBase64("")
	decoded := tryDecodeBase64(encoded)
	if decoded != "" {
		t.Errorf("empty round-trip: got %q", decoded)
	}
}

// ---------------------------------------------------------------------------
// Engine log() tests
// ---------------------------------------------------------------------------

func TestEngine_LogNil(t *testing.T) {
	e := &Engine{}
	got := e.log()
	if got == nil {
		t.Error("log() on zero-value Engine should fall back to default logger")
	}
}

func TestEngine_LogWithLogger(t *testing.T) {
	l := slog.New(slog.DiscardHandler)
	e := &Engine{logger: l}
	if e.log() != l {
		t.Error("log() should return configured logger")
	}
}

func TestNewEngine_ServiceCallerNil(t *testing.T) {
	e := NewEngine(nil, nil)
	if e.caller != nil {
		t.Error("caller should be nil when passed nil")
	}
}

// ---------------------------------------------------------------------------
// SQL-related tests
// ---------------------------------------------------------------------------

func TestWithDB_Nil(t *testing.T) {
	e := NewEngine(nil, nil, WithDB(nil))
	if e.db != nil {
		t.Error("WithDB(nil) should set db to nil")
	}
}

func TestWithDB_NonNil(t *testing.T) {
	db := &sql.DB{}
	e := NewEngine(nil, nil, WithDB(db))
	if e.db != db {
		t.Error("WithDB should set db")
	}
}

func TestWithChildBindingPolicy(t *testing.T) {
	e := NewEngine(nil, nil, WithChildBindingPolicy("frozen"))
	if e.childBindingPolicy != "frozen" {
		t.Errorf("got %q, want %q", e.childBindingPolicy, "frozen")
	}
}

func TestWithChildBindingPolicy_Empty(t *testing.T) {
	e := NewEngine(nil, nil, WithChildBindingPolicy(""))
	if e.childBindingPolicy != "" {
		t.Errorf("got %q, want empty", e.childBindingPolicy)
	}
}

func TestWithChildBindingOverride(t *testing.T) {
	e := NewEngine(nil, nil, WithChildBindingOverride("latest"))
	if e.childBindingOverride != "latest" {
		t.Errorf("got %q, want %q", e.childBindingOverride, "latest")
	}
}

func TestWithChildBindingOverride_Empty(t *testing.T) {
	e := NewEngine(nil, nil, WithChildBindingOverride(""))
	if e.childBindingOverride != "" {
		t.Errorf("got %q, want empty", e.childBindingOverride)
	}
}

// ---------------------------------------------------------------------------
// ExecuteCompiled tests
// ---------------------------------------------------------------------------

func TestExecuteCompiled_ExportNotFound(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	e := NewEngine(rt, nil)
	_, _, _, _, _, err = e.ExecuteCompiled(ctx, compiled, "nonexistent_export", nil)
	if err == nil {
		t.Fatal("expected error for nonexistent export, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

func TestExecuteCompiled_WithInput(t *testing.T) {
	// Same as ExportNotFound but with non-nil input.
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	e := NewEngine(rt, nil)
	_, _, _, _, _, err = e.ExecuteCompiled(ctx, compiled, "handle", []byte(`{"key":"value"}`))
	if err == nil {
		t.Fatal("expected error for module with no exports")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

func TestExecuteCompiled_ClosedModule(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	compiled.Close(ctx) // close before passing to ExecuteCompiled

	e := NewEngine(rt, nil)
	_, _, _, _, _, err = e.ExecuteCompiled(ctx, compiled, "handle", nil)
	if err == nil {
		t.Fatal("expected error for closed compiled module")
	}
}

func TestExecuteCompiled_SharedCompiledModule(t *testing.T) {
	// Two engines using the same compiled module.
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	e1 := NewEngine(rt, nil)
	e2 := NewEngine(rt, nil)

	_, _, _, _, _, err1 := e1.ExecuteCompiled(ctx, compiled, "handle", nil)
	_, _, _, _, _, err2 := e2.ExecuteCompiled(ctx, compiled, "handle", nil)

	if err1 == nil {
		t.Error("e1: expected error")
	}
	if err2 == nil {
		t.Error("e2: expected error")
	}
}
