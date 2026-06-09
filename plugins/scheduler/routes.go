package scheduler

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

func (p *Plugin) RegisterRoutes(mux *http.ServeMux) error {
	if mux == nil {
		return fmt.Errorf("scheduler: nil mux")
	}
	mux.HandleFunc("POST /schedules", p.handleCreate)
	mux.HandleFunc("GET /schedules", p.handleList)
	mux.HandleFunc("GET /schedules/{id}", p.handleGet)
	mux.HandleFunc("PUT /schedules/{id}", p.handleUpdate)
	mux.HandleFunc("DELETE /schedules/{id}", p.handleDelete)
	mux.HandleFunc("POST /schedules/{id}/trigger", p.handleTrigger)
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

// tenantID extracts the tenant UUID from the request context.
func (p *Plugin) tenantID(r *http.Request) uuid.UUID {
	tid, _ := auth.TenantIDFromContext(r.Context())
	return tid
}

// schedule represents a single schedule row.
type schedule struct {
	ID           uuid.UUID  `json:"id"`
	Name         string     `json:"name"`
	Cron         string     `json:"cron"`
	WorkflowName string     `json:"workflow_name"`
	Input        []byte     `json:"input"`
	Enabled      bool       `json:"enabled"`
	LastRunAt    *time.Time `json:"last_run_at,omitempty"`
	NextRunAt    *time.Time `json:"next_run_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// createScheduleRequest is the JSON body for creating a schedule.
type createScheduleRequest struct {
	Name         string          `json:"name"`
	Cron         string          `json:"cron"`
	WorkflowName string          `json:"workflow_name"`
	Input        json.RawMessage `json:"input,omitempty"`
	Enabled      *bool           `json:"enabled,omitempty"`
}

// updateScheduleRequest is the JSON body for updating a schedule.
type updateScheduleRequest struct {
	Name         *string          `json:"name,omitempty"`
	Cron         *string          `json:"cron,omitempty"`
	WorkflowName *string          `json:"workflow_name,omitempty"`
	Input        *json.RawMessage `json:"input,omitempty"`
	Enabled      *bool            `json:"enabled,omitempty"`
}

// ---- POST /schedules ----

func (p *Plugin) handleCreate(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	var req createScheduleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		p.writeError(w, 400, "invalid JSON body")
		return
	}

	if req.Name == "" {
		p.writeError(w, 400, "name is required")
		return
	}
	if req.Cron == "" {
		p.writeError(w, 400, "cron is required")
		return
	}
	if req.WorkflowName == "" {
		p.writeError(w, 400, "workflow_name is required")
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	// Validate the cron expression by computing the next run.
	now := time.Now()
	next := nextRun(req.Cron, now)
	if next.IsZero() {
		p.writeError(w, 400, "invalid cron expression or no future match found")
		return
	}

	inputBytes := []byte("{}")
	if len(req.Input) > 0 {
		inputBytes = req.Input
	}

	id := uuid.New()

	_, err := p.db.Exec(r.Context(), plugin.Rebind(`
		INSERT INTO schedules (tenant_id, id, name, cron, workflow_name, input, enabled, next_run_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now(), now())
	`, p.dialect), tid, id, req.Name, req.Cron, req.WorkflowName, inputBytes, enabled, next)
	if err != nil {
		p.logger.Error("scheduler: create", "error", err)
		p.writeError(w, 500, "failed to create schedule")
		return
	}

	p.logger.Info("scheduler: created", "id", id, "tenant", tid, "name", req.Name)

	p.writeJSON(w, 201, map[string]any{
		"id":          id,
		"name":        req.Name,
		"cron":        req.Cron,
		"enabled":     enabled,
		"next_run_at": next,
	})
}

// ---- GET /schedules ----

func (p *Plugin) handleList(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	rows, err := p.db.Query(r.Context(), plugin.Rebind(`
		SELECT id, name, cron, workflow_name, input, enabled, last_run_at, next_run_at, created_at, updated_at
		FROM schedules
		WHERE tenant_id = $1
		ORDER BY created_at DESC
	`, p.dialect), tid)
	if err != nil {
		p.logger.Error("scheduler: list", "error", err)
		p.writeError(w, 500, "failed to list schedules")
		return
	}
	defer rows.Close()

	schedules := make([]schedule, 0)
	for rows.Next() {
		var s schedule
		err := rows.Scan(
			&s.ID, &s.Name, &s.Cron, &s.WorkflowName,
			&s.Input, &s.Enabled, &s.LastRunAt, &s.NextRunAt,
			&s.CreatedAt, &s.UpdatedAt,
		)
		if err != nil {
			p.logger.Error("scheduler: scan row", "error", err)
			continue
		}
		schedules = append(schedules, s)
	}

	p.writeJSON(w, 200, schedules)
}

// ---- GET /schedules/{id} ----

func (p *Plugin) handleGet(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid schedule id")
		return
	}

	var s schedule
	err = p.db.QueryRow(r.Context(), plugin.Rebind(`
		SELECT id, name, cron, workflow_name, input, enabled, last_run_at, next_run_at, created_at, updated_at
		FROM schedules
		WHERE id = $1 AND tenant_id = $2
	`, p.dialect), id, tid).Scan(
		&s.ID, &s.Name, &s.Cron, &s.WorkflowName,
		&s.Input, &s.Enabled, &s.LastRunAt, &s.NextRunAt,
		&s.CreatedAt, &s.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		p.writeError(w, 404, "schedule not found")
		return
	}
	if err != nil {
		p.logger.Error("scheduler: get", "id", id, "error", err)
		p.writeError(w, 500, "failed to get schedule")
		return
	}

	p.writeJSON(w, 200, s)
}

// ---- PUT /schedules/{id} ----

func (p *Plugin) handleUpdate(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid schedule id")
		return
	}

	var req updateScheduleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		p.writeError(w, 400, "invalid JSON body")
		return
	}

	// Fetch the existing schedule to determine the cron expression.
	var currentCron string
	var currentEnabled bool
	err = p.db.QueryRow(r.Context(), plugin.Rebind(`
		SELECT cron, enabled FROM schedules WHERE id = $1 AND tenant_id = $2
	`, p.dialect), id, tid).Scan(&currentCron, &currentEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		p.writeError(w, 404, "schedule not found")
		return
	}
	if err != nil {
		p.logger.Error("scheduler: update fetch", "id", id, "error", err)
		p.writeError(w, 500, "failed to update schedule")
		return
	}

	cron := currentCron
	if req.Cron != nil {
		cron = *req.Cron
	}

	enabled := currentEnabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	// Recalculate next_run_at if cron or enabled changed.
	var nextRunVal *time.Time
	if (req.Cron != nil) || (req.Enabled != nil) {
		if enabled {
			nxt := nextRun(cron, time.Now())
			if nxt.IsZero() {
				p.writeError(w, 400, "invalid cron expression or no future match found")
				return
			}
			nextRunVal = &nxt
		}
	}

	// Build dynamic UPDATE query.
	query := `UPDATE schedules SET updated_at = now()`
	args := []any{}
	argIdx := 1

	if req.Name != nil {
		query += fmt.Sprintf(", name = $%d", argIdx)
		args = append(args, *req.Name)
		argIdx++
	}
	if req.Cron != nil {
		query += fmt.Sprintf(", cron = $%d", argIdx)
		args = append(args, *req.Cron)
		argIdx++
	}
	if req.WorkflowName != nil {
		query += fmt.Sprintf(", workflow_name = $%d", argIdx)
		args = append(args, *req.WorkflowName)
		argIdx++
	}
	if req.Input != nil {
		query += fmt.Sprintf(", input = $%d", argIdx)
		args = append(args, []byte(*req.Input))
		argIdx++
	}
	if req.Enabled != nil {
		query += fmt.Sprintf(", enabled = $%d", argIdx)
		args = append(args, *req.Enabled)
		argIdx++
	}
	if nextRunVal != nil {
		query += fmt.Sprintf(", next_run_at = $%d", argIdx)
		args = append(args, *nextRunVal)
		argIdx++
	}

	query += fmt.Sprintf(" WHERE id = $%d AND tenant_id = $%d", argIdx, argIdx+1)
	args = append(args, id, tid)

	_, err = p.db.Exec(r.Context(), plugin.Rebind(query, p.dialect), args...)
	if err != nil {
		p.logger.Error("scheduler: update", "id", id, "error", err)
		p.writeError(w, 500, "failed to update schedule")
		return
	}

	p.logger.Info("scheduler: updated", "id", id, "tenant", tid)
	p.writeJSON(w, 200, map[string]string{"status": "updated"})
}

// ---- DELETE /schedules/{id} ----

func (p *Plugin) handleDelete(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid schedule id")
		return
	}

	rows, err := p.db.Exec(r.Context(), plugin.Rebind(`
		DELETE FROM schedules WHERE id = $1 AND tenant_id = $2
	`, p.dialect), id, tid)
	if err != nil {
		p.logger.Error("scheduler: delete", "id", id, "error", err)
		p.writeError(w, 500, "failed to delete schedule")
		return
	}
	if rows == 0 {
		p.writeError(w, 404, "schedule not found")
		return
	}

	p.logger.Info("scheduler: deleted", "id", id, "tenant", tid)
	w.WriteHeader(http.StatusNoContent)
}

// ---- POST /schedules/{id}/trigger ----

func (p *Plugin) handleTrigger(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		p.writeError(w, 400, "invalid schedule id")
		return
	}

	var name, cron, workflowName string
	var inputBytes []byte
	err = p.db.QueryRow(r.Context(), plugin.Rebind(`
		SELECT name, cron, workflow_name, input FROM schedules WHERE id = $1 AND tenant_id = $2
	`, p.dialect), id, tid).Scan(&name, &cron, &workflowName, &inputBytes)
	if errors.Is(err, sql.ErrNoRows) {
		p.writeError(w, 404, "schedule not found")
		return
	}
	if err != nil {
		p.logger.Error("scheduler: trigger fetch", "id", id, "error", err)
		p.writeError(w, 500, "failed to trigger schedule")
		return
	}

	// Log the trigger as a one-off run. In production this would enqueue
	// a workflow execution. For now, record the trigger event.
	p.logger.Info("scheduler: triggered",
		"id", id,
		"tenant", tid,
		"name", name,
		"workflow", workflowName,
		"input", string(inputBytes),
	)

	p.writeJSON(w, 200, map[string]any{
		"status":        "triggered",
		"schedule_id":   id,
		"workflow_name": workflowName,
	})
}
