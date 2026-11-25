package main

import (
	"net/http"
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
