package backendkit

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cleat-team/cleat/monitoring/prometheus"
)

// MetricsMiddleware records request counts and durations via OpenTelemetry.
// URL paths are normalized to avoid PII leakage and label cardinality explosion.
// If metrics is nil, the middleware is a no-op.
func MetricsMiddleware(metrics *prometheus.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := newResponseWriter(w)
			next.ServeHTTP(rw, r)

			path := normalizePath(r.URL.Path)
			if metrics != nil {
				metrics.RecordHTTPRequest(r.Context(), r.Method, path, strconv.Itoa(rw.statusCode))
				metrics.RecordHTTPRequestDuration(r.Context(), time.Since(start), r.Method, path)
			}
		})
	}
}

// MetricsHandler returns an http.Handler that serves Prometheus metrics via OTel.
func MetricsHandler(metrics *prometheus.Metrics) http.Handler {
	return metrics.ServeHTTP()
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
