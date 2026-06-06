package pagerdutyalert

import (
	"database/sql"
	"errors"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
)

func (p *Plugin) RegisterRoutes(mux *http.ServeMux) error {
	if mux == nil {
		return fmt.Errorf("pagerduty: nil mux")
	}
	mux.HandleFunc("POST /pagerduty/configs", p.handleCreateConfig)
	mux.HandleFunc("GET /pagerduty/configs", p.handleListConfigs)
	mux.HandleFunc("GET /pagerduty/configs/{id}", p.handleGetConfig)
	mux.HandleFunc("PUT /pagerduty/configs/{id}", p.handleUpdateConfig)
	mux.HandleFunc("DELETE /pagerduty/configs/{id}", p.handleDeleteConfig)
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

type pdConfigJSON struct {
	ID         uuid.UUID `json:"id"`
	TenantID   uuid.UUID `json:"tenant_id"`
	Name       string    `json:"name"`
	RoutingKey string    `json:"routing_key"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type createConfigRequest struct {
	Name       string `json:"name"`
	RoutingKey string `json:"routing_key"`
}

type updateConfigRequest struct {
	Name       *string `json:"name,omitempty"`
	RoutingKey *string `json:"routing_key,omitempty"`
	Enabled    *bool   `json:"enabled,omitempty"`
}

// ---- POST /pagerduty/configs ----

func (p *Plugin) handleCreateConfig(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("pagerduty: read body", "error", err)
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
	if req.RoutingKey == "" {
		p.writeError(w, 400, "routing_key is required")
		return
	}

	id := uuid.New()
	now := time.Now()

	_, err = p.db.Exec(r.Context(), plugin.Rebind(`
			INSERT INTO pd_config (tenant_id, id, name, routing_key, enabled, created_at, updated_at)
			VALUES ($1, $2, $3, $4, true, $5, $5)
		`, p.dialect), tid, id, req.Name, req.RoutingKey, now)
	if err != nil {
		p.logger.Error("pagerduty: create config", "error", err)
		p.writeError(w, 500, "failed to create config")
		return
	}

	p.logger.Info("pagerduty: config created", "id", id, "tenant", tid)

	p.writeJSON(w, 201, pdConfigJSON{
		ID:         id,
		TenantID:   tid,
		Name:       req.Name,
		RoutingKey: req.RoutingKey,
		Enabled:    true,
		CreatedAt:  now,
		UpdatedAt:  now,
	})
}

// ---- GET /pagerduty/configs ----

func (p *Plugin) handleListConfigs(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	rows, err := p.db.Query(r.Context(), plugin.Rebind(`
			SELECT id, name, routing_key, enabled, created_at, updated_at
			FROM pd_config
			WHERE tenant_id = $1
			ORDER BY created_at DESC
		`, p.dialect), tid)
	if err != nil {
		p.logger.Error("pagerduty: list configs", "error", err)
		p.writeError(w, 500, "failed to list configs")
		return
	}
	defer rows.Close()

	var configs []pdConfigJSON
	for rows.Next() {
		var c pdConfigJSON
		if err := rows.Scan(&c.ID, &c.Name, &c.RoutingKey, &c.Enabled, &c.CreatedAt, &c.UpdatedAt); err != nil {
			p.logger.Error("pagerduty: scan config", "error", err)
			continue
		}
		c.TenantID = tid
		configs = append(configs, c)
	}

	if configs == nil {
		configs = []pdConfigJSON{}
	}

	p.writeJSON(w, 200, configs)
}

// ---- GET /pagerduty/configs/{id} ----

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

	var c pdConfigJSON
	err = p.db.QueryRow(r.Context(), plugin.Rebind(`
			SELECT id, name, routing_key, enabled, created_at, updated_at
			FROM pd_config
			WHERE id = $1 AND tenant_id = $2
		`, p.dialect), id, tid).Scan(&c.ID, &c.Name, &c.RoutingKey, &c.Enabled, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		p.writeError(w, 404, "config not found")
		return
	}
	if err != nil {
		p.logger.Error("pagerduty: get config", "error", err)
		p.writeError(w, 500, "failed to get config")
		return
	}

	c.TenantID = tid
	p.writeJSON(w, 200, c)
}

// ---- PUT /pagerduty/configs/{id} ----

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
		p.logger.Error("pagerduty: read body", "error", err)
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
	if req.RoutingKey != nil {
		setClauses = append(setClauses, fmt.Sprintf("routing_key = $%d", argIdx))
		args = append(args, *req.RoutingKey)
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
			UPDATE pd_config
			SET %s
			WHERE id = $%d AND tenant_id = $%d
		`, joinSetClauses(setClauses), argIdx, argIdx+1)

	rows, err := p.db.Exec(r.Context(), plugin.Rebind(query, p.dialect), args...)
	if err != nil {
		p.logger.Error("pagerduty: update config", "error", err)
		p.writeError(w, 500, "failed to update config")
		return
	}
	if rows == 0 {
		p.writeError(w, 404, "config not found")
		return
	}

	// Return the updated config.
	var c pdConfigJSON
	err = p.db.QueryRow(r.Context(), plugin.Rebind(`
			SELECT id, name, routing_key, enabled, created_at, updated_at
			FROM pd_config
			WHERE id = $1 AND tenant_id = $2
		`, p.dialect), id, tid).Scan(&c.ID, &c.Name, &c.RoutingKey, &c.Enabled, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		p.logger.Error("pagerduty: re-fetch config", "error", err)
		p.writeError(w, 500, "failed to retrieve updated config")
		return
	}

	c.TenantID = tid
	p.writeJSON(w, 200, c)
}

// ---- DELETE /pagerduty/configs/{id} ----

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

	rows, err := p.db.Exec(r.Context(), plugin.Rebind(`
			DELETE FROM pd_config
			WHERE id = $1 AND tenant_id = $2
		`, p.dialect), id, tid)
	if err != nil {
		p.logger.Error("pagerduty: delete config", "error", err)
		p.writeError(w, 500, "failed to delete config")
		return
	}
	if rows == 0 {
		p.writeError(w, 404, "config not found")
		return
	}

	p.logger.Info("pagerduty: config deleted", "id", id, "tenant", tid)
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
