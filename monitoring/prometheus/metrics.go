// Package prometheus provides OpenTelemetry-based metrics instrumentation for the
// cleat workflow engine. Metrics are recorded via OTel instruments and exposed in
// Prometheus exposition format on the /metrics HTTP endpoint.
//
// Usage:
//
//	m, err := prometheus.New(prometheus.Config{
//	    Namespace: "default",
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
	// Namespace is the workflow namespace label applied to all metrics.
	Namespace string
	// WorkerID uniquely identifies this worker instance.
	WorkerID string
	// ServiceName identifies the service for the OTel meter.
	ServiceName string
}

// Metrics provides OpenTelemetry-based metrics instrumentation for the cleat
// workflow engine. All methods are safe for concurrent use — OTel instruments
// are thread-safe by design.
type Metrics struct {
	config Config

	meter  metric.Meter
	reader *sdkmetric.ManualReader
	mp     *sdkmetric.MeterProvider

	// --- Counters ---
	workflowsStarted   metric.Int64Counter
	workflowsCompleted metric.Int64Counter
	workflowsFailed    metric.Int64Counter
	stepsExecuted      metric.Int64Counter
	replaySteps        metric.Int64Counter

	// --- UpDownCounters (serve as Prometheus gauges) ---
	workflowsActive  metric.Int64UpDownCounter
	workerCount      metric.Int64UpDownCounter
	eventHistorySize metric.Int64UpDownCounter

	// --- Histograms ---
	claimLatency     metric.Float64Histogram
	wasmLoadLatency  metric.Float64Histogram
	dbQueryLatency   metric.Float64Histogram
	workflowDuration metric.Float64Histogram

	// Default attributes applied to every recording.
	defaultAttrs []attribute.KeyValue

	once sync.Once
}

// New creates a new Metrics instance with the given configuration. It
// initialises all OTel instruments and starts a manual reader that will be
// polled on each /metrics request.
func New(cfg Config) (*Metrics, error) {
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "cleat-workflow-engine"
	}

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	meter := mp.Meter(
		"github.com/rcownie/durable/monitoring/prometheus",
		metric.WithInstrumentationVersion("1.0.0"),
	)

	m := &Metrics{
		config: cfg,
		meter:  meter,
		reader: reader,
		mp:     mp,
		defaultAttrs: []attribute.KeyValue{
			attribute.String("namespace", cfg.Namespace),
			attribute.String("worker_id", cfg.WorkerID),
		},
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

	m.stepsExecuted, err = meter.Int64Counter(
		"cleat_steps_executed_total",
		metric.WithDescription("Total number of durable step executions"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_steps_executed_total: %w", err)
	}

	m.replaySteps, err = meter.Int64Counter(
		"cleat_replay_steps_total",
		metric.WithDescription("Total number of replayed steps"),
	)
	if err != nil {
		return nil, fmt.Errorf("cleat_replay_steps_total: %w", err)
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
func (m *Metrics) RecordWorkflowCompleted(ctx context.Context, workflowName string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("workflow_name", workflowName),
	}, extraAttrs...)...)
	m.workflowsCompleted.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordWorkflowFailed increments the workflows-failed counter.
func (m *Metrics) RecordWorkflowFailed(ctx context.Context, workflowName string, errMsg string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("workflow_name", workflowName),
		attribute.String("error", errMsg),
	}, extraAttrs...)...)
	m.workflowsFailed.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordStepExecuted increments the steps-executed counter.
func (m *Metrics) RecordStepExecuted(ctx context.Context, workflowName string, stepName string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("workflow_name", workflowName),
		attribute.String("step_name", stepName),
	}, extraAttrs...)...)
	m.stepsExecuted.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordReplayStep increments the replay-steps counter.
func (m *Metrics) RecordReplayStep(ctx context.Context, workflowName string, stepName string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("workflow_name", workflowName),
		attribute.String("step_name", stepName),
	}, extraAttrs...)...)
	m.replaySteps.Add(ctx, 1, metric.WithAttributes(attrs...))
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
func (m *Metrics) SetWorkerCount(ctx context.Context, count int64, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(extraAttrs...)
	// UpDownCounter supports absolute values by adding delta, but the
	// simplest approach is to observe the difference from current.
	// Since the worker count is set once at startup, we use Add with
	// the absolute value, relying on the cumulative semantics to
	// represent the current count.
	m.workerCount.Add(ctx, count, metric.WithAttributes(attrs...))
}

// SetEventHistorySize sets the event history size gauge to the given value.
func (m *Metrics) SetEventHistorySize(ctx context.Context, sizeBytes int64, workflowName string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("workflow_name", workflowName),
	}, extraAttrs...)...)
	m.eventHistorySize.Add(ctx, sizeBytes, metric.WithAttributes(attrs...))
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
func (m *Metrics) RecordWorkflowDuration(ctx context.Context, duration time.Duration, workflowName string, extraAttrs ...attribute.KeyValue) {
	attrs := m.mergeAttrs(append([]attribute.KeyValue{
		attribute.String("workflow_name", workflowName),
	}, extraAttrs...)...)
	m.workflowDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
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
		typ := "counter"
		if !data.IsMonotonic {
			typ = "gauge"
		}
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", m.Name, m.Description, m.Name, typ); err != nil {
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
		typ := "counter"
		if !data.IsMonotonic {
			typ = "gauge"
		}
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", m.Name, m.Description, m.Name, typ); err != nil {
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
// exposition format specification: backslash, double-quote, and newline
// are escaped.
func escapeLabelValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}
