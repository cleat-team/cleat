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
	_, err = tx.ExecContext(ctx, `
		INSERT INTO event_history (workflow_id, step, event_type, child_name, child_input, run_id, created_at, checksum, tenant_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (workflow_id, step) DO NOTHING
	`, parentID, event.Step, string(event.EventType),
		nullStr(event.ChildName), nullStr(event.ChildInput), nullStr(childID),
		time.UnixMilli(event.TimestampMs), checksum, s.tenantID)
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

func (s *PostgresStore) GetChildResult(ctx context.Context, runID string) (string, bool, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return "", false, fmt.Errorf("get child result: begin: %w", err)
	}
	defer tx.Rollback()

	var result string
	var status string
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(result, '{}'), status FROM workflow_instances WHERE id = $1
	`, runID).Scan(&result, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, tx.Commit()
	}
	if err != nil {
		return "", false, fmt.Errorf("get child result: %w", err)
	}
	if status == "done" || status == "failed" {
		// Compact, matching the convention GetWorkflowByID and
		// GetPromise/ListPromises already follow for JSONB result/payload
		// columns: PostgreSQL's jsonb text output always inserts a space
		// after every ':' and ',', so a result written as `{"child":"done"}`
		// otherwise comes back as `{"child": "done"}`.
		compacted := bytes.NewBuffer(nil)
		if err := json.Compact(compacted, []byte(result)); err == nil {
			result = compacted.String()
		}
		return result, true, tx.Commit()
	}
	return "", false, tx.Commit()
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
