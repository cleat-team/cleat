package main

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Prometheus metric descriptors for the durable worker.
var (
	workflowsActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "durable_workflows_active",
		Help: "Currently claimed workflow instances",
	})
	workflowsClaimed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "durable_workflows_claimed_total",
		Help: "Workflows claimed from the queue",
	})
	workflowsCompleted = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "durable_workflows_completed_total",
		Help: "Workflows completed successfully",
	})
	workflowsFailed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "durable_workflows_failed_total",
		Help: "Workflows that failed",
	})
	workflowsDeadLettered = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "durable_workflows_dead_lettered_total",
		Help: "Workflows moved to dead letter queue",
	})
	durableCallsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "durable_durable_calls_total",
		Help: "DurableCall invocations",
	})
	workflowDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "durable_workflow_duration_seconds",
		Help:    "Workflow execution duration",
		Buckets: prometheus.DefBuckets,
	})
	replayDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "durable_replay_duration_seconds",
		Help:    "Replay duration",
		Buckets: prometheus.DefBuckets,
	})
	dbQueryDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "durable_db_query_duration_seconds",
		Help:    "Database query duration",
		Buckets: prometheus.DefBuckets,
	})
	wasmCompileDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "durable_wasm_compile_duration_seconds",
		Help:    "WASM compile duration",
		Buckets: prometheus.DefBuckets,
	})
	dispatchLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "durable_dispatch_latency_seconds",
		Help:    "Dispatch latency",
		Buckets: prometheus.DefBuckets,
	})
	pollWaitDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "durable_poll_wait_seconds",
		Help:    "Time spent waiting for work",
		Buckets: prometheus.DefBuckets,
	})
	wasmCacheEntries = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "durable_wasm_cache_entries",
		Help: "WASM cache entries",
	})
	wasmCacheBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "durable_wasm_cache_bytes",
		Help: "WASM cache bytes",
	})
	wasmCacheHits = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "durable_wasm_cache_hits_total",
		Help: "WASM cache hits",
	})
	wasmCacheMisses = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "durable_wasm_cache_misses_total",
		Help: "WASM cache misses",
	})
)

func init() {
	prometheus.MustRegister(
		workflowsActive, workflowsClaimed, workflowsCompleted, workflowsFailed,
		workflowsDeadLettered, durableCallsTotal, workflowDuration, replayDuration,
		dbQueryDuration, wasmCompileDuration, dispatchLatency, pollWaitDuration,
		wasmCacheEntries, wasmCacheBytes, wasmCacheHits, wasmCacheMisses,
	)
}

// handleMetrics serves Prometheus-format metrics via the promhttp handler.
func handleMetrics(w http.ResponseWriter, r *http.Request) {
	promhttp.Handler().ServeHTTP(w, r)
}
