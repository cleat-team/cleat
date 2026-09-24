package pagerdutyalert

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

func (p *Plugin) RegisterRoutes(mux plugin.Router) error {
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

func (p *Plugin) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (p *Plugin) writeError(w http.ResponseWriter, status int, msg string) {
	p.writeJSON(w, status, map[string]string{"error": msg})
}

// PagerdutyRoutingKeySecretName is the tenant-secret name a pd_config row's
// routing key is stored under. cleat#1992. Per-config, not per-tenant, for
// the same reason as datadogexport.DatadogAPIKeySecretName
// (plugins/datadogexport/routes.go): pd_config is not one row per tenant
// either. See migrations.go v3's own comment.
//
// EXPORTED, not package-private: tests/plugin-harness imports this plugin
// package (testdb.go's SeedPluginSecrets) to compute the exact same name
// triggerIncident will later read it back under -- one function, not two
// copies of a naming scheme that must never drift apart.
func PagerdutyRoutingKeySecretName(id uuid.UUID) string {
	return "pagerdutyalert.routing_key." + id.String()
}

// ---- types ----

type pdConfigJSON struct {
	ID         uuid.UUID     `json:"id"`
	TenantID   uuid.UUID     `json:"tenant_id"`
	Name       string        `json:"name"`
	RoutingKey plugin.Secret `json:"routing_key"`
	Enabled    bool          `json:"enabled"`
	CreatedAt  time.Time     `json:"created_at"`
	UpdatedAt  time.Time     `json:"updated_at"`
}

type createConfigRequest struct {
	Name       string        `json:"name"`
	RoutingKey plugin.Secret `json:"routing_key"`
}

type updateConfigRequest struct {
	Name       *string        `json:"name,omitempty"`
	RoutingKey *plugin.Secret `json:"routing_key,omitempty"`
	Enabled    *bool          `json:"enabled,omitempty"`
}

// ---- POST /pagerduty/configs ----

func (p *Plugin) handleCreateConfig(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, 401, "tenant required")
		return
	}

	var req createConfigRequest
	if !plugin.ReadJSONBody(w, r, &req) {
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

	// Secret written first -- see datadogexport's handleCreate for why
	// (plugins/datadogexport/routes.go): a failure here leaves nothing
	// behind, while a failure on the INSERT below leaves only a harmless
	// orphaned secret, logged.
	if err := p.secrets.Put(r.Context(), PagerdutyRoutingKeySecretName(id), req.RoutingKey.Reveal()); err != nil {
		p.logger.Error("pagerduty: store routing key", "error", err)
		p.writeError(w, 500, "failed to store routing key")
		return
	}

	_, err := p.db.Exec(r.Context(), plugin.Rebind(`
			INSERT INTO pd_config (tenant_id, id, name, enabled, created_at, updated_at)
			VALUES ($1, $2, $3, true, $4, $4)
		`, p.dialect), tid, id, req.Name, now)
	if err != nil {
		p.logger.Error("pagerduty: create config",
			"error", err, "orphaned_secret", PagerdutyRoutingKeySecretName(id))
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
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, 401, "tenant required")
		return
	}

	rows, err := p.db.Query(r.Context(), plugin.Rebind(`
			SELECT id, name, enabled, created_at, updated_at
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

	// c.RoutingKey is left at its zero value throughout this handler --
	// Secret's MarshalJSON always redacts regardless of content
	// (plugin/secret.go), so fetching the real value here would be a decrypt
	// spent on a byte the client can never see. See datadogexport's
	// handleList for the same reasoning.
	var configs []pdConfigJSON
	for rows.Next() {
		var c pdConfigJSON
		if err := plugin.ScanRow(rows, &c.ID, &c.Name, &c.Enabled, &c.CreatedAt, &c.UpdatedAt); err != nil {
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
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
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
	err = plugin.ScanRow(p.db.QueryRow(r.Context(), plugin.Rebind(`
			SELECT id, name, enabled, created_at, updated_at
			FROM pd_config
			WHERE id = $1 AND tenant_id = $2
		`, p.dialect), id, tid), &c.ID, &c.Name, &c.Enabled, &c.CreatedAt, &c.UpdatedAt)
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
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid config id")
		return
	}

	var req updateConfigRequest
	if !plugin.ReadJSONBody(w, r, &req) {
		return
	}

	// req.RoutingKey has no column of its own any more -- it goes through
	// p.secrets below -- but it still counts as a field being updated. See
	// datadogexport's handleUpdate for the same shape.
	if req.Name == nil && req.RoutingKey == nil && req.Enabled == nil {
		p.writeError(w, 400, "no fields to update")
		return
	}

	// Build dynamic UPDATE query for the SQL-column fields that are present.
	setClauses := []string{}
	args := []any{}
	argIdx := 1

	if req.Name != nil {
		setClauses = append(setClauses, fmt.Sprintf("name = $%d", argIdx))
		args = append(args, *req.Name)
		argIdx++
	}
	if req.Enabled != nil {
		setClauses = append(setClauses, fmt.Sprintf("enabled = $%d", argIdx))
		args = append(args, *req.Enabled)
		argIdx++
	}

	// updated_at is unconditional, so this UPDATE's WHERE id = $.. AND
	// tenant_id = $.. is always the existence-and-ownership check before the
	// secret write below, even when routing_key is the only field changing.
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

	// Secret write happens AFTER the row update confirms id belongs to tid --
	// see datadogexport's handleUpdate for why.
	if req.RoutingKey != nil {
		if err := p.secrets.Put(r.Context(), PagerdutyRoutingKeySecretName(id), req.RoutingKey.Reveal()); err != nil {
			p.logger.Error("pagerduty: rotate routing key", "error", err, "id", id)
			p.writeError(w, 500, "failed to store routing key")
			return
		}
	}

	// Return the updated config.
	var c pdConfigJSON
	err = plugin.ScanRow(p.db.QueryRow(r.Context(), plugin.Rebind(`
			SELECT id, name, enabled, created_at, updated_at
			FROM pd_config
			WHERE id = $1 AND tenant_id = $2
		`, p.dialect), id, tid), &c.ID, &c.Name, &c.Enabled, &c.CreatedAt, &c.UpdatedAt)
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
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
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

	// Best-effort, logged not failed -- see datadogexport's handleDelete for
	// why.
	if _, err := p.secrets.Retire(r.Context(), PagerdutyRoutingKeySecretName(id)); err != nil {
		p.logger.Error("pagerduty: retire routing key after delete", "error", err, "id", id)
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
