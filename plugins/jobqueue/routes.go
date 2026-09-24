package jobqueue

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// RegisterRoutes registers the job queue HTTP handlers on the given mux.
func (p *Plugin) RegisterRoutes(mux plugin.Router) error {
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

// JobResponse is the JSON shape returned for a single job.
type JobResponse struct {
	JobID     uuid.UUID `json:"job_id"`
	QueueName string    `json:"queue_name"`

	// Status is one of seven values, in the order a job can reach them:
	//
	//	pending       -> enqueued, not yet claimed
	//	running       -> claimed by a worker, being processed
	//	dispatched    -> the workflow it names was STARTED. Outcome unknown.
	//	completed     -> the workflow finished successfully (ObserveFinalize)
	//	failed        -> the workflow finished unsuccessfully (ObserveFinalize),
	//	                 OR StartWorkflow itself errored and no run ever existed
	//	dead_lettered -> the workflow exhausted its retries and was moved to
	//	                 the dead-letter queue (ObserveFinalize) -- distinct
	//	                 from "failed" because it is redrivable and "failed" is
	//	                 not. cleat#1976; before it this bucketed into "failed"
	//	                 indistinguishably, when it reached ObserveFinalize at
	//	                 all (it did not, prior to the same issue).
	//	abandoned     -> the run is gone and no outcome was ever recorded (the
	//	                 abandonment sweep, background.go) -- not "completed",
	//	                 "failed" or "dead_lettered", because none of those is
	//	                 a claim this plugin can support once the run itself
	//	                 cannot be asked
	//
	// A workflow that was cancelled or force-terminated (an operator action,
	// not a workflow-authored outcome) also reaches ObserveFinalize as of
	// cleat#1976 and is recorded here as "failed" -- task_queue has no
	// separate lifecycle for those two, unlike workflow_instances.
	//
	// UNTIL cleat#1715, "dispatched" did not exist: a job whose workflow ran
	// to completion and one whose workflow failed on its first line both read
	// "completed" the moment StartWorkflow returned, because that write
	// happened at DISPATCH, not at the workflow's own end. See RunID's
	// comment for how that stayed invisible even once run_id existed.
	Status      string          `json:"status"`
	Payload     json.RawMessage `json:"payload"`
	CreatedAt   time.Time       `json:"created_at"`
	StartedAt   *time.Time      `json:"started_at,omitempty"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`

	// RunID names the workflow run this job started, or is empty when it
	// started none.
	//
	// THE COLUMN EXISTED AND NOTHING SELECTED IT. The dispatcher has written
	// task_queue.run_id since the column was added, and no query read it back
	// and no response carried it -- so the link from a job to its run existed
	// in the database and was reachable through no API at all. cleat#1715.
	//
	// That is what made a job's status unfalsifiable from outside. Before
	// this field was exposed, a job whose workflow failed and one whose
	// workflow did the work both read `"status": "completed"`, because status
	// was written when the run was STARTED, and the only field that could
	// have distinguished them was not returned. Exposing it made the claim
	// checkable; Status's own comment above is the rest of cleat#1715, the
	// fix the checkable claim turned out to need.
	//
	// Empty rather than null-typed: a job with no def_name never dispatches a
	// workflow, and "no run" is not an error or an unknown. omitempty keeps it
	// out of those responses instead of showing a null the reader has to
	// interpret.
	RunID string `json:"run_id,omitempty"`
}

// ---- POST /jobqueue/{queue_name}/jobs ----

func (p *Plugin) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, 401, "tenant required")
		return
	}

	queueName := r.PathValue("queue_name")
	if queueName == "" {
		p.writeError(w, 400, "queue_name is required")
		return
	}

	var req enqueueRequest
	if !plugin.ReadJSONBody(w, r, &req) {
		return
	}

	jobID := uuid.New()

	var defName *string
	if req.DefName != "" {
		defName = &req.DefName
	}

	// plugin.JSONColumn.Value, not req.Payload/req.Input directly: go-mssqldb
	// maps a bare []byte arg to VARBINARY, which corrupts the NVARCHAR
	// payload/input columns on write -- a 200 with an empty body on the next
	// read, because encoding/json fails part-way through writing the
	// response. See plugin.JSONColumn. cleat#2206.
	_, err := p.db.Exec(r.Context(), plugin.Rebind(`
			INSERT INTO task_queue (tenant_id, queue_name, job_id, payload, def_name, input)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, p.dialect), tid, queueName, jobID, plugin.JSONColumn{Raw: req.Payload}, defName, plugin.JSONColumn{Raw: req.Input})
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
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
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
			SELECT job_id, queue_name, status, payload, created_at, started_at, completed_at, run_id
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
	// plugin.LimitClause, not a literal "LIMIT $N": SQL Server has no LIMIT,
	// and this endpoint answered every list request with a 500,
	// "Incorrect syntax near 'LIMIT'" -- the same bug #2191 and #2198 already
	// fixed at the other list endpoints, missed here. cleat#2206.
	query += " " + plugin.LimitClause(fmt.Sprintf("$%d", argIdx), p.dialect)
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
			payloadCol  plugin.JSONColumn
			startedAt   sql.NullTime
			completedAt sql.NullTime
			runID       sql.NullString
		)
		if err := plugin.ScanRow(rows,
			&j.JobID, &j.QueueName, &j.Status,
			&payloadCol, &j.CreatedAt,
			&startedAt, &completedAt, &runID,
		); err != nil {
			p.logger.Error("jobqueue: scan row", "error", err)
			continue
		}
		j.Payload = payloadCol.Raw
		if startedAt.Valid {
			j.StartedAt = &startedAt.Time
		}
		if completedAt.Valid {
			j.CompletedAt = &completedAt.Time
		}
		j.RunID = runID.String
		jobs = append(jobs, j)
	}

	if jobs == nil {
		jobs = []JobResponse{}
	}

	p.writeJSON(w, 200, jobs)
}

// ---- GET /jobqueue/{queue_name}/jobs/{job_id} ----

func (p *Plugin) handleGetJob(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
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
	var payloadCol plugin.JSONColumn
	var startedAt, completedAt sql.NullTime
	var runID sql.NullString

	err = plugin.ScanRow(p.db.QueryRow(r.Context(), plugin.Rebind(`
			SELECT job_id, queue_name, status, payload, created_at, started_at, completed_at, run_id
			FROM task_queue
			WHERE tenant_id = $1 AND queue_name = $2 AND job_id = $3
		`, p.dialect), tid, queueName, jobID),
		&j.JobID, &j.QueueName, &j.Status,
		&payloadCol, &j.CreatedAt,
		&startedAt, &completedAt, &runID,
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

	j.Payload = payloadCol.Raw
	if startedAt.Valid {
		j.StartedAt = &startedAt.Time
	}
	if completedAt.Valid {
		j.CompletedAt = &completedAt.Time
	}
	j.RunID = runID.String

	p.writeJSON(w, 200, j)
}

// ---- DELETE /jobqueue/{queue_name}/jobs/{job_id} ----

func (p *Plugin) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
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
