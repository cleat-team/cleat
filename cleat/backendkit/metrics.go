package backendkit

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cleat_apps_http_requests_total",
			Help: "Total number of HTTP requests handled by the backend.",
		},
		[]string{"method", "path", "status"},
	)
	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "cleat_apps_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path"},
	)
)

func init() {
	prometheus.MustRegister(httpRequestsTotal, httpRequestDuration)
}

// MetricsMiddleware records request counts and durations for Prometheus.
// URL paths are normalized to avoid PII leakage and label cardinality explosion.
func MetricsMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := newResponseWriter(w)
			next.ServeHTTP(rw, r)

			path := normalizePath(r.URL.Path)
			httpRequestsTotal.WithLabelValues(r.Method, path, strconv.Itoa(rw.statusCode)).Inc()
			httpRequestDuration.WithLabelValues(r.Method, path).Observe(time.Since(start).Seconds())
		})
	}
}

// MetricsHandler returns an http.Handler that serves Prometheus metrics.
func MetricsHandler() http.Handler {
	return promhttp.Handler()
}

// normalizePath replaces ID segments in the URL path with ":id" to prevent
// Prometheus label cardinality explosion and PII leakage.
//
// Recognized ID patterns:
//   - UUIDs (8-4-4-4-12 hex format)
//   - Stripe IDs (cus_*, sub_*, in_*, pi_*, ch_*, cs_*, pm_*, po_*)
func normalizePath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	changed := false
	for i, p := range parts {
		if isID(p) {
			parts[i] = ":id"
			changed = true
		}
	}
	if !changed {
		return path
	}
	return "/" + strings.Join(parts, "/")
}

func isID(s string) bool {
	if len(s) == 36 && s[8] == '-' && s[13] == '-' && s[18] == '-' && s[23] == '-' {
		return true
	}
	for _, prefix := range []string{"cus_", "sub_", "in_", "pi_", "ch_", "cs_", "pm_", "po_"} {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
