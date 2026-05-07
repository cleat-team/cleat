package auditlog

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/cleat/internal/auth"
)

// auditEvent represents a single audit event for JSON serialisation.
type auditEvent struct {
	ID         uuid.UUID              `json:"id"`
	TenantID   uuid.UUID              `json:"tenant_id"`
	Timestamp  time.Time              `json:"timestamp"`
	Method     string                 `json:"method"`
	Path       string                 `json:"path"`
	StatusCode int                    `json:"status_code"`
	UserID     string                 `json:"user_id"`
	IPAddress  string                 `json:"ip_address"`
	UserAgent  string                 `json:"user_agent"`
	DurationMs int                    `json:"duration_ms"`
	Metadata   map[string]interface{} `json:"metadata"`
}

// handleQueryEvents handles GET /audit/events.
func (p *Plugin) handleQueryEvents(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, http.StatusUnauthorized, "tenant required")
		return
	}

	q := r.URL.Query()

	// Build dynamic query.
	query := `
		SELECT id, tenant_id, timestamp, method, path, status_code, user_id,
		       ip_address, user_agent, duration_ms, metadata
		FROM audit_events
		WHERE tenant_id = $1
	`
	args := []interface{}{tid}
	argIdx := 2

	if method := q.Get("method"); method != "" {
		query += fmt.Sprintf(" AND method = $%d", argIdx)
		args = append(args, method)
		argIdx++
	}
	if path := q.Get("path"); path != "" {
		query += fmt.Sprintf(" AND path = $%d", argIdx)
		args = append(args, path)
		argIdx++
	}
	if statusStr := q.Get("status"); statusStr != "" {
		if status, err := strconv.Atoi(statusStr); err == nil {
			query += fmt.Sprintf(" AND status_code = $%d", argIdx)
			args = append(args, status)
			argIdx++
		}
	}
	if fromStr := q.Get("from"); fromStr != "" {
		if from, err := time.Parse(time.RFC3339, fromStr); err == nil {
			query += fmt.Sprintf(" AND timestamp >= $%d", argIdx)
			args = append(args, from)
			argIdx++
		}
	}
	if toStr := q.Get("to"); toStr != "" {
		if to, err := time.Parse(time.RFC3339, toStr); err == nil {
			query += fmt.Sprintf(" AND timestamp <= $%d", argIdx)
			args = append(args, to)
			argIdx++
		}
	}

	limit := 100
	if limitStr := q.Get("limit"); limitStr != "" {
		if v, err := strconv.Atoi(limitStr); err == nil && v > 0 {
			limit = v
			if limit > 1000 {
				limit = 1000
			}
		}
	}
	query += " ORDER BY timestamp DESC"
	query += fmt.Sprintf(" LIMIT $%d", argIdx)
	args = append(args, limit)

	rows, err := p.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		p.logger.Error("audit-log: query events", "error", err)
		p.writeError(w, http.StatusInternalServerError, "failed to query audit events")
		return
	}
	defer rows.Close()

	events := make([]auditEvent, 0)
	for rows.Next() {
		var e auditEvent
		var metadataJSON []byte
		if err := rows.Scan(
			&e.ID, &e.TenantID, &e.Timestamp, &e.Method, &e.Path,
			&e.StatusCode, &e.UserID, &e.IPAddress, &e.UserAgent,
			&e.DurationMs, &metadataJSON,
		); err != nil {
			p.logger.Error("audit-log: scan event", "error", err)
			continue
		}
		json.Unmarshal(metadataJSON, &e.Metadata)
		events = append(events, e)
	}

	p.writeJSON(w, http.StatusOK, events)
}

// ---- helpers ----

func (p *Plugin) writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (p *Plugin) writeError(w http.ResponseWriter, status int, msg string) {
	p.writeJSON(w, status, map[string]string{"error": msg})
}

// tenantID extracts the tenant UUID from the request context.
func (p *Plugin) tenantID(r *http.Request) uuid.UUID {
	tid, _ := auth.TenantIDFromContext(r.Context())
	return tid
}
