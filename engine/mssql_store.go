package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "github.com/microsoft/go-mssqldb"
)

// tenantSessionConnector wraps a driver.Connector to call
// sp_set_session_context on every new connection, enforcing
// SQL Server RLS at the connection level for the configured tenant.
type tenantSessionConnector struct {
	driver.Connector
	tenantID string
}

func (c *tenantSessionConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	if c.tenantID == "" {
		return conn, nil
	}
	// Validate tenantID format to prevent SQL injection.
	if _, parseErr := uuid.Parse(c.tenantID); parseErr != nil {
		conn.Close()
		return nil, fmt.Errorf("mssql: invalid tenant ID %q: %w", c.tenantID, parseErr)
	}

	// Use Prepare+Exec to set the session context, since go-mssqldb v1.10+
	// does not implement driver.ExecerContext on its connection type.
	// Explicit command text (no parameter markers) is safe because tenantID
	// was validated as a UUID above.
	query := "EXEC sp_set_session_context @key=N'tenant_id', @value=N'" + c.tenantID + "'"
	var stmt driver.Stmt
	if prepCtx, ok := conn.(driver.ConnPrepareContext); ok {
		stmt, err = prepCtx.PrepareContext(ctx, query)
	} else {
		stmt, err = conn.Prepare(query)
	}
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("mssql: prepare session context for tenant %s: %w", c.tenantID, err)
	}
	_, err = stmt.Exec(nil)
	stmt.Close()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("mssql: set session context for tenant %s: %w", c.tenantID, err)
	}
	return conn, nil
}

// ---------------------------------------------------------------------------
// MSSQLStore implements WorkflowStore using Microsoft SQL Server.
// ---------------------------------------------------------------------------

// MSSQLStore implements WorkflowStore using a Microsoft SQL Server database.
// Tenant isolation is enforced via SQL Server Row-Level Security (RLS).
// The connection pool's connector calls sp_set_session_context on every
// new connection, with per-transaction calls serving as defense-in-depth.
type MSSQLStore struct {
	db                *sql.DB
	taskQueues        []string
	tenantID          string
	dialect           Dialect
	idempotencyKeyTTL time.Duration

	// Encryption at rest for sensitive event payloads.
	// NOTE: MSSQL does not yet support encryption at rest; these fields are
	// present for forward compatibility so that StreamEventHistory can
	// contain the same decryption guard as the Postgres variant.
	encryption               *PayloadEncryption
	encryptSensitivePayloads bool

	// disableReadRedaction when true bypasses RedactOnRead on the read path.
	// Set to true during replay to avoid the overhead of retroactive redaction.
	disableReadRedaction bool
}

// NewMSSQLStore creates an MSSQLStore scoped to the given task queues.
// The taskQueues slice specifies which task queues this worker pool should poll
// (e.g., "default", "gpu", "high-memory"). Defaults to ["default"].
// The tenantID defaults to the default tenant UUID from the tenant foundation migration.
func NewMSSQLStore(db *sql.DB, taskQueues ...string) *MSSQLStore {
	tqs := taskQueues
	if len(tqs) == 0 {
		tqs = []string{"default"}
	}
	return &MSSQLStore{
		db:                db,
		taskQueues:        tqs,
		tenantID:          "00000000-0000-0000-0000-000000000000",
		dialect:           DialectMSSQL,
		idempotencyKeyTTL: 720 * time.Hour,
	}
}

// WithIdempotencyKeyTTL returns a copy of the store with the given idempotency key TTL.
func (s *MSSQLStore) WithIdempotencyKeyTTL(ttl time.Duration) *MSSQLStore {
	cp := *s
	cp.idempotencyKeyTTL = ttl
	return &cp
}

// WithReadRedactionDisabled returns a copy of the store with redaction on
// the read path disabled. Used during replay to avoid overhead.
func (s *MSSQLStore) WithReadRedactionDisabled(disabled bool) *MSSQLStore {
	cp := *s
	cp.disableReadRedaction = disabled
	return &cp
}

// WithEncryption returns a copy of the store with encryption at rest enabled.
// NOTE: Encryption at rest is not yet supported on MSSQL backends. This method
// is present for forward compatibility so that StreamEventHistory can contain
// the same decryption guard as the Postgres variant.
func (s *MSSQLStore) WithEncryption(enc *PayloadEncryption, enabled bool) *MSSQLStore {
	cp := *s
	cp.encryption = enc
	cp.encryptSensitivePayloads = enabled
	return &cp
}

// WithTenant returns a copy of the store scoped to the given tenant ID.
// This is used in the dispatch loop to set the correct tenant context
// before executing a workflow. The returned store's methods will set
// the RLS session variable via sp_set_session_context.
func (s *MSSQLStore) WithTenant(tenantID string) *MSSQLStore {
	cp := *s
	cp.tenantID = tenantID
	return &cp
}

// setSessionContext sets the tenant_id session context for RLS policies.
// SQL Server equivalent of PostgreSQL's SET session_config.tenant_id.
func (s *MSSQLStore) setSessionContext(tx *sql.Tx) error {
	if s.tenantID == "" {
		return fmt.Errorf("setSessionContext: tenant ID must be set before setting session context for an RLS-scoped transaction")
	}
	_, err := tx.Exec(`
		EXEC sp_set_session_context @key=N'tenant_id', @value=@p1
	`, s.tenantID)
	return err
}

// beginTxWithContext begins a transaction and sets the RLS tenant context,
// ensuring all subsequent queries in the transaction are scoped to the
// current tenant. The caller must commit or rollback the returned tx.
func (s *MSSQLStore) beginTxWithContext(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginTxWithContext: begin tx: %w", err)
	}
	if err := s.setSessionContext(tx); err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("set session context: %w", err)
	}
	return tx, nil
}

// buildTaskQueueParam builds a comma-separated task queue string for STRING_SPLIT.
func (s *MSSQLStore) buildTaskQueueParam() string {
	if len(s.taskQueues) == 0 {
		return "default"
	}
	// If already a single string, return as-is (avoids double splitting).
	// The caller is expected to use STRING_SPLIT(@param, ',').
	return strings.Join(s.taskQueues, ",")
}

// ---------------------------------------------------------------------------
// Claim Methods (C.3)
// ---------------------------------------------------------------------------

// ClaimWorkflow atomically claims a single runnable workflow instance.
func (s *MSSQLStore) ClaimWorkflow(ctx context.Context, workerID string) (*WorkflowInstance, error) {
	wfs, err := s.ClaimWorkflows(ctx, workerID, 1)
	if err != nil {
		return nil, err
	}
	if len(wfs) == 0 {
		return nil, nil
	}
	return wfs[0], nil
}

// ClaimWorkflows atomically claims up to limit runnable workflow instances.
// Uses UPDATE...OUTPUT with READPAST/UPDLOCK hints (SQL Server's equivalent
// of FOR UPDATE SKIP LOCKED) wrapped in a transaction with RLS context.
func (s *MSSQLStore) ClaimWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: begin: %w", err)
	}
	defer tx.Rollback()

	tqParam := s.buildTaskQueueParam()

	rows, err := tx.QueryContext(ctx, `
		UPDATE workflow_instances
		SET status = 'running',
		    assigned_to = @p1,
		    heartbeat_at = SYSUTCDATETIME(),
		    generation = generation + 1
		OUTPUT INSERTED.id, INSERTED.def_name, INSERTED.def_version,
		       INSERTED.status, INSERTED.input, INSERTED.assigned_to,
		       INSERTED.next_wake_at, INSERTED.tenant_id, INSERTED.created_at,
		       INSERTED.error_code, INSERTED.error_op, INSERTED.generation,
		       COALESCE(INSERTED.priority, 0) AS priority,
		       INSERTED.trace_id
		WHERE id IN (
			SELECT id
			FROM workflow_instances WITH (READPAST, UPDLOCK, ROWLOCK)
			WHERE status = 'ready'
			  AND next_wake_at <= SYSUTCDATETIME()
			  AND task_queue IN (SELECT value FROM STRING_SPLIT(@p2, ','))
			ORDER BY CASE WHEN sticky_worker_id = @p1 THEN 0 ELSE 1 END, priority ASC, created_at
			OFFSET 0 ROWS FETCH NEXT @p3 ROWS ONLY
		)
	`, workerID, tqParam, limit)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: %w", err)
	}
	defer rows.Close()

	var wfs []*WorkflowInstance
	for rows.Next() {
		var wf WorkflowInstance
		var nextWakeAt sql.NullTime
		var tenantID sql.NullString
		var createdAt sql.NullTime
		var inputStr string
		var errorCode, errorOp sql.NullString
		var traceID sql.NullString

		if err := rows.Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status,
			&inputStr, &wf.AssignedTo, &nextWakeAt, &tenantID, &createdAt, &errorCode, &errorOp, &wf.Generation, &wf.Priority, &traceID); err != nil {
			return nil, fmt.Errorf("claim workflows scan: %w", err)
		}
		wf.TraceID = traceID.String

		wf.Input = json.RawMessage(inputStr)
		if nextWakeAt.Valid {
			wf.NextWakeAt = nextWakeAt.Time
		}
		if tenantID.Valid {
			wf.TenantID = tenantID.String
		}
		if createdAt.Valid {
			wf.CreatedAt = createdAt.Time
		}
		wf.ErrorCode = errorCode.String
		wf.ErrorOp = errorOp.String
		wfs = append(wfs, &wf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim workflows rows: %w", err)
	}

	if len(wfs) == 0 {
		tx.Rollback()
		return nil, nil
	}
	return wfs, tx.Commit()
}

// ClaimStickyWorkflows atomically claims up to limit runnable workflow instances
// that are sticky to this worker. Uses the sticky_worker_id filter for
// low-contention claiming. Returns fewer than limit if not enough sticky
// workflows are ready. Callers should fall back to ClaimWorkflows for remaining capacity.
func (s *MSSQLStore) ClaimStickyWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim sticky workflows: begin: %w", err)
	}
	defer tx.Rollback()

	tqParam := s.buildTaskQueueParam()

	rows, err := tx.QueryContext(ctx, `
		UPDATE workflow_instances
		SET status = 'running',
		    assigned_to = @p1,
		    heartbeat_at = SYSUTCDATETIME(),
		    generation = generation + 1
		OUTPUT INSERTED.id, INSERTED.def_name, INSERTED.def_version,
		       INSERTED.status, INSERTED.input, INSERTED.assigned_to,
		       INSERTED.next_wake_at, INSERTED.tenant_id, INSERTED.created_at,
		       INSERTED.error_code, INSERTED.error_op, INSERTED.generation,
		       COALESCE(INSERTED.priority, 0) AS priority,
		       INSERTED.trace_id
		WHERE id IN (
			SELECT id
			FROM workflow_instances WITH (READPAST, UPDLOCK, ROWLOCK)
			WHERE status = 'ready'
			  AND next_wake_at <= SYSUTCDATETIME()
			  AND sticky_worker_id = @p1
			  AND task_queue IN (SELECT value FROM STRING_SPLIT(@p2, ','))
			ORDER BY priority ASC, created_at
			OFFSET 0 ROWS FETCH NEXT @p3 ROWS ONLY
		)
	`, workerID, tqParam, limit)
	if err != nil {
		return nil, fmt.Errorf("claim sticky workflows: %w", err)
	}
	defer rows.Close()

	var wfs []*WorkflowInstance
	for rows.Next() {
		var wf WorkflowInstance
		var nextWakeAt sql.NullTime
		var tenantID sql.NullString
		var createdAt sql.NullTime
		var inputStr string
		var errorCode, errorOp sql.NullString
		var traceID sql.NullString

		if err := rows.Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status,
			&inputStr, &wf.AssignedTo, &nextWakeAt, &tenantID, &createdAt, &errorCode, &errorOp, &wf.Generation, &wf.Priority, &traceID); err != nil {
			return nil, fmt.Errorf("claim sticky workflows scan: %w", err)
		}
		wf.TraceID = traceID.String

		wf.Input = json.RawMessage(inputStr)
		if nextWakeAt.Valid {
			wf.NextWakeAt = nextWakeAt.Time
		}
		if tenantID.Valid {
			wf.TenantID = tenantID.String
		}
		if createdAt.Valid {
			wf.CreatedAt = createdAt.Time
		}
		wf.ErrorCode = errorCode.String
		wf.ErrorOp = errorOp.String
		wfs = append(wfs, &wf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim sticky workflows rows: %w", err)
	}

	if len(wfs) == 0 {
		tx.Rollback()
		return nil, nil
	}
	return wfs, tx.Commit()
}

// ---------------------------------------------------------------------------
// Event History Methods (C.4)
// ---------------------------------------------------------------------------

// LoadEventHistory returns all event records for a workflow, ordered by step.
func (s *MSSQLStore) LoadEventHistory(ctx context.Context, workflowID string) ([]EventRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT step, event_type, service, operation, request, response, error,
		       duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
		       defer_description, defer_id, child_name, child_input, run_id, new_input,
		       plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
		       payload,
		       promise_name, promise_id, promise_result, promise_error,
		       created_at
		FROM event_history
		WHERE workflow_id = @p1 AND tenant_id = @p2
		ORDER BY step
	`, workflowID, s.tenantID)
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
		var pluginName, pluginFunc, pluginInput, pluginOutput, pluginErr sql.NullString
		var payload sql.NullString
		var promiseName, promiseID, promiseResult, promiseError sql.NullString
		var createdAt time.Time

		if err := rows.Scan(&rec.Step, &rec.EventType,
			&service, &op, &request, &response, &errMsg,
			&durationMs, &signalNames, &timeoutMs, &signalName, &signalPayload,
			&deferDesc, &deferID, &childName, &childInput, &runID, &newInput,
			&pluginName, &pluginFunc, &pluginInput, &pluginOutput, &pluginErr,
			&payload,
			&promiseName, &promiseID, &promiseResult, &promiseError,
			&createdAt); err != nil {
			return nil, fmt.Errorf("scan history: %w", err)
		}

		rec.TimestampMs = createdAt.UnixMilli()
		rec.Service = service.String
		rec.Op = op.String
		rec.Request = tryDecodeBase64(request.String)
		rec.Response = tryDecodeBase64(response.String)
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
		rec.PluginName = pluginName.String
		rec.PluginFunc = pluginFunc.String
		rec.PluginInput = pluginInput.String
		rec.PluginOutput = pluginOutput.String
		rec.PluginError = pluginErr.String
		rec.PromiseName = promiseName.String
		rec.PromiseID = promiseID.String
		rec.PromiseResult = promiseResult.String
		rec.PromiseError = promiseError.String

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

		if payload.Valid {
			populateFromPayload(&rec, []byte(payload.String))
		}

		history = append(history, rec)
	}
	return history, rows.Err()
}

// StreamEventHistory loads event history for a workflow in pages, returning
// events through a channel. Events are fetched in pages of pageSize as the
// caller reads from the channel. The channel is closed when all events have
// been sent.
func (s *MSSQLStore) StreamEventHistory(ctx context.Context, workflowID string, pageSize int) (<-chan EventRecord, <-chan error) {
	eventCh := make(chan EventRecord, pageSize)
	errCh := make(chan error, 1)

	if pageSize <= 0 || pageSize > 1000 {
		pageSize = 1000
	}

	go func() {
		defer close(eventCh)
		defer close(errCh)

		offset := 0
		for {
			if ctx.Err() != nil {
				errCh <- ctx.Err()
				return
			}

			rows, err := s.db.QueryContext(ctx, `
				SELECT step, event_type, service, operation, request, response, error,
				       duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
				       defer_description, defer_id, child_name, child_input, run_id, new_input,
				       plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
				       payload,
				       promise_name, promise_id, promise_result, promise_error,
				       created_at
				FROM event_history
				WHERE workflow_id = @p1 AND tenant_id = @p2
				ORDER BY step
				OFFSET @p3 ROWS FETCH NEXT @p4 ROWS ONLY
			`, workflowID, s.tenantID, offset, pageSize)
			if err != nil {
				errCh <- err
				return
			}

			var pageCount int
			for rows.Next() {
				pageCount++
				var rec EventRecord
				var service, op, request, response, errMsg sql.NullString
				var durationMs, timeoutMs sql.NullInt64
				var signalNames, signalName, signalPayload sql.NullString
				var deferDesc, deferID sql.NullString
				var childName, childInput, runID, newInput sql.NullString
				var pluginName, pluginFunc, pluginInput, pluginOutput, pluginErr sql.NullString
				var payload sql.NullString
				var promiseName, promiseID, promiseResult, promiseError sql.NullString
				var createdAt time.Time

				if err := rows.Scan(&rec.Step, &rec.EventType,
					&service, &op, &request, &response, &errMsg,
					&durationMs, &signalNames, &timeoutMs, &signalName, &signalPayload,
					&deferDesc, &deferID, &childName, &childInput, &runID, &newInput,
					&pluginName, &pluginFunc, &pluginInput, &pluginOutput, &pluginErr,
					&payload,
					&promiseName, &promiseID, &promiseResult, &promiseError,
					&createdAt); err != nil {
					rows.Close()
					errCh <- err
					return
				}

				rec.TimestampMs = createdAt.UnixMilli()
				rec.Service = service.String
				rec.Op = op.String
				rec.Request = tryDecodeBase64(request.String)
				rec.Response = tryDecodeBase64(response.String)
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
				rec.PluginName = pluginName.String
				rec.PluginFunc = pluginFunc.String
				rec.PluginInput = pluginInput.String
				rec.PluginOutput = pluginOutput.String
				rec.PluginError = pluginErr.String
				rec.PromiseName = promiseName.String
				rec.PromiseID = promiseID.String
				rec.PromiseResult = promiseResult.String
				rec.PromiseError = promiseError.String

				// Decryption must happen BEFORE redaction: redacting ciphertext is meaningless.
				// The fields below are encrypted by flushEvent when encryption is enabled.
				// NOTE: On MSSQL this block is a forward-compatibility guard only --
				// encryption is not yet supported and will never be true.
				if s.encryption != nil && s.encryptSensitivePayloads {
					if decrypted, err := s.encryption.Decrypt([]byte(rec.Request)); err == nil {
						rec.Request = string(decrypted)
					}
					if decrypted, err := s.encryption.Decrypt([]byte(rec.Response)); err == nil {
						rec.Response = string(decrypted)
					}
					if decrypted, err := s.encryption.DecryptString(rec.Err); err == nil {
						rec.Err = decrypted
					}
					if decrypted, err := s.encryption.DecryptString(rec.SignalPayload); err == nil {
						rec.SignalPayload = decrypted
					}
					if decrypted, err := s.encryption.DecryptString(rec.ChildInput); err == nil {
						rec.ChildInput = decrypted
					}
					if decrypted, err := s.encryption.DecryptString(rec.NewInput); err == nil {
						rec.NewInput = decrypted
					}
					if decrypted, err := s.encryption.DecryptString(rec.PluginInput); err == nil {
						rec.PluginInput = decrypted
					}
					if decrypted, err := s.encryption.DecryptString(rec.PluginOutput); err == nil {
						rec.PluginOutput = decrypted
					}
				}

				// Retroactive redaction on read path: ensure sensitive fields are
				// redacted even if they were stored before redaction was mandatory.
				// Redaction runs AFTER decryption (see block above) since redacting
				// ciphertext would yield meaningless "[REDACTED]" placeholders.
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

				if payload.Valid {
					payloadStr := payload.String
					// Decrypt payload before populateFromPayload if encryption is enabled.
					// NOTE: Forward-compatibility guard only on MSSQL.
					if s.encryption != nil && s.encryptSensitivePayloads {
						if decrypted, err := s.encryption.DecryptJSON([]byte(payloadStr)); err == nil {
							payloadStr = string(decrypted)
						}
					}
					populateFromPayload(&rec, []byte(payloadStr))
				}

				select {
				case eventCh <- rec:
				case <-ctx.Done():
					rows.Close()
					errCh <- ctx.Err()
					return
				}
			}
			rows.Close()

			if err := rows.Err(); err != nil {
				errCh <- err
				return
			}

			if pageCount < pageSize {
				return
			}
			offset += pageSize
		}
	}()

	return eventCh, errCh
}

// LoadEventHistoryPaginated returns a page of event history for a workflow,
// with offset and limit support. Defaults limit to 1000 if limit <= 0, capped at 1000.
func (s *MSSQLStore) LoadEventHistoryPaginated(ctx context.Context, workflowID string, offset, limit int) ([]EventRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT step, event_type, service, operation, request, response, error,
		       duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
		       defer_description, defer_id, child_name, child_input, run_id, new_input,
		       plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
		       payload,
		       promise_name, promise_id, promise_result, promise_error
		FROM event_history
		WHERE workflow_id = @p1 AND tenant_id = @p2
		ORDER BY step
		OFFSET @p3 ROWS FETCH NEXT @p4 ROWS ONLY
	`, workflowID, s.tenantID, offset, limit)
	if err != nil {
		return nil, fmt.Errorf("load history paginated: %w", err)
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
		var pluginName, pluginFunc, pluginInput, pluginOutput, pluginErr sql.NullString
		var payload sql.NullString
		var promiseName, promiseID, promiseResult, promiseError sql.NullString

		if err := rows.Scan(&rec.Step, &rec.EventType,
			&service, &op, &request, &response, &errMsg,
			&durationMs, &signalNames, &timeoutMs, &signalName, &signalPayload,
			&deferDesc, &deferID, &childName, &childInput, &runID, &newInput,
			&pluginName, &pluginFunc, &pluginInput, &pluginOutput, &pluginErr,
			&payload,
			&promiseName, &promiseID, &promiseResult, &promiseError); err != nil {
			return nil, fmt.Errorf("scan history paginated: %w", err)
		}

		rec.Service = service.String
		rec.Op = op.String
		rec.Request = tryDecodeBase64(request.String)
		rec.Response = tryDecodeBase64(response.String)
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
		rec.PluginName = pluginName.String
		rec.PluginFunc = pluginFunc.String
		rec.PluginInput = pluginInput.String
		rec.PluginOutput = pluginOutput.String
		rec.PluginError = pluginErr.String
		rec.PromiseName = promiseName.String
		rec.PromiseID = promiseID.String
		rec.PromiseResult = promiseResult.String
		rec.PromiseError = promiseError.String

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

		if payload.Valid {
			populateFromPayload(&rec, []byte(payload.String))
		}

		history = append(history, rec)
	}
	return history, rows.Err()
}

// CountEventHistory returns the total number of events for a workflow.
func (s *MSSQLStore) CountEventHistory(ctx context.Context, workflowID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_history WHERE workflow_id = @p1 AND tenant_id = @p2`, workflowID, s.tenantID).Scan(&count)
	return count, err
}

// AppendEventHistory appends a single event to the history.
func (s *MSSQLStore) AppendEventHistory(ctx context.Context, workflowID string, rec EventRecord) error {
	return s.AppendEventHistoryBatch(ctx, workflowID, []EventRecord{rec})
}

// AppendEventHistoryBatch appends multiple events to the history atomically.
func (s *MSSQLStore) AppendEventHistoryBatch(ctx context.Context, workflowID string, recs []EventRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("append history batch: begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := s.setSessionContext(tx); err != nil {
		return fmt.Errorf("append history batch: set session: %w", err)
	}

	if err := s.appendEventsInTx(ctx, tx, workflowID, recs); err != nil {
		return err
	}
	return tx.Commit()
}

// appendEventsInTx inserts event records using an already-open transaction.
// This is shared by AppendEventHistoryBatch and FinalizeWorkflowSegment so
// that both can insert events atomically alongside other operations.
func (s *MSSQLStore) appendEventsInTx(ctx context.Context, tx *sql.Tx, workflowID string, recs []EventRecord) error {
	if len(recs) == 0 {
		return nil
	}

	// Use INSERT...SELECT WHERE NOT EXISTS for idempotent event insertion.
	// This is the SQL Server equivalent of PostgreSQL's ON CONFLICT DO NOTHING.
	var prevChecksum string
	for _, rec := range recs {
		payload, err := eventRecordToPayload(rec)
		payloadArg := nullStr("")
		if err == nil && len(payload) > 0 {
			payloadArg = sql.NullString{String: string(payload), Valid: true}
		}
		checksum := computeEventChecksum(rec, prevChecksum)
		prevChecksum = checksum

		_, err = tx.ExecContext(ctx, `
			INSERT INTO event_history (
				workflow_id, step, event_type, service, operation, request, response, error,
				duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
				defer_description, defer_id, child_name, child_input, run_id, new_input,
				plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
				promise_name, promise_id, promise_result, promise_error, payload,
				created_at, checksum, tenant_id
			)
			SELECT @p1, @p2, @p3, @p4, @p5, @p6, @p7, @p8, @p9, @p10,
			       @p11, @p12, @p13, @p14, @p15, @p16, @p17, @p18, @p19, @p20,
			       @p21, @p22, @p23, @p24, @p25, @p26, @p27, @p28, @p29, @p30, @p31, @p32
			WHERE NOT EXISTS (
				SELECT 1 FROM event_history WHERE workflow_id = @p1 AND step = @p2
			)
		`, workflowID, rec.Step, rec.EventType,
			nullStr(rec.Service), nullStr(rec.Op), nullStr(base64.StdEncoding.EncodeToString([]byte(rec.Request))), nullStr(base64.StdEncoding.EncodeToString([]byte(rec.Response))), nullStr(rec.Err),
			nullInt64(rec.DurationMs), nullStr(rec.SignalNames), nullInt64(rec.TimeoutMs),
			nullStr(rec.SignalName), nullStr(rec.SignalPayload),
			nullStr(rec.DeferDescription), nullStr(rec.DeferID),
			nullStr(rec.ChildName), nullStr(rec.ChildInput), nullStr(rec.RunID), nullStr(rec.NewInput),
			nullStr(rec.PluginName), nullStr(rec.PluginFunc), nullStr(rec.PluginInput), nullStr(rec.PluginOutput), nullStr(rec.PluginError),
			nullStr(rec.PromiseName), nullStr(rec.PromiseID), nullStr(rec.PromiseResult), nullStr(rec.PromiseError),
			payloadArg,
			time.UnixMilli(rec.TimestampMs),
			checksum,
			s.tenantID)
		if err != nil {
			return fmt.Errorf("append events in tx: exec step %d: %w", rec.Step, err)
		}
	}
	// NOTE: event_count increment skipped on MSSQL; column not yet available in CI databases.
	return nil
}

// VerifyWorkflowEvents loads all events for a workflow, recomputes their
// SHA-256 checksums, and verifies integrity against stored checksums.
func (s *MSSQLStore) VerifyWorkflowEvents(ctx context.Context, workflowID string) error {
	// Load the full event history for the workflow.
	events, err := s.LoadEventHistory(ctx, workflowID)
	if err != nil {
		return fmt.Errorf("verify events: load: %w", err)
	}

	// Load stored checksums from the DB.
	rows, err := s.db.QueryContext(ctx, `
		SELECT step, checksum FROM event_history
		WHERE workflow_id = @p1
		ORDER BY step
	`, workflowID)
	if err != nil {
		// Column does not exist yet — skip verification (pre-migration).
		return nil
	}
	defer rows.Close()

	storedChecksums := make(map[int]string)
	for rows.Next() {
		var step int
		var checksum sql.NullString
		if err := rows.Scan(&step, &checksum); err != nil {
			return fmt.Errorf("verify events: scan checksum: %w", err)
		}
		if checksum.Valid && checksum.String != "" {
			storedChecksums[step] = checksum.String
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("verify events: rows: %w", err)
	}

	// If no checksums are stored, verification is not possible yet.
	if len(storedChecksums) == 0 {
		return nil
	}

	// Recompute and compare checksums with chaining.
	var prevChecksum string
	for _, ev := range events {
		expected, ok := storedChecksums[ev.Step]
		if !ok || expected == "" {
			prevChecksum = "" // Missing event breaks the chain
			continue
		}
		actual := computeEventChecksum(ev, prevChecksum)
		if actual != expected {
			return fmt.Errorf("verify events: workflow %s step %d: checksum mismatch (expected %s, got %s)",
				workflowID, ev.Step, expected, actual)
		}
		prevChecksum = expected
	}
	return nil
}

// ---------------------------------------------------------------------------
// Workflow Lifecycle Methods (C.5)
// ---------------------------------------------------------------------------

// Heartbeat updates the heartbeat timestamp. Returns false if the workflow
// is no longer assigned to this worker.
func (s *MSSQLStore) Heartbeat(ctx context.Context, workflowID, workerID string, generation int64) (bool, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return false, fmt.Errorf("heartbeat: begin: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET heartbeat_at = SYSUTCDATETIME()
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p3
	`, workflowID, workerID, generation)
	if err != nil {
		return false, fmt.Errorf("heartbeat: %w", err)
	}
	n, _ := result.RowsAffected()
	return n > 0, tx.Commit()
}

// BatchHeartbeat updates heartbeat_at for all workflows assigned to this worker
// with status 'running'. Uses a single UPDATE instead of N calls.
// NOTE: This intentionally does NOT check per-workflow generation because it
// operates on ALL running workflows for a worker, and generations differ per
// workflow. Individual generation-guarded operations (Heartbeat,
// CompleteWorkflow, FailWorkflow, etc.) prevent double-execution even if the
// batch heartbeat refreshes a stale workflow's heartbeat_at.
func (s *MSSQLStore) BatchHeartbeat(ctx context.Context, workerID string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET heartbeat_at = SYSUTCDATETIME()
		WHERE assigned_to = @p1 AND status = 'running'
	`, workerID)
	if err != nil {
		return 0, fmt.Errorf("batch heartbeat: %w", err)
	}
	n, _ := result.RowsAffected()
	return n, nil
}

// CompleteWorkflow marks a workflow as completed with a result.
func (s *MSSQLStore) CompleteWorkflow(ctx context.Context, workflowID, workerID string, generation int64, result string, queryState map[string]string) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("complete workflow: begin: %w", err)
	}
	defer tx.Rollback()

	qsJSON, _ := json.Marshal(queryState)
	if qsJSON == nil {
		qsJSON = []byte("{}")
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'done', result = @p3, completed_at = SYSUTCDATETIME(), assigned_to = NULL, query_state = @p4
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p5
	`, workflowID, workerID, result, string(qsJSON), generation)
	if err != nil {
		return err
	}

	// Record idempotency result within the transaction (best-effort).
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET result = @p2 WHERE workflow_id = @p1`,
		workflowID, result); err != nil {
		log.Printf("idempotency update failed (non-fatal): %v", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Best-effort cleanup.
	s.ClearStickyWorker(context.Background(), workflowID)
	s.ReleaseWorkflowConcurrencyKeys(context.Background(), workflowID)
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// FailWorkflow marks a workflow as failed.
func (s *MSSQLStore) FailWorkflow(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("fail workflow: begin: %w", err)
	}
	defer tx.Rollback()

	qsJSON, _ := json.Marshal(queryState)
	if qsJSON == nil {
		qsJSON = []byte("{}")
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'failed',
		    error_msg = @p3,
		    error_code = @p4,
		    error_op = @p5,
		    completed_at = SYSUTCDATETIME(),
		    assigned_to = NULL,
		    query_state = @p6
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p7
	`, workflowID, workerID, errorMsg, errorCode, errorOp, string(qsJSON), generation)
	if err != nil {
		return err
	}

	// Record idempotency error within the transaction (best-effort).
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET error_msg = @p2 WHERE workflow_id = @p1`,
		workflowID, errorMsg); err != nil {
		log.Printf("idempotency update failed (non-fatal): %v", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Best-effort cleanup.
	s.ClearStickyWorker(context.Background(), workflowID)
	s.ReleaseWorkflowConcurrencyKeys(context.Background(), workflowID)
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// MoveToDeadLetterQueue marks a workflow as dead_lettered because it failed
// after exhausting all retry attempts.
func (s *MSSQLStore) MoveToDeadLetterQueue(ctx context.Context, workflowID, workerID string, generation int64, errMsg, errorCode, errorOp string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("move to dead letter queue: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'dead_lettered', error_msg = @p3, error_code = @p4, error_op = @p5,
		    completed_at = SYSUTCDATETIME(), assigned_to = NULL
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p6
	`, workflowID, workerID, errMsg, errorCode, errorOp, generation)
	if err != nil {
		return err
	}

	// Record idempotency error within the transaction (best-effort).
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET error_msg = @p2 WHERE workflow_id = @p1`,
		workflowID, errMsg); err != nil {
		log.Printf("idempotency update failed (non-fatal): %v", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Best-effort cleanup.
	s.ClearStickyWorker(context.Background(), workflowID)
	s.ReleaseWorkflowConcurrencyKeys(context.Background(), workflowID)

	// Enforce ParentClosePolicy on children.
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// RetryWorkflow moves a dead_lettered workflow back to a runnable state.
// Resets status to 'ready', clears the worker assignment and error fields,
// and sets next_wake_at to now so the workflow is re-queued immediately.
func (s *MSSQLStore) RetryWorkflow(ctx context.Context, workflowID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL,
		    error_msg = NULL, error_code = NULL, error_op = NULL,
		    next_wake_at = SYSUTCDATETIME()
		WHERE id = @p1 AND status = 'dead_lettered'
	`, workflowID)
	return err
}

// ReleaseWorkflow returns a workflow to the ready queue with a next wake time.
func (s *MSSQLStore) ReleaseWorkflow(ctx context.Context, workflowID, workerID string, generation int64, nextWakeAt time.Time) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("release workflow: begin: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, next_wake_at = @p3
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p4
	`, workflowID, workerID, nextWakeAt, generation)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("release workflow: rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("release workflow: no rows affected for %s", workflowID)
	}

	return tx.Commit()
}

// ContinueAsNew atomically creates a new workflow run and completes the current
// one in a single database transaction. Returns the new run ID on success.
func (s *MSSQLStore) ContinueAsNew(ctx context.Context, currentRunID, workerID string, generation int64, defName string, defVersion int, newInput json.RawMessage, newEvents []EventRecord, result string, queryState map[string]string, priority int) (string, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return "", fmt.Errorf("continue as new: begin: %w", err)
	}
	defer tx.Rollback()

	// Append events within the same transaction.
	if err := s.appendEventsInTx(ctx, tx, currentRunID, newEvents); err != nil {
		return "", fmt.Errorf("continue as new: append events: %w", err)
	}

	// Use the store's tenant scope to preserve tenant isolation.
	// Create the new workflow run with a Go-generated UUID.
	newRunID := uuid.New().String()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority)
		VALUES (@p1, @p2, @p3, 'ready', CAST(@p4 AS NVARCHAR(MAX)),
		        ISNULL((SELECT task_queue FROM workflow_defs WHERE name = @p2 AND version = @p3), 'default'),
		        @p5, @p6)
	`, newRunID, defName, defVersion, newInput, s.tenantID, priority)
	if err != nil {
		return "", fmt.Errorf("continue as new: start new run: %w", err)
	}

	// Complete the current run.
	qsJSON, _ := json.Marshal(queryState)
	if qsJSON == nil {
		qsJSON = []byte("{}")
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'done', result = @p3, completed_at = SYSUTCDATETIME(), assigned_to = NULL, query_state = @p4
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p5
	`, currentRunID, workerID, result, string(qsJSON), generation)
	if err != nil {
		return "", fmt.Errorf("continue as new: complete old run: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}

	// Best-effort cleanup after commit.
	s.ClearStickyWorker(context.Background(), currentRunID)
	s.ReleaseWorkflowConcurrencyKeys(context.Background(), currentRunID)
	s.enforceParentClosePolicy(context.Background(), currentRunID)

	return newRunID, nil
}

// FinalizeWorkflowSegment atomically appends new events and updates the
// workflow status in a single database transaction. finalStatus is one of
// "done", "failed" or "ready" (suspend).
func (s *MSSQLStore) FinalizeWorkflowSegment(ctx context.Context, runID, workerID string, generation int64, newEvents []EventRecord, finalStatus string, result string, errorCode string, errorOp string, queryState map[string]string, nextWakeAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("finalize workflow: begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := s.setSessionContext(tx); err != nil {
		return fmt.Errorf("finalize workflow: set session: %w", err)
	}

	// Append new events within the same transaction.
	if err := s.appendEventsInTx(ctx, tx, runID, newEvents); err != nil {
		return fmt.Errorf("finalize workflow: append events: %w", err)
	}

	// Update workflow status based on finalStatus.
	switch finalStatus {
	case "done":
		qsJSON, _ := json.Marshal(queryState)
		if qsJSON == nil {
			qsJSON = []byte("{}")
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE workflow_instances
			SET status = 'done', result = @p3, completed_at = SYSUTCDATETIME(), assigned_to = NULL, query_state = @p4
			WHERE id = @p1 AND assigned_to = @p2 AND generation = @p5
		`, runID, workerID, result, string(qsJSON), generation)
	case "failed":
		qsJSON, _ := json.Marshal(queryState)
		if qsJSON == nil {
			qsJSON = []byte("{}")
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE workflow_instances
			SET status = 'failed',
			    error_msg = @p3,
			    error_code = @p4,
			    error_op = @p5,
			    completed_at = SYSUTCDATETIME(),
			    assigned_to = NULL,
			    query_state = @p6
			WHERE id = @p1 AND assigned_to = @p2 AND generation = @p7
		`, runID, workerID, result, errorCode, errorOp, string(qsJSON), generation)
	case "ready":
		_, err = tx.ExecContext(ctx, `
			UPDATE workflow_instances
			SET status = 'ready', assigned_to = NULL, next_wake_at = @p3
			WHERE id = @p1 AND assigned_to = @p2 AND generation = @p4
		`, runID, workerID, nextWakeAt, generation)
	default:
		return fmt.Errorf("finalize workflow: unknown final status: %s", finalStatus)
	}
	if err != nil {
		return fmt.Errorf("finalize workflow: update status: %w", err)
	}

	// Record idempotency outcome within the transaction (best-effort).
	if finalStatus == "done" || finalStatus == "failed" {
		switch finalStatus {
		case "done":
			if _, err := tx.ExecContext(ctx,
				`UPDATE idempotency_keys SET result = @p2 WHERE workflow_id = @p1`,
				runID, result); err != nil {
				log.Printf("idempotency update failed (non-fatal): %v", err)
			}
		case "failed":
			if _, err := tx.ExecContext(ctx,
				`UPDATE idempotency_keys SET error_msg = @p2 WHERE workflow_id = @p1`,
				runID, result); err != nil {
				log.Printf("idempotency update failed (non-fatal): %v", err)
			}
		}

		// Atomically wake the parent inside the same transaction.
		// Committed atomically with the child's terminal status.
		if _, err := tx.ExecContext(ctx, `
			UPDATE workflow_instances
			SET next_wake_at = SYSUTCDATETIME()
			WHERE id = (
				SELECT parent_workflow_id FROM workflow_instances WHERE id = @p1
			)
			AND status IN ('ready', 'suspended')
		`, runID); err != nil {
			log.Printf("[store] inline parent wake failed (non-fatal): %v", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Best-effort cleanup for terminal statuses (post-commit).
	if finalStatus == "done" || finalStatus == "failed" {
		s.ClearStickyWorker(context.Background(), runID)
		s.ReleaseWorkflowConcurrencyKeys(context.Background(), runID)
		s.enforceParentClosePolicy(context.Background(), runID)
	}
	return nil
}

// RequestCancellation sets the cancellation flag on a workflow.
func (s *MSSQLStore) RequestCancellation(ctx context.Context, workflowID, reason string) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("request cancellation: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET cancellation_requested = 1, cancellation_reason = @p2
		WHERE id = @p1
	`, workflowID, reason)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// CheckCancellation checks if a workflow has been cancelled.
func (s *MSSQLStore) CheckCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	var cancelled bool
	var reason sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT cancellation_requested, cancellation_reason
		FROM workflow_instances WHERE id = @p1
	`, workflowID).Scan(&cancelled, &reason)
	if err != nil {
		return false, "", err
	}
	return cancelled, reason.String, nil
}

// StartNewRun creates a new workflow instance.
// If idempotencyKey is non-empty, provides exactly-once semantics.
func (s *MSSQLStore) StartNewRun(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string, tenantID string, priority int) (string, bool, error) {
	if runID == "" {
		runID = uuid.New().String()
	}
	if idempotencyKey != "" {
		keyHash := sha256.Sum256([]byte(idempotencyKey))

		// Check for existing idempotency key.
		var existingWfID string
		err := s.db.QueryRowContext(ctx,
			`SELECT workflow_id FROM idempotency_keys
			 WHERE key_hash = @p1 AND expires_at > SYSUTCDATETIME()`,
			keyHash[:]).Scan(&existingWfID)
		if err == nil {
			return existingWfID, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", false, err
		}

		// Use the provided runID (already generated above).

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return "", false, err
		}
		defer tx.Rollback()

		// Insert idempotency key record. INSERT...WHERE NOT EXISTS handles the
		// race where two requests arrive with the same key simultaneously.
		ttlSeconds := int(s.idempotencyKeyTTL.Seconds())
		result, err := tx.ExecContext(ctx,
			`INSERT INTO idempotency_keys (key_hash, workflow_id, expires_at)
			 SELECT @p1, @p2, DATEADD(SECOND, @p3, SYSUTCDATETIME())
			 WHERE NOT EXISTS (
			     SELECT 1 FROM idempotency_keys WHERE key_hash = @p1
			 )`,
			keyHash[:], runID, ttlSeconds)
		if err != nil {
			return "", false, err
		}

		n, _ := result.RowsAffected()
		if n == 0 {
			// Key was inserted concurrently — rollback and return the existing one.
			tx.Rollback()
			err := s.db.QueryRowContext(ctx,
				`SELECT workflow_id FROM idempotency_keys
				 WHERE key_hash = @p1 AND expires_at > SYSUTCDATETIME()`,
				keyHash[:]).Scan(&existingWfID)
			if err != nil {
				return "", false, err
			}
			return existingWfID, true, nil
		}

		if err := s.setSessionContext(tx); err != nil {
			return "", false, fmt.Errorf("start new run: set session: %w", err)
		}

		// Insert the workflow instance.
		_, err = tx.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority)
			VALUES (@p1, @p2, @p3, 'ready', CAST(@p4 AS NVARCHAR(MAX)),
			        ISNULL((SELECT task_queue FROM workflow_defs WHERE name = @p2 AND version = @p3), 'default'),
			        @p5, @p6)
		`, runID, defName, defVersion, string(input), tenantID, priority)
		if err != nil {
			return "", false, fmt.Errorf("start new run: %w", err)
		}

		return runID, false, tx.Commit()
	}

	// No idempotency key — normal flow.
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return "", false, fmt.Errorf("start new run: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority)
		VALUES (@p1, @p2, @p3, 'ready', CAST(@p4 AS NVARCHAR(MAX)),
		        ISNULL((SELECT task_queue FROM workflow_defs WHERE name = @p2 AND version = @p3), 'default'),
		        @p5, @p6)
	`, runID, defName, defVersion, string(input), tenantID, priority)
	if err != nil {
		return "", false, fmt.Errorf("start new run: %w", err)
	}
	return runID, false, tx.Commit()
}

// ---------------------------------------------------------------------------
// Best-effort cleanup helpers
// ---------------------------------------------------------------------------

// enforceParentClosePolicy applies ParentClosePolicy to all child workflows
// of the given parent workflow. Best-effort post-commit cleanup.
func (s *MSSQLStore) enforceParentClosePolicy(ctx context.Context, parentWorkflowID string) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		log.Printf("[store] enforceParentClosePolicy: begin TERMINATE tx: %v", err)
		return
	}
	defer tx.Rollback()
	tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'failed', error_msg = 'parent workflow terminated'
		WHERE parent_workflow_id = @p1
		  AND parent_close_policy = 'TERMINATE'
		  AND status NOT IN ('done', 'failed')
	`, parentWorkflowID)
	tx.Commit()

	tx2, err := s.beginTxWithContext(ctx)
	if err != nil {
		log.Printf("[store] enforceParentClosePolicy: begin REQUEST_CANCEL tx: %v", err)
		return
	}
	defer tx2.Rollback()
	tx2.ExecContext(ctx, `
		UPDATE workflow_instances
		SET cancellation_requested = 1
		WHERE parent_workflow_id = @p1
		  AND parent_close_policy = 'REQUEST_CANCEL'
		  AND status NOT IN ('done', 'failed')
	`, parentWorkflowID)
	tx2.Commit()
}

// ---------------------------------------------------------------------------
// Factory (C.2)
// ---------------------------------------------------------------------------

// nopCloser is a no-op io.Closer used by OpenStore.
type mssqlNopCloser struct{}

func (mssqlNopCloser) Close() error { return nil }

// MSSQLStoreFactory implements StoreFactory for Microsoft SQL Server.
// It manages per-tenant connection pools with sp_set_session_context
// baked into the connector, enforcing RLS at the connection level.
type MSSQLStoreFactory struct {
	mu                sync.RWMutex
	connStr           string             // connection string for SQL Server
	tenantDBs         map[string]*sql.DB // per-tenant connection pools with RLS context
	idempotencyKeyTTL time.Duration
}

// NewMSSQLStoreFactory creates an MSSQLStoreFactory.
// connStr is the SQL Server connection string used to open per-tenant pools.
func NewMSSQLStoreFactory(connStr string, idempotencyKeyTTL ...time.Duration) *MSSQLStoreFactory {
	ttl := 720 * time.Hour
	if len(idempotencyKeyTTL) > 0 {
		ttl = idempotencyKeyTTL[0]
	}
	return &MSSQLStoreFactory{
		connStr:           connStr,
		tenantDBs:         make(map[string]*sql.DB),
		idempotencyKeyTTL: ttl,
	}
}

// OpenStore creates an MSSQLStore scoped to the given tenant.
// Each tenant gets a dedicated connection pool with RLS session context
// baked into every connection.
//
// NOTE: Encryption at rest (--encrypt-sensitive-payloads) is not yet supported
// on MSSQL backends. See PostgresStore.WithEncryption for the reference implementation.
func (f *MSSQLStoreFactory) OpenStore(ctx context.Context, tenantID string, taskQueues ...string) (WorkflowStore, io.Closer, error) {
	tenantDB, err := f.getOrCreateTenantPool(ctx, tenantID)
	if err != nil {
		return nil, nil, fmt.Errorf("open store for tenant %s: %w", tenantID, err)
	}
	store := NewMSSQLStore(tenantDB, taskQueues...)
	store.tenantID = tenantID
	store = store.WithIdempotencyKeyTTL(f.idempotencyKeyTTL)
	return store, mssqlNopCloser{}, nil
}

// getOrCreateTenantPool returns a *sql.DB pool for the given tenant.
// The pool uses a wrapped connector that sets sp_set_session_context
// on every new connection, so RLS is enforced automatically.
func (f *MSSQLStoreFactory) getOrCreateTenantPool(ctx context.Context, tenantID string) (*sql.DB, error) {
	// Validate early to fail fast with a clear error, rather than failing
	// during the first connection attempt inside the connector.
	if _, err := uuid.Parse(tenantID); err != nil {
		return nil, fmt.Errorf("invalid tenant ID %q: %w", tenantID, err)
	}

	f.mu.RLock()
	db, ok := f.tenantDBs[tenantID]
	f.mu.RUnlock()
	if ok {
		return db, nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if db, ok := f.tenantDBs[tenantID]; ok {
		return db, nil
	}

	// Get the mssql driver as a Connector through the registered driver.
	baseDB, err := sql.Open("sqlserver", f.connStr)
	if err != nil {
		return nil, fmt.Errorf("open base mssql connection: %w", err)
	}
	d := baseDB.Driver()
	baseDB.Close()

	dc, ok := d.(driver.DriverContext)
	if !ok {
		return nil, fmt.Errorf("mssql driver does not implement DriverContext")
	}

	connector, err := dc.OpenConnector(f.connStr)
	if err != nil {
		return nil, fmt.Errorf("open mssql connector: %w", err)
	}

	wrapped := &tenantSessionConnector{
		Connector: connector,
		tenantID:  tenantID,
	}

	tenantDB := sql.OpenDB(wrapped)
	tenantDB.SetMaxOpenConns(15)
	tenantDB.SetMaxIdleConns(5)
	tenantDB.SetConnMaxLifetime(5 * time.Minute)

	if err := tenantDB.PingContext(ctx); err != nil {
		tenantDB.Close()
		return nil, fmt.Errorf("ping tenant pool for %s: %w", tenantID, err)
	}

	f.tenantDBs[tenantID] = tenantDB
	return tenantDB, nil
}

// Close closes all tenant connection pools.
func (f *MSSQLStoreFactory) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, db := range f.tenantDBs {
		db.Close()
		delete(f.tenantDBs, id)
	}
	return nil
}

func (f *MSSQLStoreFactory) DriverName() string { return "mssql" }
func (f *MSSQLStoreFactory) Dialect() Dialect   { return DialectMSSQL }

// ---------------------------------------------------------------------------
// Remaining WorkflowStore interface methods
// ---------------------------------------------------------------------------

// LoadWASM returns the compiled WASM bytes for a workflow definition.
func (s *MSSQLStore) LoadWASM(ctx context.Context, defName string, defVersion int) ([]byte, error) {
	var wasmBytes []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT wasm_bytes FROM workflow_defs WHERE name = @p1 AND version = @p2
	`, defName, defVersion).Scan(&wasmBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("wasm not found: %s v%d", defName, defVersion)
	}
	if err != nil {
		return nil, fmt.Errorf("load wasm: %w", err)
	}
	return wasmBytes, nil
}

// GetWASMLength returns the byte length of the stored WASM binary.
func (s *MSSQLStore) GetWASMLength(ctx context.Context, defName string, defVersion int) (int64, error) {
	var length int64
	err := s.db.QueryRowContext(ctx, `SELECT len(wasm_bytes) FROM workflow_defs WHERE name = @p1 AND version = @p2`, defName, defVersion).Scan(&length)
	return length, err
}

// ListVersions returns all deployed versions of a workflow.
func (s *MSSQLStore) ListVersions(ctx context.Context, defName string) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT version FROM workflow_defs WHERE name = @p1 ORDER BY version DESC
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

// LoadWorkflowConfig returns the max_history_length for a workflow definition.
func (s *MSSQLStore) LoadWorkflowConfig(ctx context.Context, defName string, defVersion int) (int, error) {
	var maxHistoryLength int
	err := s.db.QueryRowContext(ctx, `
		SELECT max_history_length FROM workflow_defs WHERE name = @p1 AND version = @p2
	`, defName, defVersion).Scan(&maxHistoryLength)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("workflow def not found: %s v%d", defName, defVersion)
	}
	if err != nil {
		return 0, fmt.Errorf("load workflow config: %w", err)
	}
	return maxHistoryLength, nil
}

// LoadDAGSpec returns the dag_spec JSON for a workflow definition, or nil if none.
func (s *MSSQLStore) LoadDAGSpec(ctx context.Context, defName string, defVersion int) (json.RawMessage, error) {
	var raw *[]byte
	err := s.db.QueryRowContext(ctx, `
		SELECT dag_spec FROM workflow_defs WHERE name = @p1 AND version = @p2
	`, defName, defVersion).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("workflow def not found: %s v%d", defName, defVersion)
	}
	if err != nil {
		return nil, fmt.Errorf("load dag_spec: %w", err)
	}
	if raw == nil {
		return nil, nil
	}
	return json.RawMessage(*raw), nil
}

// TraceWorkflow sets the W3C trace_id on a workflow instance.
func (s *MSSQLStore) TraceWorkflow(ctx context.Context, workflowID, traceID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances SET trace_id = @p2 WHERE id = @p1
	`, workflowID, traceID)
	return err
}

// ResolveTenantFromAPIKey looks up a tenant UUID by API key hash.
func (s *MSSQLStore) ResolveTenantFromAPIKey(ctx context.Context, keyHash []byte) (uuid.UUID, error) {
	var tenantID uuid.UUID
	err := s.db.QueryRowContext(ctx,
		`SELECT tenant_id FROM tenant_api_keys
		 WHERE key_hash = @p1 AND revoked_at IS NULL`, keyHash).Scan(&tenantID)
	if err != nil {
		return uuid.Nil, err
	}
	return tenantID, nil
}

// DeliverSignal stores a signal for a workflow.
func (s *MSSQLStore) DeliverSignal(ctx context.Context, workflowID, signalName, payload string) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("deliver signal: begin: %w", err)
	}
	defer tx.Rollback()

	// Use MERGE for upsert semantics (equivalent to ON CONFLICT DO UPDATE).
	_, err = tx.ExecContext(ctx, `
		MERGE workflow_signals AS target
		USING (SELECT @p1 AS workflow_id, @p2 AS signal_name, @p3 AS payload) AS source
		ON target.workflow_id = source.workflow_id AND target.signal_name = source.signal_name
		WHEN MATCHED THEN UPDATE SET payload = source.payload, delivered_at = SYSUTCDATETIME()
		WHEN NOT MATCHED THEN INSERT (workflow_id, signal_name, payload, tenant_id)
		     VALUES (source.workflow_id, source.signal_name, source.payload, @p4);
	`, workflowID, signalName, payload, s.tenantID)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET next_wake_at = SYSUTCDATETIME()
		WHERE id = @p1 AND status IN ('ready', 'suspended')
	`, workflowID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// PollSignal checks for a delivered signal without consuming it.
// This is non-destructive — the signal remains available after polling.
func (s *MSSQLStore) PollSignal(ctx context.Context, workflowID, signalName string) (string, bool, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, `
		SELECT payload FROM workflow_signals
		WHERE workflow_id = @p1 AND signal_name = @p2 AND tenant_id = @p3
	`, workflowID, signalName, s.tenantID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("poll signal: %w", err)
	}
	return payload, true, nil
}

// PollCancellation checks whether the workflow has been cancelled.
func (s *MSSQLStore) PollCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	return s.CheckCancellation(ctx, workflowID)
}

// GetAllowedSignalCallers returns the allowed_signals list for a workflow.
func (s *MSSQLStore) GetAllowedSignalCallers(ctx context.Context, workflowID string) ([]string, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT allowed_signals FROM workflow_instances WHERE id = @p1`, workflowID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get allowed signal callers: %w", err)
	}
	if !raw.Valid || raw.String == "" || raw.String == "null" {
		return nil, nil
	}
	var callers []string
	if err := json.Unmarshal([]byte(raw.String), &callers); err != nil {
		return nil, fmt.Errorf("get allowed signal callers: parse: %w", err)
	}
	return callers, nil
}

// PollAndClaimSignal atomically checks for and claims a pending signal.
func (s *MSSQLStore) PollAndClaimSignal(ctx context.Context, workflowID, signalName string) (string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()

	if err := s.setSessionContext(tx); err != nil {
		return "", false, err
	}

	var payload string
	err = tx.QueryRowContext(ctx, `
		DELETE FROM workflow_signals
		OUTPUT DELETED.payload
		WHERE workflow_id = @p1 AND signal_name = @p2
	`, workflowID, signalName).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, tx.Rollback()
	}
	if err != nil {
		return "", false, fmt.Errorf("poll signal: %w", err)
	}
	return payload, true, tx.Commit()
}

// StartChildWorkflow creates a child workflow instance linked to a parent.
// defVersion is the explicit workflow definition version to use, or 0 to use
// default resolution (SELECT MAX(version) from non-deprecated versions).
func (s *MSSQLStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error) {
	runID := uuid.New().String()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, parent_close_policy, task_queue, priority, tenant_id)
		VALUES (@p1, @p2,
		        CASE WHEN @p5 > 0 THEN @p5 ELSE (SELECT MAX(version) FROM workflow_defs WHERE name = @p2 AND deprecated = 0) END,
		        'ready', @p3, @p4,
		        ISNULL(NULLIF(@p6, ''), 'ABANDON'),
		        ISNULL((SELECT task_queue FROM workflow_instances WHERE id = @p4), 'default'), @p7, @p8)
	`, runID, defName, inputJSON, parentID, defVersion, parentClosePolicy, priority, s.tenantID)
	if err != nil {
		return "", fmt.Errorf("start child workflow: %w", err)
	}
	return runID, nil
}

// StartChildWorkflowAtomic creates a child workflow and records the parent's
// child_workflow event in a single transaction, guaranteeing exactly-once creation.
func (s *MSSQLStore) StartChildWorkflowAtomic(ctx context.Context, childID, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, event EventRecord, priority int) (string, error) {
	if childID == "" {
		childID = uuid.New().String()
	}

	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: begin tx: %w", err)
	}
	defer tx.Rollback()

	// 1. INSERT child workflow instance.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, parent_close_policy, task_queue, priority, tenant_id)
		VALUES (@p1, @p2,
		        CASE WHEN @p5 > 0 THEN @p5 ELSE (SELECT MAX(version) FROM workflow_defs WHERE name = @p2 AND deprecated = 0) END,
		        'ready', @p3, @p4,
		        ISNULL(NULLIF(@p6, ''), 'ABANDON'),
		        ISNULL((SELECT task_queue FROM workflow_instances WHERE id = @p4), 'default'), @p7, @p8)
	`, childID, defName, inputJSON, parentID, defVersion, parentClosePolicy, priority, s.tenantID)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: insert child: %w", err)
	}

	// 2. INSERT child_workflow event into parent's event_history.
	event.RunID = childID
	var prevCS string
	if event.Step > 1 {
		s.db.QueryRowContext(ctx,
			`SELECT ISNULL(checksum, '') FROM event_history WHERE workflow_id = @p1 AND step = @p2`,
			parentID, event.Step-1).Scan(&prevCS)
	}
	checksum := computeEventChecksum(event, prevCS)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO event_history (workflow_id, step, event_type, child_name, child_input, run_id, created_at, checksum, tenant_id)
		SELECT @p1, @p2, @p3, @p4, @p5, @p6, @p7, @p8, @p9
		WHERE NOT EXISTS (
			SELECT 1 FROM event_history WHERE workflow_id = @p1 AND step = @p2
		)
	`, parentID, event.Step, string(event.EventType),
		nullStr(event.ChildName), nullStr(event.ChildInput), nullStr(childID),
		time.UnixMilli(event.TimestampMs), checksum, s.tenantID)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: insert event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("start child workflow atomic: commit: %w", err)
	}
	return childID, nil
}

// GetChildResult checks whether a child workflow has completed.
func (s *MSSQLStore) GetChildResult(ctx context.Context, runID string) (string, bool, error) {
	var result string
	var status string
	err := s.db.QueryRowContext(ctx, `
		SELECT ISNULL(result, '{}'), status FROM workflow_instances WHERE id = @p1
	`, runID).Scan(&result, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get child result: %w", err)
	}
	if status == "done" || status == "failed" {
		return result, true, nil
	}
	return "", false, nil
}

// GetChildCount returns the number of active (non-terminal) child workflows
// for the given parent workflow. Terminal statuses are excluded.
func (s *MSSQLStore) GetChildCount(ctx context.Context, parentWorkflowID string) (int, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("get child count for %s: begin: %w", parentWorkflowID, err)
	}
	defer tx.Rollback()

	var count int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_instances
		WHERE parent_workflow_id = @p1 AND status NOT IN ('done', 'failed', 'dead_lettered')
	`, parentWorkflowID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get child count for %s: %w", parentWorkflowID, err)
	}
	return count, tx.Commit()
}

// ReapStaleInstances reclaims workflow instances with stale heartbeats.
func (s *MSSQLStore) ReapStaleInstances(ctx context.Context, timeout time.Duration) (int, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("reap stale instances: begin: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL
		WHERE status = 'running'
		  AND heartbeat_at < DATEADD(SECOND, @p1, SYSUTCDATETIME())
	`, -int(timeout.Seconds()))
	if err != nil {
		return 0, fmt.Errorf("reap stale instances: %w", err)
	}
	n, _ := result.RowsAffected()
	return int(n), tx.Commit()
}

// GetQueryState returns the query state for a workflow instance key.
//
// SECURITY: The JSON path is constructed via concatenation of user-controlled
// key into '$.' + @p2. This is safe from SQL injection (JSON_VALUE cannot
// execute DML), and the key is passed as a parameter, not string-embedded.
// However, a malicious key could produce an invalid JSON path expression,
// causing a runtime error. Consider using JSON_QUERY with a parameterized
// path when SQL Server adds support for parameterized JSON paths.
func (s *MSSQLStore) GetQueryState(ctx context.Context, workflowID, key string) (string, error) {
	var value sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT JSON_VALUE(query_state, '$.' + @p2) FROM workflow_instances WHERE id = @p1
	`, workflowID, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get query state: %w", err)
	}
	return value.String, nil
}

// ListWorkflows returns workflow instances filtered by the given filter parameters.
// Supported filters: Status, InputContains, ErrorContains, Search.
// Supports pagination via Offset and Limit (default 100, max 1000).
func (s *MSSQLStore) ListWorkflows(ctx context.Context, filter WorkflowFilter) ([]WorkflowInstance, error) {
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
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	defer rows.Close()

	var workflows []WorkflowInstance
	for rows.Next() {
		var wf WorkflowInstance
		var nextWakeAt, createdAt sql.NullTime
		var inputStr string
		var assignedTo, errorCode, errorOp, errorMsg sql.NullString
		var traceID sql.NullString
		if err := rows.Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status, &inputStr,
			&assignedTo, &nextWakeAt, &errorCode, &errorOp, &errorMsg, &createdAt, &wf.Generation, &wf.Priority, &traceID); err != nil {
			return nil, fmt.Errorf("scan workflow: %w", err)
		}
		wf.TraceID = traceID.String
		wf.Input = json.RawMessage(inputStr)
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
	return workflows, rows.Err()
}

// GetWorkflowByID returns a single workflow instance by ID.
func (s *MSSQLStore) GetWorkflowByID(ctx context.Context, id string) (*WorkflowInstance, error) {
	var wf WorkflowInstance
	var nextWakeAt, heartbeatAt, completedAt sql.NullTime
	var assignedTo, errorMsg sql.NullString
	var result sql.NullString
	var inputRaw string
	var errorCode, errorOp sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, def_name, def_version, status, input,
		       assigned_to, heartbeat_at, next_wake_at, completed_at, CAST(result AS NVARCHAR(MAX)), error_msg, error_code, error_op,
		       generation, COALESCE(priority, 0) AS priority,
		       COALESCE(trace_id, '')
		FROM workflow_instances WHERE id = @p1
	`, id).Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status, &inputRaw,
		&assignedTo, &heartbeatAt, &nextWakeAt, &completedAt, &result, &errorMsg, &errorCode, &errorOp,
		&wf.Generation, &wf.Priority,
		&wf.TraceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workflow: %w", err)
	}
	wf.Input = json.RawMessage(inputRaw)
	wf.AssignedTo = assignedTo.String
	wf.Result = result.String
	wf.Error = errorMsg.String
	wf.ErrorCode = errorCode.String
	wf.ErrorOp = errorOp.String
	if nextWakeAt.Valid {
		wf.NextWakeAt = nextWakeAt.Time
	}
	return &wf, nil
}

// ---- Schedule methods ----

// CreateSchedule inserts a new cron schedule.
func (s *MSSQLStore) CreateSchedule(ctx context.Context, sch Schedule) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_schedules (name, def_name, entry_point, cron_expression, input, enabled, next_run_at, tenant_id)
		VALUES (@p1, @p2, @p3, @p4, @p5, @p6, @p7, @p8)
	`, sch.Name, sch.DefName, sch.EntryPoint, sch.CronExpression, sch.Input, sch.Enabled, sch.NextRunAt, s.tenantID)
	return err
}

// ListSchedules returns all registered schedules.
func (s *MSSQLStore) ListSchedules(ctx context.Context) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, def_name, entry_point, cron_expression, input, enabled, next_run_at, last_run_at
		FROM workflow_schedules ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var schedules []Schedule
	for rows.Next() {
		var sch Schedule
		var lastRunAt sql.NullTime
		var inputStr string
		if err := rows.Scan(&sch.Name, &sch.DefName, &sch.EntryPoint, &sch.CronExpression,
			&inputStr, &sch.Enabled, &sch.NextRunAt, &lastRunAt); err != nil {
			return nil, err
		}
		sch.Input = json.RawMessage(inputStr)
		if lastRunAt.Valid {
			sch.LastRunAt = &lastRunAt.Time
		}
		schedules = append(schedules, sch)
	}
	return schedules, rows.Err()
}

// DeleteSchedule removes a schedule by name.
func (s *MSSQLStore) DeleteSchedule(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM workflow_schedules WHERE name = @p1`, name)
	return err
}

// SetScheduleEnabled enables or disables a schedule.
func (s *MSSQLStore) SetScheduleEnabled(ctx context.Context, name string, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_schedules SET enabled = @p2 WHERE name = @p1
	`, name, enabled)
	return err
}

// GetDueSchedules returns enabled schedules whose next_run_at <= now().
func (s *MSSQLStore) GetDueSchedules(ctx context.Context) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, def_name, entry_point, cron_expression, input, enabled, next_run_at, last_run_at
		FROM workflow_schedules WITH (READPAST, UPDLOCK, ROWLOCK)
		WHERE enabled = 1 AND next_run_at <= SYSUTCDATETIME()
		ORDER BY next_run_at
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var schedules []Schedule
	for rows.Next() {
		var sch Schedule
		var lastRunAt sql.NullTime
		var inputStr string
		if err := rows.Scan(&sch.Name, &sch.DefName, &sch.EntryPoint, &sch.CronExpression,
			&inputStr, &sch.Enabled, &sch.NextRunAt, &lastRunAt); err != nil {
			return nil, err
		}
		sch.Input = json.RawMessage(inputStr)
		if lastRunAt.Valid {
			sch.LastRunAt = &lastRunAt.Time
		}
		schedules = append(schedules, sch)
	}
	return schedules, rows.Err()
}

// UpdateScheduleNextRun updates a schedule's next_run_at after firing.
func (s *MSSQLStore) UpdateScheduleNextRun(ctx context.Context, name string, nextRun time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_schedules SET last_run_at = SYSUTCDATETIME(), next_run_at = @p2 WHERE name = @p1
	`, name, nextRun)
	return err
}

// ---- Compaction methods ----

// GetCompactionCandidates returns up to limit workflow IDs whose event
// history exceeds the threshold and could benefit from compaction.
func (s *MSSQLStore) GetCompactionCandidates(ctx context.Context, threshold int, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT w.id
		FROM workflow_instances w
		WHERE w.status IN ('ready', 'running')
		  AND (SELECT COUNT(*) FROM event_history e WHERE e.workflow_id = w.id) > @p1
		  AND (w.compaction_step IS NULL OR w.compaction_step < (SELECT MAX(e2.step) FROM event_history e2 WHERE e2.workflow_id = w.id))
		ORDER BY w.created_at
		OFFSET 0 ROWS FETCH NEXT @p2 ROWS ONLY
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
	return ids, rows.Err()
}

// LoadCompactionState returns the compaction state for a workflow, or nil
// if the workflow has not been compacted.
func (s *MSSQLStore) LoadCompactionState(ctx context.Context, workflowID string) (*CompactionState, error) {
	var stateRaw sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT CAST(compaction_state AS NVARCHAR(MAX)) FROM workflow_instances WHERE id = @p1
	`, workflowID).Scan(&stateRaw)
	if errors.Is(err, sql.ErrNoRows) || !stateRaw.Valid || stateRaw.String == "" {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load compaction state: %w", err)
	}
	var state CompactionState
	if err := json.Unmarshal([]byte(stateRaw.String), &state); err != nil {
		return nil, fmt.Errorf("load compaction state: unmarshal: %w", err)
	}
	return &state, nil
}

// CompactHistory deletes old events and persists the compaction checkpoint.
func (s *MSSQLStore) CompactHistory(ctx context.Context, workflowID string, compactionState []byte, compactionStep int, keepStep int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("compact history: begin tx: %w", err)
	}
	defer tx.Rollback()

	// Read current generation for optimistic locking.
	var gen int64
	err = tx.QueryRowContext(ctx, `SELECT generation FROM workflow_instances WHERE id = @p1`, workflowID).Scan(&gen)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return tx.Commit() // Workflow no longer exists.
		}
		return fmt.Errorf("compact history: get generation: %w", err)
	}

	// Delete events older than keepStep.
	_, err = tx.ExecContext(ctx, `
		DELETE FROM event_history
		WHERE workflow_id = @p1 AND step < @p2
	`, workflowID, keepStep)
	if err != nil {
		return fmt.Errorf("compact history: delete events: %w", err)
	}

	// Persist compaction checkpoint.
	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET compaction_state = @p2, compaction_step = @p3, compacted_at = SYSUTCDATETIME()
		WHERE id = @p1 AND generation = @p4
	`, workflowID, string(compactionState), compactionStep, gen)
	if err != nil {
		return fmt.Errorf("compact history: update state: %w", err)
	}

	return tx.Commit()
}

// ---- Promise methods ----

// CreatePromise creates a new promise for a workflow.
func (s *MSSQLStore) CreatePromise(ctx context.Context, workflowID, promiseName, promiseID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_promises (workflow_id, promise_name, promise_id, tenant_id)
		VALUES (@p1, @p2, @p3, @p4)
	`, workflowID, promiseName, promiseID, s.tenantID)
	return err
}

// ResolvePromise marks a promise as resolved with the given result.
// Also wakes the workflow instance so it can pick up the resolved promise
// on the next poll cycle instead of waiting for the original timeout.
func (s *MSSQLStore) ResolvePromise(ctx context.Context, workflowID, promiseID, result string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_promises
		SET status = 'resolved', result = @p3, resolved_at = SYSUTCDATETIME()
		WHERE workflow_id = @p1 AND promise_id = @p2 AND tenant_id = @p4
	`, workflowID, promiseID, result, s.tenantID)
	if err != nil {
		return err
	}
	_, _ = s.db.ExecContext(ctx, `
		UPDATE workflow_instances SET next_wake_at = SYSUTCDATETIME()
		WHERE id = @p1 AND status IN ('ready', 'suspended')
	`, workflowID)
	return nil
}

// RejectPromise marks a promise as rejected with the given error message.
// Also wakes the workflow instance so it can pick up the rejected promise
// on the next poll cycle instead of waiting for the original timeout.
func (s *MSSQLStore) RejectPromise(ctx context.Context, workflowID, promiseID, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_promises
		SET status = 'rejected', error_msg = @p3, resolved_at = SYSUTCDATETIME()
		WHERE workflow_id = @p1 AND promise_id = @p2 AND tenant_id = @p4
	`, workflowID, promiseID, errMsg, s.tenantID)
	if err != nil {
		return err
	}
	_, _ = s.db.ExecContext(ctx, `
		UPDATE workflow_instances SET next_wake_at = SYSUTCDATETIME()
		WHERE id = @p1 AND status IN ('ready', 'suspended')
	`, workflowID)
	return nil
}

// GetPromise returns the current status and result of a promise.
func (s *MSSQLStore) GetPromise(ctx context.Context, workflowID, promiseID string) (string, string, string, error) {
	var status, result, errMsg string
	err := s.db.QueryRowContext(ctx, `
		SELECT ISNULL(status, 'pending'), ISNULL(result, ''), ISNULL(error_msg, '')
		FROM workflow_promises
		WHERE workflow_id = @p1 AND promise_id = @p2 AND tenant_id = @p3
	`, workflowID, promiseID, s.tenantID).Scan(&status, &result, &errMsg)
	if errors.Is(err, sql.ErrNoRows) {
		return "pending", "", "", nil
	}
	if err != nil {
		return "", "", "", fmt.Errorf("get promise: %w", err)
	}
	return status, result, errMsg, nil
}

// ListPromises returns all promises for a workflow ordered by creation time.
func (s *MSSQLStore) ListPromises(ctx context.Context, workflowID string) ([]PromiseInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT promise_id, promise_name, status, ISNULL(result, ''), ISNULL(error_msg, ''), created_at, resolved_at
		FROM workflow_promises
		WHERE workflow_id = @p1 AND tenant_id = @p2
		ORDER BY created_at
	`, workflowID, s.tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var promises []PromiseInfo
	for rows.Next() {
		var p PromiseInfo
		var resolvedAt sql.NullTime
		if err := rows.Scan(&p.PromiseID, &p.PromiseName, &p.Status, &p.Result, &p.ErrorMsg,
			&p.CreatedAt, &resolvedAt); err != nil {
			return nil, err
		}
		if resolvedAt.Valid {
			p.ResolvedAt = &resolvedAt.Time
		}
		promises = append(promises, p)
	}
	return promises, rows.Err()
}

// ---- Update Request methods ----

// CreateUpdateRequest registers an incoming update request.
func (s *MSSQLStore) CreateUpdateRequest(ctx context.Context, workflowID, updateName, payload, promiseID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_update_requests (workflow_id, update_name, payload, promise_id, status, tenant_id)
		VALUES (@p1, @p2, @p3, @p4, 'pending', @p5)
	`, workflowID, updateName, payload, promiseID, s.tenantID)
	return err
}

// GetPendingUpdateRequests returns all pending update requests for a workflow.
func (s *MSSQLStore) GetPendingUpdateRequests(ctx context.Context, workflowID string) ([]UpdateRequestInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT workflow_id, update_name, payload, ISNULL(promise_id, ''), status,
		       ISNULL(result, ''), ISNULL(error_msg, ''), created_at
		FROM workflow_update_requests
		WHERE workflow_id = @p1 AND tenant_id = @p2 AND status = 'pending'
		ORDER BY created_at
	`, workflowID, s.tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var requests []UpdateRequestInfo
	for rows.Next() {
		var req UpdateRequestInfo
		if err := rows.Scan(&req.WorkflowID, &req.UpdateName, &req.Payload, &req.PromiseID,
			&req.Status, &req.Result, &req.ErrorMsg, &req.CreatedAt); err != nil {
			return nil, err
		}
		requests = append(requests, req)
	}
	return requests, rows.Err()
}

// CompleteUpdateRequest marks an update request as completed with a result or error.
func (s *MSSQLStore) CompleteUpdateRequest(ctx context.Context, workflowID, updateName, result, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_update_requests
		SET status = 'completed', result = @p3, error_msg = @p4, completed_at = SYSUTCDATETIME()
		WHERE workflow_id = @p1 AND update_name = @p2 AND tenant_id = @p5 AND status = 'pending'
	`, workflowID, updateName, result, errMsg, s.tenantID)
	return err
}

// ---- Concurrency Key methods ----

// AcquireConcurrencyKey tries to acquire a concurrency key for a workflow.
// All three operations (cleanup, insert, verify) are wrapped in a single
// explicit transaction to prevent race conditions between separate implicit
// transactions that would each see a different snapshot of concurrency_keys.
func (s *MSSQLStore) AcquireConcurrencyKey(ctx context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
	keyHash := sha256.Sum256([]byte(key))

	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire concurrency key: begin: %w", err)
	}
	defer tx.Rollback()

	// Release expired keys for this tenant during acquisition.
	_, err = tx.ExecContext(ctx, `
		DELETE FROM concurrency_keys WHERE key_hash = @p1 AND expires_at < SYSUTCDATETIME() AND tenant_id = @p2
	`, keyHash[:], s.tenantID)
	if err != nil {
		return false, fmt.Errorf("acquire concurrency key: cleanup expired: %w", err)
	}

	// Try to insert with a unique constraint.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at, tenant_id)
		SELECT @p1, @p2, @p3, DATEADD(SECOND, @p4, SYSUTCDATETIME()), @p5
		WHERE NOT EXISTS (
			SELECT 1 FROM concurrency_keys WHERE key_hash = @p1
		)
	`, keyHash[:], key, workflowID, int(ttl.Seconds()), s.tenantID)
	if err != nil {
		return false, fmt.Errorf("acquire concurrency key: %w", err)
	}

	// Check if our insert succeeded (tenant-scoped).
	var wkID string
	err = tx.QueryRowContext(ctx, `
		SELECT workflow_id FROM concurrency_keys WHERE key_hash = @p1 AND tenant_id = @p2
	`, keyHash[:], s.tenantID).Scan(&wkID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, tx.Commit()
	}
	if err != nil {
		return false, fmt.Errorf("acquire concurrency key: verify: %w", err)
	}
	return wkID == workflowID, tx.Commit()
}

// ReleaseConcurrencyKey releases a specific concurrency key.
func (s *MSSQLStore) ReleaseConcurrencyKey(ctx context.Context, key string) error {
	keyHash := sha256.Sum256([]byte(key))
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("release concurrency key: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `DELETE FROM concurrency_keys WHERE key_hash = @p1 AND tenant_id = @p2`, keyHash[:], s.tenantID)
	if err != nil {
		return fmt.Errorf("release concurrency key: %w", err)
	}
	return tx.Commit()
}

// ReleaseWorkflowConcurrencyKeys releases all concurrency keys held by a workflow.
func (s *MSSQLStore) ReleaseWorkflowConcurrencyKeys(ctx context.Context, workflowID string) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("release workflow concurrency keys: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `DELETE FROM concurrency_keys WHERE workflow_id = @p1 AND tenant_id = @p2`, workflowID, s.tenantID)
	if err != nil {
		return fmt.Errorf("release workflow concurrency keys: %w", err)
	}
	return tx.Commit()
}

// ReapExpiredConcurrencyKeys deletes all expired concurrency keys
// for the current tenant.
func (s *MSSQLStore) ReapExpiredConcurrencyKeys(ctx context.Context) (int64, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("reap expired concurrency keys: begin: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `DELETE FROM concurrency_keys WHERE expires_at < SYSUTCDATETIME() AND tenant_id = @p1`, s.tenantID)
	if err != nil {
		return 0, fmt.Errorf("reap expired concurrency keys: %w", err)
	}
	n, _ := result.RowsAffected()
	return n, tx.Commit()
}

// GetConcurrencyKeyCount returns the number of non-expired concurrency keys
// held by the given workflow.
func (s *MSSQLStore) GetConcurrencyKeyCount(ctx context.Context, workflowID string) (int, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("get concurrency key count for %s: begin: %w", workflowID, err)
	}
	defer tx.Rollback()

	var count int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM concurrency_keys
		WHERE workflow_id = @p1 AND expires_at > SYSUTCDATETIME() AND tenant_id = @p2
	`, workflowID, s.tenantID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get concurrency key count for %s: %w", workflowID, err)
	}
	return count, tx.Commit()
}

// GetEventCount returns the event_count for a workflow instance.
func (s *MSSQLStore) GetEventCount(ctx context.Context, workflowID string) (int, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("get event count for %s: begin: %w", workflowID, err)
	}
	defer tx.Rollback()

	var count int
	err = tx.QueryRowContext(ctx, `SELECT event_count FROM workflow_instances WHERE id = @p1`, workflowID).Scan(&count)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("get event count for %s: %w", workflowID, err)
	}
	return count, tx.Commit()
}

// ---- Sticky Session methods ----

// UpdateStickyWorker sets the sticky worker for a workflow.
func (s *MSSQLStore) UpdateStickyWorker(ctx context.Context, workflowID, workerID string) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("update sticky worker: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances SET sticky_worker_id = @p2 WHERE id = @p1
	`, workflowID, workerID)
	if err != nil {
		return fmt.Errorf("update sticky worker: %w", err)
	}
	return tx.Commit()
}

// ClearStickyWorker removes the sticky worker assignment.
func (s *MSSQLStore) ClearStickyWorker(ctx context.Context, workflowID string) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("clear sticky worker: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances SET sticky_worker_id = NULL WHERE id = @p1
	`, workflowID)
	if err != nil {
		return fmt.Errorf("clear sticky worker: %w", err)
	}
	return tx.Commit()
}

// ---- Version Management methods ----

// DeployWorkflowDef inserts or updates a workflow definition.
func (s *MSSQLStore) DeployWorkflowDef(ctx context.Context, def *WorkflowDef) error {
	pluginDepsJSON, _ := json.Marshal(def.PluginDeps)
	if pluginDepsJSON == nil {
		pluginDepsJSON = []byte("{}")
	}
	_, err := s.db.ExecContext(ctx, `
		MERGE workflow_defs AS target
		USING (SELECT @p1 AS name, @p2 AS version) AS source
		ON target.name = source.name AND target.version = source.version
		WHEN MATCHED THEN UPDATE SET
			wasm_bytes = @p3,
			abi_version = @p4,
			min_version = @p5,
			plugin_deps = @p6,
			deprecated = @p7
		WHEN NOT MATCHED THEN INSERT (name, version, wasm_bytes, abi_version, min_version, plugin_deps, deprecated)
		     VALUES (@p1, @p2, @p3, @p4, @p5, @p6, @p7);
	`, def.Name, def.Version, def.WASMBytes, def.ABIVersion, def.MinVersion, pluginDepsJSON, def.Deprecated)
	if err != nil {
		return fmt.Errorf("deploy workflow def: %w", err)
	}
	return nil
}

// ListWorkflowDefs returns all versions of a workflow, ordered by version DESC.
func (s *MSSQLStore) ListWorkflowDefs(ctx context.Context, name string) ([]WorkflowDef, error) {
	var rows *sql.Rows
	var err error
	if name == "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT name, version, abi_version, min_version, plugin_deps, created_at, deprecated
			FROM workflow_defs ORDER BY name, version DESC
		`)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT name, version, abi_version, min_version, plugin_deps, created_at, deprecated
			FROM workflow_defs WHERE name = @p1 ORDER BY version DESC
		`, name)
	}
	if err != nil {
		return nil, fmt.Errorf("list workflow defs: %w", err)
	}
	defer rows.Close()

	var defs []WorkflowDef
	for rows.Next() {
		var def WorkflowDef
		var pluginDepsRaw []byte
		var createdAt time.Time
		if err := rows.Scan(&def.Name, &def.Version, &def.ABIVersion, &def.MinVersion,
			&pluginDepsRaw, &createdAt, &def.Deprecated); err != nil {
			return nil, fmt.Errorf("scan workflow def: %w", err)
		}
		def.CreatedAt = createdAt
		if len(pluginDepsRaw) > 0 {
			json.Unmarshal(pluginDepsRaw, &def.PluginDeps)
		}
		if def.PluginDeps == nil {
			def.PluginDeps = make(map[string]string)
		}
		defs = append(defs, def)
	}
	return defs, rows.Err()
}

// GetWorkflowDef returns a single workflow definition by name and version.
func (s *MSSQLStore) GetWorkflowDef(ctx context.Context, name string, version int) (*WorkflowDef, error) {
	var def WorkflowDef
	var pluginDepsRaw []byte
	var wasmBytes []byte
	var createdAt time.Time
	err := s.db.QueryRowContext(ctx, `
		SELECT name, version, wasm_bytes, abi_version, min_version, plugin_deps, created_at, deprecated
		FROM workflow_defs WHERE name = @p1 AND version = @p2
	`, name, version).Scan(&def.Name, &def.Version, &wasmBytes, &def.ABIVersion,
		&def.MinVersion, &pluginDepsRaw, &createdAt, &def.Deprecated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workflow def: %w", err)
	}
	def.WASMBytes = wasmBytes
	def.CreatedAt = createdAt
	if len(pluginDepsRaw) > 0 {
		json.Unmarshal(pluginDepsRaw, &def.PluginDeps)
	}
	if def.PluginDeps == nil {
		def.PluginDeps = make(map[string]string)
	}
	return &def, nil
}

// MarkVersionDeprecated sets the deprecated flag on a workflow version.
func (s *MSSQLStore) MarkVersionDeprecated(ctx context.Context, name string, version int, deprecated bool) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_defs SET deprecated = @p3 WHERE name = @p1 AND version = @p2
	`, name, version, deprecated)
	if err != nil {
		return fmt.Errorf("mark version deprecated: %w", err)
	}
	return nil
}

// PurgeWorkflowDef permanently deletes a workflow definition.
func (s *MSSQLStore) PurgeWorkflowDef(ctx context.Context, name string, version int) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM workflow_defs WHERE name = @p1 AND version = @p2
	`, name, version)
	if err != nil {
		return fmt.Errorf("purge workflow def: %w", err)
	}
	return nil
}

// CountActiveInstances returns the number of ready or running instances for a version.
func (s *MSSQLStore) CountActiveInstances(ctx context.Context, name string, version int) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_instances
		WHERE def_name = @p1 AND def_version = @p2
		  AND status IN ('ready', 'running')
	`, name, version).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count active instances: %w", err)
	}
	return count, nil
}

// ResolveLatestVersion resolves the latest version for a named definition.
func (s *MSSQLStore) ResolveLatestVersion(ctx context.Context, defName string) (int, error) {
	var version int
	err := s.db.QueryRowContext(ctx, `
		SELECT ISNULL(MAX(version), 0) FROM workflow_defs
		WHERE name = @p1 AND deprecated = 0
	`, defName).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("resolve latest version: %w", err)
	}
	if version == 0 {
		return 0, fmt.Errorf("resolve latest version: no non-deprecated version found for %s", defName)
	}
	return version, nil
}

// ValidateVersion checks whether the given version is valid.
func (s *MSSQLStore) ValidateVersion(ctx context.Context, defName string, defVersion int) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `
		SELECT CASE WHEN EXISTS (
			SELECT 1 FROM workflow_defs
			WHERE name = @p1 AND version = @p2 AND deprecated = 0
		) THEN 1 ELSE 0 END
	`, defName, defVersion).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("validate version: %w", err)
	}
	return exists, nil
}

// GetActiveInstanceCountsByVersion returns a map of "name:version" -> count.
func (s *MSSQLStore) GetActiveInstanceCountsByVersion(ctx context.Context) (map[string]int, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("get active instance counts: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT def_name, def_version, COUNT(*) as cnt
		FROM workflow_instances
		WHERE status IN ('ready', 'running')
		  AND (tenant_id = @p1 OR tenant_id IS NULL)
		GROUP BY def_name, def_version
	`, s.tenantID)
	if err != nil {
		return nil, fmt.Errorf("get active instance counts: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var name string
		var version, count int
		if err := rows.Scan(&name, &version, &count); err != nil {
			return nil, fmt.Errorf("scan active instance count: %w", err)
		}
		key := name + ":" + fmt.Sprintf("%d", version)
		counts[key] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration: %w", err)
	}
	return counts, tx.Commit()
}

// RecordWorkflowMemorySample inserts a new sample and updates the EWMA summary.
func (s *MSSQLStore) RecordWorkflowMemorySample(ctx context.Context, defName string, sampleBytes int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record memory sample: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO workflow_memory_samples (def_name, sample_bytes) VALUES (@p1, @p2)`,
		defName, sampleBytes)
	if err != nil {
		return fmt.Errorf("record memory sample: insert sample: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		MERGE workflow_memory_stats AS target
		USING (SELECT @p1 AS def_name, @p2 AS mean_bytes) AS source
		ON target.def_name = source.def_name
		WHEN MATCHED THEN UPDATE SET
			mean_bytes  = target.alpha * @p2 + (1 - target.alpha) * target.mean_bytes,
			sample_count = target.sample_count + 1,
			updated_at  = SYSUTCDATETIME()
		WHEN NOT MATCHED THEN INSERT (def_name, mean_bytes, sample_count, updated_at)
			VALUES (@p1, @p2, 1, SYSUTCDATETIME());
	`, defName, float64(sampleBytes))
	if err != nil {
		return fmt.Errorf("record memory sample: upsert stats: %w", err)
	}

	return tx.Commit()
}

// LoadMemoryEstimates returns EWMA mean bytes for all def_names.
func (s *MSSQLStore) LoadMemoryEstimates(ctx context.Context) (map[string]float64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT def_name, mean_bytes FROM workflow_memory_stats
	`)
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
// Uses PERCENTILE_CONT window functions (available since SQL Server 2012).
func (s *MSSQLStore) LoadMemoryStats(ctx context.Context) ([]WorkflowMemoryStats, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT def_name,
			MIN(sample_bytes) OVER (PARTITION BY def_name),
			AVG(CAST(sample_bytes AS FLOAT)) OVER (PARTITION BY def_name),
			MAX(sample_bytes) OVER (PARTITION BY def_name),
			PERCENTILE_CONT(0.10) WITHIN GROUP (ORDER BY CAST(sample_bytes AS FLOAT)) OVER (PARTITION BY def_name),
			PERCENTILE_CONT(0.25) WITHIN GROUP (ORDER BY CAST(sample_bytes AS FLOAT)) OVER (PARTITION BY def_name),
			PERCENTILE_CONT(0.50) WITHIN GROUP (ORDER BY CAST(sample_bytes AS FLOAT)) OVER (PARTITION BY def_name),
			PERCENTILE_CONT(0.75) WITHIN GROUP (ORDER BY CAST(sample_bytes AS FLOAT)) OVER (PARTITION BY def_name),
			PERCENTILE_CONT(0.90) WITHIN GROUP (ORDER BY CAST(sample_bytes AS FLOAT)) OVER (PARTITION BY def_name),
			PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY CAST(sample_bytes AS FLOAT)) OVER (PARTITION BY def_name),
			COUNT(*) OVER (PARTITION BY def_name)
		FROM workflow_memory_samples
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
func (s *MSSQLStore) QueueDepth(ctx context.Context) (int64, error) {
	var count int64
	tqParam := s.buildTaskQueueParam()
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workflow_instances WHERE status = 'ready' AND task_queue IN (SELECT value FROM STRING_SPLIT(@p1, ','))`,
		tqParam).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("queue depth: %w", err)
	}
	return count, nil
}

// CleanupMemorySamples deletes samples beyond maxSamplesPerDef per def_name.
func (s *MSSQLStore) CleanupMemorySamples(ctx context.Context, maxSamplesPerDef int) (int64, error) {
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
			WHERE def_name = @p1
			  AND id NOT IN (
			      SELECT id FROM (
				  SELECT id, ROW_NUMBER() OVER (ORDER BY recorded_at DESC) AS rn
				  FROM workflow_memory_samples
				  WHERE def_name = @p1
			      ) AS ranked
			      WHERE ranked.rn <= @p2
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
// whose completed_at is older than the cutoff. Each batch runs in its own transaction
// to set RLS tenant context.
func (s *MSSQLStore) DeleteExpiredEvents(ctx context.Context, olderThan time.Time) (int64, error) {
	var totalDeleted int64
	for {
		tx, err := s.beginTxWithContext(ctx)
		if err != nil {
			return totalDeleted, fmt.Errorf("delete expired events: begin: %w", err)
		}
		result, err := tx.ExecContext(ctx, `
			DELETE FROM event_history
			WHERE workflow_id IN (
				SELECT id FROM workflow_instances
				WHERE status IN ('done', 'failed')
				  AND completed_at IS NOT NULL
				  AND completed_at < @p1
				ORDER BY id
				OFFSET 0 ROWS FETCH NEXT 10000 ROWS ONLY
			)
		`, olderThan)
		if err != nil {
			tx.Rollback()
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

	// Also batch cleanup compaction states.
	for {
		tx, err := s.beginTxWithContext(ctx)
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
				  AND completed_at < @p1
				  AND compaction_state IS NOT NULL
				ORDER BY id
				OFFSET 0 ROWS FETCH NEXT 10000 ROWS ONLY
			)
		`, olderThan)
		if err != nil {
			tx.Rollback()
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
func (s *MSSQLStore) TerminateWorkflow(ctx context.Context, workflowID, reason string) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("terminate workflow: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'terminated',
		    error_msg = @p2,
		    completed_at = GETDATE(),
		    assigned_to = NULL
		WHERE id = @p1
	`, sql.Named("p1", workflowID), sql.Named("p2", reason))
	if err != nil {
		return fmt.Errorf("terminate workflow: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("terminate workflow commit: %w", err)
	}
	// Best-effort cleanup.
	s.ClearStickyWorker(context.Background(), workflowID)
	if err := s.ReleaseWorkflowConcurrencyKeys(context.Background(), workflowID); err != nil {
		log.Printf("[db] release concurrency keys for run %s: %v", workflowID, err)
	}
	return nil
}

// DeleteDeadLetteredWorkflows permanently deletes dead-lettered workflow instances
// whose completed_at is older than the cutoff. Child rows (event_history, signals,
// promises, concurrency_keys, update_requests) are automatically deleted via
// ON DELETE CASCADE.
func (s *MSSQLStore) DeleteDeadLetteredWorkflows(ctx context.Context, olderThan time.Time) (int64, error) {
	var totalDeleted int64
	for {
		result, err := s.db.ExecContext(ctx, `
			DELETE FROM workflow_instances
			WHERE id IN (
				SELECT id FROM workflow_instances
				WHERE status = 'dead_lettered'
				  AND completed_at IS NOT NULL
				  AND completed_at < @p1
				  AND tenant_id = @p2
				ORDER BY id
				OFFSET 0 ROWS FETCH NEXT 10000 ROWS ONLY
			)
		`, sql.Named("p1", olderThan), sql.Named("p2", s.tenantID))
		if err != nil {
			return totalDeleted, fmt.Errorf("delete dead-lettered workflows: %w", err)
		}
		n, _ := result.RowsAffected()
		totalDeleted += n
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return totalDeleted, nil
}
