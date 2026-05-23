// Package wasmtest provides a WASM-boundary integration test harness for
// cleat workflows. It compiles workflow Go source to WASM, runs it through
// the actual host engine, and uses in-memory stores so no PostgreSQL or
// other database is required.
//
// Usage:
//
//	func TestMyWorkflow(t *testing.T) {
//	    env := wasmtest.NewWasmTestEnv(t)
//	    defer env.Close()
//
//	    // Compile workflow source to WASM.
//	    wasmBytes := env.BuildWasm(t, "testdata/myworkflow")
//	    // Or compile using the cleat build pipeline:
//	    //   wasmBytes := env.BuildCleat(t, "testdata/myworkflow", "entry_point")
//
//	    // Execute the workflow.
//	    result, history, err := env.Execute(t, wasmBytes, "entry_point", `{"key":"val"}`)
//	    if err != nil { t.Fatal(err) }
//
//	    // Replay from the recorded history.
//	    result2, _, err := env.Replay(t, wasmBytes, "entry_point", `{"key":"val"}`, history)
//	    if err != nil { t.Fatal(err) }
//	    if result != result2 { t.Error("replay mismatch") }
//	}
//
// Build tag note: This package compiles on all platforms but requires the
// `go` binary in PATH for WASM compilation. Tests using BuildWasm or
// BuildCleat should be gated with `testing.Short()` so they are excluded
// from `go test ./... -short`.
package wasmtest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/cleat-team/cleat/internal/host"
)

// ---------------------------------------------------------------------------
// In-memory store implementations
// ---------------------------------------------------------------------------

// InMemorySignalStore implements host.SignalStore with in-memory maps.
type InMemorySignalStore struct {
	mu            sync.Mutex
	signals       map[string][]pendingSignal   // workflowID -> pending signals
	cancelled     map[string]cancellationState // workflowID -> cancellation state
}

type pendingSignal struct {
	name    string
	payload string
}

type cancellationState struct {
	cancelled bool
	reason    string
}

func NewInMemorySignalStore() *InMemorySignalStore {
	return &InMemorySignalStore{
		signals:   make(map[string][]pendingSignal),
		cancelled: make(map[string]cancellationState),
	}
}

func (s *InMemorySignalStore) DeliverSignal(_ context.Context, workflowID, signalName, payload string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signals[workflowID] = append(s.signals[workflowID], pendingSignal{name: signalName, payload: payload})
	return nil
}

func (s *InMemorySignalStore) PollSignal(_ context.Context, workflowID, signalName string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.signals[workflowID]
	for i, sig := range pending {
		if sig.name == signalName {
			s.signals[workflowID] = append(pending[:i], pending[i+1:]...)
			return sig.payload, true, nil
		}
	}
	return "", false, nil
}

func (s *InMemorySignalStore) PollAndClaimSignal(ctx context.Context, workflowID, signalName string) (string, bool, error) {
	return s.PollSignal(ctx, workflowID, signalName)
}

func (s *InMemorySignalStore) PollCancellation(_ context.Context, workflowID string) (bool, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs, ok := s.cancelled[workflowID]
	if !ok {
		return false, "", nil
	}
	return cs.cancelled, cs.reason, nil
}

// SetCancelled marks a workflow as cancelled (for test setup).
func (s *InMemorySignalStore) SetCancelled(workflowID, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelled[workflowID] = cancellationState{cancelled: true, reason: reason}
}

// ClearCancelled removes the cancellation flag for a workflow.
func (s *InMemorySignalStore) ClearCancelled(workflowID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cancelled, workflowID)
}

// InMemoryPromiseStore implements host.PromiseStore with an in-memory map.
type InMemoryPromiseStore struct {
	mu       sync.Mutex
	promises map[string]*promiseState // promiseID -> state
}

type promiseState struct {
	status   string // "pending", "resolved", "rejected"
	result   string
	errMsg   string
}

func NewInMemoryPromiseStore() *InMemoryPromiseStore {
	return &InMemoryPromiseStore{promises: make(map[string]*promiseState)}
}

func (s *InMemoryPromiseStore) CreatePromise(_ context.Context, _, _, promiseID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.promises[promiseID] = &promiseState{status: "pending"}
	return nil
}

func (s *InMemoryPromiseStore) ResolvePromise(_ context.Context, _, promiseID, result string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, ok := s.promises[promiseID]
	if !ok {
		return fmt.Errorf("promise %s not found", promiseID)
	}
	ps.status = "resolved"
	ps.result = result
	return nil
}

func (s *InMemoryPromiseStore) RejectPromise(_ context.Context, _, promiseID, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, ok := s.promises[promiseID]
	if !ok {
		return fmt.Errorf("promise %s not found", promiseID)
	}
	ps.status = "rejected"
	ps.errMsg = errMsg
	return nil
}

func (s *InMemoryPromiseStore) GetPromise(_ context.Context, _, promiseID string) (string, string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, ok := s.promises[promiseID]
	if !ok {
		return "", "", "", fmt.Errorf("promise %s not found", promiseID)
	}
	return ps.status, ps.result, ps.errMsg, nil
}

// InMemoryChildWorkflowStore implements host.ChildWorkflowStore with an
// in-memory map. Child workflows are simulated: StartChildWorkflow records
// the invocation with a generated run ID and stores the input; GetChildResult
// returns the pre-configured result immediately (simulating instant completion).
type InMemoryChildWorkflowStore struct {
	mu      sync.Mutex
	// childResults maps runID -> JSON result
	childResults map[string]string
	// childErrors maps runID -> error string (empty means success)
	childErrors  map[string]string
	// invocations records all child workflow invocations
	invocations  []ChildWorkflowInvocation
	// handlers maps name -> handler function (if set, takes priority)
	handlers     map[string]func(inputJSON string) (string, error)
	// autoResult is the default result returned when no handler or explicit
	// result is set for a child workflow
	autoResult   string
}

// ChildWorkflowInvocation records a single child workflow start.
type ChildWorkflowInvocation struct {
	Name      string
	InputJSON string
	RunID     string
}

func NewInMemoryChildWorkflowStore() *InMemoryChildWorkflowStore {
	return &InMemoryChildWorkflowStore{
		childResults: make(map[string]string),
		childErrors:  make(map[string]string),
		handlers:     make(map[string]func(inputJSON string) (string, error)),
		autoResult:   `{"status":"completed"}`,
	}
}

// SetAutoResult sets the default result for child workflows without
// an explicit result or handler.
func (s *InMemoryChildWorkflowStore) SetAutoResult(result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.autoResult = result
}

// SetResult pre-configures the result for a child workflow name.
func (s *InMemoryChildWorkflowStore) SetResult(name, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// We generate the run ID when StartChildWorkflow is called.
	// Pre-store the result with a marker so the first call uses it.
	s.childResults[name+":*"] = result
}

// SetError pre-configures an error for a child workflow name.
func (s *InMemoryChildWorkflowStore) SetError(name, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.childErrors[name+":*"] = errMsg
}

// SetHandler registers a handler function for a child workflow name.
func (s *InMemoryChildWorkflowStore) SetHandler(name string, fn func(inputJSON string) (string, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[name] = fn
}

// Invocations returns all recorded child workflow invocations.
func (s *InMemoryChildWorkflowStore) Invocations() []ChildWorkflowInvocation {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]ChildWorkflowInvocation, len(s.invocations))
	copy(result, s.invocations)
	return result
}

func (s *InMemoryChildWorkflowStore) StartChildWorkflow(_ context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	runID := fmt.Sprintf("child-%s-%s", defName, uuid.NewString()[:8])
	inv := ChildWorkflowInvocation{
		Name:      defName,
		InputJSON: inputJSON,
		RunID:     runID,
	}
	s.invocations = append(s.invocations, inv)

	// First check for a registered handler.
	if fn, ok := s.handlers[defName]; ok {
		result, err := fn(inputJSON)
		if err != nil {
			s.childErrors[runID] = err.Error()
		} else {
			s.childResults[runID] = result
		}
		return runID, nil
	}

	// Then check for pre-configured results.
	if result, ok := s.childResults[defName+":*"]; ok {
		s.childResults[runID] = result
		return runID, nil
	}
	if errMsg, ok := s.childErrors[defName+":*"]; ok {
		s.childErrors[runID] = errMsg
		return runID, nil
	}

	// Default: auto-complete with autoResult.
	s.childResults[runID] = s.autoResult
	return runID, nil
}

func (s *InMemoryChildWorkflowStore) StartChildWorkflowAtomic(_ context.Context, childID, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, event host.EventRecord, priority int) (string, error) {
	return s.StartChildWorkflow(context.Background(), parentID, defName, inputJSON, defVersion, parentClosePolicy, priority)
}

func (s *InMemoryChildWorkflowStore) GetChildResult(_ context.Context, runID string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if result, ok := s.childResults[runID]; ok {
		return result, true, nil
	}
	if errMsg, ok := s.childErrors[runID]; ok {
		return "", true, fmt.Errorf("%s", errMsg)
	}
	return "", false, nil
}

// InMemoryConcurrencyKeyStore implements host.ConcurrencyKeyStore with an
// in-memory map.
type InMemoryConcurrencyKeyStore struct {
	mu   sync.Mutex
	keys map[string]concurrencyKeyEntry
}

type concurrencyKeyEntry struct {
	workflowID string
	expiresAt  time.Time
}

func NewInMemoryConcurrencyKeyStore() *InMemoryConcurrencyKeyStore {
	return &InMemoryConcurrencyKeyStore{keys: make(map[string]concurrencyKeyEntry)}
}

func (s *InMemoryConcurrencyKeyStore) AcquireConcurrencyKey(_ context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Clean up expired keys.
	now := time.Now()
	for k, entry := range s.keys {
		if now.After(entry.expiresAt) {
			delete(s.keys, k)
		}
	}

	existing, exists := s.keys[key]
	if exists && existing.workflowID != workflowID {
		// Key is held by another workflow.
		return false, nil
	}

	// Acquire or re-acquire.
	s.keys[key] = concurrencyKeyEntry{
		workflowID: workflowID,
		expiresAt:  now.Add(ttl),
	}
	return true, nil
}

func (s *InMemoryConcurrencyKeyStore) ReleaseConcurrencyKey(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.keys, key)
	return nil
}

// TestWorkflowState implements host.WorkflowState for testing.
type TestWorkflowState struct {
	VersionVal    int
	MinVersionVal int
	// ChildVersions maps child workflow name -> pinned version
	ChildVersions map[string]int
	PriorityVal   int
}

func NewTestWorkflowState() *TestWorkflowState {
	return &TestWorkflowState{
		VersionVal:    1,
		MinVersionVal: 1,
		ChildVersions: make(map[string]int),
	}
}

func (s *TestWorkflowState) Version() int { return s.VersionVal }
func (s *TestWorkflowState) MinVersion() int { return s.MinVersionVal }
func (s *TestWorkflowState) Priority() int { return s.PriorityVal }
func (s *TestWorkflowState) ChildVersion(name string) (int, bool) {
	v, ok := s.ChildVersions[name]
	return v, ok
}

// mockCaller implements host.ServiceCaller and records all calls made.
type mockCaller struct {
	mu    sync.Mutex
	Calls []host.EventRecord
}

func (m *mockCaller) Call(_ context.Context, service, operation, requestJSON string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	resp := defaultResponse(service, operation)
	m.Calls = append(m.Calls, host.EventRecord{
		EventType: host.EventTypeCall,
		Service:   service,
		Op:        operation,
		Request:   requestJSON,
		Response:  resp,
	})
	return resp, nil
}

func defaultResponse(service, operation string) string {
	switch service + "." + operation {
	case "catalog.LookupItem":
		return `{"sku":"ABC-123","name":"Widget","price_cents":999,"found":true}`
	case "inventory.Reserve":
		return `{"reservation_id":"resv_abc123","status":"reserved","total_cents":3299}`
	case "inventory.Release":
		return `{"status":"released"}`
	case "payments.GetDefaultMethod":
		return `{"token":"pm_tok_555","type":"card","last_four":"4242"}`
	case "payments.Charge":
		return `{"charge_id":"chg_xyz789","status":"captured"}`
	case "payments.Refund":
		return `{"status":"refunded"}`
	case "shipping.CreateShipment":
		return `{"tracking_id":"TRACK-123456","status":"label_created"}`
	case "notifications.SendEmail":
		return `{"status":"sent"}`
	case "accounts.Withdraw":
		return `{"ref":"wd_abc123","status":"completed"}`
	case "accounts.Deposit":
		return `{"ref":"dep_def456","status":"completed"}`
	default:
		return `{}`
	}
}

// ---------------------------------------------------------------------------
// WasmTestEnv
// ---------------------------------------------------------------------------

// WasmTestEnv is the main test harness for WASM-boundary integration tests.
// It provides:
//   - In-memory stores for signals, promises, child workflows, concurrency keys
//   - A mock caller for external service calls
//   - Engine creation with full store wiring
//   - WASM compilation via `go build`
//   - Convenience Execute/Replay methods
type WasmTestEnv struct {
	t       *testing.T
	ctx     context.Context
	cancel  context.CancelFunc
	rt      *host.Runtime
	caller  *mockCaller
	engine  *host.Engine

	pluginRegistry    *host.PluginRegistry

	SignalStore        *InMemorySignalStore
	PromiseStore       *InMemoryPromiseStore
	ChildWorkflowStore *InMemoryChildWorkflowStore
	ConcurrencyStore   *InMemoryConcurrencyKeyStore
	WorkflowState      *TestWorkflowState

	// WorkflowID is the workflow instance ID used for this env.
	WorkflowID string
	// DefName is the workflow definition name used for this env.
	DefName string
	// DefVersion is the workflow definition version used for this env.
	DefVersion int
}

// WasmTestEnvOption configures a WasmTestEnv.
type WasmTestEnvOption func(*WasmTestEnv)

// WithWorkflowID sets the workflow instance ID.
func WithWorkflowID(id string) WasmTestEnvOption {
	return func(e *WasmTestEnv) { e.WorkflowID = id }
}

// WithDefName sets the workflow definition name.
func WithDefName(name string) WasmTestEnvOption {
	return func(e *WasmTestEnv) { e.DefName = name }
}

// WithDefVersion sets the workflow definition version.
func WithDefVersion(v int) WasmTestEnvOption {
	return func(e *WasmTestEnv) { e.DefVersion = v }
}

// WithPluginRegistry sets the plugin registry for plugin host function dispatch.
func WithPluginRegistry(pr *host.PluginRegistry) WasmTestEnvOption {
	return func(e *WasmTestEnv) { e.pluginRegistry = pr }
}

// NewWasmTestEnv creates a new WasmTestEnv with default in-memory stores
// and a freshly initialised Runtime. Call Close to release resources.
func NewWasmTestEnv(t *testing.T, opts ...WasmTestEnvOption) *WasmTestEnv {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

	rt, err := host.NewRuntime(ctx, 0, 0)
	if err != nil {
		cancel()
		t.Fatalf("wasmtest: NewRuntime: %v", err)
	}

	env := &WasmTestEnv{
		t:                 t,
		ctx:               ctx,
		cancel:            cancel,
		rt:                rt,
		caller:            &mockCaller{},
		SignalStore:       NewInMemorySignalStore(),
		PromiseStore:      NewInMemoryPromiseStore(),
		ChildWorkflowStore: NewInMemoryChildWorkflowStore(),
		ConcurrencyStore:  NewInMemoryConcurrencyKeyStore(),
		WorkflowState:     NewTestWorkflowState(),
		WorkflowID:        uuid.New().String(),
		DefName:           "test-workflow",
		DefVersion:        1,
	}

	for _, o := range opts {
		o(env)
	}

	engineOpts := []host.EngineOption{
		host.WithSignalStore(env.SignalStore),
		host.WithPromiseStore(env.PromiseStore),
		host.WithWorkflowState(env.WorkflowState),
		host.WithWorkflowID(env.WorkflowID),
		host.WithDefName(env.DefName),
		host.WithChildWorkflowStore(env.ChildWorkflowStore),
		host.WithConcurrencyKeyStore(env.ConcurrencyStore),
		host.WithDefVersion(env.DefVersion),
	}
	if env.pluginRegistry != nil {
		engineOpts = append(engineOpts, host.WithPluginRegistry(env.pluginRegistry))
	}
	engineOpts = append(engineOpts, wasmtimeBackendOptions()...)
	env.engine = host.NewEngine(rt, env.caller, engineOpts...)
	return env
}

// H returns the underlying Engine for direct access (e.g., to set up
// additional options not exposed by WasmTestEnv).
func (e *WasmTestEnv) H() *host.Engine {
	return e.engine
}

// Runtime returns the underlying Runtime.
func (e *WasmTestEnv) Runtime() *host.Runtime {
	return e.rt
}

// Caller returns the mock caller for inspecting recorded calls.
func (e *WasmTestEnv) Caller() *mockCaller {
	return e.caller
}

// Close releases all runtime resources.
func (e *WasmTestEnv) Close() {
	e.cancel()
	if e.rt != nil {
		_ = e.rt.Close(e.ctx)
	}
}

// BuildWasm compiles Go source files at the given package path to WASM
// using TinyGo. The path can be:
//   - An absolute directory path
//   - A relative directory path (resolved from the repo root)
//   - A package path like "./testdata/myworkflow"
//
// Returns the compiled WASM bytes. Tests using this helper should be gated
// with testing.Short() since it requires the `tinygo` binary and takes time.
func (e *WasmTestEnv) BuildWasm(t *testing.T, pkgPath string) []byte {
	t.Helper()

	// Resolve the package path.
	dir := pkgPath
	if !filepath.IsAbs(dir) && !strings.HasPrefix(dir, ".") {
		// Treat as relative to the working directory.
		dir = filepath.Join(".", dir)
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("wasmtest: resolving path %s: %v", pkgPath, err)
	}

	// Verify the directory exists.
	if info, err := os.Stat(absDir); err != nil || !info.IsDir() {
		t.Fatalf("wasmtest: directory %s does not exist: %v", absDir, err)
	}

	// Create a temp directory for the WASM output.
	tmpDir, err := os.MkdirTemp("", "wasmtest-*")
	if err != nil {
		t.Fatalf("wasmtest: creating temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	outPath := filepath.Join(tmpDir, "output.wasm")

	// Use standard Go wasip1 compilation (cleat build --target go pipeline).
	// The source must already be transformed and have generated stubs.
	cmd := exec.Command("go", "build",
		"-o", outPath,
		".",
	)
	cmd.Dir = absDir
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wasmtest: go build failed:\n%s\n%v", string(output), err)
	}

	wasmBytes, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("wasmtest: reading WASM output: %v", err)
	}

	t.Logf("wasmtest: built WASM (%d bytes) from %s", len(wasmBytes), absDir)
	return wasmBytes
}

// Execute runs a fresh workflow execution and returns the result, event
// history, and any error.
func (e *WasmTestEnv) Execute(t *testing.T, wasmBytes []byte, entryPoint string, inputJSON string) (string, []host.EventRecord, error) {
	t.Helper()

	result, history, suspended, _, _, err := e.engine.Execute(e.ctx, wasmBytes, entryPoint, json.RawMessage(inputJSON))
	if err != nil {
		return "", nil, fmt.Errorf("wasmtest: Execute: %w", err)
	}
	if suspended != nil {
		t.Logf("wasmtest: workflow suspended: %s", suspended.Reason)
		return result, history, nil
	}
	return result, history, nil
}

// Replay replays a workflow from existing event history and returns the
// result and any divergence error.
func (e *WasmTestEnv) Replay(t *testing.T, wasmBytes []byte, entryPoint string, inputJSON string, history []host.EventRecord) (string, error) {
	t.Helper()

	result, _, suspended, _, _, err := e.engine.Replay(e.ctx, wasmBytes, entryPoint, json.RawMessage(inputJSON), history)
	if err != nil {
		return "", fmt.Errorf("wasmtest: Replay: %w", err)
	}
	if suspended != nil {
		t.Logf("wasmtest: replay suspended: %s", suspended.Reason)
		return result, nil
	}
	return result, nil
}

// ExecuteAndReplay is a convenience that calls Execute then Replay with
// the recorded history, and asserts the results match.
func (e *WasmTestEnv) ExecuteAndReplay(t *testing.T, wasmBytes []byte, entryPoint string, inputJSON string) (string, error) {
	t.Helper()

	result, history, err := e.Execute(t, wasmBytes, entryPoint, inputJSON)
	if err != nil {
		return "", err
	}

	replayResult, err := e.Replay(t, wasmBytes, entryPoint, inputJSON, history)
	if err != nil {
		return "", fmt.Errorf("replay failed: %w", err)
	}

	if result != replayResult {
		t.Errorf("wasmtest: execute result %q != replay result %q", result, replayResult)
	}

	return result, nil
}

