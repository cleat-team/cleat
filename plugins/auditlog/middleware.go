package auditlog

import (
	"context"
	"net/http"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/google/uuid"
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

// recordAudit writes one audit event now, with a single attempt: the queue-less path a Plugin
// built directly uses, and what tests call to put rows on a chain. A failure is counted and
// logged (lose), not swallowed.
func (p *Plugin) recordAudit(ctx context.Context, tenantID uuid.UUID, userID, method, path string, statusCode int, ipAddress, userAgent string, duration time.Duration) {
	p.recordOnce(newQueuedEvent(tenantID, userID, method, path, statusCode, ipAddress, userAgent, duration))
}
