package prometheus

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// collectHistogramBounds collects metrics and returns the bounds for the named
// histogram by matching on the metric name prefix.
func collectHistogramBounds(t *testing.T, m *Metrics, name string) []float64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var rm metricdata.ResourceMetrics
	if err := m.reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect failed: %v", err)
	}

	for _, sm := range rm.ScopeMetrics {
		for _, metric := range sm.Metrics {
			if metric.Name != name {
				continue
			}
			switch data := metric.Data.(type) {
			case metricdata.Histogram[float64]:
				if len(data.DataPoints) > 0 {
					return data.DataPoints[0].Bounds
				}
				return nil
			default:
				t.Fatalf("metric %s is not a float64 histogram", name)
			}
		}
	}
	t.Fatalf("metric %s not found in collected data", name)
	return nil
}

func TestNewWithCustomLatencyBuckets(t *testing.T) {
	customBuckets := []float64{0.001, 0.01, 0.1, 1.0, 10.0}

	m, err := New(Config{
		WorkerID:                "test-worker",
		LatencyHistogramBuckets: customBuckets,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer m.Shutdown(context.Background())

	// Record a claim latency observation so a data point exists.
	m.RecordClaimLatency(context.Background(), 15*time.Millisecond, "test_wf")

	bounds := collectHistogramBounds(t, m, "cleat_claim_latency_seconds")

	if len(bounds) != len(customBuckets) {
		t.Fatalf("expected %d buckets, got %d", len(customBuckets), len(bounds))
	}
	for i, b := range customBuckets {
		if bounds[i] != b {
			t.Errorf("bucket[%d]: expected %v, got %v", i, b, bounds[i])
		}
	}
}

func TestNewWithDefaultLatencyBuckets(t *testing.T) {
	m, err := New(Config{
		WorkerID: "test-worker",
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer m.Shutdown(context.Background())

	m.RecordClaimLatency(context.Background(), 15*time.Millisecond, "test_wf")

	bounds := collectHistogramBounds(t, m, "cleat_claim_latency_seconds")

	expected := []float64{0.001, 0.005, 0.010, 0.025, 0.050, 0.100, 0.250, 0.500, 1.000, 2.500, 5.000}
	if len(bounds) != len(expected) {
		t.Fatalf("expected %d buckets, got %d", len(expected), len(bounds))
	}
	for i, b := range expected {
		if bounds[i] != b {
			t.Errorf("bucket[%d]: expected %v, got %v", i, b, bounds[i])
		}
	}
}

func TestNewWithNilLatencyBuckets(t *testing.T) {
	// nil slice should behave the same as empty/default.
	m, err := New(Config{
		WorkerID:                "test-worker",
		LatencyHistogramBuckets: nil,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer m.Shutdown(context.Background())

	m.RecordClaimLatency(context.Background(), 15*time.Millisecond, "test_wf")

	bounds := collectHistogramBounds(t, m, "cleat_claim_latency_seconds")

	expected := []float64{0.001, 0.005, 0.010, 0.025, 0.050, 0.100, 0.250, 0.500, 1.000, 2.500, 5.000}
	if len(bounds) != len(expected) {
		t.Fatalf("expected %d buckets, got %d", len(expected), len(bounds))
	}
	for i, b := range expected {
		if bounds[i] != b {
			t.Errorf("bucket[%d]: expected %v, got %v", i, b, bounds[i])
		}
	}
}

func TestExecutionDurationHistogramsUnaffected(t *testing.T) {
	// Custom latency buckets should NOT affect workflow/replay/fresh duration.
	customBuckets := []float64{0.010, 0.100, 1.000}

	m, err := New(Config{
		WorkerID:                "test-worker",
		LatencyHistogramBuckets: customBuckets,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer m.Shutdown(context.Background())

	m.RecordWorkflowDuration(context.Background(), 5*time.Second, "test_wf", "completed", "default")

	bounds := collectHistogramBounds(t, m, "cleat_workflow_duration_seconds")

	expected := []float64{0.010, 0.050, 0.100, 0.250, 0.500, 1.000, 2.500, 5.000, 10.000, 30.000, 60.000, 120.000, 300.000}
	if len(bounds) != len(expected) {
		t.Fatalf("expected %d buckets, got %d", len(expected), len(bounds))
	}
	for i, b := range expected {
		if bounds[i] != b {
			t.Errorf("bucket[%d]: expected %v, got %v", i, b, bounds[i])
		}
	}
}

func TestCustomLatencyBuckets_AllLatencyHistograms(t *testing.T) {
	// Verify all 8 latency histograms use custom buckets when set.
	customBuckets := []float64{0.005, 0.05, 0.5, 5.0}

	m, err := New(Config{
		WorkerID:                "test-worker",
		LatencyHistogramBuckets: customBuckets,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer m.Shutdown(context.Background())

	latencyNames := []string{
		"cleat_claim_latency_seconds",
		"cleat_wasm_load_latency_seconds",
		"cleat_db_query_latency_seconds",
		"cleat_dispatch_latency_seconds",
		"cleat_apps_http_request_duration_seconds",
		"cleat_poll_wait_seconds",
		"cleat_wasm_compile_duration_seconds",
		"cleat_plugin_call_duration_seconds",
	}

	// Record at least one observation for each.
	m.RecordClaimLatency(context.Background(), 10*time.Millisecond, "wf")
	m.RecordWasmLoadLatency(context.Background(), 100*time.Millisecond, "def1")
	m.RecordDBQueryLatency(context.Background(), 5*time.Millisecond, "select")
	m.RecordDispatchLatency(context.Background(), 50*time.Millisecond, "default")
	m.RecordHTTPRequestDuration(context.Background(), 30*time.Millisecond, "GET", "/")
	m.RecordPollWaitDuration(context.Background(), 100*time.Millisecond)
	m.RecordWasmCompileDuration(context.Background(), 200*time.Millisecond, "def1")
	m.RecordPluginCallDuration(context.Background(), 10*time.Millisecond, "slack", "notify")

	for _, name := range latencyNames {
		t.Run(strings.TrimPrefix(name, "cleat_"), func(t *testing.T) {
			bounds := collectHistogramBounds(t, m, name)
			if len(bounds) != len(customBuckets) {
				t.Errorf("%s: expected %d buckets, got %d", name, len(customBuckets), len(bounds))
				return
			}
			for i, b := range customBuckets {
				if bounds[i] != b {
					t.Errorf("%s: bucket[%d]: expected %v, got %v", name, i, b, bounds[i])
				}
			}
		})
	}
}
