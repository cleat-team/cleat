package datadogexport

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

func (p *Plugin) RegisterRoutes(mux *http.ServeMux) error {
	if mux == nil {
		return fmt.Errorf("datadog-export: nil mux")
	}
	mux.HandleFunc("POST /datadog/configs", p.handleCreate)
	mux.HandleFunc("GET /datadog/configs", p.handleList)
	mux.HandleFunc("GET /datadog/configs/{id}", p.handleGet)
	mux.HandleFunc("PUT /datadog/configs/{id}", p.handleUpdate)
	mux.HandleFunc("DELETE /datadog/configs/{id}", p.handleDelete)
	return nil
}

// ---- helpers ----

func (p *Plugin) writeJSON(w http.ResponseWriter, status int, v any) {
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

type configJSON struct {
	ID            uuid.UUID     `json:"id"`
	TenantID      uuid.UUID     `json:"tenant_id"`
	Name          string        `json:"name"`
	APIKey        plugin.Secret `json:"api_key"`
	Site          string        `json:"site"`
	MetricsPrefix string        `json:"metrics_prefix"`
	Enabled       bool          `json:"enabled"`
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
}

type createConfigRequest struct {
	Name          string        `json:"name"`
	APIKey        plugin.Secret `json:"api_key"`
	Site          string        `json:"site,omitempty"`
	MetricsPrefix string        `json:"metrics_prefix,omitempty"`
}

type updateConfigRequest struct {
	Name          *string        `json:"name,omitempty"`
	APIKey        *plugin.Secret `json:"api_key,omitempty"`
	Site          *string        `json:"site,omitempty"`
	MetricsPrefix *string        `json:"metrics_prefix,omitempty"`
	Enabled       *bool          `json:"enabled,omitempty"`
}

// ---- POST /datadog/configs ----

func (p *Plugin) handleCreate(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("datadog-export: read body", "error", err)
		p.writeError(w, 500, "failed to read body")
		return
	}
	defer r.Body.Close()

	var req createConfigRequest
	if err := json.Unmarshal(body, &req); err != nil {
		p.writeError(w, 400, "invalid request body")
		return
	}
	if req.APIKey.Reveal() == "" {
		p.writeError(w, 400, "api_key is required")
		return
	}

	site := req.Site
	if site == "" {
		site = "datadoghq.com"
	}
	prefix := req.MetricsPrefix
	if prefix == "" {
		prefix = "cleat"
	}

	id := uuid.New()
	now := time.Now()

	_, err = p.db.Exec(r.Context(), plugin.Rebind(`
			INSERT INTO dd_config (tenant_id, id, name, api_key, site, metrics_prefix, enabled, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, true, $7, $7)
		`, p.dialect), tid, id, req.Name, req.APIKey.Reveal(), site, prefix, now)
	if err != nil {
		p.logger.Error("datadog-export: create config", "error", err)
		p.writeError(w, 500, "failed to create config")
		return
	}

	p.logger.Info("datadog-export: config created", "id", id, "tenant", tid)
	// The API key is redacted on every response, including create -- the
	// caller already has the value they just sent, so echoing it back adds
	// nothing and is one more path to get wrong.
	p.writeJSON(w, 201, configJSON{
		ID:            id,
		TenantID:      tid,
		Name:          req.Name,
		APIKey:        req.APIKey,
		Site:          site,
		MetricsPrefix: prefix,
		Enabled:       true,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
}

// ---- GET /datadog/configs ----

func (p *Plugin) handleList(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	rows, err := p.db.Query(r.Context(), plugin.Rebind(`
			SELECT id, name, api_key, site, metrics_prefix, enabled, created_at, updated_at
			FROM dd_config
			WHERE tenant_id = $1
			ORDER BY created_at DESC
		`, p.dialect), tid)
	if err != nil {
		p.logger.Error("datadog-export: list configs", "error", err)
		p.writeError(w, 500, "failed to list configs")
		return
	}
	defer rows.Close()

	var configs []configJSON
	for rows.Next() {
		var c configJSON
		if err := rows.Scan(&c.ID, &c.Name, &c.APIKey, &c.Site, &c.MetricsPrefix, &c.Enabled, &c.CreatedAt, &c.UpdatedAt); err != nil {
			p.logger.Error("datadog-export: scan config", "error", err)
			continue
		}
		c.TenantID = tid
		configs = append(configs, c)
	}

	if configs == nil {
		configs = []configJSON{}
	}

	p.writeJSON(w, 200, configs)
}

// ---- GET /datadog/configs/{id} ----

func (p *Plugin) handleGet(w http.ResponseWriter, r *http.Request) {
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

	var c configJSON
	err = p.db.QueryRow(r.Context(), plugin.Rebind(`
			SELECT id, name, api_key, site, metrics_prefix, enabled, created_at, updated_at
			FROM dd_config
			WHERE id = $1 AND tenant_id = $2
		`, p.dialect), id, tid).Scan(&c.ID, &c.Name, &c.APIKey, &c.Site, &c.MetricsPrefix, &c.Enabled, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		p.writeError(w, 404, "config not found")
		return
	}
	if err != nil {
		p.logger.Error("datadog-export: get config", "error", err)
		p.writeError(w, 500, "failed to get config")
		return
	}

	c.TenantID = tid
	p.writeJSON(w, 200, c)
}

// ---- PUT /datadog/configs/{id} ----

func (p *Plugin) handleUpdate(w http.ResponseWriter, r *http.Request) {
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
		p.logger.Error("datadog-export: read body", "error", err)
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
	args := []any{}
	argIdx := 1

	if req.Name != nil {
		setClauses = append(setClauses, fmt.Sprintf("name = $%d", argIdx))
		args = append(args, *req.Name)
		argIdx++
	}
	if req.APIKey != nil {
		setClauses = append(setClauses, fmt.Sprintf("api_key = $%d", argIdx))
		args = append(args, req.APIKey.Reveal())
		argIdx++
	}
	if req.Site != nil {
		setClauses = append(setClauses, fmt.Sprintf("site = $%d", argIdx))
		args = append(args, *req.Site)
		argIdx++
	}
	if req.MetricsPrefix != nil {
		setClauses = append(setClauses, fmt.Sprintf("metrics_prefix = $%d", argIdx))
		args = append(args, *req.MetricsPrefix)
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
			UPDATE dd_config
			SET %s
			WHERE id = $%d AND tenant_id = $%d
		`, joinSetClauses(setClauses), argIdx, argIdx+1)

	rows, err := p.db.Exec(r.Context(), plugin.Rebind(query, p.dialect), args...)
	if err != nil {
		p.logger.Error("datadog-export: update config", "error", err)
		p.writeError(w, 500, "failed to update config")
		return
	}
	if rows == 0 {
		p.writeError(w, 404, "config not found")
		return
	}

	// Return the updated config.
	var c configJSON
	err = p.db.QueryRow(r.Context(), plugin.Rebind(`
			SELECT id, name, api_key, site, metrics_prefix, enabled, created_at, updated_at
			FROM dd_config
			WHERE id = $1 AND tenant_id = $2
		`, p.dialect), id, tid).Scan(&c.ID, &c.Name, &c.APIKey, &c.Site, &c.MetricsPrefix, &c.Enabled, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		p.logger.Error("datadog-export: re-fetch config", "error", err)
		p.writeError(w, 500, "failed to retrieve updated config")
		return
	}

	c.TenantID = tid
	p.writeJSON(w, 200, c)
}

// ---- DELETE /datadog/configs/{id} ----

func (p *Plugin) handleDelete(w http.ResponseWriter, r *http.Request) {
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
			DELETE FROM dd_config
			WHERE id = $1 AND tenant_id = $2
		`, p.dialect), id, tid)
	if err != nil {
		p.logger.Error("datadog-export: delete config", "error", err)
		p.writeError(w, 500, "failed to delete config")
		return
	}
	if rows == 0 {
		p.writeError(w, 404, "config not found")
		return
	}

	p.logger.Info("datadog-export: config deleted", "id", id, "tenant", tid)
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
