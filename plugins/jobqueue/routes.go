package jobqueue

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// RegisterRoutes registers the job queue HTTP handlers on the given mux.
func (p *Plugin) RegisterRoutes(mux *http.ServeMux) error {
	if mux == nil {
		return fmt.Errorf("jobqueue: nil mux")
	}
	mux.HandleFunc("POST /jobqueue/{queue_name}/jobs", p.handleEnqueue)
	mux.HandleFunc("GET /jobqueue/{queue_name}/jobs", p.handleListJobs)
	mux.HandleFunc("GET /jobqueue/{queue_name}/jobs/{job_id}", p.handleGetJob)
	mux.HandleFunc("DELETE /jobqueue/{queue_name}/jobs/{job_id}", p.handleCancelJob)
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

// enqueueRequest is the JSON body shape accepted by the enqueue endpoint.
type enqueueRequest struct {
	DefName string          `json:"def_name,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// tenantID extracts the tenant UUID from the request context. Returns the
// zero UUID if no tenant is set.
func (p *Plugin) tenantID(r *http.Request) uuid.UUID {
	tid, _ := auth.TenantIDFromContext(r.Context())
	return tid
}

// JobResponse is the JSON shape returned for a single job.
type JobResponse struct {
	JobID       uuid.UUID       `json:"job_id"`
	QueueName   string          `json:"queue_name"`
	Status      string          `json:"status"`
	Payload     json.RawMessage `json:"payload"`
	CreatedAt   time.Time       `json:"created_at"`
	StartedAt   *time.Time      `json:"started_at,omitempty"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
}

// ---- POST /jobqueue/{queue_name}/jobs ----

func (p *Plugin) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	queueName := r.PathValue("queue_name")
	if queueName == "" {
		p.writeError(w, 400, "queue_name is required")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("jobqueue: read body", "error", err)
		p.writeError(w, 500, "failed to read body")
		return
	}
	defer r.Body.Close()

	var req enqueueRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			p.writeError(w, 400, "invalid JSON payload")
			return
		}
	}

	jobID := uuid.New()

	var defName *string
	if req.DefName != "" {
		defName = &req.DefName
	}

	_, err = p.db.Exec(r.Context(), plugin.Rebind(`
			INSERT INTO task_queue (tenant_id, queue_name, job_id, payload, def_name, input)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, p.dialect), tid, queueName, jobID, req.Payload, defName, req.Input)
	if err != nil {
		p.logger.Error("jobqueue: enqueue", "error", err)
		p.writeError(w, 500, "failed to enqueue job")
		return
	}

	p.logger.Info("jobqueue: enqueued",
		"job_id", jobID,
		"queue", queueName,
		"tenant", tid,
		"def_name", req.DefName,
	)

	p.writeJSON(w, 201, map[string]any{
		"job_id":     jobID,
		"queue_name": queueName,
		"status":     "pending",
	})
}

// ---- GET /jobqueue/{queue_name}/jobs ----

func (p *Plugin) handleListJobs(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	queueName := r.PathValue("queue_name")
	if queueName == "" {
		p.writeError(w, 400, "queue_name is required")
		return
	}

	statusFilter := r.URL.Query().Get("status")
	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		if v, err := strconv.Atoi(limitStr); err == nil && v > 0 && v <= 1000 {
			limit = v
		}
	}

	query := `
			SELECT job_id, queue_name, status, payload, created_at, started_at, completed_at
			FROM task_queue
			WHERE tenant_id = $1 AND queue_name = $2
		`
	args := []any{tid, queueName}
	argIdx := 3

	if statusFilter != "" {
		query += fmt.Sprintf(" AND status = $%d", argIdx)
		args = append(args, statusFilter)
		argIdx++
	}

	query += " ORDER BY created_at DESC"
	query += fmt.Sprintf(" LIMIT $%d", argIdx)
	args = append(args, limit)

	rows, err := p.db.Query(r.Context(), plugin.Rebind(query, p.dialect), args...)
	if err != nil {
		p.logger.Error("jobqueue: list jobs", "error", err)
		p.writeError(w, 500, "failed to list jobs")
		return
	}
	defer rows.Close()

	var jobs []JobResponse
	for rows.Next() {
		var (
			j           JobResponse
			payloadRaw  []byte
			startedAt   sql.NullTime
			completedAt sql.NullTime
		)
		if err := rows.Scan(
			&j.JobID, &j.QueueName, &j.Status,
			&payloadRaw, &j.CreatedAt,
			&startedAt, &completedAt,
		); err != nil {
			p.logger.Error("jobqueue: scan row", "error", err)
			continue
		}
		j.Payload = json.RawMessage(payloadRaw)
		if startedAt.Valid {
			j.StartedAt = &startedAt.Time
		}
		if completedAt.Valid {
			j.CompletedAt = &completedAt.Time
		}
		jobs = append(jobs, j)
	}

	if jobs == nil {
		jobs = []JobResponse{}
	}

	p.writeJSON(w, 200, jobs)
}

// ---- GET /jobqueue/{queue_name}/jobs/{job_id} ----

func (p *Plugin) handleGetJob(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	queueName := r.PathValue("queue_name")
	jobIDStr := r.PathValue("job_id")
	if queueName == "" || jobIDStr == "" {
		p.writeError(w, 400, "queue_name and job_id are required")
		return
	}

	jobID, err := uuid.Parse(jobIDStr)
	if err != nil {
		p.writeError(w, 400, "invalid job_id")
		return
	}

	var j JobResponse
	var payloadRaw []byte
	var startedAt, completedAt sql.NullTime

	err = p.db.QueryRow(r.Context(), plugin.Rebind(`
			SELECT job_id, queue_name, status, payload, created_at, started_at, completed_at
			FROM task_queue
			WHERE tenant_id = $1 AND queue_name = $2 AND job_id = $3
		`, p.dialect), tid, queueName, jobID).Scan(
		&j.JobID, &j.QueueName, &j.Status,
		&payloadRaw, &j.CreatedAt,
		&startedAt, &completedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		p.writeError(w, 404, "job not found")
		return
	}
	if err != nil {
		p.logger.Error("jobqueue: get job", "job_id", jobIDStr, "error", err)
		p.writeError(w, 500, "failed to get job")
		return
	}

	j.Payload = json.RawMessage(payloadRaw)
	if startedAt.Valid {
		j.StartedAt = &startedAt.Time
	}
	if completedAt.Valid {
		j.CompletedAt = &completedAt.Time
	}

	p.writeJSON(w, 200, j)
}

// ---- DELETE /jobqueue/{queue_name}/jobs/{job_id} ----

func (p *Plugin) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	queueName := r.PathValue("queue_name")
	jobIDStr := r.PathValue("job_id")
	if queueName == "" || jobIDStr == "" {
		p.writeError(w, 400, "queue_name and job_id are required")
		return
	}

	jobID, err := uuid.Parse(jobIDStr)
	if err != nil {
		p.writeError(w, 400, "invalid job_id")
		return
	}

	rows, err := p.db.Exec(r.Context(), plugin.Rebind(`
			UPDATE task_queue
			SET status = 'failed', completed_at = now()
			WHERE tenant_id = $1 AND queue_name = $2 AND job_id = $3 AND status = 'pending'
		`, p.dialect), tid, queueName, jobID)
	if err != nil {
		p.logger.Error("jobqueue: cancel job", "job_id", jobIDStr, "error", err)
		p.writeError(w, 500, "failed to cancel job")
		return
	}
	if rows == 0 {
		p.writeError(w, 404, "job not found or not pending")
		return
	}

	p.logger.Info("jobqueue: cancelled", "job_id", jobIDStr, "queue", queueName, "tenant", tid)
	w.WriteHeader(http.StatusNoContent)
}
