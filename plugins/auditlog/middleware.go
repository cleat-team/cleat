package auditlog

import (
	"context"
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
		// The infrastructure paths (/healthz, /livez, /readyz, /metrics: auth.IsInfrastructurePath) are
		// not recorded. They are polled by kubelets, load balancers and scrapers with no credential, every
		// few seconds, from every replica, so they would be most of the log and each row costs a chain
		// append under the tenant's head lock. The list is fixed and not configurable, and it is the same
		// list that is answered without authentication, so a path cannot be exempt from one and not the
		// other. /api/admin/health needs a credential and IS recorded. cleat#2007.
		if auth.IsInfrastructurePath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// An operator acts ON a tenant, it is not one, so its row goes on the chain of the tenant
		// the HANDLER says it acted on -- the tenant's owner can then read, in their own
		// tamper-evident log, who acted on them -- and otherwise on the default tenant's, where
		// the operator:<KeyID> subject tells it apart. cleat#3559; see auth.AuditTarget for why
		// the handler is asked rather than the path.
		//
		// A tenant already in the context is IGNORED for an operator request, not preferred. An
		// operator request has none, so one that is present was put there by something that does
		// not authenticate (the header: tenant resolver reads a client-supplied header outside the
		// auth chain), and trusting it would let a caller choose whose chain the row lands on.
		// A handler that acts on that tenant says so itself, after confirming the target exists.
		var operatorTarget *auth.AuditTarget
		if _, isOperator := auth.OperatorFromContext(r.Context()); isOperator {
			var ctx context.Context
			ctx, operatorTarget = auth.WithAuditTarget(r.Context())
			r = r.WithContext(ctx)
		}
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rw, r)
		duration := time.Since(start)

		tid, _ := auth.TenantIDFromContext(r.Context())
		if operatorTarget != nil {
			tid, _ = operatorTarget.Tenant() // the nil UUID, the default tenant's, when none was noted
		}
		// ok is deliberately ignored: an unauthenticated request has no
		// subject to record, and that is a fact worth an empty user_id, not a
		// reason to refuse the request or fail this write. cleat#1881.
		userID, _ := auth.SubjectFromContext(r.Context())
		p.enqueueAudit(tid, userID, r.Method, r.URL.Path, rw.statusCode, r.RemoteAddr, r.UserAgent(), duration)
	})
}
