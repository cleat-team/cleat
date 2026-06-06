package main

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Prometheus metric descriptors for the cleat worker.
var (
	// ---- Gauges ----
	workflowsActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cleat_workflows_active",
		Help: "Currently claimed workflow instances",
	})

	wasmCacheEntries = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cleat_wasm_cache_entries",
		Help: "WASM cache entries",
	})
	wasmCacheBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cleat_wasm_cache_bytes",
		Help: "WASM cache bytes",
	})

	// ---- Counters ----
	workflowsClaimed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cleat_workflows_claimed_total",
		Help: "Workflows claimed from the queue",
	})
	workflowsCompleted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cleat_workflows_completed_total",
		Help: "Workflows completed successfully",
	}, []string{"def_name", "task_queue"})
	workflowsFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cleat_workflows_failed_total",
		Help: "Workflows that failed",
	}, []string{"def_name", "task_queue"})
	workflowsDeadLettered = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cleat_workflows_dead_lettered_total",
		Help: "Workflows moved to dead letter queue",
	})
	wasmCacheHits = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cleat_wasm_cache_hits_total",
		Help: "WASM cache hits",
	})
	wasmCacheMisses = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cleat_wasm_cache_misses_total",
		Help: "WASM cache misses",
	})

	// ---- Background loop metrics ----
	backgroundLoopsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cleat_background_loops_total",
		Help: "Total background loop iterations by loop_name and status",
	}, []string{"loop_name", "status"})
	backgroundLoopDuration = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cleat_background_loop_duration_seconds",
		Help: "Duration of the last background loop iteration by loop_name",
	}, []string{"loop_name"})
	backgroundLoopItemsProcessed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cleat_background_loop_items_processed",
		Help: "Items processed in the last background loop iteration by loop_name",
	}, []string{"loop_name"})
	backgroundLoopLastRun = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cleat_background_loop_last_run_seconds",
		Help: "Unix timestamp of the last successful background loop run by loop_name",
	}, []string{"loop_name"})
	backgroundLoopRestarts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cleat_background_loop_restarts_total",
		Help: "Total background loop restarts by loop_name",
	}, []string{"loop_name"})

	// ---- Replay / Fresh step metrics ----
	replayDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "cleat_replay_duration_seconds",
		Help:    "Duration of workflow replay execution",
		Buckets: []float64{0.1, 0.5, 1, 5, 10, 30, 60, 300, 600, 1800, 3600},
	})
	freshDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cleat_fresh_duration_seconds",
		Help:    "Duration of first-run workflow execution",
		Buckets: []float64{0.1, 0.5, 1, 5, 10, 30, 60, 300, 600, 1800, 3600},
	}, []string{"def_name"})

	// ---- Histograms with domain-specific buckets ----
	workflowDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cleat_workflow_duration_seconds",
		Help:    "Workflow execution duration by def_name and status",
		Buckets: []float64{0.1, 0.5, 1, 5, 10, 30, 60, 300, 600, 1800, 3600},
	}, []string{"def_name", "status", "task_queue"})
	dbQueryDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cleat_db_query_duration_seconds",
		Help:    "Database query duration by operation",
		Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5},
	}, []string{"operation"})
	wasmCompileDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cleat_wasm_compile_duration_seconds",
		Help:    "WASM compile duration by def_name",
		Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 2, 5},
	}, []string{"def_name"})
	dispatchLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cleat_dispatch_latency_seconds",
		Help:    "Time from workflow creation to claim by task_queue",
		Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30},
	}, []string{"task_queue"})
	pollWaitDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "cleat_poll_wait_seconds",
		Help:    "Time spent waiting for work in poll queries",
		Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1},
	})
	workerCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cleat_worker_count",
		Help: "Number of cleat workers connected to this database",
	})
	queueDepth = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cleat_queue_depth",
		Help: "Ready workflows waiting in task queues",
	})
	replayThroughput = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cleat_replay_throughput_steps_per_second",
		Help: "Current replay step throughput rate",
	})
	freshThroughput = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cleat_fresh_throughput_steps_per_second",
		Help: "Current fresh step throughput rate",
	})
	eventsDeletedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cleat_events_deleted_total",
		Help: "Number of expired event history rows deleted by the retention policy",
	})
	retentionLastRunTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cleat_retention_last_run_timestamp",
		Help: "Unix timestamp of the last retention policy run",
	})

	// ---- P3.1 Workflow stuck metrics ----
	workflowsStuck = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cleat_workflows_stuck",
		Help: "Number of workflow instances stalled beyond the configured stall threshold",
	})

	// ---- P3.2 Missing metrics ----
	memoryPressureRatio = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cleat_memory_pressure_ratio",
		Help: "Current memory pressure ratio (0.0-1.0) from MemoryMonitor",
	})
	eventHistorySizeBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cleat_event_history_size_bytes",
		Help: "Estimated total bytes of event_history table",
	})
	eventHistoryRowCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cleat_event_history_row_count",
		Help: "Total row count of event_history table",
	})
	reaperInstancesClaimedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cleat_reaper_instances_claimed_total",
		Help: "Workflow instances reclaimed by the stale-instance reaper",
	}, []string{"status"})

	// ---- P3.5 Concurrency key metrics ----
	concurrencyKeysTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cleat_concurrency_keys_total",
		Help: "Total number of concurrency keys currently tracked",
	})
	concurrencyKeysExpiringSoon = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cleat_concurrency_keys_expiring_soon",
		Help: "Number of concurrency keys approaching expiry while owning workflow is still running",
	})

	// ---- P3.6 Plugin connection pool metrics ----
	pluginConnectionsInUse = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cleat_plugin_connections_in_use",
		Help: "Current number of open plugin database connections",
	})
	pluginConnectionsMax = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cleat_plugin_connections_max",
		Help: "Maximum configured plugin database connections",
	})
)

func init() {
	prometheus.MustRegister(
		workflowsActive, workflowsClaimed, workflowsCompleted, workflowsFailed,
		workflowsDeadLettered, workflowDuration, replayDuration, freshDuration,
		dbQueryDuration, wasmCompileDuration, dispatchLatency, pollWaitDuration,
		wasmCacheEntries, wasmCacheBytes, wasmCacheHits, wasmCacheMisses,
		backgroundLoopsTotal, backgroundLoopDuration, backgroundLoopItemsProcessed,
		backgroundLoopLastRun, backgroundLoopRestarts,
		eventsDeletedTotal, retentionLastRunTimestamp,
		workerCount,
		workflowsStuck,
		memoryPressureRatio, eventHistorySizeBytes, eventHistoryRowCount,
		reaperInstancesClaimedTotal,
		concurrencyKeysTotal, concurrencyKeysExpiringSoon,
		pluginConnectionsInUse, pluginConnectionsMax,
		queueDepth,
		replayThroughput, freshThroughput,
	)
}

// SetQueueDepth sets the queue depth gauge from the memory controller state.
func SetQueueDepth(d int64) { queueDepth.Set(float64(d)) }

// SetReplayThroughput sets the replay throughput gauge (events/sec).
func SetReplayThroughput(v float64) { replayThroughput.Set(v) }

// SetFreshThroughput sets the fresh throughput gauge (events/sec).
func SetFreshThroughput(v float64) { freshThroughput.Set(v) }

// handleMetrics serves Prometheus-format metrics via the promhttp handler.
func handleMetrics(w http.ResponseWriter, r *http.Request) {
	promhttp.Handler().ServeHTTP(w, r)
}
