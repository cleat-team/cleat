package slacknotify

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/cleat/internal/auth"
)

func (p *Plugin) RegisterRoutes(mux *http.ServeMux) error {
	if mux == nil {
		return fmt.Errorf("slack-notify: nil mux")
	}
	mux.HandleFunc("POST /slack/configs", p.handleCreateConfig)
	mux.HandleFunc("GET /slack/configs", p.handleListConfigs)
	mux.HandleFunc("GET /slack/configs/{id}", p.handleGetConfig)
	mux.HandleFunc("PUT /slack/configs/{id}", p.handleUpdateConfig)
	mux.HandleFunc("DELETE /slack/configs/{id}", p.handleDeleteConfig)
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

type slackConfigJSON struct {
	ID             uuid.UUID  `json:"id"`
	TenantID       uuid.UUID  `json:"tenant_id"`
	Name           string     `json:"name"`
	WebhookURL     string     `json:"webhook_url"`
	DefaultChannel *string    `json:"default_channel,omitempty"`
	Enabled        bool       `json:"enabled"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type createConfigRequest struct {
	Name           string  `json:"name"`
	WebhookURL     string  `json:"webhook_url"`
	DefaultChannel *string `json:"default_channel,omitempty"`
}

type updateConfigRequest struct {
	Name           *string `json:"name,omitempty"`
	WebhookURL     *string `json:"webhook_url,omitempty"`
	DefaultChannel *string `json:"default_channel,omitempty"` // nil = no change; pointer to empty string = clear
	Enabled        *bool   `json:"enabled,omitempty"`
}

// ---- POST /slack/configs ----

func (p *Plugin) handleCreateConfig(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("slack-notify: read body", "error", err)
		p.writeError(w, 500, "failed to read body")
		return
	}
	defer r.Body.Close()

	var req createConfigRequest
	if err := json.Unmarshal(body, &req); err != nil {
		p.writeError(w, 400, "invalid request body")
		return
	}
	if req.Name == "" {
		p.writeError(w, 400, "name is required")
		return
	}
	if req.WebhookURL == "" {
		p.writeError(w, 400, "webhook_url is required")
		return
	}

	id := uuid.New()
	now := time.Now()

	_, err = p.db.ExecContext(r.Context(), `
		INSERT INTO slack_config (tenant_id, id, name, webhook_url, default_channel, enabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, true, $6, $6)
	`, tid, id, req.Name, req.WebhookURL, req.DefaultChannel, now)
	if err != nil {
		p.logger.Error("slack-notify: create config", "error", err)
		p.writeError(w, 500, "failed to create config")
		return
	}

	p.logger.Info("slack-notify: config created", "id", id, "tenant", tid)

	p.writeJSON(w, 201, slackConfigJSON{
		ID:             id,
		TenantID:       tid,
		Name:           req.Name,
		WebhookURL:     req.WebhookURL,
		DefaultChannel: req.DefaultChannel,
		Enabled:        true,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
}

// ---- GET /slack/configs ----

func (p *Plugin) handleListConfigs(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	rows, err := p.db.QueryContext(r.Context(), `
		SELECT id, name, webhook_url, default_channel, enabled, created_at, updated_at
		FROM slack_config
		WHERE tenant_id = $1
		ORDER BY created_at DESC
	`, tid)
	if err != nil {
		p.logger.Error("slack-notify: list configs", "error", err)
		p.writeError(w, 500, "failed to list configs")
		return
	}
	defer rows.Close()

	var configs []slackConfigJSON
	for rows.Next() {
		var c slackConfigJSON
		if err := rows.Scan(&c.ID, &c.Name, &c.WebhookURL, &c.DefaultChannel, &c.Enabled, &c.CreatedAt, &c.UpdatedAt); err != nil {
			p.logger.Error("slack-notify: scan config", "error", err)
			continue
		}
		c.TenantID = tid
		configs = append(configs, c)
	}

	if configs == nil {
		configs = []slackConfigJSON{}
	}

	p.writeJSON(w, 200, configs)
}

// ---- GET /slack/configs/{id} ----

func (p *Plugin) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid config id")
		return
	}

	var c slackConfigJSON
	err = p.db.QueryRowContext(r.Context(), `
		SELECT id, name, webhook_url, default_channel, enabled, created_at, updated_at
		FROM slack_config
		WHERE id = $1 AND tenant_id = $2
	`, id, tid).Scan(&c.ID, &c.Name, &c.WebhookURL, &c.DefaultChannel, &c.Enabled, &c.CreatedAt, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		p.writeError(w, 404, "config not found")
		return
	}
	if err != nil {
		p.logger.Error("slack-notify: get config", "error", err)
		p.writeError(w, 500, "failed to get config")
		return
	}

	c.TenantID = tid
	p.writeJSON(w, 200, c)
}

// ---- PUT /slack/configs/{id} ----

func (p *Plugin) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid config id")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("slack-notify: read body", "error", err)
		p.writeError(w, 500, "failed to read body")
		return
	}
	defer r.Body.Close()

	var req updateConfigRequest
	if err := json.Unmarshal(body, &req); err != nil {
		p.writeError(w, 400, "invalid request body")
		return
	}

	// Build dynamic UPDATE query for the fields that are present.
	setClauses := []string{}
	args := []interface{}{}
	argIdx := 1

	if req.Name != nil {
		setClauses = append(setClauses, fmt.Sprintf("name = $%d", argIdx))
		args = append(args, *req.Name)
		argIdx++
	}
	if req.WebhookURL != nil {
		setClauses = append(setClauses, fmt.Sprintf("webhook_url = $%d", argIdx))
		args = append(args, *req.WebhookURL)
		argIdx++
	}
	if req.DefaultChannel != nil {
		// Allow setting to empty string to clear the default_channel.
		v := *req.DefaultChannel
		if v == "" {
			setClauses = append(setClauses, fmt.Sprintf("default_channel = $%d", argIdx))
			args = append(args, nil)
		} else {
			setClauses = append(setClauses, fmt.Sprintf("default_channel = $%d", argIdx))
			args = append(args, v)
		}
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
		UPDATE slack_config
		SET %s
		WHERE id = $%d AND tenant_id = $%d
	`, joinSetClauses(setClauses), argIdx, argIdx+1)

	result, err := p.db.ExecContext(r.Context(), query, args...)
	if err != nil {
		p.logger.Error("slack-notify: update config", "error", err)
		p.writeError(w, 500, "failed to update config")
		return
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		p.writeError(w, 404, "config not found")
		return
	}

	// Return the updated config.
	var c slackConfigJSON
	err = p.db.QueryRowContext(r.Context(), `
		SELECT id, name, webhook_url, default_channel, enabled, created_at, updated_at
		FROM slack_config
		WHERE id = $1 AND tenant_id = $2
	`, id, tid).Scan(&c.ID, &c.Name, &c.WebhookURL, &c.DefaultChannel, &c.Enabled, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		p.logger.Error("slack-notify: re-fetch config", "error", err)
		p.writeError(w, 500, "failed to retrieve updated config")
		return
	}

	c.TenantID = tid
	p.writeJSON(w, 200, c)
}

// ---- DELETE /slack/configs/{id} ----

func (p *Plugin) handleDeleteConfig(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid config id")
		return
	}

	result, err := p.db.ExecContext(r.Context(), `
		DELETE FROM slack_config
		WHERE id = $1 AND tenant_id = $2
	`, id, tid)
	if err != nil {
		p.logger.Error("slack-notify: delete config", "error", err)
		p.writeError(w, 500, "failed to delete config")
		return
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		p.writeError(w, 404, "config not found")
		return
	}

	p.logger.Info("slack-notify: config deleted", "id", id, "tenant", tid)
	w.WriteHeader(http.StatusNoContent)
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
