// Package main provides Prometheus-compatible metrics for the cleat-worker
// memory-aware concurrency control subsystem. Metrics are exposed through
// the /metrics HTTP endpoint.
package main

import (
	"fmt"
	"io"
	"sync/atomic"
)

// -- Memory controller Prometheus metrics gauges --
//
// metricsDesiredConcurrency is set once at startup in main.go from the
// --concurrency flag. All other metrics are updated by updateMemoryMetrics
// after each Tick() call.
var (
	metricsMemoryRSS          int64 // gauge: current process RSS / cgroup memory.current
	metricsMemoryAvailable    int64 // gauge: available memory bytes
	metricsMemoryTotal        int64 // gauge: total allocatable bytes
	metricsMemoryPressure     int64 // gauge: 0-1000 (scaled from 0.0-1.0)
	metricsConcurrencyLimit   int64 // gauge: current effective concurrency cap
	metricsScalingPressure    int64 // gauge: 0-1000 (scaled from 0.0-1.0)
	metricsQueueDepth         int64 // gauge: ready workflows in task queues
	metricsDesiredConcurrency int64 // gauge: configured --concurrency
)

// emitMemoryMetrics writes all memory-related Prometheus metrics in text
// format to w. It reads the current controller state atomically and
// iterates over the per-definition memory estimates for label-based metrics.
func emitMemoryMetrics(w io.Writer, state MemoryControllerState, defEstimates map[string]float64) {
	desired := atomic.LoadInt64(&metricsDesiredConcurrency)

	fmt.Fprintf(w, "# HELP cleat_memory_rss_bytes Worker process RSS in bytes\n")
	fmt.Fprintf(w, "# TYPE cleat_memory_rss_bytes gauge\n")
	fmt.Fprintf(w, "cleat_memory_rss_bytes %d\n\n", state.UsedBytes)

	fmt.Fprintf(w, "# HELP cleat_memory_available_bytes Available memory in bytes\n")
	fmt.Fprintf(w, "# TYPE cleat_memory_available_bytes gauge\n")
	fmt.Fprintf(w, "cleat_memory_available_bytes %d\n\n", state.AvailableBytes)

	fmt.Fprintf(w, "# HELP cleat_memory_total_bytes Total allocatable memory in bytes\n")
	fmt.Fprintf(w, "# TYPE cleat_memory_total_bytes gauge\n")
	fmt.Fprintf(w, "cleat_memory_total_bytes %d\n\n", state.TotalBytes)

	fmt.Fprintf(w, "# HELP cleat_memory_pressure Memory pressure 0.0-1.0\n")
	fmt.Fprintf(w, "# TYPE cleat_memory_pressure gauge\n")
	fmt.Fprintf(w, "cleat_memory_pressure %f\n\n", state.Pressure)

	fmt.Fprintf(w, "# HELP cleat_scaling_pressure Composite scaling pressure 0.0-1.0\n")
	fmt.Fprintf(w, "# TYPE cleat_scaling_pressure gauge\n")
	fmt.Fprintf(w, "cleat_scaling_pressure %f\n\n", state.ScalingPressure)

	fmt.Fprintf(w, "# HELP cleat_queue_depth Ready workflows waiting in task queues\n")
	fmt.Fprintf(w, "# TYPE cleat_queue_depth gauge\n")
	fmt.Fprintf(w, "cleat_queue_depth %d\n\n", state.QueueDepth)

	fmt.Fprintf(w, "# HELP cleat_concurrency_limit Current effective concurrency cap\n")
	fmt.Fprintf(w, "# TYPE cleat_concurrency_limit gauge\n")
	fmt.Fprintf(w, "cleat_concurrency_limit %d\n\n", state.DynamicConcurrency)

	fmt.Fprintf(w, "# HELP cleat_desired_concurrency Configured --concurrency value\n")
	fmt.Fprintf(w, "# TYPE cleat_desired_concurrency gauge\n")
	fmt.Fprintf(w, "cleat_desired_concurrency %d\n\n", desired)

	// Per-definition memory estimates with def_name label.
	fmt.Fprintf(w, "# HELP cleat_workflow_memory_estimate_bytes Estimated memory per workflow execution by def_name\n")
	fmt.Fprintf(w, "# TYPE cleat_workflow_memory_estimate_bytes gauge\n")
	for name, estimate := range defEstimates {
		fmt.Fprintf(w, "cleat_workflow_memory_estimate_bytes{def_name=%q} %d\n", name, int(estimate))
	}
}

// updateMemoryMetrics atomically updates all memory metric gauges from the
// given controller state. metricsDesiredConcurrency is not updated here; it
// is set once at startup in main.go.
func updateMemoryMetrics(state MemoryControllerState) {
	atomic.StoreInt64(&metricsMemoryRSS, int64(state.UsedBytes))
	atomic.StoreInt64(&metricsMemoryAvailable, int64(state.AvailableBytes))
	atomic.StoreInt64(&metricsMemoryTotal, int64(state.TotalBytes))
	atomic.StoreInt64(&metricsMemoryPressure, int64(state.Pressure*1000))
	atomic.StoreInt64(&metricsConcurrencyLimit, int64(state.DynamicConcurrency))
	atomic.StoreInt64(&metricsScalingPressure, int64(state.ScalingPressure*1000))
	atomic.StoreInt64(&metricsQueueDepth, state.QueueDepth)
}
