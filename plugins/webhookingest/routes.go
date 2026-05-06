package webhookingest

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/durable/internal/auth"
)

func (p *Plugin) RegisterRoutes(mux *http.ServeMux) error {
	if mux == nil {
		return fmt.Errorf("webhook-ingest: nil mux")
	}
	// Inbound webhook endpoint -- no tenant auth required.
	// Source ID in the URL identifies the tenant.
	mux.HandleFunc("POST /ingest/{source_id}", p.handleIngestWebhook)

	// Management endpoints -- require tenant auth.
	mux.HandleFunc("GET /ingest/sources", p.handleListSources)
	mux.HandleFunc("POST /ingest/sources", p.handleCreateSource)
	mux.HandleFunc("GET /ingest/sources/{id}", p.handleGetSource)
	mux.HandleFunc("DELETE /ingest/sources/{id}", p.handleDeleteSource)
	mux.HandleFunc("GET /ingest/events", p.handleListEvents)
	return nil
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

// tenantID extracts the tenant UUID from the request context. Returns the
// zero UUID if no tenant is set.
func (p *Plugin) tenantID(r *http.Request) uuid.UUID {
	tid, _ := auth.TenantIDFromContext(r.Context())
	return tid
}

// ---- types ----

type webhookSourceJSON struct {
	ID         uuid.UUID `json:"id"`
	TenantID   uuid.UUID `json:"tenant_id"`
	Name       string    `json:"name"`
	SourceType string    `json:"source_type"`
	Secret     string    `json:"secret"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type createSourceRequest struct {
	Name       string `json:"name"`
	SourceType string `json:"source_type"`
	Secret     string `json:"secret,omitempty"`
}

type webhookEventJSON struct {
	ID         uuid.UUID       `json:"id"`
	SourceID   uuid.UUID       `json:"source_id"`
	TenantID   uuid.UUID       `json:"tenant_id"`
	EventType  string          `json:"event_type"`
	Headers    json.RawMessage `json:"headers"`
	Payload    json.RawMessage `json:"payload"`
	ReceivedAt time.Time       `json:"received_at"`
	Processed  bool            `json:"processed"`
}

// ---- POST /ingest/{source_id} ----

func (p *Plugin) handleIngestWebhook(w http.ResponseWriter, r *http.Request) {
	sourceIDStr := r.PathValue("source_id")
	sourceID, err := uuid.Parse(sourceIDStr)
	if err != nil {
		p.writeError(w, 400, "invalid source id")
		return
	}

	// Look up the webhook source.
	var source webhookSourceJSON
	err = p.db.QueryRowContext(r.Context(), `
		SELECT id, tenant_id, name, source_type, secret, enabled, created_at, updated_at
		FROM webhook_sources
		WHERE id = $1
	`, sourceID).Scan(&source.ID, &source.TenantID, &source.Name, &source.SourceType,
		&source.Secret, &source.Enabled, &source.CreatedAt, &source.UpdatedAt)
	if err == sql.ErrNoRows {
		p.writeError(w, 404, "source not found")
		return
	}
	if err != nil {
		p.logger.Error("webhook-ingest: lookup source", "source_id", sourceID, "error", err)
		p.writeError(w, 500, "failed to look up source")
		return
	}

	if !source.Enabled {
		p.writeError(w, 403, "source is disabled")
		return
	}

	// Read the request body.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("webhook-ingest: read body", "error", err)
		p.writeError(w, 500, "failed to read body")
		return
	}
	defer r.Body.Close()

	// Verify HMAC-SHA256 signature if the source has a secret configured.
	if source.Secret != "" {
		sig := r.Header.Get("X-Hub-Signature-256")
		if sig == "" {
			p.writeError(w, 401, "missing signature")
			return
		}
		mac := hmac.New(sha256.New, []byte(source.Secret))
		mac.Write(body)
		expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(expected), []byte(sig)) {
			p.writeError(w, 401, "invalid signature")
			return
		}
	}

	// Store request headers as JSON.
	headersMap := make(map[string]string)
	for k, v := range r.Header {
		headersMap[k] = strings.Join(v, ", ")
	}
	headersJSON, err := json.Marshal(headersMap)
	if err != nil {
		p.logger.Error("webhook-ingest: marshal headers", "error", err)
		p.writeError(w, 500, "failed to encode headers")
		return
	}

	// Store the payload. Try to parse as JSON first; if not valid JSON,
	// wrap it as a JSON string.
	var payloadJSON []byte
	if json.Valid(body) {
		payloadJSON = body
	} else {
		payloadJSON, _ = json.Marshal(string(body))
	}

	eventType := r.Header.Get("X-Github-Event")
	if eventType == "" {
		eventType = r.Header.Get("X-Event-Type")
	}
	if eventType == "" {
		eventType = "webhook"
	}

	eventID := uuid.New()
	now := time.Now()

	_, err = p.db.ExecContext(r.Context(), `
		INSERT INTO webhook_events (id, source_id, tenant_id, event_type, headers, payload, received_at, processed)
		VALUES ($1, $2, $3, $4, $5, $6, $7, false)
	`, eventID, sourceID, source.TenantID, eventType, string(headersJSON), string(payloadJSON), now)
	if err != nil {
		p.logger.Error("webhook-ingest: store event", "error", err)
		p.writeError(w, 500, "failed to store event")
		return
	}

	p.logger.Info("webhook-ingest: event received",
		"event_id", eventID,
		"source_id", sourceID,
		"tenant", source.TenantID,
		"event_type", eventType,
	)

	p.writeJSON(w, 201, map[string]interface{}{
		"id":         eventID,
		"event_type": eventType,
		"received":   true,
	})
}

// ---- GET /ingest/sources ----

func (p *Plugin) handleListSources(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	rows, err := p.db.QueryContext(r.Context(), `
		SELECT id, tenant_id, name, source_type, secret, enabled, created_at, updated_at
		FROM webhook_sources
		WHERE tenant_id = $1
		ORDER BY created_at DESC
	`, tid)
	if err != nil {
		p.logger.Error("webhook-ingest: list sources", "error", err)
		p.writeError(w, 500, "failed to list sources")
		return
	}
	defer rows.Close()

	var sources []webhookSourceJSON
	for rows.Next() {
		var s webhookSourceJSON
		if err := rows.Scan(&s.ID, &s.TenantID, &s.Name, &s.SourceType,
			&s.Secret, &s.Enabled, &s.CreatedAt, &s.UpdatedAt); err != nil {
			p.logger.Error("webhook-ingest: scan source", "error", err)
			continue
		}
		sources = append(sources, s)
	}

	if sources == nil {
		sources = []webhookSourceJSON{}
	}

	p.writeJSON(w, 200, sources)
}

// ---- POST /ingest/sources ----

func (p *Plugin) handleCreateSource(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("webhook-ingest: read body", "error", err)
		p.writeError(w, 500, "failed to read body")
		return
	}
	defer r.Body.Close()

	var req createSourceRequest
	if err := json.Unmarshal(body, &req); err != nil {
		p.writeError(w, 400, "invalid request body")
		return
	}
	if req.Name == "" {
		p.writeError(w, 400, "name is required")
		return
	}
	if req.SourceType == "" {
		req.SourceType = "generic"
	}

	id := uuid.New()
	now := time.Now()

	_, err = p.db.ExecContext(r.Context(), `
		INSERT INTO webhook_sources (tenant_id, id, name, source_type, secret, enabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, true, $6, $6)
	`, tid, id, req.Name, req.SourceType, req.Secret, now)
	if err != nil {
		p.logger.Error("webhook-ingest: create source", "error", err)
		p.writeError(w, 500, "failed to create source")
		return
	}

	// Build the endpoint URL for this source.
	endpointURL := fmt.Sprintf("/ingest/%s", id)

	p.logger.Info("webhook-ingest: source created", "id", id, "tenant", tid)

	p.writeJSON(w, 201, map[string]interface{}{
		"id":           id,
		"tenant_id":    tid,
		"name":         req.Name,
		"source_type":  req.SourceType,
		"secret":       req.Secret,
		"enabled":      true,
		"endpoint_url": endpointURL,
		"created_at":   now,
		"updated_at":   now,
	})
}

// ---- GET /ingest/sources/{id} ----

func (p *Plugin) handleGetSource(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid source id")
		return
	}

	var s webhookSourceJSON
	err = p.db.QueryRowContext(r.Context(), `
		SELECT id, tenant_id, name, source_type, secret, enabled, created_at, updated_at
		FROM webhook_sources
		WHERE id = $1 AND tenant_id = $2
	`, id, tid).Scan(&s.ID, &s.TenantID, &s.Name, &s.SourceType,
		&s.Secret, &s.Enabled, &s.CreatedAt, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		p.writeError(w, 404, "source not found")
		return
	}
	if err != nil {
		p.logger.Error("webhook-ingest: get source", "error", err)
		p.writeError(w, 500, "failed to get source")
		return
	}

	p.writeJSON(w, 200, s)
}

// ---- DELETE /ingest/sources/{id} ----

func (p *Plugin) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid source id")
		return
	}

	result, err := p.db.ExecContext(r.Context(), `
		DELETE FROM webhook_sources
		WHERE id = $1 AND tenant_id = $2
	`, id, tid)
	if err != nil {
		p.logger.Error("webhook-ingest: delete source", "error", err)
		p.writeError(w, 500, "failed to delete source")
		return
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		p.writeError(w, 404, "source not found")
		return
	}

	p.logger.Info("webhook-ingest: source deleted", "id", id, "tenant", tid)
	w.WriteHeader(http.StatusNoContent)
}

// ---- GET /ingest/events ----

func (p *Plugin) handleListEvents(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	query := `
		SELECT id, source_id, tenant_id, event_type, headers, payload, received_at, processed
		FROM webhook_events
		WHERE tenant_id = $1
	`
	args := []interface{}{tid}
	argIdx := 2

	if sourceIDStr := r.URL.Query().Get("source_id"); sourceIDStr != "" {
		sourceID, err := uuid.Parse(sourceIDStr)
		if err == nil {
			query += fmt.Sprintf(" AND source_id = $%d", argIdx)
			args = append(args, sourceID)
			argIdx++
		}
	}

	if eventType := r.URL.Query().Get("event_type"); eventType != "" {
		query += fmt.Sprintf(" AND event_type = $%d", argIdx)
		args = append(args, eventType)
		argIdx++
	}

	if processedStr := r.URL.Query().Get("processed"); processedStr != "" {
		processed := processedStr == "true"
		query += fmt.Sprintf(" AND processed = $%d", argIdx)
		args = append(args, processed)
		argIdx++
	}

	query += " ORDER BY received_at DESC"
	query += fmt.Sprintf(" LIMIT $%d", argIdx)
	args = append(args, 100)

	rows, err := p.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		p.logger.Error("webhook-ingest: list events", "error", err)
		p.writeError(w, 500, "failed to list events")
		return
	}
	defer rows.Close()

	var events []webhookEventJSON
	for rows.Next() {
		var (
			e          webhookEventJSON
			headersRaw []byte
			payloadRaw []byte
		)
		if err := rows.Scan(
			&e.ID, &e.SourceID, &e.TenantID, &e.EventType,
			&headersRaw, &payloadRaw, &e.ReceivedAt, &e.Processed,
		); err != nil {
			p.logger.Error("webhook-ingest: scan event", "error", err)
			continue
		}
		e.Headers = json.RawMessage(headersRaw)
		e.Payload = json.RawMessage(payloadRaw)
		events = append(events, e)
	}

	if events == nil {
		events = []webhookEventJSON{}
	}

	p.writeJSON(w, 200, events)
}
