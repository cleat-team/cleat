package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

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

func (s *PostgresStore) ResolvePromise(ctx context.Context, workflowID, promiseID, result string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("resolve promise: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_promises SET status = $3, result = $4, resolved_at = now()
		WHERE workflow_id = $1 AND promise_id = $2
	`, workflowID, promiseID, "resolved", result)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances SET next_wake_at = now()
		WHERE id = $1 AND status IN ('ready', 'suspended')
	`, workflowID)
	if err != nil {
		return err
	}
	pgNotify(ctx, tx, s.notifyChannel)
	return tx.Commit()
}

// RejectPromise marks a promise as rejected with the given error message.
// Also wakes the workflow instance so it can pick up the rejected promise
// on the next poll cycle instead of waiting for the original timeout.

func (s *PostgresStore) RejectPromise(ctx context.Context, workflowID, promiseID, errMsg string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("reject promise: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_promises SET status = $3, error_msg = $4, resolved_at = now()
		WHERE workflow_id = $1 AND promise_id = $2
	`, workflowID, promiseID, "rejected", errMsg)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances SET next_wake_at = now()
		WHERE id = $1 AND status IN ('ready', 'suspended')
	`, workflowID)
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
	`, workflowID, updateName, result, errMsg, s.tenantID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// NextCronTime computes the next firing time for a 5-field cron expression
// (minute hour day-of-month month day-of-week) from the given time.
func NextCronTime(cronExpr string, from time.Time) time.Time {
	fields := strings.Fields(cronExpr)
	if len(fields) != 5 {
		return from.Add(24 * time.Hour) // fallback: daily
	}

	// Start at the next minute.
	t := from.Truncate(time.Minute).Add(time.Minute)

	// Search up to 4 years ahead.
	end := from.AddDate(4, 0, 0)
	for t.Before(end) {
		if matchField(fields[0], t.Minute(), 0, 59) &&
			matchField(fields[1], t.Hour(), 0, 23) &&
			matchField(fields[2], t.Day(), 1, 31) &&
			matchField(fields[3], int(t.Month()), 1, 12) &&
			matchField(fields[4], int(t.Weekday()), 0, 6) {
			// Also verify day-of-month is valid for this month.
			if t.Day() <= daysInMonth(t.Year(), t.Month()) {
				return t
			}
		}
		t = t.Add(time.Minute)
	}
	return from.Add(24 * time.Hour)
}

func matchField(pattern string, value int, min, max int) bool {
	if pattern == "*" {
		return true
	}
	// Handle step values: */N
	if strings.HasPrefix(pattern, "*/") {
		step := atoi(strings.TrimPrefix(pattern, "*/"))
		if step > 0 {
			return (value-min)%step == 0
		}
		return false
	}
	// Handle comma-separated lists.
	for _, part := range strings.Split(pattern, ",") {
		part = strings.TrimSpace(part)
		// Handle ranges: N-M
		if strings.Contains(part, "-") {
			rangeParts := strings.Split(part, "-")
			lo, hi := atoi(rangeParts[0]), atoi(rangeParts[1])
			if value >= lo && value <= hi {
				return true
			}
		} else if atoi(part) == value {
			return true
		}
	}
	return false
}

func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func atoi(s string) int {
	var n int
	fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n
}

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
