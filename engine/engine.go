package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"sync/atomic"

	"github.com/tetratelabs/wazero"

	"github.com/cleat-team/cleat/monitoring/prometheus"
	"github.com/cleat-team/cleat/wasm"
)

// DebugTiming enables verbose per-step/per-execution timing output to stderr
// and structured logs. Set the CLEAT_DEBUG_TIMING=1 environment variable to
// enable it. Default off — timing I/O adds measurable overhead at high concurrency.
var DebugTiming = os.Getenv("CLEAT_DEBUG_TIMING") == "1"

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
	maxQuotaSchedules       int

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

	noPerStepFlush bool // skip per-step flushEvent; rely on FinalizeWorkflowSegment for persistence

	// ambiguityResolver is consulted when replay finds a call whose outcome
	// was never recorded (IMPROVEMENT-PLAN 1.4 phase E). Nil means ambiguity
	// is reported to the workflow, which is the behaviour without it.
	ambiguityResolver AmbiguityResolver

	// intentOps holds the "service.operation" keys declared as
	// WriteAheadIntent (IMPROVEMENT-PLAN 1.4 phase D). Empty means every call
	// is AtLeastOnce, which is what shipped before and costs nothing.
	intentOps map[string]bool

	batchFlusher    *BatchFlusher          // batch flusher for higher throughput event persistence
	flusherRegistry *TenantFlusherRegistry // per-tenant adaptive batch flushers based on step rate

	cancellationCheckInterval time.Duration // throttle PollCancellation; 0 = every step

	Metrics *prometheus.Metrics

	wasmCumulativeAllocMax int64         // max cumulative WASM allocation in bytes (0 = unlimited)
	wasmCumulativeAlloc    *atomic.Int64 // shared cumulative allocation counter in bytes
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

// WithMaxQuotaSchedules sets the max cron schedules a tenant may hold.
//
// Unlike the other three quotas this one is per tenant, not per workflow: a
// schedule outlives the run that created it, so counting them against that
// run would let a workflow create its limit, exit, and be started again.
// Zero means unlimited, matching the others.
func WithMaxQuotaSchedules(n int) EngineOption {
	return func(e *Engine) { e.maxQuotaSchedules = n }
}

// WithMaxQuotaConcurrencyKeys sets the max concurrency keys per workflow.
func WithMaxQuotaConcurrencyKeys(n int) EngineOption {
	return func(e *Engine) { e.maxQuotaConcurrencyKeys = n }
}

// WithWasmCumulativeAllocationMax sets the max cumulative WASM linear memory
// allocation in bytes (0 = unlimited) and the shared atomic counter used to
// track current cumulative allocation across all concurrent engines.
func WithWasmCumulativeAllocationMax(maxBytes int64, counter *atomic.Int64) EngineOption {
	return func(e *Engine) {
		e.wasmCumulativeAllocMax = maxBytes
		e.wasmCumulativeAlloc = counter
	}
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

// WasmtimeLanguages are the guest languages served by the wasmtime backend.
// Everything else falls back to the wazero Runtime.
//
// This is the single source of truth, and it is single deliberately. There used
// to be two: cmd/cleat-worker registered "go" alone, and cleat/wasmtest
// registered go, assemblyscript, python and java. They disagreed in both
// directions -- the harness ran Python on a backend the worker never sends it
// to, and neither routed Rust -- so a test passing in the harness said nothing
// about the configuration the product actually runs.
//
// Membership means "verified to load and execute on wasmtime", not "ought to
// work". Each entry here was run before it was added:
//
//   - go: the primary path and the backend of record (CLAUDE.md).
//
//   - assemblyscript, java: reached wasmtime for a long time by accident --
//     DetectLanguage could not identify them and defaulted to "go" -- and were
//     confirmed to load and execute before being named explicitly. See 2.72.
//
//   - rust: exercised by tests/cross-language, which builds the same
//     wasm32-unknown-unknown cdylib that `cleat build --target rust` ships. All
//     seven tests pass on wasmtime, including both cross-replay directions
//     (execute under one runtime, replay the recorded history under the other),
//     plus TestPluginCalls_Wasm_Rust in the plugin harness.
//
//     Rust was previously excluded on the grounds that "wasmtime-go v44 still
//     crashes on fn.Call for Rust cdylib core modules". That does not
//     reproduce. The reason was true when written, as far as anyone can tell,
//     and outlived its cause -- the same shape as the stale CGO_ENABLED=0 note
//     in CLAUDE.md. Until 2026-08-04 it could not have been rechecked cheaply:
//     tests/cross-language built wasm32-wasip1 rather than the shipped target,
//     so the suite covered an artifact no user runs.
//
//   - python: added 2026-08-05, and it is the entry whose history is worth
//     knowing. Python is a Component Model guest, not a core module, so it
//     takes backend_wasmtime.go's component branch rather than the ordinary
//     instantiation path. There are two implementations of that branch: the
//     native one in component_cgo.go, which hands the component to wasmtime's
//     own Component Model runtime, and a hand-rolled decomposition path that
//     re-implements shared-everything dynamic linking in Go.
//
//     Only the second one ever ran. The native path sat behind the
//     wasmtime_component_cgo build tag, which no build, CI job, Makefile or
//     Dockerfile set, so every build got a stub that returned "not built" and
//     fell through to decomposition -- where componentize-py output stops at
//     `undefined element: out of bounds table access` instantiating instance
//     52 (module 8). Three sessions read that error as the state of
//     Python-on-wasmtime. It was the state of the fallback.
//
//     With the headers vendored (engine/wasmtimeinc) so the tag could be
//     dropped, the same component runs: executes, records its durable call,
//     returns. No change to the component path's logic was needed -- it was
//     correct and uncompiled. Verified with a real HostHandler on a component
//     componentize-py built fresh, not on the stale checked-in fixture, and
//     the acceptance test named in IMPROVEMENT-PLAN 2.72,
//     TestPythonWasmEndToEnd, is unskipped.
//
// Absent, and why:
//
//   - nothing. Every language cleat builds for is served by wasmtime. See
//     IMPROVEMENT-PLAN 3.30 for what that leaves the wazero runtime doing:
//     it is no longer the fallback for any language, and an unfenced backend
//     that nothing routes to is a liability rather than a safety net.
//
// See IMPROVEMENT-PLAN.md 2.72 and 1.5/2.28.
var WasmtimeLanguages = []string{"go", "assemblyscript", "java", "rust", "python"}

// RunsOnWasmtime reports whether a detected guest language is served by the
// wasmtime backend.
//
// Callers that build an Engine should register backends from WasmtimeLanguages
// rather than consulting this; it exists for the worker, which also has to
// decide whether to construct a wazero Runtime at all. Those two decisions have
// to agree, and reading them from the same list is what makes them agree.
func RunsOnWasmtime(lang string) bool {
	for _, l := range WasmtimeLanguages {
		if l == lang {
			return true
		}
	}
	return false
}

// WithBackends registers one backend for several languages at once.
//
// backendForWasm looks the detected language up in this map and returns nil
// when it is absent, which sends the workflow to the fallback runtime. So the
// set of languages registered here is the whole of the routing decision, and
// registering them one call at a time made it easy to leave one out silently.
func WithBackends(languages []string, backend WasmBackend) EngineOption {
	return func(e *Engine) {
		if e.backends == nil {
			e.backends = make(map[string]WasmBackend)
		}
		for _, lang := range languages {
			e.backends[lang] = backend
		}
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

// WithNoPerStepFlush disables per-step flushEvent calls. Events are still
// accumulated in the session history and persisted atomically by
// FinalizeWorkflowSegment. This improves throughput at the cost of losing
// in-flight events on crash.
func WithNoPerStepFlush(v bool) EngineOption { return func(e *Engine) { e.noPerStepFlush = v } }

// WithFlusherRegistry sets the tenant-keyed adaptive batch flusher registry.
// When set, recordEvent uses the tenant-specific flusher to decide between
// direct per-step flushing and batched event persistence.
func WithFlusherRegistry(r *TenantFlusherRegistry) EngineOption {
	return func(e *Engine) { e.flusherRegistry = r }
}

// WithBatchFlusher sets the batch flusher for higher throughput event
// persistence. When set, recordEvent submits events to the batch flusher
// instead of executing individual INSERT statements.
func WithBatchFlusher(bf *BatchFlusher) EngineOption { return func(e *Engine) { e.batchFlusher = bf } }

// WithCancellationCheckInterval sets the minimum wall-clock interval between
// PollCancellation DB queries. Zero (the default) checks on every durable step.
func WithCancellationCheckInterval(d time.Duration) EngineOption {
	return func(e *Engine) { e.cancellationCheckInterval = d }
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

// DB returns the tenant-scoped database connection.
func (e *Engine) DB() *sql.DB { return e.db }

// TenantID returns the tenant identifier.
func (e *Engine) TenantID() string { return e.tenantID }

// getAdaptiveFlusher returns the tenant-specific AdaptiveFlusher from the
// registry, or nil if no registry is configured.
func (e *Engine) getAdaptiveFlusher() *AdaptiveFlusher {
	if e.flusherRegistry == nil {
		return nil
	}
	return e.flusherRegistry.For(e.tenantID)
}

// EncryptSensitivePayloads returns whether sensitive payload encryption is enabled.
func (e *Engine) EncryptSensitivePayloads() bool { return e.encryptSensitivePayloads }

// Encryption returns the payload encryption instance.
func (e *Engine) Encryption() *PayloadEncryption { return e.encryption }

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
	if e.rt == nil {
		return "", nil, nil, nil, nil, fmt.Errorf("host: no runtime available for WASM compilation; register a backend for this language with WithBackend")
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
