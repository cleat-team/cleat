package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestSetQueueDepth(t *testing.T) {
	SetQueueDepth(42)
	if v := testutil.ToFloat64(queueDepth); v != 42.0 {
		t.Errorf("expected queueDepth=42.0, got %v", v)
	}
	SetQueueDepth(0)
	if v := testutil.ToFloat64(queueDepth); v != 0.0 {
		t.Errorf("expected queueDepth=0.0, got %v", v)
	}
}

func TestSetReplayThroughput(t *testing.T) {
	SetReplayThroughput(150.5)
	if v := testutil.ToFloat64(replayThroughput); v != 150.5 {
		t.Errorf("expected replayThroughput=150.5, got %v", v)
	}
	SetReplayThroughput(0)
	if v := testutil.ToFloat64(replayThroughput); v != 0.0 {
		t.Errorf("expected replayThroughput=0.0, got %v", v)
	}
}

func TestSetFreshThroughput(t *testing.T) {
	SetFreshThroughput(75.25)
	if v := testutil.ToFloat64(freshThroughput); v != 75.25 {
		t.Errorf("expected freshThroughput=75.25, got %v", v)
	}
	SetFreshThroughput(0)
	if v := testutil.ToFloat64(freshThroughput); v != 0.0 {
		t.Errorf("expected freshThroughput=0.0, got %v", v)
	}
}

func TestMetricsInit_RegistersScalarMetrics(t *testing.T) {
	// Verify that init() registered key scalar (non-Vec) metrics with the default registry.
	// Vec metrics (CounterVec, GaugeVec, HistogramVec) may not appear in Gather output
	// until they have child metrics created via WithLabelValues.
	metricFamilies, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}

	registered := make(map[string]bool)
	for _, mf := range metricFamilies {
		registered[mf.GetName()] = true
	}

	// Only check scalar metrics that are guaranteed to appear.
	expected := []string{
		"cleat_workflows_active",
		"cleat_wasm_cache_entries",
		"cleat_wasm_cache_bytes",
		"cleat_workflows_claimed_total",
		"cleat_workflows_dead_lettered_total",
		"cleat_wasm_cache_hits_total",
		"cleat_wasm_cache_misses_total",
		"cleat_replay_duration_seconds",
		"cleat_poll_wait_seconds",
		"cleat_worker_count",
		"cleat_queue_depth",
		"cleat_replay_throughput_steps_per_second",
		"cleat_fresh_throughput_steps_per_second",
		"cleat_events_deleted_total",
		"cleat_retention_last_run_timestamp",
		"cleat_workflows_stuck",
		"cleat_memory_pressure_ratio",
		"cleat_event_history_size_bytes",
		"cleat_event_history_row_count",
		"cleat_concurrency_keys_total",
		"cleat_concurrency_keys_expiring_soon",
		"cleat_plugin_connections_in_use",
		"cleat_plugin_connections_max",
	}

	for _, name := range expected {
		if !registered[name] {
			t.Errorf("expected metric %q to be registered", name)
		}
	}
}

func TestHandleMetrics(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	handleMetrics(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	if w.Body.Len() == 0 {
		t.Error("expected non-empty response body")
	}
}
