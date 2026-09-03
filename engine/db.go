package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/lib/pq"

	"github.com/cleat-team/cleat/monitoring/prometheus"
)

// WorkflowDef is a row from the workflow_defs table.
// It represents a deployed version of a workflow definition.

// PostgresStore implements WorkflowStore using a PostgreSQL database.
type PostgresStore struct {
	db                *sql.DB
	taskQueues        []string
	tenantID          string
	dialect           Dialect
	idempotencyKeyTTL time.Duration
	notifyChannel     string // PostgreSQL NOTIFY channel; empty = disabled

	// Encryption at rest for sensitive event payloads.
	encryption *PayloadEncryption
	// NOTE: encryption currently applies only to the per-event write path
	// (flushEvent). The batch write path (appendEventsInTx) stores events
	// in plaintext; adding encryption there would double-encrypt events
	// that flow through both paths. Until the paths are unified or
	// exclusive, full coverage requires routing all events through the per-event path.
	encryptSensitivePayloads bool
	metrics                  *prometheus.Metrics

	// disableReadRedaction when true bypasses RedactOnRead on the read path.
	// Set to true during replay to avoid the overhead of retroactive redaction.
	disableReadRedaction bool

	syncCommitOff bool // SET LOCAL synchronous_commit = off in finalize tx

	logger *slog.Logger

	Metrics *prometheus.Metrics
}

// SetSyncCommitOff sets synchronous_commit = off for finalize transactions.
func (s *PostgresStore) SetSyncCommitOff(v bool) { s.syncCommitOff = v }

// NewPostgresStore creates a PostgresStore scoped to the given task queues.
// The taskQueues slice specifies which task queues this worker pool should poll
// (e.g., "default", "gpu", "high-memory"). Defaults to ["default"].
// The tenantID defaults to the default tenant UUID from the tenant foundation migration.
func NewPostgresStore(db *sql.DB, taskQueues ...string) *PostgresStore {
	tqs := taskQueues
	if len(tqs) == 0 {
		tqs = []string{"default"}
	}
	return &PostgresStore{
		db:                db,
		taskQueues:        tqs,
		tenantID:          "00000000-0000-0000-0000-000000000000",
		dialect:           DialectPostgres,
		idempotencyKeyTTL: 720 * time.Hour,
	}
}

// WithIdempotencyKeyTTL returns a copy of the store with the given idempotency key TTL.
func (s *PostgresStore) WithIdempotencyKeyTTL(ttl time.Duration) *PostgresStore {
	cp := *s
	cp.idempotencyKeyTTL = ttl
	return &cp
}

// WithTenant returns a copy of the store scoped to the given tenant ID.
// This is used in the dispatch loop to set the correct tenant context
// before executing a workflow. The returned store's methods will set
// the RLS session variable via set_config.
func (s *PostgresStore) WithTenant(tenantID string) *PostgresStore {
	cp := *s
	cp.tenantID = tenantID
	return &cp
}

// WithLogger returns a copy of the store with the given structured logger.
func (s *PostgresStore) WithLogger(l *slog.Logger) *PostgresStore {
	cp := *s
	cp.logger = l
	return &cp
}

// log returns the configured logger or the default logger.
func (s *PostgresStore) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// WithEncryption returns a copy of the store with encryption at rest enabled.
func (s *PostgresStore) WithEncryption(enc *PayloadEncryption, enabled bool) *PostgresStore {
	cp := *s
	cp.encryption = enc
	cp.encryptSensitivePayloads = enabled
	return &cp
}

// WithReadRedactionDisabled returns a copy of the store with redaction on
// the read path disabled. Used during replay to avoid overhead.
func (s *PostgresStore) WithReadRedactionDisabled(disabled bool) *PostgresStore {
	cp := *s
	cp.disableReadRedaction = disabled
	return &cp
}

// WithNotifyChannel returns a copy of the store that sends PostgreSQL NOTIFY
// on dispatchable state changes (new workflows, released timers, signals, promises).
func (s *PostgresStore) WithNotifyChannel(channel string) *PostgresStore {
	cp := *s
	cp.notifyChannel = channel
	return &cp
}

// decryptAndRedactEventRecord decrypts sensitive event record fields (when
// encryption is enabled) and applies retroactive redaction. Decryption errors
// are logged and the field is set to "[DECRYPTION_FAILED]" so it is clear the
// data is unreadable rather than silently keeping ciphertext.
// decryptField decrypts an encrypted field value, logging on failure.
// When useBytesDecrypt is true, the value is treated as raw ciphertext
// (Decrypt); otherwise it is treated as a base64-encoded ciphertext
// (DecryptString).
func (s *PostgresStore) decryptField(encrypted, fieldName, workflowID string, step int, useBytesDecrypt bool) string {
	var decrypted string
	var err error
	if useBytesDecrypt {
		var b []byte
		b, err = s.encryption.Decrypt([]byte(encrypted))
		decrypted = string(b)
	} else {
		decrypted, err = s.encryption.DecryptString(encrypted)
	}
	if err != nil {
		s.log().WarnContext(context.Background(), "decrypt failed", "field", fieldName, "workflow_id", workflowID, "step", step, "error", err)
		if s.Metrics != nil {
			s.Metrics.RecordDecryptionError(context.Background())
		}
		return "[DECRYPTION_FAILED]"
	}
	return decrypted
}

func (s *PostgresStore) decryptAndRedactEventRecord(rec *EventRecord, workflowID string) {
	if s.encryption != nil && s.encryptSensitivePayloads {
		// Request and Response are base64-decoded by tryDecodeBase64,
		// so they hold raw ciphertext bytes and must be decrypted via Decrypt.
		rec.Request = s.decryptField(rec.Request, "Request", workflowID, rec.Step, true)
		rec.Response = s.decryptField(rec.Response, "Response", workflowID, rec.Step, true)
		// Err, SignalPayload, ChildInput, NewInput, PluginInput, PluginOutput,
		// PromiseResult, PromiseError are stored as base64-encoded ciphertexts
		// (no extra base64 layer), so DecryptString is correct.
		rec.Err = s.decryptField(rec.Err, "Err", workflowID, rec.Step, false)
		rec.SignalPayload = s.decryptField(rec.SignalPayload, "SignalPayload", workflowID, rec.Step, false)
		rec.ChildInput = s.decryptField(rec.ChildInput, "ChildInput", workflowID, rec.Step, false)
		rec.NewInput = s.decryptField(rec.NewInput, "NewInput", workflowID, rec.Step, false)
		rec.PluginInput = s.decryptField(rec.PluginInput, "PluginInput", workflowID, rec.Step, false)
		rec.PluginOutput = s.decryptField(rec.PluginOutput, "PluginOutput", workflowID, rec.Step, false)
		rec.PromiseResult = s.decryptField(rec.PromiseResult, "PromiseResult", workflowID, rec.Step, false)
		rec.PromiseError = s.decryptField(rec.PromiseError, "PromiseError", workflowID, rec.Step, false)
	}

	// Retroactive redaction on read path.
	if !s.disableReadRedaction {
		rec.Request = RedactOnRead(rec.Request)
		rec.Response = RedactOnRead(rec.Response)
		rec.Err = RedactOnRead(rec.Err)
		rec.SignalPayload = RedactOnRead(rec.SignalPayload)
		rec.ChildInput = RedactOnRead(rec.ChildInput)
		rec.NewInput = RedactOnRead(rec.NewInput)
		rec.PluginInput = RedactOnRead(rec.PluginInput)
		rec.PluginOutput = RedactOnRead(rec.PluginOutput)
		rec.PromiseResult = RedactOnRead(rec.PromiseResult)
		rec.PromiseError = RedactOnRead(rec.PromiseError)
	}
}

// decryptPayloadJSON decrypts the payload JSONB column if encryption is
// enabled and returns the decrypted (or original) payload string.
func (s *PostgresStore) decryptPayloadJSON(payloadStr string) string {
	if s.encryption != nil && s.encryptSensitivePayloads && payloadStr != "" {
		if decrypted, err := s.encryption.DecryptJSON([]byte(payloadStr)); err == nil {
			return string(decrypted)
		} else {
			s.log().WarnContext(context.Background(), "decrypt payload JSON failed", "error", err)
			if s.Metrics != nil {
				s.Metrics.RecordDecryptionError(context.Background())
			}
		}
	}
	return payloadStr
}

// setRLSOnTx executes SELECT set_config to set the RLS tenant_id
// for the given transaction. This ensures the RLS policy on tenant-scoped
// tables correctly filters rows by the current tenant. Must be called
// after BEGIN and before any tenant-scoped queries.
func (s *PostgresStore) setRLSOnTx(tx *sql.Tx) error {
	if s.tenantID == "" {
		return fmt.Errorf("setRLSOnTx: tenant ID must be set before beginning an RLS-scoped transaction")
	}
	_, err := tx.Exec("SELECT set_config('cleat.tenant_id', $1, true)", s.tenantID)
	return err
}

// beginTxWithRLS begins a transaction and sets the RLS tenant context,
// ensuring all subsequent queries in the transaction are scoped to the
// current tenant. The caller must commit or rollback the returned tx.
func (s *PostgresStore) beginTxWithRLS(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginTxWithRLS: begin tx: %w", err)
	}
	if err := s.setRLSOnTx(tx); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("set row-level security: %w", err)
	}
	return tx, nil
}

// Heartbeat renews this worker's lease on one workflow instance, fenced on
// (assigned_to, generation), and reports whether the lease still held.
//
// # The decision: wire it, not delete it (B4)
//
// This was the one per-workflow generation-checked heartbeat in the store and
// nothing called it: cmd/cleat-worker calls only BatchHeartbeat, which by its
// own doc comment does not check generation because it refreshes every
// workflow this worker holds in one statement. A generation-checked function
// nothing calls is a trap for the next reader -- it reads like a safety net
// that is actually just dead code.
//
// # Where it is actually used now
//
// Not as the primary fencing mechanism for the two hot paths B4 found
// unfenced. Postgres's per-step flush (engine/flush.go's insertEventSQL) and
// all three dialects' write-ahead-intent statements
// (engine/store_intent.go) fold the (assigned_to, generation) check directly
// into the INSERT/UPDATE's own WHERE clause instead -- see insertEventSQL's
// doc for why: an earlier version of this fix called Heartbeat as a separate
// statement before every write, and a round-trip-counting measurement
// against the real test database found that cost exactly double the round
// trips per event (8 vs 4, tenanted path) for a guarantee that was still
// only an argument about timing, not an atomic fact. Heartbeat's own SQL
// did not need to change for that rewrite; the callers did.
//
// Heartbeat is still called from three places, all of them the case where
// folding the check into the write statement was not available or not worth
// it:
//
//   - flush.go's afterFencedInsert and store_intent.go's
//     intentFenceOrNotPending call it as a *disambiguation* step, and only
//     on the rare path where a fenced write's own statement reported zero
//     rows affected -- distinguishing "the fence failed" from "the row was
//     already terminal / not pending", which are both legitimate zero-row
//     outcomes for different reasons. The common case (a row was actually
//     written) never reaches this call.
//   - engine/flush.go calls it once, upfront, before dispatching to
//     MySQLStore's/MSSQLStore's flushEventForStep (flush_dialect.go). Those
//     two dialects' per-step insert goes through appendEventsInTxOpts, a
//     function also used for genuinely unfenced multi-event batch writes
//     (FinalizeWorkflowSegment, AppendEventHistoryBatch), so folding a fence
//     predicate into its SQL would fence those other callers too; a
//     Heartbeat-before-write check, scoped to the one caller that needs it,
//     was the trade made instead. See flush_dialect.go's perStepEventFlusher
//     doc.
//   - adaptive_flush.go's partitionFencedBatch does not call this method
//     directly -- it runs the equivalent renewal for every distinct claim in
//     a batch in one query -- but exists for the same reason: a single
//     Heartbeat call cannot fence a batch spanning many workflow instances
//     at once.
//
// For the two call sites that still do a Heartbeat-before-write (unlike the
// disambiguation callers, these do incur it on every call, not just the rare
// path): a successful Heartbeat does not just check the lease, it renews it
// -- heartbeat_at = now(), unconditionally, for the row that matched. Since
// ReapStaleInstances only reclaims a workflow whose heartbeat_at predates the
// reap timeout (tens of seconds in every deployment config this repo ships),
// the window that matters is "between the renewal and the timeout elapsing",
// not "between the renewal and the write milliseconds later" -- which is why
// this is safe despite being two statements rather than one, for the two
// places it is still used that way.
func (s *PostgresStore) Heartbeat(ctx context.Context, workflowID, workerID string, generation int64) (bool, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return false, fmt.Errorf("heartbeat: begin: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET heartbeat_at = now()
		WHERE id = $1 AND assigned_to = $2 AND generation = $3
	`, workflowID, workerID, generation)
	if err != nil {
		return false, fmt.Errorf("heartbeat: %w", err)
	}
	n, _ := result.RowsAffected()
	return n > 0, tx.Commit()
}

// BatchHeartbeat updates heartbeat_at for all workflows assigned to this worker.
// NOTE: This intentionally does NOT check per-workflow generation because it
// operates on ALL running workflows for a worker, and generations differ per
// workflow. Individual generation-guarded operations (Heartbeat,
// CompleteWorkflow, FailWorkflow, etc.) prevent double-execution even if the
// batch heartbeat refreshes a stale workflow's heartbeat_at.
func (s *PostgresStore) BatchHeartbeat(ctx context.Context, workerID string) (int64, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("batch heartbeat: begin: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET heartbeat_at = now()
		WHERE assigned_to = $1 AND status = 'running'
	`, workerID)
	if err != nil {
		return 0, fmt.Errorf("batch heartbeat: %w", err)
	}
	n, _ := result.RowsAffected()
	return n, tx.Commit()
}

// CompleteWorkflow marks a workflow as done.
func (s *PostgresStore) GetQueryState(ctx context.Context, workflowID, key string) (string, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return "", fmt.Errorf("get query state: begin: %w", err)
	}
	defer tx.Rollback()

	var value sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT query_state ->> $2 FROM workflow_instances WHERE id = $1
	`, workflowID, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", tx.Commit()
	}
	if err != nil {
		return "", fmt.Errorf("get query state: %w", err)
	}
	return value.String, tx.Commit()
}

// ListWorkflows returns workflow instances filtered by the given filter parameters,
// ordered by creation time DESC. Supports search by input content, error message,
// and combined full-text search, as well as pagination via Offset/Limit.
func (s *PostgresStore) ListWorkflows(ctx context.Context, filter WorkflowFilter) ([]WorkflowInstance, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("list workflows: begin: %w", err)
	}
	defer tx.Rollback()

	d := s.dialect
	qb := NewQueryBuilder(d,
		"SELECT "+d.workflowInstanceColumns()+" FROM workflow_instances WHERE 1=1",
	)

	if filter.Status != "" {
		qb.AddCondition("status = %s", filter.Status)
	}
	if filter.InputContains != "" {
		qb.AddLikeCondition(d.castExpr("input"), "%"+filter.InputContains+"%", true)
	}
	if filter.ErrorContains != "" {
		qb.AddLikeCondition("error_msg", "%"+filter.ErrorContains+"%", true)
	}
	if filter.Search != "" {
		pattern := "%" + filter.Search + "%"
		icol := d.castExpr("input")
		rcol := d.castExpr("result")
		n := qb.NextPos()
		// Search matches the workflow's def_name in addition to its
		// input/result/error content: a general "Search" box (as opposed to
		// the more targeted InputContains/ErrorContains filters) is most
		// often used to find workflows of a given type by name, e.g. an
		// admin dashboard search box (cmd/cleat-worker/server.go passes the
		// "search" query param straight through to this filter).
		qb.AddRaw(fmt.Sprintf("AND (%s OR %s OR %s OR %s)",
			d.likeExpr(icol, n, true),
			d.likeExpr(rcol, n+1, true),
			d.likeExpr("error_msg", n+2, true),
			d.likeExpr("def_name", n+3, true)))
		qb.AddArgs(pattern, pattern, pattern, pattern)
	}

	qb.AddRaw("ORDER BY created_at DESC")

	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	} else if limit > 1000 {
		limit = 1000
	}

	if filter.Offset > 0 {
		qb.AddRaw(d.limitOffset(qb.NextPos(), qb.NextPos()+1, true))
		qb.AddArgs(limit, filter.Offset)
	} else {
		qb.AddRaw(d.limitOffset(qb.NextPos(), 0, false))
		qb.AddArgs(limit)
	}

	query, args := qb.SQL()
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	defer rows.Close()

	var workflows []WorkflowInstance
	for rows.Next() {
		var wf WorkflowInstance
		var nextWakeAt, createdAt sql.NullTime
		var assignedTo, errorCode, errorOp, errorMsg sql.NullString
		if err := rows.Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status, &wf.Input,
			&assignedTo, &nextWakeAt, &errorCode, &errorOp, &errorMsg, &createdAt, &wf.Generation, &wf.Priority, &wf.TraceID); err != nil {
			return nil, fmt.Errorf("scan workflow: %w", err)
		}
		if nextWakeAt.Valid {
			wf.NextWakeAt = nextWakeAt.Time
		}
		if createdAt.Valid {
			wf.CreatedAt = createdAt.Time
		}
		wf.AssignedTo = assignedTo.String
		wf.ErrorCode = errorCode.String
		wf.ErrorOp = errorOp.String
		wf.Error = errorMsg.String
		workflows = append(workflows, wf)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return workflows, tx.Commit()
}

// GetWorkflowByID returns a single workflow instance by ID.
func (s *PostgresStore) GetWorkflowByID(ctx context.Context, id string) (*WorkflowInstance, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("get workflow: begin: %w", err)
	}
	defer tx.Rollback()

	var wf WorkflowInstance
	var nextWakeAt, heartbeatAt, completedAt sql.NullTime
	var assignedTo, errorMsg sql.NullString
	var result sql.NullString
	var errorCode, errorOp sql.NullString
	var inputRaw json.RawMessage

	err = tx.QueryRowContext(ctx, `
		SELECT id, def_name, def_version, status, input,
		       assigned_to, heartbeat_at, next_wake_at, completed_at, result #>> '{}', error_msg, error_code, error_op,
		       generation, COALESCE(priority, 0) AS priority,
		       COALESCE(trace_id, ''), tenant_id
		FROM workflow_instances WHERE id = $1
	`, id).Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status, &inputRaw,
		&assignedTo, &heartbeatAt, &nextWakeAt, &completedAt, &result, &errorMsg, &errorCode, &errorOp,
		&wf.Generation, &wf.Priority,
		&wf.TraceID, &wf.TenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, fmt.Errorf("get workflow: %w", err)
	}
	wf.Input = inputRaw
	wf.AssignedTo = assignedTo.String
	if result.Valid {
		compacted := bytes.NewBuffer(nil)
		if err := json.Compact(compacted, []byte(result.String)); err == nil {
			result.String = compacted.String()
		}
	}
	wf.Result = result.String
	wf.Error = errorMsg.String
	wf.ErrorCode = errorCode.String
	wf.ErrorOp = errorOp.String
	if nextWakeAt.Valid {
		wf.NextWakeAt = nextWakeAt.Time
	}
	return &wf, tx.Commit()
}

// ---- Schedule methods ----

func (s *PostgresStore) CreateSchedule(ctx context.Context, sch Schedule) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("create schedule: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_schedules (name, def_name, entry_point, cron_expression, input, enabled, next_run_at, tenant_id, timezone, misfire_policy, catch_up_limit, overlap_policy)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`, sch.Name, sch.DefName, sch.EntryPoint, sch.CronExpression, sch.Input, sch.Enabled, sch.NextRunAt, s.tenantID,
		scheduleTimezoneOrDefault(sch.Timezone), MisfirePolicyOrDefault(sch.MisfirePolicy),
		CatchUpLimitOrDefault(sch.CatchUpLimit), OverlapPolicyOrDefault(sch.OverlapPolicy))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) ListSchedules(ctx context.Context) ([]Schedule, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("list schedules: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT name, def_name, entry_point, cron_expression, input, enabled, next_run_at, last_run_at, timezone, tenant_id, misfire_policy, catch_up_limit, overlap_policy, COALESCE(last_run_id, '')
		FROM workflow_schedules WHERE tenant_id = $1 ORDER BY name
	`, s.tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var schedules []Schedule
	for rows.Next() {
		var sch Schedule
		var lastRunAt sql.NullTime
		if err := rows.Scan(&sch.Name, &sch.DefName, &sch.EntryPoint, &sch.CronExpression,
			&sch.Input, &sch.Enabled, &sch.NextRunAt, &lastRunAt, &sch.Timezone, &sch.TenantID,
			&sch.MisfirePolicy, &sch.CatchUpLimit, &sch.OverlapPolicy, &sch.LastRunID); err != nil {
			return nil, err
		}
		if lastRunAt.Valid {
			sch.LastRunAt = &lastRunAt.Time
		}
		schedules = append(schedules, sch)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return schedules, tx.Commit()
}

func (s *PostgresStore) DeleteSchedule(ctx context.Context, name string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("delete schedule: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `DELETE FROM workflow_schedules WHERE name = $1 AND tenant_id = $2`, name, s.tenantID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) SetScheduleEnabled(ctx context.Context, name string, enabled bool) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("set schedule enabled: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_schedules SET enabled = $2 WHERE name = $1 AND tenant_id = $3
	`, name, enabled, s.tenantID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) GetDueSchedules(ctx context.Context) ([]Schedule, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("get due schedules: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT name, def_name, entry_point, cron_expression, input, enabled, next_run_at, last_run_at, timezone, tenant_id, misfire_policy, catch_up_limit, overlap_policy, COALESCE(last_run_id, '')
		FROM workflow_schedules
		WHERE enabled = true AND next_run_at <= now() AND tenant_id = $1
		FOR UPDATE SKIP LOCKED
	`, s.tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Shared with GetDueSchedulesAcrossTenants, whose column list lives in
	// migrations/postgres/024_cross_tenant_schedules.sql. One scan is the only
	// thing keeping that function's RETURNS TABLE in step with this code.
	schedules, err := scanDueSchedules(rows)
	if err != nil {
		return nil, err
	}
	return schedules, tx.Commit()
}

func (s *PostgresStore) UpdateScheduleNextRun(ctx context.Context, name string, nextRun time.Time) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("update schedule next run: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_schedules SET next_run_at = $2, last_run_at = now() WHERE name = $1 AND tenant_id = $3
	`, name, nextRun, s.tenantID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// CompactHistory deletes old events and saves compaction state for a workflow.
func (s *PostgresStore) CompactHistory(ctx context.Context, workflowID string, compactionState []byte, compactionStep int, keepStep int) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("compact history: begin: %w", err)
	}
	defer tx.Rollback()

	// Read current generation for optimistic locking.
	var gen int64
	err = tx.QueryRowContext(ctx, `SELECT generation FROM workflow_instances WHERE id = $1`, workflowID).Scan(&gen)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return tx.Commit() // Workflow no longer exists.
		}
		return fmt.Errorf("compact history: get generation: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		DELETE FROM event_history WHERE workflow_id = $1 AND step < $2
	`, workflowID, keepStep)
	if err != nil {
		return fmt.Errorf("compact history: delete: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET compaction_state = $1, compacted_at = now(), compaction_step = $2
		WHERE id = $3 AND generation = $4
	`, compactionState, compactionStep, workflowID, gen)
	if err != nil {
		return fmt.Errorf("compact history: update: %w", err)
	}

	return tx.Commit()
}

// GetCompactionCandidates returns workflow IDs that need compaction.
func (s *PostgresStore) GetCompactionCandidates(ctx context.Context, threshold int, limit int) ([]string, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("get compaction candidates: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT w.id
		FROM workflow_instances w
		JOIN (
			SELECT workflow_id, COUNT(*) AS cnt
			FROM event_history
			GROUP BY workflow_id
		) e ON w.id = e.workflow_id
		WHERE e.cnt > $1
		  AND (w.compaction_step IS NULL OR w.compaction_step < e.cnt - $1)
		ORDER BY e.cnt DESC
		LIMIT $2
	`, threshold, limit)
	if err != nil {
		return nil, fmt.Errorf("get compaction candidates: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan compaction candidate: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, tx.Commit()
}

// LoadCompactionState loads the compaction state JSON for a workflow instance.
func (s *PostgresStore) LoadCompactionState(ctx context.Context, workflowID string) (*CompactionState, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("load compaction state: begin: %w", err)
	}
	defer tx.Rollback()

	var rawJSON []byte
	err = tx.QueryRowContext(ctx, `
		SELECT compaction_state FROM workflow_instances
		WHERE id = $1
	`, workflowID).Scan(&rawJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, fmt.Errorf("load compaction state: %w", err)
	}
	if rawJSON == nil {
		return nil, tx.Commit()
	}
	var cs CompactionState
	if err := json.Unmarshal(rawJSON, &cs); err != nil {
		return nil, fmt.Errorf("unmarshal compaction state: %w", err)
	}
	return &cs, tx.Commit()
}

// ---- PromiseStore interface implementation ----

// CreatePromise creates a new promise for a workflow instance.
func (s *PostgresStore) AcquireConcurrencyKey(ctx context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire concurrency key: begin: %w", err)
	}
	defer tx.Rollback()

	// Delete expired keys for this key hash within the current tenant.
	_, err = tx.ExecContext(ctx, `DELETE FROM concurrency_keys WHERE key_hash = digest($1, 'sha256') AND expires_at < now() AND tenant_id = $2`, key, s.tenantID)
	if err != nil {
		return false, fmt.Errorf("acquire concurrency key: delete expired: %w", err)
	}

	// Try to insert. ON CONFLICT DO NOTHING means if the key_hash already exists,
	// the RETURNING clause returns no rows.
	// make_interval(secs => <float>) rather than fmt.Sprintf("%d seconds",
	// int(ttl.Seconds())).
	//
	// That truncated: a 500 ms TTL became "0 seconds", so the key was born
	// expired and the next caller took it -- two workflows holding the same
	// mutual-exclusion key, with nothing logged. Sub-second is not an exotic
	// input here: the guest API is specified in milliseconds
	// (engine/locking.go passes time.Duration(ttlMs)*time.Millisecond), so
	// truncating to whole seconds contradicts the contract callers are
	// written against. IMPROVEMENT-PLAN 3.34.
	var returnedWorkflowID string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at, tenant_id)
		VALUES (digest($1, 'sha256'), $1, $2, now() + make_interval(secs => $3), $4)
		ON CONFLICT (key_hash) DO NOTHING
		RETURNING workflow_id
	`, key, workflowID, ttl.Seconds(), s.tenantID).Scan(&returnedWorkflowID)

	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("acquire concurrency key: %w", err)
	}
	return true, tx.Commit()
}

// ReleaseConcurrencyKey releases a specific concurrency key.
func (s *PostgresStore) ReleaseConcurrencyKey(ctx context.Context, key string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("release concurrency key: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `DELETE FROM concurrency_keys WHERE key_hash = digest($1, 'sha256') AND tenant_id = $2`, key, s.tenantID)
	if err != nil {
		return fmt.Errorf("release concurrency key: %w", err)
	}
	return tx.Commit()
}

// ReleaseWorkflowConcurrencyKeys releases all concurrency keys held by a workflow.
func (s *PostgresStore) ReleaseWorkflowConcurrencyKeys(ctx context.Context, workflowID string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("release workflow concurrency keys: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `DELETE FROM concurrency_keys WHERE workflow_id = $1 AND tenant_id = $2`, workflowID, s.tenantID)
	if err != nil {
		return fmt.Errorf("release workflow concurrency keys: %w", err)
	}
	return tx.Commit()
}

// ReapExpiredConcurrencyKeys deletes all expired concurrency keys
// for the current tenant. Returns the number of keys deleted.
func (s *PostgresStore) ReapExpiredConcurrencyKeys(ctx context.Context) (int64, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("reap expired concurrency keys: begin: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `DELETE FROM concurrency_keys WHERE expires_at < now() AND tenant_id = $1`, s.tenantID)
	if err != nil {
		return 0, fmt.Errorf("reap expired concurrency keys: %w", err)
	}
	n, _ := result.RowsAffected()
	return n, tx.Commit()
}

// GetConcurrencyKeyCount returns the number of non-expired concurrency keys
// held by the given workflow.
func (s *PostgresStore) GetConcurrencyKeyCount(ctx context.Context, workflowID string) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("get concurrency key count for %s: begin: %w", workflowID, err)
	}
	defer tx.Rollback()

	var count int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM concurrency_keys
		WHERE workflow_id = $1 AND expires_at > now() AND tenant_id = $2
	`, workflowID, s.tenantID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get concurrency key count for %s: %w", workflowID, err)
	}
	return count, tx.Commit()
}

// GetEventCount returns the event_count for a workflow instance.
func (s *PostgresStore) GetEventCount(ctx context.Context, workflowID string) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("get event count for %s: begin: %w", workflowID, err)
	}
	defer tx.Rollback()

	var count int
	err = tx.QueryRowContext(ctx, `SELECT event_count FROM workflow_instances WHERE id = $1`, workflowID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get event count for %s: %w", workflowID, err)
	}
	return count, tx.Commit()
}

// ---- Sticky Session implementations (Feature 10) ----

// UpdateStickyWorker sets the sticky worker for a workflow.
func (s *PostgresStore) UpdateStickyWorker(ctx context.Context, workflowID, workerID string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("update sticky worker: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances SET sticky_worker_id = $2 WHERE id = $1
	`, workflowID, workerID)
	if err != nil {
		return fmt.Errorf("update sticky worker: %w", err)
	}
	return tx.Commit()
}

// ClearStickyWorker removes the sticky worker assignment.
func (s *PostgresStore) ClearStickyWorker(ctx context.Context, workflowID string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("clear sticky worker: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances SET sticky_worker_id = NULL WHERE id = $1
	`, workflowID)
	if err != nil {
		return fmt.Errorf("clear sticky worker: %w", err)
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// Update Request methods (Feature 3: Update Handler)
// ---------------------------------------------------------------------------

// CreateUpdateRequest registers an incoming update request for a workflow.
func (s *PostgresStore) RecordWorkflowMemorySample(ctx context.Context, defName string, sampleBytes int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record memory sample: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO workflow_memory_samples (def_name, sample_bytes) VALUES ($1, $2)`,
		defName, sampleBytes)
	if err != nil {
		return fmt.Errorf("record memory sample: insert sample: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_memory_stats (def_name, mean_bytes, sample_count, updated_at)
		VALUES ($1, $2, 1, now())
		ON CONFLICT (def_name) DO UPDATE SET
			mean_bytes   = (workflow_memory_stats.alpha * $2 + (1 - workflow_memory_stats.alpha) * workflow_memory_stats.mean_bytes),
			sample_count = workflow_memory_stats.sample_count + 1,
			updated_at   = now()
	`, defName, float64(sampleBytes))
	if err != nil {
		return fmt.Errorf("record memory sample: upsert stats: %w", err)
	}

	return tx.Commit()
}

// LoadMemoryEstimates returns EWMA mean bytes for all def_names.
func (s *PostgresStore) LoadMemoryEstimates(ctx context.Context) (map[string]float64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT def_name, mean_bytes FROM workflow_memory_stats`)
	if err != nil {
		return nil, fmt.Errorf("load memory estimates: %w", err)
	}
	defer rows.Close()

	estimates := make(map[string]float64)
	for rows.Next() {
		var name string
		var mean float64
		if err := rows.Scan(&name, &mean); err != nil {
			return nil, fmt.Errorf("load memory estimates: scan: %w", err)
		}
		estimates[name] = mean
	}
	return estimates, rows.Err()
}

// LoadMemoryStats returns full distribution statistics for all def_names.
func (s *PostgresStore) LoadMemoryStats(ctx context.Context) ([]WorkflowMemoryStats, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT def_name,
		       MIN(sample_bytes)::BIGINT,
		       AVG(sample_bytes),
		       MAX(sample_bytes)::BIGINT,
		       COALESCE(percentile_cont(0.10) WITHIN GROUP (ORDER BY sample_bytes)::BIGINT, 0),
		       COALESCE(percentile_cont(0.25) WITHIN GROUP (ORDER BY sample_bytes)::BIGINT, 0),
		       COALESCE(percentile_cont(0.50) WITHIN GROUP (ORDER BY sample_bytes)::BIGINT, 0),
		       COALESCE(percentile_cont(0.75) WITHIN GROUP (ORDER BY sample_bytes)::BIGINT, 0),
		       COALESCE(percentile_cont(0.90) WITHIN GROUP (ORDER BY sample_bytes)::BIGINT, 0),
		       COALESCE(percentile_cont(0.99) WITHIN GROUP (ORDER BY sample_bytes)::BIGINT, 0),
		       COUNT(*)::INTEGER
		FROM workflow_memory_samples
		GROUP BY def_name
		ORDER BY def_name
	`)
	if err != nil {
		return nil, fmt.Errorf("load memory stats: %w", err)
	}
	defer rows.Close()

	var stats []WorkflowMemoryStats
	for rows.Next() {
		var st WorkflowMemoryStats
		if err := rows.Scan(&st.DefName, &st.MinBytes, &st.AvgBytes, &st.MaxBytes,
			&st.P10, &st.P25, &st.P50, &st.P75, &st.P90, &st.P99, &st.SampleCount); err != nil {
			return nil, fmt.Errorf("load memory stats: scan: %w", err)
		}
		stats = append(stats, st)
	}
	return stats, rows.Err()
}

// QueueDepth returns the count of ready workflows in the store's task queues.
func (s *PostgresStore) QueueDepth(ctx context.Context) (int64, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("queue depth: begin: %w", err)
	}
	defer tx.Rollback()

	var count int64
	err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workflow_instances WHERE status = 'ready' AND task_queue = ANY($1)`,
		pq.Array(s.taskQueues)).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("queue depth: %w", err)
	}
	return count, tx.Commit()
}

// CleanupMemorySamples deletes samples beyond maxSamplesPerDef per def_name.
func (s *PostgresStore) CleanupMemorySamples(ctx context.Context, maxSamplesPerDef int) (int64, error) {
	defRows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT def_name FROM workflow_memory_samples`)
	if err != nil {
		return 0, fmt.Errorf("cleanup memory samples: list defs: %w", err)
	}
	defer defRows.Close()

	var defNames []string
	for defRows.Next() {
		var name string
		if err := defRows.Scan(&name); err != nil {
			return 0, fmt.Errorf("cleanup memory samples: scan def: %w", err)
		}
		defNames = append(defNames, name)
	}
	if err := defRows.Err(); err != nil {
		return 0, err
	}

	var totalDeleted int64
	for _, defName := range defNames {
		result, err := s.db.ExecContext(ctx, `
			DELETE FROM workflow_memory_samples
			WHERE def_name = $1
			  AND id NOT IN (
			      SELECT id FROM workflow_memory_samples
			      WHERE def_name = $1
			      ORDER BY recorded_at DESC
			      LIMIT $2
			  )
		`, defName, maxSamplesPerDef)
		if err != nil {
			return totalDeleted, fmt.Errorf("cleanup memory samples: delete %s: %w", defName, err)
		}
		n, _ := result.RowsAffected()
		totalDeleted += n
	}
	return totalDeleted, nil
}

// DeleteExpiredEvents deletes event history rows for completed/failed workflows
// whose completed_at is older than the cutoff. It uses batching to avoid
// locking the event_history table when there are millions of rows to delete.
func (s *PostgresStore) DeleteExpiredEvents(ctx context.Context, olderThan time.Time) (int64, error) {
	var totalDeleted int64
	for {
		tx, err := s.beginTxWithRLS(ctx)
		if err != nil {
			return totalDeleted, fmt.Errorf("delete expired events: begin: %w", err)
		}
		result, err := tx.ExecContext(ctx, `
			DELETE FROM event_history
			WHERE workflow_id IN (
				SELECT id FROM workflow_instances
				WHERE status IN ('done', 'failed')
				  AND completed_at IS NOT NULL
				  AND completed_at < $1
				LIMIT 10000
			)
		`, olderThan)
		if err != nil {
			_ = tx.Rollback()
			return totalDeleted, fmt.Errorf("delete expired events: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return totalDeleted, fmt.Errorf("delete expired events: commit: %w", err)
		}
		n, _ := result.RowsAffected()
		totalDeleted += n
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Also batch cleanup compaction states for those workflows.
	for {
		tx, err := s.beginTxWithRLS(ctx)
		if err != nil {
			return totalDeleted, fmt.Errorf("delete expired events: begin compaction: %w", err)
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE workflow_instances
			SET compaction_state = NULL, compaction_step = NULL, compacted_at = NULL
			WHERE id IN (
				SELECT id FROM workflow_instances
				WHERE status IN ('done', 'failed')
				  AND completed_at IS NOT NULL
				  AND completed_at < $1
				  AND compaction_state IS NOT NULL
				LIMIT 10000
			)
		`, olderThan)
		if err != nil {
			_ = tx.Rollback()
			break
		}
		if err := tx.Commit(); err != nil {
			break
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	return totalDeleted, nil
}

// TerminateWorkflow force-terminates a workflow, setting status to 'terminated'.
// Unlike FailWorkflow, this does not require the worker to own the workflow.
func (s *PostgresStore) TerminateWorkflow(ctx context.Context, workflowID, reason string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("terminate workflow: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'terminated',
		    error_msg = $2,
		    completed_at = now(),
		    assigned_to = NULL,
		    generation = generation + 1
		WHERE id = $1
	`, workflowID, reason)
	if err != nil {
		return fmt.Errorf("terminate workflow: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("terminate workflow commit: %w", err)
	}
	releaseWorkflowResources(s.log(), s, workflowID)
	// IMPROVEMENT-PLAN 3.79. Terminate is a terminal transition, and the close
	// policy is what stops a closed parent leaving orphans behind. Every other
	// terminal path enforces it -- FinalizeWorkflowSegment for done/failed, and
	// adminForceResolve, which is an operator verb on an unclaimed workflow
	// exactly like this one. This path did not, so terminating a parent left
	// its TERMINATE children running while force-completing the same parent
	// failed them, with nothing recording why the two differed.
	s.enforceParentClosePolicy(context.Background(), workflowID)
	return nil
}

// DeleteDeadLetteredWorkflows permanently deletes dead-lettered workflow instances
// whose completed_at is older than the cutoff.
//
// Two bugs were found and fixed here together (both discovered verifying
// Stream I / Finding S3's tenant-deletion work, which shares this function's
// FK-graph question):
//
//  1. The previous version ran its DELETE on s.db directly -- the plain
//     pool, with no RLS context set. workflow_instances carries
//     `FORCE ROW LEVEL SECURITY` with a fail-closed policy
//     (cleat.assert_tenant_set()), so under a real RLS-enforcing connection
//     (any role that is not a superuser and does not own the table -- e.g.
//     cleat_app in production) that statement does not silently do nothing:
//     it raises "cleat.tenant_id is not set" and the whole call errors.
//     Verified directly against a real cleat_rls_test_role connection: the
//     old query, run as that role with the tenant_id predicate satisfied but
//     no set_config call preceding it, fails with exactly that error. Fixed
//     by running inside beginTxWithRLS, which calls setRLSOnTx before any
//     query -- the same pattern every other tenant-scoped method here uses.
//  2. The doc comment claimed child rows -- "event_history, signals,
//     promises, concurrency_keys, update_requests" -- are "automatically
//     deleted via ON DELETE CASCADE". True for four of the five, but
//     migrations/postgres/003_procedures.sql deliberately DROPs the FK from
//     event_history to workflow_instances ("no longer needed; events are
//     deleted on terminal") because finalize_workflow_status() deletes a
//     workflow's events itself when it reaches 'done' or 'failed'.
//     MoveToDeadLetterQueue does not call finalize_workflow_status -- it
//     does a plain UPDATE ... SET status = 'dead_lettered' -- so a
//     dead-lettered workflow's event_history rows are never deleted there
//     either. Verified directly: seeding a dead_lettered workflow with one
//     event_history row and running the old DELETE FROM workflow_instances
//     query left that event_history row in place, orphaned (no
//     workflow_instances row it can still join to) and undeletable by any
//     later call to this function, since it only ever looks at
//     workflow_instances.status. Fixed by deleting event_history explicitly,
//     by the same batch of IDs, in the same transaction.
func (s *PostgresStore) DeleteDeadLetteredWorkflows(ctx context.Context, olderThan time.Time) (int64, error) {
	var totalDeleted int64
	for {
		n, err := s.deleteDeadLetteredWorkflowsBatch(ctx, olderThan)
		if err != nil {
			return totalDeleted, err
		}
		totalDeleted += n
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return totalDeleted, nil
}

// deleteDeadLetteredWorkflowsBatch deletes up to 10000 dead-lettered
// workflow instances (and their orphan-prone event_history rows) in a
// single RLS-scoped transaction, returning how many workflow_instances rows
// were removed.
func (s *PostgresStore) deleteDeadLetteredWorkflowsBatch(ctx context.Context, olderThan time.Time) (int64, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("delete dead-lettered workflows: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM workflow_instances
		WHERE status = 'dead_lettered'
		  AND completed_at IS NOT NULL
		  AND completed_at < $1
		  AND tenant_id = $2
		ORDER BY id
		LIMIT 10000
	`, olderThan, s.tenantID)
	if err != nil {
		return 0, fmt.Errorf("delete dead-lettered workflows: select batch: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("delete dead-lettered workflows: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("delete dead-lettered workflows: rows: %w", err)
	}
	rows.Close()

	if len(ids) == 0 {
		return 0, tx.Commit()
	}

	// event_history has no FK to workflow_instances (see the doc comment
	// above) -- must be deleted explicitly or these rows are orphaned the
	// moment the workflow_instances row below is gone.
	if _, err := tx.ExecContext(ctx, `DELETE FROM event_history WHERE workflow_id = ANY($1)`, pq.Array(ids)); err != nil {
		return 0, fmt.Errorf("delete dead-lettered workflows: delete event_history: %w", err)
	}

	result, err := tx.ExecContext(ctx, `DELETE FROM workflow_instances WHERE id = ANY($1)`, pq.Array(ids))
	if err != nil {
		return 0, fmt.Errorf("delete dead-lettered workflows: delete instances: %w", err)
	}
	n, _ := result.RowsAffected()
	return n, tx.Commit()
}

// DeleteCompletedWorkflows permanently deletes workflow_instances rows in a
// terminal, no-further-action status ('done', 'failed', 'terminated') whose
// completed_at is older than the cutoff. See the interface doc
// (store_interface.go) for why 'dead_lettered' is deliberately excluded --
// it has its own lifecycle and its own deletion path.
//
// This is Finding S2: DeleteExpiredEvents deletes event_history for
// 'done'/'failed' workflows but never touches the workflow_instances row
// itself, so that table was unbounded by anything but lifetime workflow
// count. This is the method that actually reclaims it.
//
// Follows deleteDeadLetteredWorkflowsBatch's pattern exactly, including the
// same FK-graph fix: event_history has no FK back to workflow_instances on
// PostgreSQL (dropped deliberately by migrations/postgres/003_procedures.sql
// because finalize_workflow_status deletes a 'done'/'failed' workflow's
// events itself) so it must be deleted explicitly here rather than assumed
// to cascade. That assumption is also wrong for 'terminated' workflows on
// this dialect specifically: TerminateWorkflow does not call
// finalize_workflow_status, so a force-terminated workflow's events are
// never deleted by any other path either.
func (s *PostgresStore) DeleteCompletedWorkflows(ctx context.Context, olderThan time.Time) (int64, error) {
	var totalDeleted int64
	for {
		n, err := s.deleteCompletedWorkflowsBatch(ctx, olderThan)
		if err != nil {
			return totalDeleted, err
		}
		totalDeleted += n
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return totalDeleted, nil
}

// deleteCompletedWorkflowsBatch deletes up to 10000 terminal workflow
// instances (and their orphan-prone event_history rows) in a single
// RLS-scoped transaction, returning how many workflow_instances rows were
// removed.
func (s *PostgresStore) deleteCompletedWorkflowsBatch(ctx context.Context, olderThan time.Time) (int64, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("delete completed workflows: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM workflow_instances
		WHERE status IN ('done', 'failed', 'terminated')
		  AND completed_at IS NOT NULL
		  AND completed_at < $1
		  AND tenant_id = $2
		ORDER BY id
		LIMIT 10000
	`, olderThan, s.tenantID)
	if err != nil {
		return 0, fmt.Errorf("delete completed workflows: select batch: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("delete completed workflows: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("delete completed workflows: rows: %w", err)
	}
	rows.Close()

	if len(ids) == 0 {
		return 0, tx.Commit()
	}

	// event_history has no FK to workflow_instances on this dialect (see the
	// doc comment above) -- must be deleted explicitly or these rows are
	// orphaned the moment the workflow_instances row below is gone.
	if _, err := tx.ExecContext(ctx, `DELETE FROM event_history WHERE workflow_id = ANY($1)`, pq.Array(ids)); err != nil {
		return 0, fmt.Errorf("delete completed workflows: delete event_history: %w", err)
	}

	result, err := tx.ExecContext(ctx, `DELETE FROM workflow_instances WHERE id = ANY($1)`, pq.Array(ids))
	if err != nil {
		return 0, fmt.Errorf("delete completed workflows: delete instances: %w", err)
	}
	n, _ := result.RowsAffected()
	return n, tx.Commit()
}

// tryDecodeBase64 attempts to base64-decode s. If decoding fails (e.g. the
// value is a legacy plaintext that was never encoded), it returns s as-is.
// This provides backward compatibility for events stored before base64
// encoding was introduced.
func tryDecodeBase64(s string) string {
	if s == "" {
		return s
	}
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return s // not base64-encoded, return raw string
	}
	return string(decoded)
}

// tryEncodeBase64 is a symmetric counterpart to tryDecodeBase64.  It encodes
// s as base64 so that values read back through tryDecodeBase64 are restored
// correctly.  Call sites that previously stored plain text can switch to this
// function: the old (un-encoded) values are still handled by tryDecodeBase64's
// fallback, and new values are properly round-tripped.
func tryEncodeBase64(s string) string {
	if s == "" {
		return s
	}
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// PostgresStoreFactory implements StoreFactory for PostgreSQL.
type PostgresStoreFactory struct {
	db                *sql.DB
	schemaName        string
	idempotencyKeyTTL time.Duration
	notifyChannel     string // PostgreSQL NOTIFY channel; empty = disabled

	encryption               *PayloadEncryption
	encryptSensitivePayloads bool
	metrics                  *prometheus.Metrics
	syncCommitOff            bool

	logger *slog.Logger
}

// WithSyncCommitOff sets synchronous_commit = off for finalize transactions.
func (f *PostgresStoreFactory) WithSyncCommitOff(v bool) *PostgresStoreFactory {
	f.syncCommitOff = v
	return f
}

// NewPostgresStoreFactory creates a PostgresStoreFactory.
// The db connection must already be open. schemaName is the PostgreSQL
// schema for cleat tables (defaults to "public").
func NewPostgresStoreFactory(db *sql.DB, schemaName string, idempotencyKeyTTL ...time.Duration) *PostgresStoreFactory {
	if schemaName == "" {
		schemaName = "public"
	}
	ttl := 720 * time.Hour
	if len(idempotencyKeyTTL) > 0 {
		ttl = idempotencyKeyTTL[0]
	}
	return &PostgresStoreFactory{
		db:                db,
		schemaName:        schemaName,
		idempotencyKeyTTL: ttl,
	}
}

// WithEncryption sets encryption at rest on the factory. When enabled,
// sensitive payload fields are encrypted before being written to the database.
func (f *PostgresStoreFactory) WithEncryption(enc *PayloadEncryption, enabled bool) *PostgresStoreFactory {
	f.encryption = enc
	f.encryptSensitivePayloads = enabled
	return f
}

// WithNotifyChannel sets the PostgreSQL NOTIFY channel for dispatch wake-up.
// When non-empty, OpenStore configures the returned PostgresStore to send
// pg_notify on dispatchable state changes.
func (f *PostgresStoreFactory) WithNotifyChannel(channel string) *PostgresStoreFactory {
	f.notifyChannel = channel
	return f
}

// WithMetrics sets the metrics instance on the factory. Stores created by
// OpenStore will inherit it.
func (f *PostgresStoreFactory) WithMetrics(m *prometheus.Metrics) *PostgresStoreFactory {
	f.metrics = m
	return f
}

// WithLogger sets the structured logger on the factory. Stores created by
// OpenStore will inherit it.
func (f *PostgresStoreFactory) WithLogger(l *slog.Logger) *PostgresStoreFactory {
	f.logger = l
	return f
}

// OpenStore creates a PostgresStore scoped to the given tenant and task queues.
func (f *PostgresStoreFactory) OpenStore(ctx context.Context, tenantID string, taskQueues ...string) (WorkflowStore, io.Closer, error) {
	// Ensure the schema exists.
	if f.schemaName != "" && f.schemaName != "public" {
		if _, err := f.db.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS `+pq.QuoteIdentifier(f.schemaName)); err != nil {
			return nil, nil, fmt.Errorf("create schema %s: %w", f.schemaName, err)
		}
	}
	store := NewPostgresStore(f.db, taskQueues...)
	store.tenantID = tenantID
	if f.encryption != nil && f.encryptSensitivePayloads {
		store = store.WithEncryption(f.encryption, true)
	}
	store = store.WithIdempotencyKeyTTL(f.idempotencyKeyTTL)
	store = store.WithLogger(f.logger)
	if f.notifyChannel != "" {
		store = store.WithNotifyChannel(f.notifyChannel)
	}
	if f.metrics != nil {
		store.Metrics = f.metrics
	}
	store.syncCommitOff = f.syncCommitOff
	return store, nopCloser{}, nil
}

// DriverName returns "postgres".
func (f *PostgresStoreFactory) DriverName() string { return "postgres" }

// Dialect returns DialectPostgres.
func (f *PostgresStoreFactory) Dialect() Dialect { return DialectPostgres }

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// ClaimDueSchedule advances a schedule's next_run_at, but only if it still
// holds expectedNextRun. See the interface doc for why this is a CAS.
func (s *PostgresStore) ClaimDueSchedule(ctx context.Context, name string, expectedNextRun, newNextRun time.Time, runID string) (bool, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return false, fmt.Errorf("claim due schedule: begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_schedules
		SET next_run_at = $2, last_run_at = now(),
		    last_run_id = CASE WHEN $5 = '' THEN last_run_id ELSE $5 END
		WHERE name = $1 AND tenant_id = $4 AND next_run_at = $3
	`, name, newNextRun, expectedNextRun, s.tenantID, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n == 1, nil
}
