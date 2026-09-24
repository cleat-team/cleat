package auditlog

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
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

// recordAudit inserts a single audit event into the database.
func (p *Plugin) recordAudit(ctx context.Context, tenantID uuid.UUID, userID, method, path string, statusCode int, ipAddress, userAgent string, duration time.Duration) {
	if p.db == nil {
		return
	}

	// Use a background context with a short timeout so the goroutine
	// does not hang if the request context is already cancelled.
	//
	// THE TENANT HAS TO BE PUT BACK. Deriving from context.Background() is
	// deliberate and correct -- an audit row must be written even if the
	// request it describes was cancelled -- but it discards everything the
	// request context carried, including the tenant. Once audit_events has a
	// row-level policy (migrations.go v2, cleat#1278) an insert with no tenant
	// is refused outright:
	//
	//	cleat.tenant_id is not set -- tenant context required for RLS-scoped query
	//
	// tenantID is a parameter of this function, so nothing has to be looked up
	// or plumbed: the value was in hand the whole time and only the carrier was
	// lost. plugin.ForTenant is the API for exactly that -- NOT
	// plugin.AcrossAllTenants, which would pass every test here and silently
	// disable isolation for every audit write. See the contrast in
	// plugin/crosstenant.go.
	insertCtx, cancel := context.WithTimeout(plugin.ForTenant(context.Background(), tenantID), 5*time.Second)
	defer cancel()

	durationMs := int(duration.Milliseconds())

	// The insert is a chain append (chain_store.go): it needs the tenant's previous
	// hash, so it cannot be a bare INSERT any more. The id is still supplied by the
	// application rather than defaulted by the database, for the reason cleat#958
	// gave -- MySQL has no default for a CHAR(36) key, and every insert there failed
	// with `Error 1364 (HY000): Field 'id' doesn't have a default value` while the
	// error was logged and swallowed and the only symptom was an empty table, which
	// reads as evidence that nothing happened. A hash over the row needs the id
	// before the insert in any case.
	err := p.appendChained(insertCtx, chainEvent{
		tenantID: tenantID, userID: userID, method: method, path: path,
		statusCode: statusCode, ipAddress: ipAddress, userAgent: userAgent, durationMs: durationMs,
	})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		p.logger.Error("audit-log: record event", "error", err)
	}
}

// enqueueAudit enqueues an audit event for deferred writing.
// If the buffer is nil (backward compat with direct construction),
// it falls back to calling recordAudit synchronously.
func (p *Plugin) enqueueAudit(tenantID uuid.UUID, userID, method, path string, statusCode int, ipAddress, userAgent string, duration time.Duration) {
	if p.buffer == nil {
		p.recordAudit(context.Background(), tenantID, userID, method, path, statusCode, ipAddress, userAgent, duration)
		return
	}
	select {
	case p.buffer <- queuedAuditEvent{
		tenantID:   tenantID,
		userID:     userID,
		method:     method,
		path:       path,
		statusCode: statusCode,
		ipAddress:  ipAddress,
		userAgent:  userAgent,
		duration:   duration,
	}:
	default:
		p.logger.Warn("audit-log: buffer full, dropping event")
	}
}
