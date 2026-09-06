package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

func (s *MSSQLStore) DeliverSignal(ctx context.Context, workflowID, signalName, payload string) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("deliver signal: begin: %w", err)
	}
	defer tx.Rollback()

	// A plain INSERT, and it is worth saying what it replaced because the
	// replaced statement was the subtlest tenant bug in this file.
	//
	// This was a MERGE, for upsert semantics, and IMPROVEMENT-PLAN 3.215
	// removed the upsert: a second signal of the same name overwrote the
	// first's payload with no error. With no MATCHED branch there is no
	// UPDATE, so the cross-tenant hazard that made `target.tenant_id = @p4`
	// load-bearing in the ON clause is gone with it -- a caller holding
	// another tenant's workflow id can no longer overwrite that workflow's
	// pending payload, because nothing overwrites anything. tenant_id is still
	// in the column list, scoping the row this call creates, and the wake below
	// is still scoped explicitly.
	//
	// The blind spot that hid the original defect is still worth remembering:
	// an audit asking whether `tenant_id` appears anywhere in the statement
	// answered yes, off the INSERT column list, which says nothing about the
	// row a MERGE MATCHES. That is why the gate 3.86 describes needs a
	// position-aware check rather than a substring one.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_signals (workflow_id, signal_name, payload, tenant_id)
		VALUES (@p1, @p2, @p3, @p4)
	`, workflowID, signalName, encodeJSONPayload(payload), s.tenantID)
	if err != nil {
		return err
	}

	// Scoped for the same reason as the MERGE above: without it, delivering a
	// signal to an id belonging to another tenant woke that tenant's workflow.
	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET next_wake_at = SYSUTCDATETIME()
		WHERE id = @p1 AND status IN ('ready', 'suspended') AND tenant_id = @p2
	`, workflowID, s.tenantID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// PollSignal returns the oldest unconsumed delivery with this name, without
// consuming it. ConsumeSignal removes it by id once the caller's
// signal_received event is durable.
func (s *MSSQLStore) PollSignal(ctx context.Context, workflowID, signalName string) (SignalDelivery, bool, error) {
	var id int64
	var payload string
	err := s.db.QueryRowContext(ctx, `
		SELECT TOP 1 id, payload FROM workflow_signals
		WHERE workflow_id = @p1 AND signal_name = @p2 AND tenant_id = @p3
		ORDER BY id
	`, workflowID, signalName, s.tenantID).Scan(&id, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return SignalDelivery{}, false, nil
	}
	if err != nil {
		return SignalDelivery{}, false, fmt.Errorf("poll signal: %w", err)
	}
	return SignalDelivery{ID: id, Payload: decodeJSONPayload(payload)}, true, nil
}

func (s *MSSQLStore) PollCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	return s.CheckCancellation(ctx, workflowID)
}

func (s *MSSQLStore) GetAllowedSignalCallers(ctx context.Context, workflowID string) ([]string, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT allowed_signals FROM workflow_instances WHERE id = @p1 AND tenant_id = @p2`,
		workflowID, s.tenantID).Scan(&raw)
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

// SetAllowedSignalCallers replaces the allowed_signals list for a workflow.
// See PostgresStore.SetAllowedSignalCallers. IMPROVEMENT-PLAN 3.15.
//
// In a transaction with setSessionContext, matching every other MSSQL write
// path (mssql_lifecycle.go, mssql_events.go, and PollAndClaimSignal below).
// SQL Server's RLS predicates read SESSION_CONTEXT, and the connector's
// per-connection setting does not survive the connection being recycled --
// IMPROVEMENT-PLAN 2.71.
//
// Stated as consistency rather than as a proven requirement, because it was
// falsified and it is not one at this suite's granularity: removing this call
// leaves every test in allowed_signals_writer_test.go green. Within a single
// test the connector's setting is still in force, so nothing returns the
// connection to the pool between the setup and the write, which is the only
// moment 2.71 is about. Keeping the call is still right -- production recycles
// connections constantly and the failure mode there is an UPDATE the policy
// filters to zero rows, which this method reports as ErrWorkflowNotFound, a
// missing workflow that is not missing. What would be wrong is claiming a test
// covers it.
func (s *MSSQLStore) SetAllowedSignalCallers(ctx context.Context, workflowID string, callers []string) error {
	return withRollbackGuaranteedRetry(ctx, "set allowed signal callers", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		return s.setAllowedSignalCallersOnce(ctx, workflowID, callers)
	})
}

func (s *MSSQLStore) setAllowedSignalCallersOnce(ctx context.Context, workflowID string, callers []string) error {
	encoded, err := encodeAllowedSignals(callers)
	if err != nil {
		return fmt.Errorf("set allowed signal callers: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set allowed signal callers: begin: %w", err)
	}
	defer tx.Rollback()

	if err := s.setSessionContext(tx); err != nil {
		return fmt.Errorf("set allowed signal callers: session context: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE workflow_instances SET allowed_signals = @p1 WHERE id = @p2 AND tenant_id = @p3`,
		encoded, workflowID, s.tenantID)
	if err != nil {
		return fmt.Errorf("set allowed signal callers: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set allowed signal callers: rows affected: %w", err)
	}
	if n == 0 {
		return ErrWorkflowNotFound
	}
	return tx.Commit()
}

// ConsumeSignal removes one delivery by id.
//
// No retry wrapper, and no transaction. The method this replaces read and
// deleted in one step, so it needed both: a transaction to be atomic, and
// withRollbackGuaranteedRetry to be safe to repeat. A DELETE of one known id
// is atomic on its own, and repeating it is the documented no-op -- so a
// retried DELETE cannot consume a second signal, which is the failure the old
// retry commentary existed to rule out.

func (s *MSSQLStore) ConsumeSignal(ctx context.Context, workflowID string, id int64) error {
	return mssqlRetry(ctx, "consume signal", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		_, err := s.db.ExecContext(ctx, `
			DELETE FROM workflow_signals
			WHERE id = @p1 AND workflow_id = @p2 AND tenant_id = @p3
		`, id, workflowID, s.tenantID)
		return err
	})
}

func (s *MSSQLStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error) {
	runID := uuid.New().String()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, parent_close_policy, task_queue, priority, tenant_id)
		VALUES (@p1, @p2,
		        CASE WHEN @p5 > 0 THEN @p5 ELSE (SELECT MAX(version) FROM workflow_defs WHERE name = @p2 AND deprecated = 0 AND tenant_id = @p8) END,
		        'ready', @p3, @p4,
		        ISNULL(NULLIF(@p6, ''), 'ABANDON'),
		        ISNULL((SELECT task_queue FROM workflow_instances WHERE id = @p4), 'default'), @p7, @p8)
	`, runID, defName, inputJSON, parentID, defVersion, parentClosePolicy, priority, s.tenantID)
	if err != nil {
		return "", fmt.Errorf("start child workflow: %w", err)
	}
	return runID, nil
}

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
		        CASE WHEN @p5 > 0 THEN @p5 ELSE (SELECT MAX(version) FROM workflow_defs WHERE name = @p2 AND deprecated = 0 AND tenant_id = @p8) END,
		        'ready', @p3, @p4,
		        ISNULL(NULLIF(@p6, ''), 'ABANDON'),
		        ISNULL((SELECT task_queue FROM workflow_instances WHERE id = @p4), 'default'), @p7, @p8)
	`, childID, defName, inputJSON, parentID, defVersion, parentClosePolicy, priority, s.tenantID)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: insert child: %w", err)
	}

	// 2. INSERT child_workflow event into parent's event_history.
	event.RunID = childID
	// previousStoredChecksum, not a hand-rolled read: it runs on tx (so it sees
	// this transaction and carries its RLS/tenant context), qualifies by
	// tenant_id, and distinguishes "no predecessor" from a failed read. The
	// copy that used to be here ran on s.db -- the raw pool, no RLS context --
	// and discarded the error, so under a non-superuser role it silently
	// checksummed against an empty predecessor and broke the chain.
	prevCS, err := s.previousStoredChecksum(ctx, tx, parentID, event.Step)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: previous checksum: %w", err)
	}
	checksum := computeEventChecksum(event, prevCS)
	// See PostgresStore.StartChildWorkflowAtomic for why the payload column
	// matters here: it carries the fields the checksum covers that have no
	// column of their own, and LoadEventHistory restores them from it. Omitted,
	// every child_workflow event failed VerifyWorkflowEvents.
	payloadJSON, _ := eventRecordToPayload(event)
	payloadArg := nullStr("")
	if len(payloadJSON) > 0 {
		payloadArg = sql.NullString{String: string(payloadJSON), Valid: true}
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO event_history (workflow_id, step, event_type, child_name, child_input, run_id, created_at, checksum, tenant_id, payload)
		SELECT @p1, @p2, @p3, @p4, @p5, @p6, @p7, @p8, @p9, @p10
		WHERE NOT EXISTS (
			SELECT 1 FROM event_history WHERE workflow_id = @p1 AND step = @p2
		)
	`, parentID, event.Step, string(event.EventType),
		nullStr(event.ChildName), nullStr(event.ChildInput), nullStr(childID),
		time.UnixMilli(event.TimestampMs), checksum, s.tenantID, payloadArg)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: insert event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("start child workflow atomic: commit: %w", err)
	}
	return childID, nil
}

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

func (s *MSSQLStore) CreatePromise(ctx context.Context, workflowID, promiseName, promiseID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_promises (workflow_id, promise_name, promise_id, tenant_id)
		VALUES (@p1, @p2, @p3, @p4)
	`, workflowID, promiseName, promiseID, s.tenantID)
	return err
}

func (s *MSSQLStore) ResolvePromise(ctx context.Context, promiseID, result string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE workflow_promises
		SET status = 'resolved', result = @p2, resolved_at = SYSUTCDATETIME()
		WHERE promise_id = @p1 AND tenant_id = @p3
	`, promiseID, result, s.tenantID)
	if err != nil {
		return err
	}
	if n, raErr := res.RowsAffected(); raErr == nil && n == 0 {
		return fmt.Errorf("resolve promise %s: %w", promiseID, ErrPromiseNotFound)
	}
	_, _ = s.db.ExecContext(ctx, `
		UPDATE workflow_instances SET next_wake_at = SYSUTCDATETIME()
		WHERE id = (SELECT workflow_id FROM workflow_promises
		            WHERE promise_id = @p1 AND tenant_id = @p2)
		  AND status IN ('ready', 'suspended')
	`, promiseID, s.tenantID)
	return nil
}

func (s *MSSQLStore) RejectPromise(ctx context.Context, promiseID, errMsg string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE workflow_promises
		SET status = 'rejected', error_msg = @p2, resolved_at = SYSUTCDATETIME()
		WHERE promise_id = @p1 AND tenant_id = @p3
	`, promiseID, errMsg, s.tenantID)
	if err != nil {
		return err
	}
	if n, raErr := res.RowsAffected(); raErr == nil && n == 0 {
		return fmt.Errorf("reject promise %s: %w", promiseID, ErrPromiseNotFound)
	}
	_, _ = s.db.ExecContext(ctx, `
		UPDATE workflow_instances SET next_wake_at = SYSUTCDATETIME()
		WHERE id = (SELECT workflow_id FROM workflow_promises
		            WHERE promise_id = @p1 AND tenant_id = @p2)
		  AND status IN ('ready', 'suspended')
	`, promiseID, s.tenantID)
	return nil
}

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

func (s *MSSQLStore) CreateUpdateRequest(ctx context.Context, workflowID, updateName, payload, promiseID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_update_requests (workflow_id, update_name, payload, promise_id, status, tenant_id)
		VALUES (@p1, @p2, @p3, @p4, 'pending', @p5)
	`, workflowID, updateName, encodeJSONPayload(payload), promiseID, s.tenantID)
	return err
}

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
		req.Payload = decodeJSONPayload(req.Payload)
		requests = append(requests, req)
	}
	return requests, rows.Err()
}

func (s *MSSQLStore) CompleteUpdateRequest(ctx context.Context, workflowID, updateName, result, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_update_requests
		SET status = 'completed', result = @p3, error_msg = @p4, completed_at = SYSUTCDATETIME()
		WHERE workflow_id = @p1 AND update_name = @p2 AND tenant_id = @p5 AND status = 'pending'
	`, workflowID, updateName, result, errMsg, s.tenantID)
	return err
}

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
	result, err := tx.ExecContext(ctx, `
		INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at, tenant_id)
		SELECT @p1, @p2, @p3, DATEADD(MICROSECOND, @p6, DATEADD(SECOND, @p4, SYSUTCDATETIME())), @p5
		WHERE NOT EXISTS (
			SELECT 1 FROM concurrency_keys WHERE key_hash = @p1 AND expires_at > SYSUTCDATETIME()
		)
	`, keyHash[:], key, workflowID, int(ttl/time.Second), s.tenantID,
		int((ttl % time.Second).Microseconds()))
	if err != nil {
		return false, fmt.Errorf("acquire concurrency key: %w", err)
	}
	n, _ := result.RowsAffected()
	return n > 0, tx.Commit()
}

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

func (s *MSSQLStore) ReapExpiredConcurrencyKeys(ctx context.Context) (int64, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("reap expired concurrency keys: begin: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `DELETE FROM concurrency_keys WHERE expires_at <= SYSUTCDATETIME() AND tenant_id = @p1`, s.tenantID)
	if err != nil {
		return 0, fmt.Errorf("reap expired concurrency keys: %w", err)
	}
	n, _ := result.RowsAffected()
	return n, tx.Commit()
}

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
