package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

func (s *PostgresStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return "", fmt.Errorf("start child workflow: begin: %w", err)
	}
	defer tx.Rollback()

	var runID string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, parent_close_policy, task_queue, tenant_id, priority)
		VALUES (gen_random_uuid(), $1,
		        CASE WHEN $4 > 0 THEN $4 ELSE (SELECT MAX(version) FROM workflow_defs WHERE name = $1 AND NOT deprecated AND tenant_id = $6) END,
		        'ready', $2, $3,
		        COALESCE(NULLIF($5, ''), 'ABANDON'),
		        COALESCE((SELECT task_queue FROM workflow_instances WHERE id = $3), 'default'),
			$6, $7)
		RETURNING id
	`, defName, inputJSON, parentID, defVersion, parentClosePolicy, s.tenantID, priority).Scan(&runID)
	if err != nil {
		return "", fmt.Errorf("start child workflow: %w", err)
	}
	pgNotify(ctx, tx, s.notifyChannel)
	return runID, tx.Commit()
}

// StartChildWorkflowAtomic creates a child workflow and records the parent's
// child_workflow event in a single transaction, guaranteeing exactly-once creation.

func (s *PostgresStore) StartChildWorkflowAtomic(ctx context.Context, childID, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, event EventRecord, priority int) (string, error) {
	if childID == "" {
		childID = uuid.New().String()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := s.setRLSOnTx(tx); err != nil {
		return "", fmt.Errorf("start child workflow atomic: set rls: %w", err)
	}

	// Debug: check what MAX(version) resolves to.
	var resolvedVersion int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT MAX(version) FROM workflow_defs WHERE name = $1 AND NOT deprecated AND tenant_id = $2), -1)`,
		defName, s.tenantID).Scan(&resolvedVersion); err != nil {
		resolvedVersion = -2
	}
	s.log().DebugContext(ctx, "StartChildWorkflowAtomic",
		"def_name", defName, "def_version", defVersion, "resolved_version", resolvedVersion, "tenant_id", s.tenantID, "parent_id", parentID)

	// 1. INSERT child workflow instance.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, parent_close_policy, task_queue, tenant_id, priority)
		VALUES ($1, $2,
		        CASE WHEN $5 > 0 THEN $5 ELSE (SELECT MAX(version) FROM workflow_defs WHERE name = $2 AND NOT deprecated AND tenant_id = $7) END,
		        'ready', $3, $4,
		        COALESCE(NULLIF($6, ''), 'ABANDON'),
		        COALESCE((SELECT task_queue FROM workflow_instances WHERE id = $4), 'default'),
			$7, $8)
	`, childID, defName, inputJSON, parentID, defVersion, parentClosePolicy, s.tenantID, priority)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: insert child: %w", err)
	}

	// 2. INSERT child_workflow event into the parent's event_history.
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

	// The payload column, which this INSERT used to omit.
	//
	// It is not a duplicate of the named columns. eventRecordToPayload is what
	// the checksum is computed over, and for child_workflow it covers
	// parent_workflow_id and parent_close_policy -- neither of which has a
	// column on event_history. LoadEventHistory restores those fields by
	// calling populateFromPayload on this column; with the column NULL they
	// come back empty, VerifyWorkflowEvents recomputes the checksum without
	// them, and it cannot match what was written here. Every workflow that
	// spawned a child failed verification, deterministically.
	//
	// Plaintext, matching the batch write path: see the note on
	// PostgresStore.encryption for why encryption is confined to flushEvent.
	payloadJSON, _ := eventRecordToPayload(event)
	payloadArg := nullStr("")
	if len(payloadJSON) > 0 {
		payloadArg = sql.NullString{String: string(payloadJSON), Valid: true}
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO event_history (workflow_id, step, event_type, child_name, child_input, run_id, created_at, checksum, tenant_id, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (workflow_id, step) DO NOTHING
	`, parentID, event.Step, string(event.EventType),
		nullStr(event.ChildName), nullStr(event.ChildInput), nullStr(childID),
		time.UnixMilli(event.TimestampMs), checksum, s.tenantID, payloadArg)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: insert event: %w", err)
	}

	pgNotify(ctx, tx, s.notifyChannel)
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("start child workflow atomic: commit: %w", err)
	}
	return childID, nil
}

// GetChildResult checks whether a child workflow has completed (status 'done' or 'failed').

func (s *PostgresStore) GetChildResult(ctx context.Context, runID string) (ChildOutcome, error) {
	// Resolve the chain first: the run the parent STARTED is not necessarily
	// the run that holds the answer. A child that continues as new leaves its
	// first run at status 'done' with an empty result -- 'done' because it was
	// superseded, not because it finished -- and reading that row returned {}
	// while the real result sat on the last run in the chain (cleat#955).
	//
	// Nothing distinguishes "done because it continued" from "done because it
	// finished" on the row itself. The successor lookup does: a run with no
	// successor is the terminal one. Only WHICH row is read changes here;
	// everything below -- the status test, the result compaction -- is
	// untouched.
	runID, err := terminalRunID(ctx, runID, s.successorOfRun)
	if err != nil {
		return ChildOutcome{}, err
	}
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return ChildOutcome{}, fmt.Errorf("get child result: begin: %w", err)
	}
	defer tx.Rollback()

	var result string
	var status string
	var errMsg sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(result, '{}'), status, error_msg FROM workflow_instances WHERE id = $1
	`, runID).Scan(&result, &status, &errMsg)
	if errors.Is(err, sql.ErrNoRows) {
		return ChildOutcome{}, tx.Commit()
	}
	if err != nil {
		return ChildOutcome{}, fmt.Errorf("get child result: %w", err)
	}
	if status == "failed" || status == "dead_lettered" {
		// dead_lettered is terminal and was missing here until cleat#1213,
		// while GetChildCount forty lines down has always excluded all three
		// of ('done', 'failed', 'dead_lettered'). Two definitions of terminal
		// in one file, and only this one decides whether a parent stops
		// waiting -- so a parent awaiting a child that exhausted its retries
		// suspended, was re-claimed on its next_wake_at, replayed, got the
		// same non-answer and suspended again, for the life of the deployment.
		//
		// Reported as FAILED rather than left to a later retry, and the reason
		// is that the alternative does not exist: nothing pushes a parent
		// awake. executor.go names "child completion via wakeParent"; there is
		// no wakeParent in this repo. The only wake is the timeout, so "the
		// parent waits for the child to be retried" and "the parent replays
		// forever" are the same behaviour, and only one of them is a story.
		//
		// A failed run's `result` column is never written -- migration 053
		// routes finalize's payload to `error_msg` on this branch -- so
		// returning the result here would return the '{}' from the COALESCE
		// above, which is exactly the empty success cleat#1115 is about.
		// MoveToDeadLetterQueue writes its reason to the same column.
		return ChildOutcome{Completed: true, Failed: true, Error: errMsg.String}, tx.Commit()
	}
	if status == "done" {
		// Compact, matching the convention GetWorkflowByID and
		// GetPromise/ListPromises already follow for JSONB result/payload
		// columns: PostgreSQL's jsonb text output always inserts a space
		// after every ':' and ',', so a result written as `{"child":"done"}`
		// otherwise comes back as `{"child": "done"}`.
		compacted := bytes.NewBuffer(nil)
		if err := json.Compact(compacted, []byte(result)); err == nil {
			result = compacted.String()
		}
		return ChildOutcome{Completed: true, Result: result}, tx.Commit()
	}
	return ChildOutcome{}, tx.Commit()
}

// GetChildCount returns the number of active (non-terminal) child workflows
// for the given parent workflow. Terminal statuses are excluded.

func (s *PostgresStore) GetChildCount(ctx context.Context, parentWorkflowID string) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("get child count for %s: begin: %w", parentWorkflowID, err)
	}
	defer tx.Rollback()

	var count int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_instances
		WHERE parent_workflow_id = $1 AND status NOT IN ('done', 'failed', 'dead_lettered')
	`, parentWorkflowID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get child count for %s: %w", parentWorkflowID, err)
	}
	return count, tx.Commit()
}

// ReapStaleInstances reclaims workflow instances with stale heartbeats.

// GetChildCompletedAtMs returns the child's completion instant in Unix
// milliseconds. See the ChildWorkflowStore doc comment for why PollChild needs
// the instant rather than a boolean, and engine/children.go's
// pollChildIsDeterministic for the clock-domain caveat.
//
// completed_at is written by now() inside finalize_workflow_status, so this is
// the DATABASE clock.
func (s *PostgresStore) GetChildCompletedAtMs(ctx context.Context, runID string) (int64, bool, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("get child completed_at: begin: %w", err)
	}
	defer tx.Rollback()

	var completedAt sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT completed_at FROM workflow_instances WHERE id = $1
	`, runID).Scan(&completedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, tx.Commit()
	}
	if err != nil {
		return 0, false, fmt.Errorf("get child completed_at: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("get child completed_at: commit: %w", err)
	}
	if !completedAt.Valid {
		return 0, false, nil
	}
	return completedAt.Time.UnixMilli(), true, nil
}
