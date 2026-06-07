package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// gatherGaugeValue collects a gauge metric by name from the default gatherer
// and returns its value. Returns 0 if the metric is not found.
func gatherGaugeValue(t *testing.T, name string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			if mf.GetType() != dto.MetricType_GAUGE {
				t.Fatalf("metric %q is not a gauge", name)
			}
			metrics := mf.GetMetric()
			if len(metrics) == 0 {
				return 0
			}
			return metrics[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("metric %q not found", name)
	return 0
}

// gatherMetricNames returns the set of metric names registered with the default gatherer.
func gatherMetricNames(t *testing.T) map[string]bool {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	names := make(map[string]bool, len(mfs))
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	return names
}

func TestMetrics_Init_RegistersAllMetrics(t *testing.T) {
	names := gatherMetricNames(t)

	required := []string{
		"cleat_workflows_active",
		"cleat_wasm_cache_entries",
		"cleat_wasm_cache_bytes",
		"cleat_workflows_claimed_total",
		"cleat_workflows_dead_lettered_total",
		"cleat_wasm_cache_hits_total",
		"cleat_wasm_cache_misses_total",
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
		"cleat_replay_duration_seconds",
	}

	for _, name := range required {
		if !names[name] {
			t.Errorf("metric %q not registered after init()", name)
		}
	}
}

func TestSetReplayThroughput(t *testing.T) {
	SetReplayThroughput(99.5)
	val := gatherGaugeValue(t, "cleat_replay_throughput_steps_per_second")
	if val != 99.5 {
		t.Errorf("replay throughput = %v, want 99.5", val)
	}

	// Test zero.
	SetReplayThroughput(0)
	val = gatherGaugeValue(t, "cleat_replay_throughput_steps_per_second")
	if val != 0 {
		t.Errorf("replay throughput after zero = %v, want 0", val)
	}
}

func TestSetFreshThroughput(t *testing.T) {
	SetFreshThroughput(42.0)
	val := gatherGaugeValue(t, "cleat_fresh_throughput_steps_per_second")
	if val != 42.0 {
		t.Errorf("fresh throughput = %v, want 42.0", val)
	}

	SetFreshThroughput(0)
	val = gatherGaugeValue(t, "cleat_fresh_throughput_steps_per_second")
	if val != 0 {
		t.Errorf("fresh throughput after zero = %v, want 0", val)
	}
}

func TestSetQueueDepth(t *testing.T) {
	SetQueueDepth(100)
	val := gatherGaugeValue(t, "cleat_queue_depth")
	if val != 100 {
		t.Errorf("queue depth = %v, want 100", val)
	}

	SetQueueDepth(0)
	val = gatherGaugeValue(t, "cleat_queue_depth")
	if val != 0 {
		t.Errorf("queue depth after zero = %v, want 0", val)
	}
}

func TestHandleMetrics_Returns200(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handleMetrics(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}

	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
}

func TestHandleMetrics_ContentIncludesMetrics(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handleMetrics(rec, req)

	body := rec.Body.String()
	if body == "" {
		t.Fatal("expected non-empty response body")
	}
	if !strings.Contains(body, "cleat_") {
		t.Errorf("expected response to contain cleat_ metrics, got: %s...", body[:min(len(body), 100)])
	}
}
