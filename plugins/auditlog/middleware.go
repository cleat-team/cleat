package auditlog

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/cleat-team/cleat/internal/auth"
	"github.com/cleat-team/cleat/internal/plugin"
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
		p.enqueueAudit(tid, r.Method, r.URL.Path, rw.statusCode, r.RemoteAddr, r.UserAgent(), duration)
	})
}

// recordAudit inserts a single audit event into the database.
func (p *Plugin) recordAudit(ctx context.Context, tenantID uuid.UUID, method, path string, statusCode int, ipAddress, userAgent string, duration time.Duration) {
	if p.db == nil {
		return
	}

	// Use a background context with a short timeout so the goroutine
	// does not hang if the request context is already cancelled.
	insertCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	durationMs := int(duration.Milliseconds())

	_, err := p.db.Exec(insertCtx, plugin.Rebind(`
			INSERT INTO audit_events (tenant_id, method, path, status_code, user_id, ip_address, user_agent, duration_ms)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, p.dialect), tenantID, method, path, statusCode, "", ipAddress, userAgent, durationMs)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		p.logger.Error("audit-log: record event", "error", err)
	}
}

// enqueueAudit enqueues an audit event for deferred writing.
// If the buffer is nil (backward compat with direct construction),
// it falls back to calling recordAudit synchronously.
func (p *Plugin) enqueueAudit(tenantID uuid.UUID, method, path string, statusCode int, ipAddress, userAgent string, duration time.Duration) {
	if p.buffer == nil {
		p.recordAudit(context.Background(), tenantID, method, path, statusCode, ipAddress, userAgent, duration)
		return
	}
	select {
	case p.buffer <- queuedAuditEvent{
		tenantID:   tenantID,
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
