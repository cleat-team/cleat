package backendkit

import (
	"log/slog"
	"net/http"
	"time"
)

// CORSMiddleware adds CORS headers with configurable allowed origins.
// Pass "*" to allow all origins.
func CORSMiddleware(allowedOrigins ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			allowAll := false
			allowed := origin == ""
			for _, ao := range allowedOrigins {
				if ao == "*" {
					allowAll = true
					allowed = true
					break
				}
				if ao == origin {
					allowed = true
					break
				}
			}
			if allowAll {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if allowed && origin != "" {
				w.Header().Set("Access-Control-Allow-Origin", origin)
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Tenant-ID")

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func newResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Flush delegates to the real writer's http.Flusher; embedding http.ResponseWriter does not promote it,
// so without this a streaming handler behind LoggingMiddleware is told streaming is unsupported.
// cleat#2254.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer.
func (rw *responseWriter) Unwrap() http.ResponseWriter { return rw.ResponseWriter }

// LoggingMiddleware logs each request as structured key=value pairs via slog.
// Pass nil for logger to use slog.Default().
func LoggingMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := newResponseWriter(w)
			next.ServeHTTP(rw, r)
			logger.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rw.statusCode,
				"duration", time.Since(start).Round(time.Microsecond).String(),
				"request_id", rw.Header().Get("X-Request-ID"),
			)
		})
	}
}
