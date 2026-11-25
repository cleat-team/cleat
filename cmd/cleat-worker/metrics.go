package main

import (
	"context"
	"net/http"

	"github.com/cleat-team/cleat/monitoring/prometheus"
)

// handleMetrics serves Prometheus-format metrics via the OTel-based metrics
// subsystem. It delegates to the global worker's Metrics instance, which was
// created during startup and wired into the Worker struct.
func handleMetrics(w http.ResponseWriter, r *http.Request) {
	if globalWorker != nil && globalWorker.Metrics != nil {
		globalWorker.Metrics.ServeHTTP().ServeHTTP(w, r)
		return
	}
	http.Error(w, "metrics not available", http.StatusServiceUnavailable)
}

// startInFlightSeriesAtZero records a 0 for cleat_workflows_active, the gauge of runs executing now, so a worker that has not run anything yet reports 0 and not nothing.
// The exporter writes an instrument out only after its first measurement, and on a worker that had run nothing an operator could not tell "idle" from "the exporter is broken"
// (cleat#4474). The series carries workflow_name="" because the gauge is labelled by definition name and the worker cannot know any yet; the first run adds its own series beside it.
// The residency series are started at zero by engine.NewResidentSet, which owns their label values.
func startInFlightSeriesAtZero(m *prometheus.Metrics) {
	if m == nil {
		return
	}
	m.AddWorkflowActive(context.Background(), 0, "")
}
