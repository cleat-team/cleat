package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/tetratelabs/wazero"

	"github.com/cleat-team/cleat/monitoring/prometheus"
	"github.com/cleat-team/cleat/wasm"
)

// Engine provides cleat execution semantics (Execute/Replay) on top of a
// Runtime using a checkpoint/replay model.
type Engine struct {
	rt                   *Runtime
	caller               ServiceCaller
	fetcher              Fetcher
	signalStore          SignalStore
	promiseStore         PromiseStore
	state                WorkflowState
	workflowID           string
	childWfStore         ChildWorkflowStore
	concurrencyKeyStore  ConcurrencyKeyStore
	compactionState      *CompactionState
	pluginRegistry       *PluginRegistry
	pluginStreamRegistry *PluginStreamRegistry
	updateHandler        func(name, payload string) (string, error)
	pluginCallGuard      *PluginCallGuard
	pluginCallObserver   PluginCallObserver
	tenantID             string
	db                   *sql.DB  // tenant-scoped DB for plugin host functions
	maxRetries           int      // retry ceiling; 0 = MaxRetryAttempts
	schema               string   // PostgreSQL schema name
	peerSchemas          []string // peer schemas for cross-instance operations

	defName                string
	defVersion             int
	versionValidateFn      func() error
	allowVersionMismatch   bool
	workflowEventVerifier  func(ctx context.Context, workflowID string) error
	failOnChecksumMismatch bool

	workerID               string
	wasmInstanceTimeout    time.Duration
	defaultWorkflowTimeout time.Duration

	continueAsNewHandler func(ctx context.Context, currentRunID, workerID string, generation int64, defName string, defVersion int, newInput string, newEvents []EventRecord, result string, queryState map[string]string, priority int) (newRunID string, err error)

	encryption               *PayloadEncryption
	encryptSensitivePayloads bool
	workflowStore            WorkflowStore

	maxQuotaEvents          int
	maxQuotaChildren        int
	maxQuotaConcurrencyKeys int

	backends             map[string]WasmBackend
	defaultBackend       string
	maxEventsPerWorkflow int // triggers auto-ContinueAsNew; 0 = unlimited
	initialEventCount    int
	requireSignalAuth    bool
	signalAuthCheck      func(ctx context.Context, targetWorkflowID, callerDefName string) error
	traceID              string // W3C Trace Context trace-id
	stepCallback         ReplayStepCallback
	logger               *slog.Logger

	childBindingPolicy   string // from WASM metadata; defines how child versions are resolved
	childBindingOverride string // from env/flag; overrides policy for debugging (e.g. "latest")

	Metrics *prometheus.Metrics
}

// WithSignalStore sets the signal store.
func WithSignalStore(ss SignalStore) EngineOption { return func(e *Engine) { e.signalStore = ss } }

// WithPromiseStore sets the promise store.
func WithPromiseStore(ps PromiseStore) EngineOption { return func(e *Engine) { e.promiseStore = ps } }

// WithWorkflowState sets the workflow state for version info.
func WithWorkflowState(ws WorkflowState) EngineOption { return func(e *Engine) { e.state = ws } }

// WithWorkflowID sets the workflow instance ID.
func WithWorkflowID(id string) EngineOption { return func(e *Engine) { e.workflowID = id } }

// WithTraceID sets the W3C Trace Context trace-id.
func WithTraceID(id string) EngineOption { return func(e *Engine) { e.traceID = id } }

// WithChildWorkflowStore sets the child workflow store.
func WithChildWorkflowStore(cws ChildWorkflowStore) EngineOption {
	return func(e *Engine) { e.childWfStore = cws }
}

// WithFetcher sets the HTTP fetcher.
func WithFetcher(f Fetcher) EngineOption { return func(e *Engine) { e.fetcher = f } }

func WithConcurrencyKeyStore(cks ConcurrencyKeyStore) EngineOption {
	return func(e *Engine) { e.concurrencyKeyStore = cks }
}

// WithCompactionState sets the compaction state.
func WithCompactionState(cs *CompactionState) EngineOption {
	return func(e *Engine) { e.compactionState = cs }
}

// WithDefName sets the workflow definition name.
func WithDefName(name string) EngineOption { return func(e *Engine) { e.defName = name } }

// WithDefVersion sets the workflow definition version.
func WithDefVersion(v int) EngineOption { return func(e *Engine) { e.defVersion = v } }

// WithPluginRegistry sets the plugin registry.
func WithPluginRegistry(pr *PluginRegistry) EngineOption {
	return func(e *Engine) { e.pluginRegistry = pr }
}

// WithPluginStreamRegistry sets the streaming plugin registry.
func WithPluginStreamRegistry(psr *PluginStreamRegistry) EngineOption {
	return func(e *Engine) { e.pluginStreamRegistry = psr }
}

// WithUpdateHandler sets the update handler function.
func WithUpdateHandler(fn func(name, payload string) (string, error)) EngineOption {
	return func(e *Engine) { e.updateHandler = fn }
}

// WithTenantID sets the tenant ID.
func WithTenantID(id string) EngineOption { return func(e *Engine) { e.tenantID = id } }

// WithPluginCallGuard sets the plugin call guard.
func WithPluginCallGuard(g *PluginCallGuard) EngineOption {
	return func(e *Engine) { e.pluginCallGuard = g }
}

// WithPluginCallObserver sets a post-invocation observer.
func WithPluginCallObserver(o PluginCallObserver) EngineOption {
	return func(e *Engine) { e.pluginCallObserver = o }
}

// WithSchema sets the PostgreSQL schema name.
func WithSchema(schema string) EngineOption { return func(e *Engine) { e.schema = schema } }

// WithPeerSchemas sets peer schemas for cross-instance operations.
func WithPeerSchemas(schemas []string) EngineOption {
	return func(e *Engine) { e.peerSchemas = schemas }
}

// WithDB sets a tenant-scoped DB connection.
func WithDB(db *sql.DB) EngineOption { return func(e *Engine) { e.db = db } }

// WithWorkflowStore sets the workflow store.
func WithWorkflowStore(store WorkflowStore) EngineOption {
	return func(e *Engine) { e.workflowStore = store }
}

// WithRequireSignalAuth enables signal authorization checks.
func WithRequireSignalAuth(v bool) EngineOption { return func(e *Engine) { e.requireSignalAuth = v } }

// WithSignalAuthCheck sets signal authorization function.
func WithSignalAuthCheck(fn func(ctx context.Context, targetWorkflowID, callerDefName string) error) EngineOption {
	return func(e *Engine) { e.signalAuthCheck = fn }
}

// WithEncryption sets encryption at rest for sensitive event payloads.
func WithEncryption(enc *PayloadEncryption, enabled bool) EngineOption {
	return func(e *Engine) {
		e.encryption = enc
		e.encryptSensitivePayloads = enabled
	}
}

// WithMaxQuotaEvents sets the max events before quota/auto-ContinueAsNew.
func WithMaxQuotaEvents(n int) EngineOption {
	return func(e *Engine) { e.maxQuotaEvents = n; e.maxEventsPerWorkflow = n }
}

// WithMaxQuotaChildren sets the max child workflows per workflow.
func WithMaxQuotaChildren(n int) EngineOption { return func(e *Engine) { e.maxQuotaChildren = n } }

// WithMaxQuotaConcurrencyKeys sets the max concurrency keys per workflow.
func WithMaxQuotaConcurrencyKeys(n int) EngineOption {
	return func(e *Engine) { e.maxQuotaConcurrencyKeys = n }
}

// WithMaxRetryAttempts sets a ceiling on retry attempts.
func WithMaxRetryAttempts(n int) EngineOption { return func(e *Engine) { e.maxRetries = n } }

// WithInitialEventCount sets the starting event count.
func WithInitialEventCount(n int) EngineOption { return func(e *Engine) { e.initialEventCount = n } }

// WithVersionValidation sets version compatibility validation.
func WithVersionValidation(fn func() error) EngineOption {
	return func(e *Engine) { e.versionValidateFn = fn }
}

// WithAllowVersionMismatch allows replay despite version compatibility failures.
func WithAllowVersionMismatch(allow bool) EngineOption {
	return func(e *Engine) { e.allowVersionMismatch = allow }
}

// WithWorkflowEventVerifier sets checksum verification for replay.
func WithWorkflowEventVerifier(fn func(ctx context.Context, workflowID string) error, failOnMismatch bool) EngineOption {
	return func(e *Engine) { e.workflowEventVerifier = fn; e.failOnChecksumMismatch = failOnMismatch }
}

// WithWorkerID sets the worker instance identifier.
func WithWorkerID(id string) EngineOption { return func(e *Engine) { e.workerID = id } }

// WithWASMInstanceTimeout sets the per-execution WAT timeout.
func WithWASMInstanceTimeout(d time.Duration) EngineOption {
	return func(e *Engine) { e.wasmInstanceTimeout = d }
}

// WithDefaultWorkflowTimeout sets the total workflow timeout.
func WithDefaultWorkflowTimeout(d time.Duration) EngineOption {
	return func(e *Engine) { e.defaultWorkflowTimeout = d }
}

// WithContinueAsNewHandler sets a handler for atomic ContinueAsNew transitions.
func WithContinueAsNewHandler(fn func(ctx context.Context, currentRunID, workerID string, generation int64, defName string, defVersion int, newInput string, newEvents []EventRecord, result string, queryState map[string]string, priority int) (newRunID string, err error)) EngineOption {
	return func(e *Engine) { e.continueAsNewHandler = fn }
}

// WithBackend registers a WasmBackend for a language key.
func WithBackend(language string, backend WasmBackend) EngineOption {
	return func(e *Engine) {
		if e.backends == nil {
			e.backends = make(map[string]WasmBackend)
		}
		e.backends[language] = backend
	}
}

// WithReplayStepCallback sets a callback invoked after each replayed event.
func WithReplayStepCallback(cb ReplayStepCallback) EngineOption {
	return func(e *Engine) { e.stepCallback = cb }
}

// WithLogger sets the structured logger (default: slog.Default()).
func WithLogger(l *slog.Logger) EngineOption { return func(e *Engine) { e.logger = l } }

// WithChildBindingPolicy sets the child binding policy from WASM metadata.
// The policy determines how child workflow versions are resolved at runtime:
//   - "frozen"       — use pinned ChildVersions from the metadata
//   - "stable"       — resolve to the version tagged "stable" at child creation time
//   - "latest"       — always resolve to MAX(version)
//   - "" (empty)     — will be inferred by EffectivePolicy: "frozen" if ChildVersions present, else "latest"
func WithChildBindingPolicy(policy string) EngineOption {
	return func(e *Engine) { e.childBindingPolicy = policy }
}

// WithChildBindingOverride overrides the child binding policy for debugging.
// For example, "latest" forces resolution to the latest version regardless
// of the compiled-in policy. This is a worker-level, cross-tenant setting
// intended for development environments only.
func WithChildBindingOverride(override string) EngineOption {
	return func(e *Engine) { e.childBindingOverride = override }
}

// log returns the engine's logger, falling back to slog.Default().
func (e *Engine) log() *slog.Logger {
	if e.logger != nil {
		return e.logger
	}
	return slog.Default()
}

// NewEngine creates an Engine backed by the given Runtime and ServiceCaller.
func NewEngine(rt *Runtime, caller ServiceCaller, opts ...EngineOption) *Engine {
	e := &Engine{rt: rt, caller: caller, backends: make(map[string]WasmBackend), defaultBackend: "go"}
	for _, o := range opts {
		o(e)
	}
	if e.logger == nil {
		e.logger = slog.Default()
	}
	return e
}

// Execute runs a fresh workflow execution. If the workflow suspends (sleep,
// await signals), it returns a nil result with non-nil SuspendResult.
// If the WASM binary uses the Component Model format, it decomposes it into
// constituent core modules following the component instance DAG.
func (e *Engine) Execute(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage) (result string, history []EventRecord, suspended *SuspendResult, deferrals map[string]string, queryState map[string]string, err error) {
	if backend := e.backendForWasm(wasmBytes); backend != nil {
		return e.executeWithBackend(ctx, backend, wasmBytes, entryPoint, input, nil)
	}
	if isComponentWasm(wasmBytes) {
		bundle, parseErr := wasm.ParseComponentBundle(wasmBytes)
		if parseErr != nil {
			return "", nil, nil, nil, nil, fmt.Errorf("host: parse component bundle: %w", parseErr)
		}
		return e.executeComponent(ctx, bundle, entryPoint, input)
	}
	compiled, err := e.rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return "", nil, nil, nil, nil, fmt.Errorf("host: compile module: %w", err)
	}
	defer compiled.Close(ctx)
	return e.executeCompiled(ctx, compiled, entryPoint, input, nil, wasmBytes)
}

// ExecuteCompiled is like Execute but takes a pre-compiled module.
func (e *Engine) ExecuteCompiled(ctx context.Context, compiled wazero.CompiledModule, entryPoint string, input json.RawMessage) (result string, history []EventRecord, suspended *SuspendResult, deferrals map[string]string, queryState map[string]string, err error) {
	return e.executeCompiled(ctx, compiled, entryPoint, input, nil, nil)
}
