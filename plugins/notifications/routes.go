package notifications

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/cleat/internal/auth"
	"github.com/rcownie/cleat/internal/plugin"
)

func (p *Plugin) RegisterRoutes(mux *http.ServeMux) error {
	if mux == nil {
		return fmt.Errorf("notifications: nil mux")
	}
	mux.HandleFunc("POST /webhooks", p.handleCreateWebhook)
	mux.HandleFunc("GET /webhooks", p.handleListWebhooks)
	mux.HandleFunc("GET /webhooks/{id}", p.handleGetWebhook)
	mux.HandleFunc("PUT /webhooks/{id}", p.handleUpdateWebhook)
	mux.HandleFunc("DELETE /webhooks/{id}", p.handleDeleteWebhook)
	mux.HandleFunc("GET /webhooks/{id}/deliveries", p.handleListDeliveries)
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

type webhookConfigJSON struct {
	ID        uuid.UUID              `json:"id"`
	TenantID  uuid.UUID              `json:"tenant_id"`
	URL       string                 `json:"url"`
	Secret    string                 `json:"secret"`
	Events    []string               `json:"events"`
	Enabled   bool                   `json:"enabled"`
	CreatedAt time.Time              `json:"created_at"`
	UpdatedAt time.Time              `json:"updated_at"`
}

type createWebhookRequest struct {
	URL    string   `json:"url"`
	Secret string   `json:"secret,omitempty"`
	Events []string `json:"events"`
}

type updateWebhookRequest struct {
	URL     *string   `json:"url,omitempty"`
	Secret  *string   `json:"secret,omitempty"`
	Events  *[]string `json:"events,omitempty"`
	Enabled *bool     `json:"enabled,omitempty"`
}

type deliveryJSON struct {
	ID            uuid.UUID       `json:"id"`
	WebhookID     uuid.UUID       `json:"webhook_id"`
	EventType     string          `json:"event_type"`
	Payload       json.RawMessage `json:"payload"`
	Status        string          `json:"status"`
	AttemptCount  int             `json:"attempt_count"`
	LastAttemptAt *time.Time      `json:"last_attempt_at,omitempty"`
	NextAttemptAt *time.Time      `json:"next_attempt_at,omitempty"`
	DeliveredAt   *time.Time      `json:"delivered_at,omitempty"`
	ResponseCode  *int            `json:"response_code,omitempty"`
	ResponseBody  *string         `json:"response_body,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

// ---- POST /webhooks ----

func (p *Plugin) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("notifications: read body", "error", err)
		p.writeError(w, 500, "failed to read body")
		return
	}
	defer r.Body.Close()

	var req createWebhookRequest
	if err := json.Unmarshal(body, &req); err != nil {
		p.writeError(w, 400, "invalid request body")
		return
	}
	if req.URL == "" {
		p.writeError(w, 400, "url is required")
		return
	}
	if req.Events == nil {
		req.Events = []string{}
	}

	eventsJSON, err := json.Marshal(req.Events)
	if err != nil {
		p.logger.Error("notifications: marshal events", "error", err)
		p.writeError(w, 500, "failed to encode events")
		return
	}

	id := uuid.New()
	now := time.Now()

	_, err = p.db.Exec(r.Context(), plugin.Rebind(`
			INSERT INTO webhook_config (tenant_id, id, url, secret, events, enabled, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, true, $6, $6)
		`, p.dialect), tid, id, req.URL, req.Secret, string(eventsJSON), now)
	if err != nil {
		p.logger.Error("notifications: create webhook", "error", err)
		p.writeError(w, 500, "failed to create webhook")
		return
	}

	p.logger.Info("notifications: webhook created", "id", id, "tenant", tid)

	p.writeJSON(w, 201, webhookConfigJSON{
		ID:        id,
		TenantID:  tid,
		URL:       req.URL,
		Secret:    req.Secret,
		Events:    req.Events,
		Enabled:   true,
		CreatedAt: now,
		UpdatedAt: now,
	})
}

// ---- GET /webhooks ----

func (p *Plugin) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	rows, err := p.db.Query(r.Context(), plugin.Rebind(`
			SELECT id, url, secret, events, enabled, created_at, updated_at
			FROM webhook_config
			WHERE tenant_id = $1
			ORDER BY created_at DESC
		`, p.dialect), tid)
	if err != nil {
		p.logger.Error("notifications: list webhooks", "error", err)
		p.writeError(w, 500, "failed to list webhooks")
		return
	}
	defer rows.Close()

	var configs []webhookConfigJSON
	for rows.Next() {
		var (
			c         webhookConfigJSON
			eventsRaw []byte
		)
		if err := rows.Scan(&c.ID, &c.URL, &c.Secret, &eventsRaw, &c.Enabled, &c.CreatedAt, &c.UpdatedAt); err != nil {
			p.logger.Error("notifications: scan webhook", "error", err)
			continue
		}
		c.TenantID = tid
		json.Unmarshal(eventsRaw, &c.Events)
		configs = append(configs, c)
	}

	if configs == nil {
		configs = []webhookConfigJSON{}
	}

	p.writeJSON(w, 200, configs)
}

// ---- GET /webhooks/{id} ----

func (p *Plugin) handleGetWebhook(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid webhook id")
		return
	}

	var (
		c         webhookConfigJSON
		eventsRaw []byte
	)
	err = p.db.QueryRow(r.Context(), plugin.Rebind(`
			SELECT id, url, secret, events, enabled, created_at, updated_at
			FROM webhook_config
			WHERE id = $1 AND tenant_id = $2
		`, p.dialect), id, tid).Scan(&c.ID, &c.URL, &c.Secret, &eventsRaw, &c.Enabled, &c.CreatedAt, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		p.writeError(w, 404, "webhook not found")
		return
	}
	if err != nil {
		p.logger.Error("notifications: get webhook", "error", err)
		p.writeError(w, 500, "failed to get webhook")
		return
	}

	c.TenantID = tid
	json.Unmarshal(eventsRaw, &c.Events)

	p.writeJSON(w, 200, c)
}

// ---- PUT /webhooks/{id} ----

func (p *Plugin) handleUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid webhook id")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("notifications: read body", "error", err)
		p.writeError(w, 500, "failed to read body")
		return
	}
	defer r.Body.Close()

	var req updateWebhookRequest
	if err := json.Unmarshal(body, &req); err != nil {
		p.writeError(w, 400, "invalid request body")
		return
	}

	// Build dynamic UPDATE query for the fields that are present.
	setClauses := []string{}
	args := []interface{}{}
	argIdx := 1

	if req.URL != nil {
		setClauses = append(setClauses, fmt.Sprintf("url = $%d", argIdx))
		args = append(args, *req.URL)
		argIdx++
	}
	if req.Secret != nil {
		setClauses = append(setClauses, fmt.Sprintf("secret = $%d", argIdx))
		args = append(args, *req.Secret)
		argIdx++
	}
	if req.Events != nil {
		eventsJSON, err := json.Marshal(*req.Events)
		if err != nil {
			p.logger.Error("notifications: marshal events", "error", err)
			p.writeError(w, 500, "failed to encode events")
			return
		}
		setClauses = append(setClauses, fmt.Sprintf("events = $%d", argIdx))
		args = append(args, string(eventsJSON))
		argIdx++
	}
	if req.Enabled != nil {
		setClauses = append(setClauses, fmt.Sprintf("enabled = $%d", argIdx))
		args = append(args, *req.Enabled)
		argIdx++
	}

	if len(setClauses) == 0 {
		p.writeError(w, 400, "no fields to update")
		return
	}

	setClauses = append(setClauses, "updated_at = now()")
	args = append(args, id, tid)

	query := fmt.Sprintf(`
			UPDATE webhook_config
			SET %s
			WHERE id = $%d AND tenant_id = $%d
		`, joinSetClauses(setClauses), argIdx, argIdx+1)

	rows, err := p.db.Exec(r.Context(), plugin.Rebind(query, p.dialect), args...)
	if err != nil {
		p.logger.Error("notifications: update webhook", "error", err)
		p.writeError(w, 500, "failed to update webhook")
		return
	}
	if rows == 0 {
		p.writeError(w, 404, "webhook not found")
		return
	}

	// Return the updated webhook config.
	var (
		c         webhookConfigJSON
		eventsRaw []byte
	)
	err = p.db.QueryRow(r.Context(), plugin.Rebind(`
			SELECT id, url, secret, events, enabled, created_at, updated_at
			FROM webhook_config
			WHERE id = $1 AND tenant_id = $2
		`, p.dialect), id, tid).Scan(&c.ID, &c.URL, &c.Secret, &eventsRaw, &c.Enabled, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		p.logger.Error("notifications: re-fetch webhook", "error", err)
		p.writeError(w, 500, "failed to retrieve updated webhook")
		return
	}

	c.TenantID = tid
	json.Unmarshal(eventsRaw, &c.Events)

	p.writeJSON(w, 200, c)
}

// ---- DELETE /webhooks/{id} ----

func (p *Plugin) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid webhook id")
		return
	}

	rows, err := p.db.Exec(r.Context(), plugin.Rebind(`
			DELETE FROM webhook_config
			WHERE id = $1 AND tenant_id = $2
		`, p.dialect), id, tid)
	if err != nil {
		p.logger.Error("notifications: delete webhook", "error", err)
		p.writeError(w, 500, "failed to delete webhook")
		return
	}
	if rows == 0 {
		p.writeError(w, 404, "webhook not found")
		return
	}

	p.logger.Info("notifications: webhook deleted", "id", id, "tenant", tid)
	w.WriteHeader(http.StatusNoContent)
}

// ---- GET /webhooks/{id}/deliveries ----

func (p *Plugin) handleListDeliveries(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	webhookID, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid webhook id")
		return
	}

	// Verify the webhook belongs to the tenant.
	var exists bool
	err = p.db.QueryRow(r.Context(), plugin.Rebind(`
			SELECT EXISTS(SELECT 1 FROM webhook_config WHERE id = $1 AND tenant_id = $2)
		`, p.dialect), webhookID, tid).Scan(&exists)
	if err != nil {
		p.logger.Error("notifications: verify webhook", "error", err)
		p.writeError(w, 500, "failed to verify webhook")
		return
	}
	if !exists {
		p.writeError(w, 404, "webhook not found")
		return
	}

	query := `
			SELECT id, webhook_id, event_type, payload, status, attempt_count,
			       last_attempt_at, next_attempt_at, delivered_at,
			       response_code, response_body, created_at
			FROM webhook_delivery
			WHERE webhook_id = $1
		`
	args := []interface{}{webhookID}
	argIdx := 2

	if statusFilter := r.URL.Query().Get("status"); statusFilter != "" {
		query += fmt.Sprintf(" AND status = $%d", argIdx)
		args = append(args, statusFilter)
		argIdx++
	}

	query += " ORDER BY created_at DESC"
	query += fmt.Sprintf(" LIMIT $%d", argIdx)
	args = append(args, 100)

	rows, err := p.db.Query(r.Context(), plugin.Rebind(query, p.dialect), args...)
	if err != nil {
		p.logger.Error("notifications: list deliveries", "error", err)
		p.writeError(w, 500, "failed to list deliveries")
		return
	}
	defer rows.Close()

	var deliveries []deliveryJSON
	for rows.Next() {
		var (
			d             deliveryJSON
			payloadRaw    []byte
			lastAttemptAt sql.NullTime
			nextAttemptAt sql.NullTime
			deliveredAt   sql.NullTime
			responseCode  sql.NullInt64
			responseBody  sql.NullString
		)
		if err := rows.Scan(
			&d.ID, &d.WebhookID, &d.EventType, &payloadRaw,
			&d.Status, &d.AttemptCount,
			&lastAttemptAt, &nextAttemptAt, &deliveredAt,
			&responseCode, &responseBody, &d.CreatedAt,
		); err != nil {
			p.logger.Error("notifications: scan delivery", "error", err)
			continue
		}
		d.Payload = json.RawMessage(payloadRaw)
		if lastAttemptAt.Valid {
			d.LastAttemptAt = &lastAttemptAt.Time
		}
		if nextAttemptAt.Valid {
			d.NextAttemptAt = &nextAttemptAt.Time
		}
		if deliveredAt.Valid {
			d.DeliveredAt = &deliveredAt.Time
		}
		if responseCode.Valid {
			v := int(responseCode.Int64)
			d.ResponseCode = &v
		}
		if responseBody.Valid {
			d.ResponseBody = &responseBody.String
		}
		deliveries = append(deliveries, d)
	}

	if deliveries == nil {
		deliveries = []deliveryJSON{}
	}

	p.writeJSON(w, 200, deliveries)
}

// joinSetClauses joins SET clause fragments with ", ".
func joinSetClauses(clauses []string) string {
	result := ""
	for i, c := range clauses {
		if i > 0 {
			result += ", "
		}
		result += c
	}
	return result
}
