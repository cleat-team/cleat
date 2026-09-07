package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cespare/xxhash/v2"
)

func (s *PostgresStore) CreatePromise(ctx context.Context, workflowID, promiseName, promiseID string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("create promise: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_promises (workflow_id, promise_id, promise_name, status, tenant_id)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (workflow_id, promise_id) DO NOTHING
	`, workflowID, promiseID, promiseName, "pending", s.tenantID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ResolvePromise marks a promise as resolved with the given result.
// Also wakes the workflow instance so it can pick up the resolved promise
// on the next poll cycle instead of waiting for the original timeout.

// ErrPromiseNotFound is returned when settling a promise matched no row.
//
// Settling one that does not exist used to be a SILENT no-op in all three
// dialects: the UPDATE ran, matched nothing, and returned nil. Making it
// loud (#818) is what made settling-by-ID-alone safe to introduce (#813):
// a settle that reaches no row now says so, rather than reporting success
// to a caller holding an ID that is wrong or expired.
//
// It says nothing about WHY the row was absent -- no such promise, wrong
// tenant, already purged -- deliberately, and for the same reason
// ErrWorkflowNotFound does not: distinguishing them is an existence oracle
// over IDs the caller was not given.
var ErrPromiseNotFound = errors.New("promise not found")

func (s *PostgresStore) ResolvePromise(ctx context.Context, promiseID, result string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("resolve promise: begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_promises SET status = $2, result = $3, resolved_at = now()
		WHERE promise_id = $1
	`, promiseID, "resolved", result)
	if err != nil {
		return err
	}
	if n, raErr := res.RowsAffected(); raErr == nil && n == 0 {
		return fmt.Errorf("resolve promise %s: %w", promiseID, ErrPromiseNotFound)
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances SET next_wake_at = now()
		WHERE id = (SELECT workflow_id FROM workflow_promises WHERE promise_id = $1)
		  AND status IN ('ready', 'suspended')
	`, promiseID)
	if err != nil {
		return err
	}
	pgNotify(ctx, tx, s.notifyChannel)
	return tx.Commit()
}

// RejectPromise marks a promise as rejected with the given error message.
// Also wakes the workflow instance so it can pick up the rejected promise
// on the next poll cycle instead of waiting for the original timeout.

func (s *PostgresStore) RejectPromise(ctx context.Context, promiseID, errMsg string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("reject promise: begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_promises SET status = $2, error_msg = $3, resolved_at = now()
		WHERE promise_id = $1
	`, promiseID, "rejected", errMsg)
	if err != nil {
		return err
	}
	if n, raErr := res.RowsAffected(); raErr == nil && n == 0 {
		return fmt.Errorf("reject promise %s: %w", promiseID, ErrPromiseNotFound)
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances SET next_wake_at = now()
		WHERE id = (SELECT workflow_id FROM workflow_promises WHERE promise_id = $1)
		  AND status IN ('ready', 'suspended')
	`, promiseID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// GetPromise returns the current status and result of a promise.

func (s *PostgresStore) GetPromise(ctx context.Context, workflowID, promiseID string) (status string, result string, errMsg string, err error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return "", "", "", fmt.Errorf("get promise: begin: %w", err)
	}
	defer tx.Rollback()

	var resultStr, errStr sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT status, result #>> '{}', error_msg FROM workflow_promises
		WHERE workflow_id = $1 AND promise_id = $2 AND tenant_id = $3
	`, workflowID, promiseID, s.tenantID).Scan(&status, &resultStr, &errStr)
	if errors.Is(err, sql.ErrNoRows) {
		return "pending", "", "", tx.Commit()
	}
	if err != nil {
		return "", "", "", err
	}
	if resultStr.Valid {
		compacted := bytes.NewBuffer(nil)
		if err := json.Compact(compacted, []byte(resultStr.String)); err == nil {
			resultStr.String = compacted.String()
		}
	}
	return status, resultStr.String, errStr.String, tx.Commit()
}

// ListPromises returns all promises for a workflow ordered by creation time.

func (s *PostgresStore) ListPromises(ctx context.Context, workflowID string) ([]PromiseInfo, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("list promises: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT promise_id, promise_name, status, COALESCE(result #>> '{}', ''), COALESCE(error_msg, ''), created_at, resolved_at
		FROM workflow_promises
		WHERE workflow_id = $1 AND tenant_id = $2
		ORDER BY priority ASC, created_at
	`, workflowID, s.tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var promises []PromiseInfo
	for rows.Next() {
		var pi PromiseInfo
		var resolvedAt sql.NullTime
		if err := rows.Scan(&pi.PromiseID, &pi.PromiseName, &pi.Status, &pi.Result, &pi.ErrorMsg, &pi.CreatedAt, &resolvedAt); err != nil {
			return nil, err
		}
		if len(pi.Result) > 0 {
			compacted := bytes.NewBuffer(nil)
			if err := json.Compact(compacted, []byte(pi.Result)); err == nil {
				pi.Result = compacted.String()
			}
		}
		if resolvedAt.Valid {
			pi.ResolvedAt = &resolvedAt.Time
		}
		promises = append(promises, pi)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return promises, tx.Commit()
}

// ---- Concurrency Key implementations (Feature 5) ----

// AcquireConcurrencyKey tries to acquire a concurrency key for a workflow.
// Returns true if acquired, false if already held by another workflow.

func (s *PostgresStore) CreateUpdateRequest(ctx context.Context, workflowID, updateName, payload, promiseID string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("create update request: begin: %w", err)
	}
	defer tx.Rollback()

	// encodeJSONPayload, not `"` + payload + `"`: the concatenation produces
	// invalid JSON the moment the payload contains a quote or a backslash, and
	// is then rejected by the very column it exists to satisfy. That is the
	// second half of 2.60c, which fixed it for signals and left this copy
	// behind. IMPROVEMENT-PLAN 3.19.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_update_requests (workflow_id, update_name, payload, promise_id, status, tenant_id)
		VALUES ($1, $2, $3, $4, 'pending', $5)
	`, workflowID, updateName, encodeJSONPayload(payload), promiseID, s.tenantID)
	if err != nil {
		return err
	}

	// Wake the workflow, exactly as DeliverSignal does.
	//
	// Not optional: an update is delivered at a DISPATCH POINT in the guest,
	// and a suspended workflow reaches no dispatch point. Without this the
	// request sits pending until something else happens to wake the workflow --
	// which for a workflow waiting on a signal or a long sleep may be never.
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET next_wake_at = now()
		WHERE id = $1 AND status IN ('ready', 'suspended')
	`, workflowID); err != nil {
		return err
	}
	pgNotify(ctx, tx, s.notifyChannel)
	return tx.Commit()
}

// GetPendingUpdateRequests returns all pending (not yet dispatched) update requests.

func (s *PostgresStore) GetPendingUpdateRequests(ctx context.Context, workflowID string) ([]UpdateRequestInfo, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("get pending update requests: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT workflow_id, update_name, payload #>> '{}', COALESCE(promise_id, ''), status,
		       COALESCE(result #>> '{}', ''), COALESCE(error_msg, ''), created_at
		FROM workflow_update_requests
		WHERE workflow_id = $1 AND tenant_id = $2 AND status = 'pending'
		ORDER BY priority ASC, created_at
	`, workflowID, s.tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var requests []UpdateRequestInfo
	for rows.Next() {
		var r UpdateRequestInfo
		if err := rows.Scan(&r.WorkflowID, &r.UpdateName, &r.Payload, &r.PromiseID,
			&r.Status, &r.Result, &r.ErrorMsg, &r.CreatedAt); err != nil {
			return nil, err
		}
		if len(r.Payload) > 0 {
			compacted := bytes.NewBuffer(nil)
			if err := json.Compact(compacted, []byte(r.Payload)); err == nil {
				r.Payload = compacted.String()
			}
		}
		if len(r.Result) > 0 {
			compacted := bytes.NewBuffer(nil)
			if err := json.Compact(compacted, []byte(r.Result)); err == nil {
				r.Result = compacted.String()
			}
		}
		requests = append(requests, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return requests, tx.Commit()
}

// CompleteUpdateRequest marks an update request as completed with a result or error.

// jsonOrNull renders "" as SQL NULL rather than as an empty string.
//
// `workflow_update_requests.result` is JSONB on Postgres and JSON on MySQL, and
// **"" is not valid JSON** -- Postgres rejects it with
// `invalid input syntax for type json (22P02)` and MySQL with an invalid-JSON
// error. Every FAILING update completes with an empty result by construction:
// cleat/runtime_updates.go passes "" on all three failure paths (no handler
// registered, validator refusal, handler error), and the worker's stranded-update
// sweep passes "" too.
//
// So before this helper, an update that failed could not be recorded as failed.
// The UPDATE errored, the row stayed `pending`, and the caller's promise was
// never settled -- the exact symptom updates were built to fix, restored on the
// failure path. See IMPROVEMENT-PLAN 3.245.
//
// The column is nullable in all three dialects and GetPendingUpdateRequests
// already reads it back through COALESCE(...,”), so NULL round-trips to "" and
// nothing above the store sees a difference.
//
// All three dialects reject "", and it took a measurement to know that. SQL
// Server stores `result` as NVARCHAR(MAX), and `migrations/mssql/001_schema.sql`
// carries a CHECK on `payload` only -- so reading 001 says MSSQL accepts "" and
// silently holds a non-JSON value. It does not:
// `migrations/mssql/037_json_column_checks.sql` adds
//
//	CHECK (result IS NULL OR ISJSON(result) = 1)
//
// and the falsification failed there too, with a CHECK-constraint conflict
// rather than a JSON parse error. That is CLAUDE.md's rule about migrations --
// find the highest-numbered one that defines a thing before concluding
// anything -- and the first version of this comment broke it.
//
// Re-derive rather than trusting this paragraph:
//
//	grep -rln ck_workflow_update_requests_result migrations/mssql/
func jsonOrNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *PostgresStore) CompleteUpdateRequest(ctx context.Context, workflowID, updateName, result, errMsg string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("complete update request: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_update_requests
		SET status = 'completed', result = $3, error_msg = $4, completed_at = now()
		WHERE workflow_id = $1 AND update_name = $2 AND tenant_id = $5 AND status = 'pending'
	`, workflowID, updateName, jsonOrNull(result), errMsg, s.tenantID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// NextCronTime, matchField and the cron parser now live in cron.go.

// computeEventChecksum computes an xxHash64 checksum of the event record's data,
// chained with the previous event's checksum so that deleting an event breaks
// the chain for all subsequent events. When previousChecksum is empty (first
// event or unavailable), it is omitted from the computation.
//
// xxHash64 is ~20x faster than SHA-256 and sufficient for integrity verification;
// the chain is not a security boundary (cryptographic hashing is unnecessary).
func computeEventChecksum(rec EventRecord, previousChecksum string) string {
	payload, err := eventRecordToPayload(rec)
	if err != nil {
		data := fmt.Sprintf("%d:%s:%s:%s:%s:%s", rec.Step, rec.EventType, rec.Service, rec.Op, rec.Request, rec.Response)
		h := xxhash.Sum64String(data)
		if previousChecksum == "" {
			return fmt.Sprintf("%016x", h)
		}
		return fmt.Sprintf("%016x", xxhash.Sum64String(previousChecksum+":"+fmt.Sprintf("%016x", h)))
	}
	h := xxhash.Sum64(payload)
	if previousChecksum == "" {
		return fmt.Sprintf("%016x", h)
	}
	return fmt.Sprintf("%016x", xxhash.Sum64String(previousChecksum+":"+fmt.Sprintf("%016x", h)))
}

// VerifyWorkflowEvents loads all events for a workflow, recomputes their
// SHA-256 checksums, and verifies integrity. When the checksum column is
// available (after migration), it compares stored vs. recomputed checksums.
// Before the migration, it computes checksums silently and returns nil.
//
// Required migration for full verification:
//
//	ALTER TABLE event_history ADD COLUMN IF NOT EXISTS checksum TEXT;
