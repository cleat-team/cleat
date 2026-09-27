package datadogexport

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

// DatadogAPIKeySecretName is the tenant-secret name a dd_config row's API key
// is stored under. cleat#1992.
//
// PER-CONFIG, NOT PER-TENANT: dd_config is not one row per tenant -- a tenant
// can have several named Datadog configs (own id, site, metrics_prefix) -- so
// a single fixed name like "datadogexport.api_key" would collide across a
// tenant's own configs (tenant secrets are keyed (tenant_id, name), a
// singleton per name). Keying by config id preserves that multi-config
// capability with no product change. See migrations.go v4's own comment.
//
// EXPORTED for symmetry with pagerdutyalert.PagerdutyRoutingKeySecretName,
// which tests/plugin-harness does call cross-package (testdb.go's
// SeedPluginSecrets) -- and so any future out-of-package caller that needs
// to seed or verify a dd_config secret computes the name the same way,
// rather than reimplementing the scheme.
func DatadogAPIKeySecretName(id uuid.UUID) string {
	return "datadogexport.api_key." + id.String()
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
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, 401, "tenant required")
		return
	}

	var req createConfigRequest
	if !plugin.ReadJSONBody(w, r, &req) {
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

	// The secret is written FIRST. If it fails, nothing else has happened --
	// no orphaned config row. If the INSERT below fails after this succeeds,
	// the secret is orphaned under a name no config row references; harmless
	// (unreachable, never resolved by anything) but logged so it is not a
	// silent leak of key material nobody can account for.
	if err := p.secrets.Put(r.Context(), DatadogAPIKeySecretName(id), req.APIKey.Reveal()); err != nil {
		p.logger.Error("datadog-export: store api key", "error", err)
		p.writeError(w, 500, "failed to store api key")
		return
	}

	_, err := p.db.Exec(r.Context(), plugin.Rebind(`
			INSERT INTO dd_config (tenant_id, id, name, site, metrics_prefix, enabled, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, true, $6, $6)
		`, p.dialect), tid, id, req.Name, site, prefix, now)
	if err != nil {
		p.logger.Error("datadog-export: create config",
			"error", err, "orphaned_secret", DatadogAPIKeySecretName(id))
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
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, 401, "tenant required")
		return
	}

	rows, err := p.db.Query(r.Context(), plugin.Rebind(`
			SELECT id, name, site, metrics_prefix, enabled, created_at, updated_at
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

	// c.APIKey is left at its zero value throughout this handler. Secret's
	// MarshalJSON always emits RedactedPlaceholder regardless of content
	// (plugin/secret.go), so a real value here would never reach the
	// response -- fetching it from tenant secrets would be a decrypt spent on
	// a byte the client can never see.
	var configs []configJSON
	for rows.Next() {
		var c configJSON
		if err := plugin.ScanRow(rows, &c.ID, &c.Name, &c.Site, &c.MetricsPrefix, &c.Enabled, &c.CreatedAt, &c.UpdatedAt); err != nil {
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

	var c configJSON
	err = plugin.ScanRow(p.db.QueryRow(r.Context(), plugin.Rebind(`
			SELECT id, name, site, metrics_prefix, enabled, created_at, updated_at
			FROM dd_config
			WHERE id = $1 AND tenant_id = $2
		`, p.dialect), id, tid), &c.ID, &c.Name, &c.Site, &c.MetricsPrefix, &c.Enabled, &c.CreatedAt, &c.UpdatedAt)
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

	// req.APIKey has no column of its own any more -- it goes through
	// p.secrets below -- but it still counts as a field being updated, or a
	// request that touches only api_key would wrongly 400 as "no fields to
	// update".
	if req.Name == nil && req.APIKey == nil && req.Site == nil && req.MetricsPrefix == nil && req.Enabled == nil {
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

	// updated_at is unconditional, so setClauses is never empty here even
	// when api_key is the only field the caller asked to change -- which
	// keeps this UPDATE, and its WHERE id = $.. AND tenant_id = $.., as the
	// single existence-and-ownership check before the secret write below.
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

	// The secret write happens AFTER the row update confirms id belongs to
	// tid -- so a request naming another tenant's (or no) config id never
	// reaches p.secrets.Put at all, rather than writing an orphaned secret
	// under the caller's own tenant for an id that is not theirs.
	if req.APIKey != nil {
		if err := p.secrets.Put(r.Context(), DatadogAPIKeySecretName(id), req.APIKey.Reveal()); err != nil {
			p.logger.Error("datadog-export: rotate api key", "error", err, "id", id)
			p.writeError(w, 500, "failed to store api key")
			return
		}
	}

	// Return the updated config.
	var c configJSON
	err = plugin.ScanRow(p.db.QueryRow(r.Context(), plugin.Rebind(`
			SELECT id, name, site, metrics_prefix, enabled, created_at, updated_at
			FROM dd_config
			WHERE id = $1 AND tenant_id = $2
		`, p.dialect), id, tid), &c.ID, &c.Name, &c.Site, &c.MetricsPrefix, &c.Enabled, &c.CreatedAt, &c.UpdatedAt)
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

	// Best-effort: the config row is already gone, which is the operation
	// the caller asked for and got. A failure here leaves a retired-but-not-
	// yet-retired secret with no config row pointing at it -- inert, since
	// nothing looks it up by an id that no longer exists -- so it is logged
	// rather than turned into a 500 for an otherwise-successful delete.
	if _, err := p.secrets.Retire(r.Context(), DatadogAPIKeySecretName(id)); err != nil {
		p.logger.Error("datadog-export: retire api key after delete", "error", err, "id", id)
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
