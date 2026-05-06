package host

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// WorkflowInstance is a row from workflow_instances.
type WorkflowInstance struct {
	ID         string
	DefName    string
	DefVersion int
	Status     string
	Input      json.RawMessage
	AssignedTo string
	NextWakeAt time.Time
}

// WorkflowStore is the database interface for the worker.
type WorkflowStore interface {
	// ClaimWorkflow atomically dequeues a runnable workflow instance.
	// Uses SELECT ... FOR UPDATE SKIP LOCKED.
	ClaimWorkflow(ctx context.Context, workerID string) (*WorkflowInstance, error)

	// LoadEventHistory returns the full event history for a workflow.
	LoadEventHistory(ctx context.Context, workflowID string) ([]EventRecord, error)

	// AppendEventHistory appends a single event to the history.
	// Uses ON CONFLICT (workflow_id, step) DO NOTHING for idempotency.
	AppendEventHistory(ctx context.Context, workflowID string, rec EventRecord) error

	// AppendEventHistoryBatch appends multiple events atomically.
	AppendEventHistoryBatch(ctx context.Context, workflowID string, recs []EventRecord) error

	// LoadWASM returns the compiled WASM bytes for a workflow definition.
	LoadWASM(ctx context.Context, defName string, defVersion int) ([]byte, error)

	// ListVersions returns all deployed versions of a workflow.
	ListVersions(ctx context.Context, defName string) ([]int, error)

	// Heartbeat updates the heartbeat timestamp to prevent timeout.
	// Returns false if the workflow is no longer assigned to this worker.
	Heartbeat(ctx context.Context, workflowID, workerID string) (bool, error)

	// CompleteWorkflow marks a workflow as completed with a result.
	CompleteWorkflow(ctx context.Context, workflowID, workerID, result string) error

	// FailWorkflow marks a workflow as failed.
	FailWorkflow(ctx context.Context, workflowID, workerID, errMsg string) error

	// ReleaseWorkflow returns a workflow to the ready queue.
	// Used when a workflow suspends (sleep/await signals).
	ReleaseWorkflow(ctx context.Context, workflowID, workerID string, nextWakeAt time.Time) error

	// RequestCancellation sets the cancellation flag on a workflow.
	RequestCancellation(ctx context.Context, workflowID, reason string) error

	// CheckCancellation checks if a workflow has been cancelled.
	CheckCancellation(ctx context.Context, workflowID string) (cancelled bool, reason string, err error)

	// DeliverSignal stores a signal for a workflow.
	DeliverSignal(ctx context.Context, workflowID, signalName, payload string) error

	// PollAndClaimSignal atomically checks for and claims a pending signal.
	PollAndClaimSignal(ctx context.Context, workflowID, signalName string) (payload string, found bool, err error)

	// StartNewRun creates a new workflow instance (for ContinueAsNew and child workflows).
	StartNewRun(ctx context.Context, defName string, defVersion int, input json.RawMessage) (runID string, err error)
}

// PostgresStore implements WorkflowStore using a PostgreSQL database.
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore creates a PostgresStore.
func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// ClaimWorkflow atomically claims a runnable workflow using SKIP LOCKED.
func (s *PostgresStore) ClaimWorkflow(ctx context.Context, workerID string) (*WorkflowInstance, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("claim: begin tx: %w", err)
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx, `
		UPDATE workflow_instances
		SET status = 'running',
		    assigned_to = $2,
		    heartbeat_at = now()
		WHERE id = (
			SELECT id FROM workflow_instances
			WHERE status = 'ready'
			  AND next_wake_at <= now()
			ORDER BY created_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, def_name, def_version, status, input, assigned_to, next_wake_at
	`, workerID, workerID) // first $1 is not used in subquery

	// The above won't work directly because the subquery can't reference $1.
	// Let me use a cleaner approach.
	_ = row
	tx.Rollback()

	// Correct approach: use a CTE or direct UPDATE.
	return s.claimWorkflowImpl(ctx, workerID)
}

func (s *PostgresStore) claimWorkflowImpl(ctx context.Context, workerID string) (*WorkflowInstance, error) {
	var wf WorkflowInstance
	var nextWakeAt sql.NullTime

	err := s.db.QueryRowContext(ctx, `
		UPDATE workflow_instances
		SET status = 'running',
		    assigned_to = $1,
		    heartbeat_at = now()
		WHERE id = (
			SELECT id FROM workflow_instances
			WHERE status = 'ready'
			  AND next_wake_at <= now()
			ORDER BY created_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, def_name, def_version, status, input, assigned_to, next_wake_at
	`, workerID).Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status, &wf.Input,
		&wf.AssignedTo, &nextWakeAt)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim workflow: %w", err)
	}

	if nextWakeAt.Valid {
		wf.NextWakeAt = nextWakeAt.Time
	}
	return &wf, nil
}

// LoadEventHistory returns all event records for a workflow, ordered by step.
func (s *PostgresStore) LoadEventHistory(ctx context.Context, workflowID string) ([]EventRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT step, event_type, service, operation, request, response, error,
		       duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
		       defer_description, defer_id, child_name, child_input, run_id, new_input
		FROM event_history
		WHERE workflow_id = $1
		ORDER BY step
	`, workflowID)
	if err != nil {
		return nil, fmt.Errorf("load history: %w", err)
	}
	defer rows.Close()

	var history []EventRecord
	for rows.Next() {
		var rec EventRecord
		var service, op, request, response, errMsg sql.NullString
		var durationMs, timeoutMs sql.NullInt64
		var signalNames, signalName, signalPayload sql.NullString
		var deferDesc, deferID sql.NullString
		var childName, childInput, runID, newInput sql.NullString

		if err := rows.Scan(&rec.Step, &rec.EventType,
			&service, &op, &request, &response, &errMsg,
			&durationMs, &signalNames, &timeoutMs, &signalName, &signalPayload,
			&deferDesc, &deferID, &childName, &childInput, &runID, &newInput); err != nil {
			return nil, fmt.Errorf("scan history: %w", err)
		}

		rec.Service = service.String
		rec.Op = op.String
		rec.Request = request.String
		rec.Response = response.String
		rec.Err = errMsg.String
		rec.DurationMs = durationMs.Int64
		rec.SignalNames = signalNames.String
		rec.TimeoutMs = timeoutMs.Int64
		rec.SignalName = signalName.String
		rec.SignalPayload = signalPayload.String
		rec.DeferDescription = deferDesc.String
		rec.DeferID = deferID.String
		rec.ChildName = childName.String
		rec.ChildInput = childInput.String
		rec.RunID = runID.String
		rec.NewInput = newInput.String

		history = append(history, rec)
	}
	return history, rows.Err()
}

// AppendEventHistoryBatch appends multiple events to the history.
func (s *PostgresStore) AppendEventHistoryBatch(ctx context.Context, workflowID string, recs []EventRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("append history batch: begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO event_history (workflow_id, step, event_type, service, operation, request, response, error,
			duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
			defer_description, defer_id, child_name, child_input, run_id, new_input)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
		ON CONFLICT (workflow_id, step) DO NOTHING
	`)
	if err != nil {
		return fmt.Errorf("append history batch: prepare: %w", err)
	}
	defer stmt.Close()

	for _, rec := range recs {
		_, err := stmt.ExecContext(ctx, workflowID, rec.Step, rec.EventType,
			nullStr(rec.Service), nullStr(rec.Op), nullStr(rec.Request), nullStr(rec.Response), nullStr(rec.Err),
			nullInt64(rec.DurationMs), nullStr(rec.SignalNames), nullInt64(rec.TimeoutMs),
			nullStr(rec.SignalName), nullStr(rec.SignalPayload),
			nullStr(rec.DeferDescription), nullStr(rec.DeferID),
			nullStr(rec.ChildName), nullStr(rec.ChildInput), nullStr(rec.RunID), nullStr(rec.NewInput))
		if err != nil {
			return fmt.Errorf("append history batch: exec step %d: %w", rec.Step, err)
		}
	}
	return tx.Commit()
}

// AppendEventHistory appends a single event to the history.
func (s *PostgresStore) AppendEventHistory(ctx context.Context, workflowID string, rec EventRecord) error {
	return s.AppendEventHistoryBatch(ctx, workflowID, []EventRecord{rec})
}

// LoadWASM returns the WASM bytes for a workflow definition.
func (s *PostgresStore) LoadWASM(ctx context.Context, defName string, defVersion int) ([]byte, error) {
	var wasmBytes []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT wasm_bytes FROM workflow_defs WHERE name = $1 AND version = $2
	`, defName, defVersion).Scan(&wasmBytes)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("wasm not found: %s v%d", defName, defVersion)
	}
	if err != nil {
		return nil, fmt.Errorf("load wasm: %w", err)
	}
	return wasmBytes, nil
}

// ListVersions returns all deployed versions of a workflow.
func (s *PostgresStore) ListVersions(ctx context.Context, defName string) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT version FROM workflow_defs WHERE name = $1 ORDER BY version DESC
	`, defName)
	if err != nil {
		return nil, fmt.Errorf("list versions: %w", err)
	}
	defer rows.Close()

	var versions []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		versions = append(versions, v)
	}
	return versions, rows.Err()
}

// Heartbeat updates the heartbeat timestamp.
func (s *PostgresStore) Heartbeat(ctx context.Context, workflowID, workerID string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET heartbeat_at = now()
		WHERE id = $1 AND assigned_to = $2
	`, workflowID, workerID)
	if err != nil {
		return false, fmt.Errorf("heartbeat: %w", err)
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// CompleteWorkflow marks a workflow as done.
func (s *PostgresStore) CompleteWorkflow(ctx context.Context, workflowID, workerID, result string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'done', result = $3, completed_at = now(), assigned_to = NULL
		WHERE id = $1 AND assigned_to = $2
	`, workflowID, workerID, result)
	return err
}

// FailWorkflow marks a workflow as failed.
func (s *PostgresStore) FailWorkflow(ctx context.Context, workflowID, workerID, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'failed', error_msg = $3, completed_at = now(), assigned_to = NULL
		WHERE id = $1 AND assigned_to = $2
	`, workflowID, workerID, errMsg)
	return err
}

// ReleaseWorkflow returns a workflow to the queue with a next wake time.
func (s *PostgresStore) ReleaseWorkflow(ctx context.Context, workflowID, workerID string, nextWakeAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, next_wake_at = $3
		WHERE id = $1 AND assigned_to = $2
	`, workflowID, workerID, nextWakeAt)
	return err
}

// RequestCancellation sets the cancellation flag.
func (s *PostgresStore) RequestCancellation(ctx context.Context, workflowID, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET cancellation_requested = true, cancellation_reason = $2
		WHERE id = $1
	`, workflowID, reason)
	return err
}

// CheckCancellation checks if a workflow has been cancelled.
func (s *PostgresStore) CheckCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	var cancelled bool
	var reason sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT cancellation_requested, cancellation_reason
		FROM workflow_instances WHERE id = $1
	`, workflowID).Scan(&cancelled, &reason)
	if err != nil {
		return false, "", err
	}
	return cancelled, reason.String, nil
}
// PollAndClaimSignal atomically checks for and claims a pending signal.
func (s *PostgresStore) PollAndClaimSignal(ctx context.Context, workflowID, signalName string) (string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()

	var payload string
	err = tx.QueryRowContext(ctx, `
		DELETE FROM workflow_signals
		WHERE workflow_id = $1 AND signal_name = $2
		RETURNING payload
	`, workflowID, signalName).Scan(&payload)
	if err == sql.ErrNoRows {
		return "", false, tx.Rollback()
	}
	if err != nil {
		return "", false, fmt.Errorf("poll signal: %w", err)
	}
	return payload, true, tx.Commit()
}

// StartNewRun creates a new workflow instance.
func (s *PostgresStore) StartNewRun(ctx context.Context, defName string, defVersion int, input json.RawMessage) (string, error) {
	var runID string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input)
		VALUES (gen_random_uuid(), $1, $2, 'ready', $3)
		RETURNING id
	`, defName, defVersion, input).Scan(&runID)
	if err != nil {
		return "", fmt.Errorf("start new run: %w", err)
	}
	return runID, nil
}

// ---- SignalStore interface implementation ----

// DeliverSignal satisfies the SignalStore interface.
func (s *PostgresStore) DeliverSignal(ctx context.Context, workflowID, signalName, payload string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_signals (workflow_id, signal_name, payload)
		VALUES ($1, $2, $3)
		ON CONFLICT (workflow_id, signal_name) DO UPDATE SET payload = $3, delivered_at = now()
	`, workflowID, signalName, payload)
	return err
}

// PollSignal satisfies the SignalStore interface by checking for a delivered signal.
func (s *PostgresStore) PollSignal(ctx context.Context, workflowID, signalName string) (string, bool, error) {
	return s.PollAndClaimSignal(ctx, workflowID, signalName)
}

// PollCancellation satisfies the SignalStore interface.
func (s *PostgresStore) PollCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	return s.CheckCancellation(ctx, workflowID)
}

// nullStr returns a sql.NullString that is valid if s is non-empty.
func nullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// nullInt64 returns a sql.NullInt64 that is valid if v is non-zero.
func nullInt64(v int64) sql.NullInt64 {
	return sql.NullInt64{Int64: v, Valid: v != 0}
}
