package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
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
		        CASE WHEN $4 > 0 THEN $4 ELSE (SELECT MAX(version) FROM workflow_defs WHERE name = $1 AND NOT deprecated) END,
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
		`SELECT COALESCE((SELECT MAX(version) FROM workflow_defs WHERE name = $1 AND NOT deprecated), -1)`,
		defName).Scan(&resolvedVersion); err != nil {
		resolvedVersion = -2
	}
	log.Printf("[engine] StartChildWorkflowAtomic: defName=%q defVersion=%d resolvedVersion=%d tenantID=%s parentID=%s",
		defName, defVersion, resolvedVersion, s.tenantID, parentID)

	// 1. INSERT child workflow instance.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, parent_close_policy, task_queue, tenant_id, priority)
		VALUES ($1, $2,
		        CASE WHEN $5 > 0 THEN $5 ELSE (SELECT MAX(version) FROM workflow_defs WHERE name = $2 AND NOT deprecated) END,
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
	var prevCS string
	if event.Step > 1 {
		s.db.QueryRowContext(ctx,
			`SELECT COALESCE(checksum, '') FROM event_history WHERE workflow_id = $1 AND step = $2`,
			parentID, event.Step-1).Scan(&prevCS)
	}
	checksum := computeEventChecksum(event, prevCS)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO event_history (workflow_id, step, event_type, child_name, child_input, run_id, created_at, checksum)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (workflow_id, step) DO NOTHING
	`, parentID, event.Step, string(event.EventType),
		nullStr(event.ChildName), nullStr(event.ChildInput), nullStr(childID),
		time.UnixMilli(event.TimestampMs), checksum)
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

// StartChildWorkflowInSchema creates a child workflow in the given target schema.
// Implements CrossSchemaChildStore for cross-instance workflow cooperation.

func (s *PostgresStore) StartChildWorkflowInSchema(ctx context.Context, targetSchema, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error) {
	var runID string
	q := fmt.Sprintf(`
		INSERT INTO %s.workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, parent_close_policy, task_queue, priority)
		VALUES (gen_random_uuid(), $1,
		        CASE WHEN $4 > 0 THEN $4 ELSE (SELECT MAX(version) FROM %s.workflow_defs WHERE name = $1 AND NOT deprecated) END,
		        'ready', $2, $3,
		        COALESCE(NULLIF($5, ''), 'ABANDON'),
		        COALESCE((SELECT task_queue FROM %s.workflow_instances WHERE id = $3), 'default'), $6)
		RETURNING id
	`, pq.QuoteIdentifier(targetSchema), pq.QuoteIdentifier(targetSchema), pq.QuoteIdentifier(targetSchema))
	if err := s.db.QueryRowContext(ctx, q, defName, inputJSON, parentID, defVersion, parentClosePolicy, priority).Scan(&runID); err != nil {
		return "", fmt.Errorf("start child workflow in schema %q: %w", targetSchema, err)
	}
	return runID, nil
}

// GetChildResultInSchema polls a child workflow in the given target schema.

func (s *PostgresStore) GetChildResultInSchema(ctx context.Context, targetSchema, runID string) (string, bool, error) {
	var result string
	var status string
	q := fmt.Sprintf(`SELECT COALESCE(result, '{}'), status FROM %s.workflow_instances WHERE id = $1`,
		pq.QuoteIdentifier(targetSchema))
	err := s.db.QueryRowContext(ctx, q, runID).Scan(&result, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get child result in schema %q: %w", targetSchema, err)
	}
	if status == "done" || status == "failed" {
		return result, true, nil
	}
	return "", false, nil
}

// ReapStaleInstances reclaims workflow instances with stale heartbeats.
