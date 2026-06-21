// Package prometheus provides OpenTelemetry-based metrics instrumentation for the
// cleat workflow engine. Metrics are recorded via OTel instruments and exposed in
// Prometheus exposition format on the /metrics HTTP endpoint.
//
// Usage:
//
//	m, err := prometheus.New(prometheus.Config{
//	    WorkerID:  "abc123",
//	})
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer m.Shutdown(context.Background())
//
//	m.RecordWorkflowStarted(ctx, "order_processing")
//	m.RecordWorkflowCompleted(ctx, "order_processing")
//
//	http.Handle("/metrics", m.ServeHTTP())
package prometheus

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// Config holds configuration for the Metrics instance.
type Config struct {
	// WorkerID uniquely identifies this worker instance.
	WorkerID string
	// ServiceName identifies the service for the OTel meter.
	ServiceName string
}

// Metrics provides OpenTelemetry-based metrics instrumentation for the cleat
// workflow engine. All methods are safe for concurrent use -- OTel instruments
// are thread-safe by design.
type Metrics struct {
	config Config

	meter  metric.Meter
	reader *sdkmetric.ManualReader
	mp     *sdkmetric.MeterProvider

	// --- Counters (Int64Counter) ---
	workflowsStarted         metric.Int64Counter
	workflowsCompleted       metric.Int64Counter
	workflowsFailed          metric.Int64Counter
	freshSteps               metric.Int64Counter
	replaySteps              metric.Int64Counter
	calls                    metric.Int64Counter
	replayFailures           metric.Int64Counter
	replayChecksumFailures   metric.Int64Counter
	ambiguousCalls           metric.Int64Counter
	compactionEventsDeleted  metric.Int64Counter
	encryptionErrors         metric.Int64Counter
	decryptionErrors         metric.Int64Counter
	continueAsNew            metric.Int64Counter
	wasmFuelExhausted        metric.Int64Counter
	workflowsDeadLettered    metric.Int64Counter
	workflowsClaimed         metric.Int64Counter
	wasmCacheHits            metric.Int64Counter
	wasmCacheMisses          metric.Int64Counter
	eventsDeleted            metric.Int64Counter
	backgroundLoops          metric.Int64Counter
	backgroundLoopRestarts   metric.Int64Counter
	reaperInstancesClaimed   metric.Int64Counter
	httpRequests             metric.Int64Counter

	// --- UpDownCounters (Int64UpDownCounter) ---
	workflowsActive              metric.Int64UpDownCounter
	workerCount                  metric.Int64UpDownCounter
	eventHistorySize             metric.Int64UpDownCounter
	wasmCacheEntries             metric.Int64UpDownCounter
	wasmCacheBytes               metric.Int64UpDownCounter
	workflowsStuck               metric.Int64UpDownCounter
	eventHistoryRowCount         metric.Int64UpDownCounter
	concurrencyKeysTotal         metric.Int64UpDownCounter
	concurrencyKeysExpiringSoon  metric.Int64UpDownCounter
	pluginConnectionsInUse       metric.Int64UpDownCounter
	pluginConnectionsMax         metric.Int64UpDownCounter
	memoryRSS                    metric.Int64UpDownCounter
	memoryAvailable              metric.Int64UpDownCounter
	memoryTotal                  metric.Int64UpDownCounter
	concurrencyLimit             metric.Int64UpDownCounter
	desiredConcurrency           metric.Int64UpDownCounter
	workflowMemoryEstimate       metric.Int64UpDownCounter

	// --- Int64Gauges ---
	queueDepth                     metric.Int64Gauge
	retentionLastRunTimestamp      metric.Int64Gauge
	backgroundLoopItemsProcessed   metric.Int64Gauge
	freshStepCountGauge            metric.Int64Gauge
	replayStepCountGauge           metric.Int64Gauge

	// --- Float64Gauges ---
	replayThroughput    metric.Float64Gauge
	freshThroughput     metric.Float64Gauge
	memoryPressureRatio metric.Float64Gauge
	memoryPressure      metric.Float64Gauge
	scalingPressure     metric.Float64Gauge
	backgroundLoopDuration metric.Float64Gauge
	backgroundLoopLastRun  metric.Float64Gauge

	// --- Histograms (Float64Histogram) ---
	claimLatency        metric.Float64Histogram
	wasmLoadLatency     metric.Float64Histogram
	dbQueryLatency      metric.Float64Histogram
	workflowDuration    metric.Float64Histogram
	pluginCallDuration  metric.Float64Histogram
	replayDuration      metric.Float64Histogram
	freshDuration       metric.Float64Histogram
	pollWaitDuration    metric.Float64Histogram
	wasmCompileDuration metric.Float64Histogram
	dispatchLatency     metric.Float64Histogram
	httpRequestDuration metric.Float64Histogram

	// Default attributes applied to every recording.
	defaultAttrs []attribute.KeyValue

	// Delta tracking for UpDownCounters used as absolute-value gauges.
	mu                        sync.Mutex
	lastWorkerCount           int64
	lastEventHistorySize      map[string]int64 // keyed by workflowName
	lastRSS                   int64
	lastAvailable             int64
	lastTotal                 int64
	lastConcurrencyLimit      int64
	lastDesiredConcurrency    int64
	lastWorkflowMemoryEstimate map[string]float64 // keyed by defName
	lastWasmCacheEntries      int64
	lastWasmCacheBytes        int64
	lastWorkflowsStuck        int64
	lastEventHistoryRowCount  int64
	lastConcurrencyKeysTotal           int64
	lastConcurrencyKeysExpiringSoon    int64
	lastPluginConnectionsInUse         int64
	lastPluginConnectionsMax           int64

	once sync.Once
}

// New creates a new Metrics instance with the given configuration. It
// initialises all OTel instruments and starts a manual reader that will be
// polled on each /metrics request.
func New(cfg Config) (*Metrics, error) {
	if cfg.ServiceName == "" {
		cfg.ServiceName = "cleat-workflow-engine"
	}

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	meter := mp.Meter(
		"github.com/cleat-team/cleat/monitoring/prometheus",
		metric.WithInstrumentationVersion("1.0.0"),
	)

	m := &Metrics{
		config: cfg,
		meter:  meter,
		reader: reader,
		mp:     mp,
		defaultAttrs: []attribute.KeyValue{
			attribute.String("worker_id", cfg.WorkerID),
		},
		lastEventHistorySize:       make(map[string]int64),
		lastWorkflowMemoryEstimate: make(map[string]float64),
	}

	var err error

	// --- Initialise counters ---

	m.workflowsStarted, err = meter.Int64Counter(
		"cleat_workflows_started_total",
		metric.WithDescription("Total number of workflows started"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_workflows_started_total: %w", err)
	}

	m.workflowsCompleted, err = meter.Int64Counter(
		"cleat_workflows_completed_total",
		metric.WithDescription("Total number of workflows completed successfully"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_workflows_completed_total: %w", err)
	}

	m.workflowsFailed, err = meter.Int64Counter(
		"cleat_workflows_failed_total",
		metric.WithDescription("Total number of workflows that failed"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_workflows_failed_total: %w", err)
	}

	m.freshSteps, err = meter.Int64Counter(
		"cleat_fresh_steps_total",
		metric.WithDescription("Fresh steps executed (first-run or past recorded history)"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_fresh_steps_total: %w", err)
	}

	m.replaySteps, err = meter.Int64Counter(
		"cleat_replay_steps_total",
		metric.WithDescription("Replay steps served from cached history"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_replay_steps_total: %w", err)
	}

	m.calls, err = meter.Int64Counter(
		"cleat_calls_total",
		metric.WithDescription("DurableCall invocations (fresh, not replay)"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_calls_total: %w", err)
	}

	m.replayFailures, err = meter.Int64Counter(
		"cleat_replay_failures_total",
		metric.WithDescription("Number of replay divergence errors"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_replay_failures_total: %w", err)
	}

	m.replayChecksumFailures, err = meter.Int64Counter(
		"cleat_replay_checksum_failures_total",
		metric.WithDescription("Number of event history checksum verification failures on replay"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_replay_checksum_failures_total: %w", err)
	}

	m.ambiguousCalls, err = meter.Int64Counter(
		"cleat_ambiguous_calls_total",
		metric.WithDescription("Total number of ambiguous call outcomes detected during replay"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_ambiguous_calls_total: %w", err)
	}

	m.compactionEventsDeleted, err = meter.Int64Counter(
		"cleat_compaction_events_deleted_total",
		metric.WithDescription("Number of events deleted by history compaction"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_compaction_events_deleted_total: %w", err)
	}

	m.encryptionErrors, err = meter.Int64Counter(
		"cleat_encryption_errors_total",
		metric.WithDescription("Total number of encryption failures during event flush"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_encryption_errors_total: %w", err)
	}

	m.decryptionErrors, err = meter.Int64Counter(
		"cleat_decryption_errors_total",
		metric.WithDescription("Total number of decryption failures on the read path"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_decryption_errors_total: %w", err)
	}

	m.continueAsNew, err = meter.Int64Counter(
		"cleat_continue_as_new_total",
		metric.WithDescription("Total ContinueAsNew triggers by reason"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_continue_as_new_total: %w", err)
	}

	m.wasmFuelExhausted, err = meter.Int64Counter(
		"cleat_wasm_fuel_exhausted_total",
		metric.WithDescription("Total number of WASM fuel exhaustion events"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_wasm_fuel_exhausted_total: %w", err)
	}

	m.workflowsDeadLettered, err = meter.Int64Counter(
		"cleat_workflows_dead_lettered_total",
		metric.WithDescription("Workflows moved to dead letter queue"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_workflows_dead_lettered_total: %w", err)
	}

	m.workflowsClaimed, err = meter.Int64Counter(
		"cleat_workflows_claimed_total",
		metric.WithDescription("Workflows claimed from the queue"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_workflows_claimed_total: %w", err)
	}

	m.wasmCacheHits, err = meter.Int64Counter(
		"cleat_wasm_cache_hits_total",
		metric.WithDescription("WASM cache hits"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_wasm_cache_hits_total: %w", err)
	}

	m.wasmCacheMisses, err = meter.Int64Counter(
		"cleat_wasm_cache_misses_total",
		metric.WithDescription("WASM cache misses"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_wasm_cache_misses_total: %w", err)
	}

	m.eventsDeleted, err = meter.Int64Counter(
		"cleat_events_deleted_total",
		metric.WithDescription("Number of expired event history rows deleted by the retention policy"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_events_deleted_total: %w", err)
	}

	m.backgroundLoops, err = meter.Int64Counter(
		"cleat_background_loops_total",
		metric.WithDescription("Total number of background loop iterations by loop and status"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_background_loops_total: %w", err)
	}

	m.backgroundLoopRestarts, err = meter.Int64Counter(
		"cleat_background_loop_restarts_total",
		metric.WithDescription("Total number of background loop restarts"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_background_loop_restarts_total: %w", err)
	}

	m.reaperInstancesClaimed, err = meter.Int64Counter(
		"cleat_reaper_instances_claimed_total",
		metric.WithDescription("Workflow instances reclaimed by the stale-instance reaper"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_reaper_instances_claimed_total: %w", err)
	}

	m.httpRequests, err = meter.Int64Counter(
		"cleat_apps_http_requests_total",
		metric.WithDescription("Total number of HTTP requests served by backendkit apps"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_apps_http_requests_total: %w", err)
	}

	// --- Initialise UpDownCounters ---

	m.workflowsActive, err = meter.Int64UpDownCounter(
		"cleat_workflows_active",
		metric.WithDescription("Number of currently active (in-flight) workflows"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_workflows_active: %w", err)
	}

	m.workerCount, err = meter.Int64UpDownCounter(
		"cleat_worker_count",
		metric.WithDescription("Number of worker instances (set to 1 per worker process)"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_worker_count: %w", err)
	}

	m.eventHistorySize, err = meter.Int64UpDownCounter(
		"cleat_event_history_size_bytes",
		metric.WithDescription("Estimated total event history size in bytes"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_event_history_size_bytes: %w", err)
	}

	m.wasmCacheEntries, err = meter.Int64UpDownCounter(
		"cleat_wasm_cache_entries",
		metric.WithDescription("Number of entries in the WASM module cache"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_wasm_cache_entries: %w", err)
	}

	m.wasmCacheBytes, err = meter.Int64UpDownCounter(
		"cleat_wasm_cache_bytes",
		metric.WithDescription("Total bytes used by the WASM module cache"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_wasm_cache_bytes: %w", err)
	}

	m.workflowsStuck, err = meter.Int64UpDownCounter(
		"cleat_workflows_stuck",
		metric.WithDescription("Number of workflow instances stalled beyond the configured stall threshold"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_workflows_stuck: %w", err)
	}

	m.eventHistoryRowCount, err = meter.Int64UpDownCounter(
		"cleat_event_history_row_count",
		metric.WithDescription("Total row count of event_history table"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_event_history_row_count: %w", err)
	}

	m.concurrencyKeysTotal, err = meter.Int64UpDownCounter(
		"cleat_concurrency_keys_total",
		metric.WithDescription("Total number of concurrency keys currently tracked"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_concurrency_keys_total: %w", err)
	}

	m.concurrencyKeysExpiringSoon, err = meter.Int64UpDownCounter(
		"cleat_concurrency_keys_expiring_soon",
		metric.WithDescription("Number of concurrency keys approaching expiry"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_concurrency_keys_expiring_soon: %w", err)
	}

	m.pluginConnectionsInUse, err = meter.Int64UpDownCounter(
		"cleat_plugin_connections_in_use",
		metric.WithDescription("Number of plugin connections currently in use"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_plugin_connections_in_use: %w", err)
	}

	m.pluginConnectionsMax, err = meter.Int64UpDownCounter(
		"cleat_plugin_connections_max",
		metric.WithDescription("Maximum number of plugin connections"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_plugin_connections_max: %w", err)
	}

	m.memoryRSS, err = meter.Int64UpDownCounter(
		"cleat_memory_rss_bytes",
		metric.WithDescription("Worker process RSS in bytes"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_memory_rss_bytes: %w", err)
	}

	m.memoryAvailable, err = meter.Int64UpDownCounter(
		"cleat_memory_available_bytes",
		metric.WithDescription("Available memory in bytes"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_memory_available_bytes: %w", err)
	}

	m.memoryTotal, err = meter.Int64UpDownCounter(
		"cleat_memory_total_bytes",
		metric.WithDescription("Total allocatable memory in bytes"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_memory_total_bytes: %w", err)
	}

	m.concurrencyLimit, err = meter.Int64UpDownCounter(
		"cleat_concurrency_limit",
		metric.WithDescription("Current effective concurrency cap"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_concurrency_limit: %w", err)
	}

	m.desiredConcurrency, err = meter.Int64UpDownCounter(
		"cleat_desired_concurrency",
		metric.WithDescription("Configured --concurrency value"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_desired_concurrency: %w", err)
	}

	m.workflowMemoryEstimate, err = meter.Int64UpDownCounter(
		"cleat_workflow_memory_estimate_bytes",
		metric.WithDescription("Estimated memory per workflow execution by def_name"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_workflow_memory_estimate_bytes: %w", err)
	}

	// --- Initialise Int64Gauges ---

	m.queueDepth, err = meter.Int64Gauge(
		"cleat_queue_depth",
		metric.WithDescription("Ready workflows waiting in task queues"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_queue_depth: %w", err)
	}

	m.retentionLastRunTimestamp, err = meter.Int64Gauge(
		"cleat_retention_last_run_timestamp",
		metric.WithDescription("Unix timestamp of the last retention policy run"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_retention_last_run_timestamp: %w", err)
	}

	m.backgroundLoopItemsProcessed, err = meter.Int64Gauge(
		"cleat_background_loop_items_processed",
		metric.WithDescription("Number of items processed in the last background loop iteration"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_background_loop_items_processed: %w", err)
	}

	m.freshStepCountGauge, err = meter.Int64Gauge(
		"cleat_fresh_step_count",
		metric.WithDescription("Cumulative count of fresh steps executed"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_fresh_step_count: %w", err)
	}

	m.replayStepCountGauge, err = meter.Int64Gauge(
		"cleat_replay_step_count",
		metric.WithDescription("Cumulative count of replay steps served from cached history"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_replay_step_count: %w", err)
	}

	// --- Initialise Float64Gauges ---

	m.replayThroughput, err = meter.Float64Gauge(
		"cleat_replay_throughput_steps_per_second",
		metric.WithDescription("Current replay step throughput rate"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_replay_throughput_steps_per_second: %w", err)
	}

	m.freshThroughput, err = meter.Float64Gauge(
		"cleat_fresh_throughput_steps_per_second",
		metric.WithDescription("Current fresh step throughput rate"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_fresh_throughput_steps_per_second: %w", err)
	}

	m.memoryPressureRatio, err = meter.Float64Gauge(
		"cleat_memory_pressure_ratio",
		metric.WithDescription("Current memory pressure ratio (0.0-1.0)"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_memory_pressure_ratio: %w", err)
	}

	m.memoryPressure, err = meter.Float64Gauge(
		"cleat_memory_pressure",
		metric.WithDescription("Memory pressure 0.0-1.0"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_memory_pressure: %w", err)
	}

	m.scalingPressure, err = meter.Float64Gauge(
		"cleat_scaling_pressure",
		metric.WithDescription("Composite scaling pressure 0.0-1.0"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_scaling_pressure: %w", err)
	}

	m.backgroundLoopDuration, err = meter.Float64Gauge(
		"cleat_background_loop_duration_seconds",
		metric.WithDescription("Duration of the last background loop iteration"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_background_loop_duration_seconds: %w", err)
	}

	m.backgroundLoopLastRun, err = meter.Float64Gauge(
		"cleat_background_loop_last_run_seconds",
		metric.WithDescription("Unix timestamp of the last background loop run"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_background_loop_last_run_seconds: %w", err)
	}

	// --- Initialise histograms ---

	m.claimLatency, err = meter.Float64Histogram(
		"cleat_claim_latency_seconds",
		metric.WithDescription("Time to claim a workflow from the queue"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(
			0.001, 0.005, 0.010, 0.025, 0.050, 0.100, 0.250, 0.500, 1.000, 2.500, 5.000,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_claim_latency_seconds: %w", err)
	}

	m.wasmLoadLatency, err = meter.Float64Histogram(
		"cleat_wasm_load_latency_seconds",
		metric.WithDescription("Time to load a WASM module from storage"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(
			0.001, 0.005, 0.010, 0.025, 0.050, 0.100, 0.250, 0.500, 1.000, 2.500, 5.000, 10.000,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_wasm_load_latency_seconds: %w", err)
	}

	m.dbQueryLatency, err = meter.Float64Histogram(
		"cleat_db_query_latency_seconds",
		metric.WithDescription("Database query latency"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(
			0.001, 0.005, 0.010, 0.025, 0.050, 0.100, 0.250, 0.500, 1.000, 2.500, 5.000, 10.000,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_db_query_latency_seconds: %w", err)
	}

	m.workflowDuration, err = meter.Float64Histogram(
		"cleat_workflow_duration_seconds",
		metric.WithDescription("Total workflow execution duration (wall clock)"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(
			0.010, 0.050, 0.100, 0.250, 0.500, 1.000, 2.500, 5.000, 10.000, 30.000, 60.000, 120.000, 300.000,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_workflow_duration_seconds: %w", err)
	}

	m.pluginCallDuration, err = meter.Float64Histogram(
		"cleat_plugin_call_duration_seconds",
		metric.WithDescription("Plugin call duration by plugin and function"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(
			0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_plugin_call_duration_seconds: %w", err)
	}

	m.replayDuration, err = meter.Float64Histogram(
		"cleat_replay_duration_seconds",
		metric.WithDescription("Duration of workflow replay execution"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(
			0.1, 0.5, 1, 5, 10, 30, 60, 300, 600, 1800, 3600,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_replay_duration_seconds: %w", err)
	}

	m.freshDuration, err = meter.Float64Histogram(
		"cleat_fresh_duration_seconds",
		metric.WithDescription("Duration of first-run workflow execution"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(
			0.1, 0.5, 1, 5, 10, 30, 60, 300, 600, 1800, 3600,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_fresh_duration_seconds: %w", err)
	}

	m.pollWaitDuration, err = meter.Float64Histogram(
		"cleat_poll_wait_seconds",
		metric.WithDescription("Time spent waiting for work in poll queries"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(
			0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_poll_wait_seconds: %w", err)
	}

	m.wasmCompileDuration, err = meter.Float64Histogram(
		"cleat_wasm_compile_duration_seconds",
		metric.WithDescription("WASM compile duration by def_name"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(
			0.01, 0.05, 0.1, 0.5, 1, 2, 5,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_wasm_compile_duration_seconds: %w", err)
	}

	m.dispatchLatency, err = meter.Float64Histogram(
		"cleat_dispatch_latency_seconds",
		metric.WithDescription("Time from workflow creation to claim by task_queue"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(
			0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_dispatch_latency_seconds: %w", err)
	}

	m.httpRequestDuration, err = meter.Float64Histogram(
		"cleat_apps_http_request_duration_seconds",
		metric.WithDescription("HTTP request duration for backendkit apps"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(
			0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_apps_http_request_duration_seconds: %w", err)
	}

	return m, nil
}

// --- Counter recording methods ---

// RecordWorkflowStarted increments the workflows-started counter.
// workflowName is recorded as a label.
func (m *Metrics) RecordWorkflowStarted(ctx context.Context, workflowName string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("workflow_name", workflowName),
	}, extraAttrs...)...)
	m.workflowsStarted.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordWorkflowCompleted increments the workflows-completed counter.
// workflowName and taskQueue are recorded as labels.
func (m *Metrics) RecordWorkflowCompleted(ctx context.Context, workflowName string, taskQueue string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("workflow_name", workflowName),
		attribute.String("task_queue", taskQueue),
	}, extraAttrs...)...)
	m.workflowsCompleted.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordWorkflowFailed increments the workflows-failed counter.
// workflowName, error, and taskQueue are recorded as labels.
func (m *Metrics) RecordWorkflowFailed(ctx context.Context, workflowName string, errMsg string, taskQueue string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("workflow_name", workflowName),
		attribute.String("error", errMsg),
		attribute.String("task_queue", taskQueue),
	}, extraAttrs...)...)
	m.workflowsFailed.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordFreshStep increments the fresh-steps-executed counter.
// workflowName is recorded as a label so the runner can scope step
// counts to the correct variant (matching cleat_workflows_completed_total).
func (m *Metrics) RecordFreshStep(ctx context.Context, workflowName string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("workflow_name", workflowName),
	}, extraAttrs...)...)
	m.freshSteps.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordReplayStep increments the replay-steps counter by def_name.
func (m *Metrics) RecordReplayStep(ctx context.Context, defName string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("def_name", defName),
	}, extraAttrs...)...)
	m.replaySteps.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordCall increments the durable-call-invocations counter.
func (m *Metrics) RecordCall(ctx context.Context, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.calls.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordReplayFailure increments the replay-failures counter.
func (m *Metrics) RecordReplayFailure(ctx context.Context, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.replayFailures.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordReplayChecksumFailure increments the replay-checksum-failures counter.
func (m *Metrics) RecordReplayChecksumFailure(ctx context.Context, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.replayChecksumFailures.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordAmbiguousCall increments the ambiguous-calls counter.
func (m *Metrics) RecordAmbiguousCall(ctx context.Context, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.ambiguousCalls.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// AddCompactionEventsDeleted adds to the compaction-events-deleted counter.
func (m *Metrics) AddCompactionEventsDeleted(ctx context.Context, delta int64, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.compactionEventsDeleted.Add(ctx, delta, metric.WithAttributes(attrs...))
}

// RecordEncryptionError increments the encryption-errors counter.
func (m *Metrics) RecordEncryptionError(ctx context.Context, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.encryptionErrors.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordDecryptionError increments the decryption-errors counter.
func (m *Metrics) RecordDecryptionError(ctx context.Context, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.decryptionErrors.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordContinueAsNew increments the continue-as-new counter by reason.
func (m *Metrics) RecordContinueAsNew(ctx context.Context, reason string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("reason", reason),
	}, extraAttrs...)...)
	m.continueAsNew.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordWasmFuelExhausted increments the wasm-fuel-exhausted counter.
func (m *Metrics) RecordWasmFuelExhausted(ctx context.Context, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.wasmFuelExhausted.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordPluginCallDuration records a plugin call duration by plugin and function.
func (m *Metrics) RecordPluginCallDuration(ctx context.Context, duration time.Duration, pluginName, functionName string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("plugin", pluginName),
		attribute.String("func", functionName),
	}, extraAttrs...)...)
	m.pluginCallDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
}

// RecordWorkflowsDeadLettered increments the workflows-dead-lettered counter.
func (m *Metrics) RecordWorkflowsDeadLettered(ctx context.Context, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.workflowsDeadLettered.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordWorkflowsClaimed increments the workflows-claimed counter by count.
func (m *Metrics) RecordWorkflowsClaimed(ctx context.Context, count int64, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.workflowsClaimed.Add(ctx, count, metric.WithAttributes(attrs...))
}

// RecordWasmCacheHit increments the wasm-cache-hits counter.
func (m *Metrics) RecordWasmCacheHit(ctx context.Context, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.wasmCacheHits.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordWasmCacheMiss increments the wasm-cache-misses counter.
func (m *Metrics) RecordWasmCacheMiss(ctx context.Context, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.wasmCacheMisses.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordEventsDeleted adds to the events-deleted counter.
func (m *Metrics) RecordEventsDeleted(ctx context.Context, count int64, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.eventsDeleted.Add(ctx, count, metric.WithAttributes(attrs...))
}

// RecordBackgroundLoop increments the background-loops counter.
func (m *Metrics) RecordBackgroundLoop(ctx context.Context, loopName, status string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("loop_name", loopName),
		attribute.String("status", status),
	}, extraAttrs...)...)
	m.backgroundLoops.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordBackgroundLoopRestart increments the background-loop-restarts counter.
func (m *Metrics) RecordBackgroundLoopRestart(ctx context.Context, loopName string, count int64, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("loop_name", loopName),
	}, extraAttrs...)...)
	m.backgroundLoopRestarts.Add(ctx, count, metric.WithAttributes(attrs...))
}

// RecordReaperInstanceClaimed increments the reaper-instances-claimed counter.
func (m *Metrics) RecordReaperInstanceClaimed(ctx context.Context, status string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("status", status),
	}, extraAttrs...)...)
	m.reaperInstancesClaimed.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordHTTPRequest increments the HTTP requests counter.
func (m *Metrics) RecordHTTPRequest(ctx context.Context, method, path, status string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("method", method),
		attribute.String("path", path),
		attribute.String("status", status),
	}, extraAttrs...)...)
	m.httpRequests.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// --- UpDownCounter methods ---

// AddWorkflowActive increments or decrements the active workflow gauge.
// Pass a positive value to increment, negative to decrement.
func (m *Metrics) AddWorkflowActive(ctx context.Context, delta int64, workflowName string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("workflow_name", workflowName),
	}, extraAttrs...)...)
	m.workflowsActive.Add(ctx, delta, metric.WithAttributes(attrs...))
}

// SetWorkerCount sets the worker count gauge to the given value.
// Uses delta tracking to convert absolute values to UpDownCounter deltas.
func (m *Metrics) SetWorkerCount(ctx context.Context, count int64, extraAttrs ...attribute.KeyValue) {
	m.mu.Lock()
	delta := count - m.lastWorkerCount
	m.lastWorkerCount = count
	m.mu.Unlock()

	attrs := m.mergeAttrs(extraAttrs...)
	m.workerCount.Add(ctx, delta, metric.WithAttributes(attrs...))
}

// SetEventHistorySize sets the event history size gauge to the given value.
// Uses delta tracking to convert absolute values to UpDownCounter deltas.
func (m *Metrics) SetEventHistorySize(ctx context.Context, sizeBytes int64, workflowName string, extraAttrs ...attribute.KeyValue) {
	m.mu.Lock()
	last, exists := m.lastEventHistorySize[workflowName]
	delta := sizeBytes - last
	if !exists {
		delta = sizeBytes
	}
	m.lastEventHistorySize[workflowName] = sizeBytes
	m.mu.Unlock()

	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("workflow_name", workflowName),
	}, extraAttrs...)...)
	m.eventHistorySize.Add(ctx, delta, metric.WithAttributes(attrs...))
}

// SetPluginConnectionsInUse sets the plugin connections in use gauge.
// Uses delta tracking to convert absolute values to UpDownCounter deltas.
func (m *Metrics) SetPluginConnectionsInUse(ctx context.Context, count int64, extraAttrs ...attribute.KeyValue) {
	m.mu.Lock()
	delta := count - m.lastPluginConnectionsInUse
	m.lastPluginConnectionsInUse = count
	m.mu.Unlock()

	attrs := m.mergeAttrs(extraAttrs...)
	m.pluginConnectionsInUse.Add(ctx, delta, metric.WithAttributes(attrs...))
}

// SetPluginConnectionsMax sets the plugin connections max gauge.
// Uses delta tracking to convert absolute values to UpDownCounter deltas.
func (m *Metrics) SetPluginConnectionsMax(ctx context.Context, count int64, extraAttrs ...attribute.KeyValue) {
	m.mu.Lock()
	delta := count - m.lastPluginConnectionsMax
	m.lastPluginConnectionsMax = count
	m.mu.Unlock()

	attrs := m.mergeAttrs(extraAttrs...)
	m.pluginConnectionsMax.Add(ctx, delta, metric.WithAttributes(attrs...))
}

// SetWasmCacheEntries sets the WASM cache entries gauge.
// Uses delta tracking to convert absolute values to UpDownCounter deltas.
func (m *Metrics) SetWasmCacheEntries(ctx context.Context, count int64, extraAttrs ...attribute.KeyValue) {
	m.mu.Lock()
	delta := count - m.lastWasmCacheEntries
	m.lastWasmCacheEntries = count
	m.mu.Unlock()

	attrs := m.mergeAttrs(extraAttrs...)
	m.wasmCacheEntries.Add(ctx, delta, metric.WithAttributes(attrs...))
}

// SetWasmCacheBytes sets the WASM cache bytes gauge.
// Uses delta tracking to convert absolute values to UpDownCounter deltas.
func (m *Metrics) SetWasmCacheBytes(ctx context.Context, bytes int64, extraAttrs ...attribute.KeyValue) {
	m.mu.Lock()
	delta := bytes - m.lastWasmCacheBytes
	m.lastWasmCacheBytes = bytes
	m.mu.Unlock()

	attrs := m.mergeAttrs(extraAttrs...)
	m.wasmCacheBytes.Add(ctx, delta, metric.WithAttributes(attrs...))
}

// SetWorkflowsStuck sets the workflows stuck gauge.
// Uses delta tracking to convert absolute values to UpDownCounter deltas.
func (m *Metrics) SetWorkflowsStuck(ctx context.Context, count int64, extraAttrs ...attribute.KeyValue) {
	m.mu.Lock()
	delta := count - m.lastWorkflowsStuck
	m.lastWorkflowsStuck = count
	m.mu.Unlock()

	attrs := m.mergeAttrs(extraAttrs...)
	m.workflowsStuck.Add(ctx, delta, metric.WithAttributes(attrs...))
}

// SetEventHistoryRowCount sets the event history row count gauge.
// Uses delta tracking to convert absolute values to UpDownCounter deltas.
func (m *Metrics) SetEventHistoryRowCount(ctx context.Context, count int64, extraAttrs ...attribute.KeyValue) {
	m.mu.Lock()
	delta := count - m.lastEventHistoryRowCount
	m.lastEventHistoryRowCount = count
	m.mu.Unlock()

	attrs := m.mergeAttrs(extraAttrs...)
	m.eventHistoryRowCount.Add(ctx, delta, metric.WithAttributes(attrs...))
}

// SetConcurrencyKeysTotal sets the concurrency keys total gauge.
// Uses delta tracking to convert absolute values to UpDownCounter deltas.
func (m *Metrics) SetConcurrencyKeysTotal(ctx context.Context, count int64, extraAttrs ...attribute.KeyValue) {
	m.mu.Lock()
	delta := count - m.lastConcurrencyKeysTotal
	m.lastConcurrencyKeysTotal = count
	m.mu.Unlock()

	attrs := m.mergeAttrs(extraAttrs...)
	m.concurrencyKeysTotal.Add(ctx, delta, metric.WithAttributes(attrs...))
}

// SetConcurrencyKeysExpiringSoon sets the concurrency keys expiring soon gauge.
// Uses delta tracking to convert absolute values to UpDownCounter deltas.
func (m *Metrics) SetConcurrencyKeysExpiringSoon(ctx context.Context, count int64, extraAttrs ...attribute.KeyValue) {
	m.mu.Lock()
	delta := count - m.lastConcurrencyKeysExpiringSoon
	m.lastConcurrencyKeysExpiringSoon = count
	m.mu.Unlock()

	attrs := m.mergeAttrs(extraAttrs...)
	m.concurrencyKeysExpiringSoon.Add(ctx, delta, metric.WithAttributes(attrs...))
}

// --- Memory UpDownCounter methods (delta-tracked) ---

// RecordMemoryRSS records worker process RSS in bytes.
// Converts the absolute value to a delta for the UpDownCounter.
func (m *Metrics) RecordMemoryRSS(ctx context.Context, bytes int64, extraAttrs ...attribute.KeyValue) {
	m.mu.Lock()
	delta := bytes - m.lastRSS
	m.lastRSS = bytes
	m.mu.Unlock()

	attrs := m.mergeAttrs(extraAttrs...)
	m.memoryRSS.Add(ctx, delta, metric.WithAttributes(attrs...))
}

// RecordMemoryAvailable records available memory in bytes.
// Converts the absolute value to a delta for the UpDownCounter.
func (m *Metrics) RecordMemoryAvailable(ctx context.Context, bytes int64, extraAttrs ...attribute.KeyValue) {
	m.mu.Lock()
	delta := bytes - m.lastAvailable
	m.lastAvailable = bytes
	m.mu.Unlock()

	attrs := m.mergeAttrs(extraAttrs...)
	m.memoryAvailable.Add(ctx, delta, metric.WithAttributes(attrs...))
}

// RecordMemoryTotal records total allocatable memory in bytes.
// Converts the absolute value to a delta for the UpDownCounter.
func (m *Metrics) RecordMemoryTotal(ctx context.Context, bytes int64, extraAttrs ...attribute.KeyValue) {
	m.mu.Lock()
	delta := bytes - m.lastTotal
	m.lastTotal = bytes
	m.mu.Unlock()

	attrs := m.mergeAttrs(extraAttrs...)
	m.memoryTotal.Add(ctx, delta, metric.WithAttributes(attrs...))
}

// RecordConcurrencyLimit records the current effective concurrency cap.
// Converts the absolute value to a delta for the UpDownCounter.
func (m *Metrics) RecordConcurrencyLimit(ctx context.Context, limit int64, extraAttrs ...attribute.KeyValue) {
	m.mu.Lock()
	delta := limit - m.lastConcurrencyLimit
	m.lastConcurrencyLimit = limit
	m.mu.Unlock()

	attrs := m.mergeAttrs(extraAttrs...)
	m.concurrencyLimit.Add(ctx, delta, metric.WithAttributes(attrs...))
}

// RecordDesiredConcurrency records the configured --concurrency value.
// Converts the absolute value to a delta for the UpDownCounter.
func (m *Metrics) RecordDesiredConcurrency(ctx context.Context, desired int64, extraAttrs ...attribute.KeyValue) {
	m.mu.Lock()
	delta := desired - m.lastDesiredConcurrency
	m.lastDesiredConcurrency = desired
	m.mu.Unlock()

	attrs := m.mergeAttrs(extraAttrs...)
	m.desiredConcurrency.Add(ctx, delta, metric.WithAttributes(attrs...))
}

// RecordWorkflowMemoryEstimate records the estimated memory per workflow execution
// by def_name. Converts the absolute value to a delta for the UpDownCounter.
func (m *Metrics) RecordWorkflowMemoryEstimate(ctx context.Context, defName string, bytes float64, extraAttrs ...attribute.KeyValue) {
	m.mu.Lock()
	last, exists := m.lastWorkflowMemoryEstimate[defName]
	delta := int64(bytes - last)
	if !exists {
		delta = int64(bytes)
	}
	m.lastWorkflowMemoryEstimate[defName] = bytes
	m.mu.Unlock()

	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("def_name", defName),
	}, extraAttrs...)...)
	m.workflowMemoryEstimate.Add(ctx, delta, metric.WithAttributes(attrs...))
}

// --- Int64Gauge methods ---

// SetQueueDepth sets the queue depth gauge.
func (m *Metrics) SetQueueDepth(ctx context.Context, depth int64, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.queueDepth.Record(ctx, depth, metric.WithAttributes(attrs...))
}

// SetRetentionLastRunTimestamp sets the retention last run timestamp gauge.
func (m *Metrics) SetRetentionLastRunTimestamp(ctx context.Context, ts int64, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.retentionLastRunTimestamp.Record(ctx, ts, metric.WithAttributes(attrs...))
}

// SetBackgroundLoopItemsProcessed sets the background loop items processed gauge.
func (m *Metrics) SetBackgroundLoopItemsProcessed(ctx context.Context, loopName string, count int64, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("loop_name", loopName),
	}, extraAttrs...)...)
	m.backgroundLoopItemsProcessed.Record(ctx, count, metric.WithAttributes(attrs...))
}

// --- Float64Gauge methods ---

// SetReplayThroughput sets the replay step throughput rate gauge.
func (m *Metrics) SetReplayThroughput(ctx context.Context, rate float64, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.replayThroughput.Record(ctx, rate, metric.WithAttributes(attrs...))
}

// SetFreshThroughput sets the fresh step throughput rate gauge.
func (m *Metrics) SetFreshThroughput(ctx context.Context, rate float64, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.freshThroughput.Record(ctx, rate, metric.WithAttributes(attrs...))
}

// SetFreshStepCount sets the cumulative fresh step count gauge.
func (m *Metrics) SetFreshStepCount(ctx context.Context, val int64, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.freshStepCountGauge.Record(ctx, val, metric.WithAttributes(attrs...))
}

// SetReplayStepCount sets the cumulative replay step count gauge.
func (m *Metrics) SetReplayStepCount(ctx context.Context, val int64, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.replayStepCountGauge.Record(ctx, val, metric.WithAttributes(attrs...))
}

// SetMemoryPressureRatio sets the memory pressure ratio gauge.
func (m *Metrics) SetMemoryPressureRatio(ctx context.Context, ratio float64, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.memoryPressureRatio.Record(ctx, ratio, metric.WithAttributes(attrs...))
}

// SetMemoryPressure sets the memory pressure gauge.
func (m *Metrics) SetMemoryPressure(ctx context.Context, pressure float64, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.memoryPressure.Record(ctx, pressure, metric.WithAttributes(attrs...))
}

// SetScalingPressure sets the composite scaling pressure gauge.
func (m *Metrics) SetScalingPressure(ctx context.Context, pressure float64, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.scalingPressure.Record(ctx, pressure, metric.WithAttributes(attrs...))
}

// SetBackgroundLoopDuration sets the background loop duration gauge.
func (m *Metrics) SetBackgroundLoopDuration(ctx context.Context, loopName string, seconds float64, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("loop_name", loopName),
	}, extraAttrs...)...)
	m.backgroundLoopDuration.Record(ctx, seconds, metric.WithAttributes(attrs...))
}

// SetBackgroundLoopLastRun sets the background loop last run timestamp gauge.
func (m *Metrics) SetBackgroundLoopLastRun(ctx context.Context, loopName string, ts float64, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("loop_name", loopName),
	}, extraAttrs...)...)
	m.backgroundLoopLastRun.Record(ctx, ts, metric.WithAttributes(attrs...))
}

// --- Histogram recording methods ---

// RecordClaimLatency records the time taken to claim a workflow.
func (m *Metrics) RecordClaimLatency(ctx context.Context, duration time.Duration, workflowName string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("workflow_name", workflowName),
	}, extraAttrs...)...)
	m.claimLatency.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
}

// RecordWasmLoadLatency records the time taken to load a WASM module.
func (m *Metrics) RecordWasmLoadLatency(ctx context.Context, duration time.Duration, defName string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("def_name", defName),
	}, extraAttrs...)...)
	m.wasmLoadLatency.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
}

// RecordDBQueryLatency records a database query duration.
func (m *Metrics) RecordDBQueryLatency(ctx context.Context, duration time.Duration, operation string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("operation", operation),
	}, extraAttrs...)...)
	m.dbQueryLatency.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
}

// RecordWorkflowDuration records the total execution duration of a workflow.
func (m *Metrics) RecordWorkflowDuration(ctx context.Context, duration time.Duration, workflowName string, status string, taskQueue string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("workflow_name", workflowName),
		attribute.String("status", status),
		attribute.String("task_queue", taskQueue),
	}, extraAttrs...)...)
	m.workflowDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
}

// RecordReplayDuration records the duration of workflow replay execution.
func (m *Metrics) RecordReplayDuration(ctx context.Context, duration time.Duration, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.replayDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
}

// RecordFreshDuration records the duration of first-run workflow execution by def_name.
func (m *Metrics) RecordFreshDuration(ctx context.Context, duration time.Duration, defName string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("def_name", defName),
	}, extraAttrs...)...)
	m.freshDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
}

// RecordPollWaitDuration records the time spent waiting for work in poll queries.
func (m *Metrics) RecordPollWaitDuration(ctx context.Context, duration time.Duration, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	m.pollWaitDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
}

// RecordWasmCompileDuration records WASM compile duration by def_name.
func (m *Metrics) RecordWasmCompileDuration(ctx context.Context, duration time.Duration, defName string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("def_name", defName),
	}, extraAttrs...)...)
	m.wasmCompileDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
}

// RecordDispatchLatency records time from workflow creation to claim by task_queue.
func (m *Metrics) RecordDispatchLatency(ctx context.Context, duration time.Duration, taskQueue string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("task_queue", taskQueue),
	}, extraAttrs...)...)
	m.dispatchLatency.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
}

// RecordHTTPRequestDuration records an HTTP request duration for backendkit apps.
func (m *Metrics) RecordHTTPRequestDuration(ctx context.Context, duration time.Duration, method, path string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("method", method),
		attribute.String("path", path),
	}, extraAttrs...)...)
	m.httpRequestDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
}

// --- HTTP handler ---

// ServeHTTP returns an http.Handler that serves the current metrics in
// Prometheus exposition format (text/plain; version=0.0.4).
func (m *Metrics) ServeHTTP() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Use a shorter timeout for collecting metrics to avoid
		// blocking the HTTP handler.
		collectCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		var rm metricdata.ResourceMetrics
		if err := m.reader.Collect(collectCtx, &rm); err != nil {
			http.Error(w, "metrics collection error: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if err := writePrometheusText(w, &rm); err != nil {
			// Partial write may have occurred; nothing we can do.
			return
		}
	})
}

// --- Lifecycle ---

// Shutdown flushes and shuts down the metrics pipeline. It should be called
// when the worker is shutting down.
func (m *Metrics) Shutdown(ctx context.Context) error {
	return m.mp.Shutdown(ctx)
}

// --- Internal helpers ---

// mergeAttrs combines default attributes with call-specific attributes.
// Call-specific attributes override defaults if they share the same key.
func (m *Metrics) mergeAttrs(attrs ...attribute.KeyValue) []attribute.KeyValue {
	if len(attrs) == 0 {
		return m.defaultAttrs
	}
	// Build a set of keys from call-specific attrs to detect overrides.
	override := make(map[attribute.Key]struct{}, len(attrs))
	for _, a := range attrs {
		override[a.Key] = struct{}{}
	}
	result := make([]attribute.KeyValue, 0, len(m.defaultAttrs)+len(attrs))
	for _, d := range m.defaultAttrs {
		if _, ok := override[d.Key]; !ok {
			result = append(result, d)
		}
	}
	result = append(result, attrs...)
	return result
}

// --- Prometheus text format writer ---

// stripTotalSuffix removes the _total suffix from a metric name if present.
// This is used for # HELP and # TYPE lines where Prometheus convention omits
// the suffix for counter metrics.
func stripTotalSuffix(name string) string {
	if strings.HasSuffix(name, "_total") {
		return name[:len(name)-6]
	}
	return name
}

// writePrometheusText formats a ResourceMetrics tree into Prometheus
// exposition format and writes it to w.
func writePrometheusText(w io.Writer, rm *metricdata.ResourceMetrics) error {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if err := writeMetric(w, m); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeMetric writes a single metric in Prometheus exposition format.
func writeMetric(w io.Writer, m metricdata.Metrics) error {
	// Write the HELP and TYPE lines at most once per metric name.
	switch data := m.Data.(type) {
	case metricdata.Sum[int64]:
		if len(data.DataPoints) == 0 {
			return nil
		}
		helpName := m.Name
		typ := "counter"
		if !data.IsMonotonic {
			typ = "gauge"
		} else {
			// Strip _total suffix only for counter metrics in HELP/TYPE lines.
			helpName = stripTotalSuffix(helpName)
		}
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", helpName, m.Description, helpName, typ); err != nil {
			return err
		}
		for _, dp := range data.DataPoints {
			labels := formatLabels(dp.Attributes)
			if _, err := fmt.Fprintf(w, "%s%s %d\n", m.Name, labels, dp.Value); err != nil {
				return err
			}
		}

	case metricdata.Sum[float64]:
		if len(data.DataPoints) == 0 {
			return nil
		}
		helpName := m.Name
		typ := "counter"
		if !data.IsMonotonic {
			typ = "gauge"
		} else {
			helpName = stripTotalSuffix(helpName)
		}
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", helpName, m.Description, helpName, typ); err != nil {
			return err
		}
		for _, dp := range data.DataPoints {
			labels := formatLabels(dp.Attributes)
			if _, err := fmt.Fprintf(w, "%s%s %g\n", m.Name, labels, dp.Value); err != nil {
				return err
			}
		}

	case metricdata.Gauge[int64]:
		if len(data.DataPoints) == 0 {
			return nil
		}
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", m.Name, m.Description, m.Name); err != nil {
			return err
		}
		for _, dp := range data.DataPoints {
			labels := formatLabels(dp.Attributes)
			if _, err := fmt.Fprintf(w, "%s%s %d\n", m.Name, labels, dp.Value); err != nil {
				return err
			}
		}

	case metricdata.Gauge[float64]:
		if len(data.DataPoints) == 0 {
			return nil
		}
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", m.Name, m.Description, m.Name); err != nil {
			return err
		}
		for _, dp := range data.DataPoints {
			labels := formatLabels(dp.Attributes)
			if _, err := fmt.Fprintf(w, "%s%s %g\n", m.Name, labels, dp.Value); err != nil {
				return err
			}
		}

	case metricdata.Histogram[int64]:
		if len(data.DataPoints) == 0 {
			return nil
		}
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", m.Name, m.Description, m.Name); err != nil {
			return err
		}
		for _, dp := range data.DataPoints {
			if err := writeHistogramDataPoint(w, m.Name, dp.Attributes, dp.Bounds, dp.BucketCounts, dp.Count, float64(dp.Sum)); err != nil {
				return err
			}
		}

	case metricdata.Histogram[float64]:
		if len(data.DataPoints) == 0 {
			return nil
		}
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", m.Name, m.Description, m.Name); err != nil {
			return err
		}
		for _, dp := range data.DataPoints {
			if err := writeHistogramDataPoint(w, m.Name, dp.Attributes, dp.Bounds, dp.BucketCounts, dp.Count, float64(dp.Sum)); err != nil {
				return err
			}
		}
	}

	return nil
}

// writeHistogramDataPoint writes a single histogram data point in Prometheus
// exposition format, including _bucket, _count, and _sum lines.
func writeHistogramDataPoint(
	w io.Writer,
	name string,
	attrs attribute.Set,
	bounds []float64,
	bucketCounts []uint64,
	count uint64,
	sum float64,
) error {
	labels := formatLabels(attrs)

	// Write _bucket lines.
	for i, bound := range bounds {
		le := fmt.Sprintf("%g", bound)
		if _, err := fmt.Fprintf(w, "%s_bucket{%s,le=%q} %d\n", name, labels, le, bucketCounts[i]); err != nil {
			return err
		}
	}
	// +Inf bucket
	tailCount := count
	if len(bucketCounts) > 0 {
		tailCount = bucketCounts[len(bucketCounts)-1]
	}
	if _, err := fmt.Fprintf(w, "%s_bucket{%s,le=%q} %d\n", name, labels, "+Inf", tailCount); err != nil {
		return err
	}

	// _count and _sum.
	if _, err := fmt.Fprintf(w, "%s_count{%s} %d\n", name, labels, count); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%s_sum{%s} %g\n", name, labels, sum); err != nil {
		return err
	}

	return nil
}

// formatLabels renders an attribute.Set as a Prometheus label string.
// The result is either empty (no labels) or of the form `{key="val",key2="val2"}`.
func formatLabels(attrs attribute.Set) string {
	if attrs.Len() == 0 {
		return ""
	}
	// Collect and sort keys for deterministic output.
	iter := attrs.Iter()
	kvs := make([]attribute.KeyValue, 0, attrs.Len())
	for iter.Next() {
		kvs = append(kvs, iter.Attribute())
	}
	sort.Slice(kvs, func(i, j int) bool {
		return kvs[i].Key < kvs[j].Key
	})

	var buf strings.Builder
	buf.WriteByte('{')
	for i, kv := range kvs {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(string(kv.Key))
		buf.WriteString(`="`)
		buf.WriteString(escapeLabelValue(kv.Value.AsString()))
		buf.WriteByte('"')
	}
	buf.WriteByte('}')
	return buf.String()
}

// escapeLabelValue escapes a Prometheus label value according to the
// exposition format specification: backslash, double-quote, newline,
// and carriage return are escaped.
func escapeLabelValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	return s
}
