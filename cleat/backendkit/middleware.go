package backendkit

import (
	"log"
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
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

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

// LoggingMiddleware logs each request with method, path, status code, and duration.
func LoggingMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := newResponseWriter(w)
			next.ServeHTTP(rw, r)
			log.Printf("%s %s %d %s",
				r.Method,
				r.URL.Path,
				rw.statusCode,
				time.Since(start).Round(time.Microsecond),
			)
		})
	}
}
