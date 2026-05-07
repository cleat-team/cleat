package featureflags

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
		return fmt.Errorf("feature-flags: nil mux")
	}
	mux.HandleFunc("POST /features/flags", p.handleCreate)
	mux.HandleFunc("GET /features/flags", p.handleList)
	mux.HandleFunc("GET /features/flags/{id}", p.handleGet)
	mux.HandleFunc("PUT /features/flags/{id}", p.handleUpdate)
	mux.HandleFunc("DELETE /features/flags/{id}", p.handleDelete)
	mux.HandleFunc("POST /features/evaluate", p.handleEvaluate)
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

type flagJSON struct {
	ID                uuid.UUID       `json:"id"`
	TenantID          uuid.UUID       `json:"tenant_id"`
	Key               string          `json:"key"`
	Name              string          `json:"name,omitempty"`
	Description       string          `json:"description,omitempty"`
	Enabled           bool            `json:"enabled"`
	Rules             json.RawMessage `json:"rules,omitempty"`
	RolloutPercentage int             `json:"rollout_percentage"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

type createFlagRequest struct {
	Key               string          `json:"key"`
	Name              string          `json:"name,omitempty"`
	Description       string          `json:"description,omitempty"`
	Enabled           bool            `json:"enabled"`
	Rules             json.RawMessage `json:"rules,omitempty"`
	RolloutPercentage int             `json:"rollout_percentage"`
}

type updateFlagRequest struct {
	Key               *string          `json:"key,omitempty"`
	Name              *string          `json:"name,omitempty"`
	Description       *string          `json:"description,omitempty"`
	Enabled           *bool            `json:"enabled,omitempty"`
	Rules             *json.RawMessage `json:"rules,omitempty"`
	RolloutPercentage *int             `json:"rollout_percentage,omitempty"`
}

type evaluateRequest struct {
	Key     string                 `json:"key"`
	Context EvaluationContext      `json:"context"`
}

// ---- POST /features/flags ----

func (p *Plugin) handleCreate(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("feature-flags: read body", "error", err)
		p.writeError(w, 500, "failed to read body")
		return
	}
	defer r.Body.Close()

	var req createFlagRequest
	if err := json.Unmarshal(body, &req); err != nil {
		p.writeError(w, 400, "invalid request body")
		return
	}
	if req.Key == "" {
		p.writeError(w, 400, "key is required")
		return
	}
	rollout := req.RolloutPercentage
	if rollout < 0 {
		rollout = 0
	} else if rollout > 100 {
		rollout = 100
	}

	// Normalize rules to valid JSON array.
	rulesJSON := req.Rules
	if rulesJSON == nil || len(rulesJSON) == 0 {
		rulesJSON = json.RawMessage("[]")
	}

	id := uuid.New()
	now := time.Now()

	_, err = p.db.ExecContext(r.Context(), `
		INSERT INTO feature_flags (tenant_id, id, key, name, description, enabled, rules, rollout_percentage, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, tid, id, req.Key, req.Name, req.Description, req.Enabled, rulesJSON, rollout, now, now)
	if err != nil {
		p.logger.Error("feature-flags: create", "key", req.Key, "error", err)
		p.writeError(w, 500, "failed to create feature flag")
		return
	}

	p.logger.Info("feature-flags: created",
		"key", req.Key,
		"id", id,
		"tenant", tid,
	)

	flag := flagJSON{
		ID:                id,
		TenantID:          tid,
		Key:               req.Key,
		Name:              req.Name,
		Description:       req.Description,
		Enabled:           req.Enabled,
		Rules:             rulesJSON,
		RolloutPercentage: rollout,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	p.writeJSON(w, 201, flag)
}

// ---- GET /features/flags ----

func (p *Plugin) handleList(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	rows, err := p.db.QueryContext(r.Context(), `
		SELECT id, tenant_id, key, name, description, enabled, rules, rollout_percentage, created_at, updated_at
		FROM feature_flags
		WHERE tenant_id = $1
		ORDER BY created_at DESC
	`, tid)
	if err != nil {
		p.logger.Error("feature-flags: list", "error", err)
		p.writeError(w, 500, "failed to list feature flags")
		return
	}
	defer rows.Close()

	var flags []flagJSON
	for rows.Next() {
		var f flagJSON
		if err := rows.Scan(&f.ID, &f.TenantID, &f.Key, &f.Name, &f.Description,
			&f.Enabled, &f.Rules, &f.RolloutPercentage, &f.CreatedAt, &f.UpdatedAt); err != nil {
			p.logger.Error("feature-flags: scan row", "error", err)
			continue
		}
		flags = append(flags, f)
	}

	if flags == nil {
		flags = []flagJSON{}
	}

	p.writeJSON(w, 200, flags)
}

// ---- GET /features/flags/{id} ----

func (p *Plugin) handleGet(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid flag id")
		return
	}

	var f flagJSON
	err = p.db.QueryRowContext(r.Context(), `
		SELECT id, tenant_id, key, name, description, enabled, rules, rollout_percentage, created_at, updated_at
		FROM feature_flags
		WHERE id = $1 AND tenant_id = $2
	`, id, tid).Scan(&f.ID, &f.TenantID, &f.Key, &f.Name, &f.Description,
		&f.Enabled, &f.Rules, &f.RolloutPercentage, &f.CreatedAt, &f.UpdatedAt)
	if err == sql.ErrNoRows {
		p.writeError(w, 404, "feature flag not found")
		return
	}
	if err != nil {
		p.logger.Error("feature-flags: get", "id", id, "error", err)
		p.writeError(w, 500, "failed to retrieve feature flag")
		return
	}

	p.writeJSON(w, 200, f)
}

// ---- PUT /features/flags/{id} ----

func (p *Plugin) handleUpdate(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid flag id")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("feature-flags: read body", "error", err)
		p.writeError(w, 500, "failed to read body")
		return
	}
	defer r.Body.Close()

	var req updateFlagRequest
	if err := json.Unmarshal(body, &req); err != nil {
		p.writeError(w, 400, "invalid request body")
		return
	}

	// Build dynamic update query — only set non-nil fields.
	now := time.Now()
	query := "UPDATE feature_flags SET updated_at = $1"
	args := []interface{}{now}
	argIdx := 2

	if req.Key != nil {
		query += fmt.Sprintf(", key = $%d", argIdx)
		args = append(args, *req.Key)
		argIdx++
	}
	if req.Name != nil {
		query += fmt.Sprintf(", name = $%d", argIdx)
		args = append(args, *req.Name)
		argIdx++
	}
	if req.Description != nil {
		query += fmt.Sprintf(", description = $%d", argIdx)
		args = append(args, *req.Description)
		argIdx++
	}
	if req.Enabled != nil {
		query += fmt.Sprintf(", enabled = $%d", argIdx)
		args = append(args, *req.Enabled)
		argIdx++
	}
	if req.Rules != nil {
		query += fmt.Sprintf(", rules = $%d", argIdx)
		args = append(args, *req.Rules)
		argIdx++
	}
	if req.RolloutPercentage != nil {
		rollout := *req.RolloutPercentage
		if rollout < 0 {
			rollout = 0
		} else if rollout > 100 {
			rollout = 100
		}
		query += fmt.Sprintf(", rollout_percentage = $%d", argIdx)
		args = append(args, rollout)
		argIdx++
	}

	query += fmt.Sprintf(" WHERE id = $%d AND tenant_id = $%d", argIdx, argIdx+1)
	args = append(args, id, tid)

	result, err := p.db.ExecContext(r.Context(), query, args...)
	if err != nil {
		p.logger.Error("feature-flags: update", "id", id, "error", err)
		p.writeError(w, 500, "failed to update feature flag")
		return
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		p.writeError(w, 404, "feature flag not found")
		return
	}

	// Return the updated flag.
	var f flagJSON
	err = p.db.QueryRowContext(r.Context(), `
		SELECT id, tenant_id, key, name, description, enabled, rules, rollout_percentage, created_at, updated_at
		FROM feature_flags
		WHERE id = $1 AND tenant_id = $2
	`, id, tid).Scan(&f.ID, &f.TenantID, &f.Key, &f.Name, &f.Description,
		&f.Enabled, &f.Rules, &f.RolloutPercentage, &f.CreatedAt, &f.UpdatedAt)
	if err != nil {
		p.logger.Error("feature-flags: re-fetch after update", "id", id, "error", err)
		p.writeError(w, 500, "flag updated but failed to retrieve")
		return
	}

	p.logger.Info("feature-flags: updated",
		"id", id,
		"tenant", tid,
	)

	p.writeJSON(w, 200, f)
}

// ---- DELETE /features/flags/{id} ----

func (p *Plugin) handleDelete(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid flag id")
		return
	}

	result, err := p.db.ExecContext(r.Context(), `
		DELETE FROM feature_flags
		WHERE id = $1 AND tenant_id = $2
	`, id, tid)
	if err != nil {
		p.logger.Error("feature-flags: delete", "id", id, "error", err)
		p.writeError(w, 500, "failed to delete feature flag")
		return
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		p.writeError(w, 404, "feature flag not found")
		return
	}

	p.logger.Info("feature-flags: deleted", "id", id, "tenant", tid)
	w.WriteHeader(http.StatusNoContent)
}

// ---- POST /features/evaluate ----

func (p *Plugin) handleEvaluate(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("feature-flags: read body", "error", err)
		p.writeError(w, 500, "failed to read body")
		return
	}
	defer r.Body.Close()

	var req evaluateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		p.writeError(w, 400, "invalid request body")
		return
	}
	if req.Key == "" {
		p.writeError(w, 400, "key is required")
		return
	}

	// Look up the flag by tenant_id and key.
	var f flagJSON
	err = p.db.QueryRowContext(r.Context(), `
		SELECT id, tenant_id, key, name, description, enabled, rules, rollout_percentage, created_at, updated_at
		FROM feature_flags
		WHERE tenant_id = $1 AND key = $2
	`, tid, req.Key).Scan(&f.ID, &f.TenantID, &f.Key, &f.Name, &f.Description,
		&f.Enabled, &f.Rules, &f.RolloutPercentage, &f.CreatedAt, &f.UpdatedAt)
	if err == sql.ErrNoRows {
		p.writeError(w, 404, "feature flag not found")
		return
	}
	if err != nil {
		p.logger.Error("feature-flags: evaluate lookup", "key", req.Key, "error", err)
		p.writeError(w, 500, "failed to look up feature flag")
		return
	}

	flag := &Flag{
		ID:                f.ID.String(),
		TenantID:          f.TenantID.String(),
		Key:               f.Key,
		Name:              f.Name,
		Description:       f.Description,
		Enabled:           f.Enabled,
		Rules:             f.Rules,
		RolloutPercentage: f.RolloutPercentage,
	}

	result := EvaluateFlag(flag, req.Context)
	p.writeJSON(w, 200, result)
}
