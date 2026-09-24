package auditlog

import (
	"net/http"
	"time"

	"github.com/cleat-team/cleat/auth"
)

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
	written    bool
}

func (rw *responseWriter) WriteHeader(code int) {
	if !rw.written {
		rw.statusCode = code
		rw.written = true
		rw.ResponseWriter.WriteHeader(code)
	}
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.written {
		rw.WriteHeader(http.StatusOK)
	}
	return rw.ResponseWriter.Write(b)
}

// Flush delegates to the real writer's http.Flusher. Embedding http.ResponseWriter promotes only that
// interface's methods, so without this every handler behind this middleware that does
// `w.(http.Flusher)` (the workflow /stream route, eventstore's SSE, the audit export) was told
// streaming is not supported. A flush before any write sends an implicit 200, so it counts as the
// header having been written; statusCode already defaults to 200. cleat#2254.
func (rw *responseWriter) Flush() {
	rw.written = true
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer (SetWriteDeadline, EnableFullDuplex).
func (rw *responseWriter) Unwrap() http.ResponseWriter { return rw.ResponseWriter }

// Middleware wraps every request and records an audit event.
func (p *Plugin) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rw, r)
		duration := time.Since(start)

		tid, _ := auth.TenantIDFromContext(r.Context())
		// ok is deliberately ignored: an unauthenticated request has no
		// subject to record, and that is a fact worth an empty user_id, not a
		// reason to refuse the request or fail this write. cleat#1881.
		userID, _ := auth.SubjectFromContext(r.Context())
		p.enqueueAudit(tid, userID, r.Method, r.URL.Path, rw.statusCode, r.RemoteAddr, r.UserAgent(), duration)
	})
}
