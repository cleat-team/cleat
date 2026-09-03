package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/lib/pq"
)

// Admin re-replay -- IMPROVEMENT-PLAN 3.20's third body, and the one that was
// deliberately left a stub until 1.4 phases D-F existed.
//
// It resets a stopped workflow to 'ready' so the dispatcher picks it up and
// replays its recorded history, continuing from where it stopped. That is not
// the same as the dead-letter reprocess path, which starts a *new* run from the
// definition and input: this one preserves every step already recorded, so a
// workflow that failed on its ninth call does not re-issue the first eight.
//
// Why it needed D-F rather than a fourth UPDATE, which is what 3.20 warned
// about: replaying a history whose last call was left mid-flight by a crash
// means replaying into an [AMBIGUOUS] step. Before phase E there was no way to
// resolve one, and before phase F no way for an operator to. Re-replaying into
// an unresolved ambiguity just reproduces the same failure, which is why
// ReReplay refuses it and says which step to resolve first.

// adminReReplayMiss turns a zero-row re-replay UPDATE into the specific reason.
// Three outcomes rather than adminResolveMiss's two: the status precondition
// can fail with the generation matching, and reporting that as a generation
// mismatch would send an operator looking for a concurrent writer that does not
// exist.
func adminReReplayMiss(status string, stored, requested int64, workflowID string, found bool) error {
	if !found {
		return adminNotFound(adminActionReReplay, workflowID)
	}
	if stored != requested {
		return adminGenerationMismatch(adminActionReReplay, workflowID, stored, requested)
	}
	return fmt.Errorf("admin %s: workflow %s is %s, and only %v can be re-replayed",
		adminActionReReplay, workflowID, status, reReplayableStatuses)
}

// reReplayAudit is the audit record for a re-replay. It reuses adminForce so
// the operator/action/reason mapping stays in the one place EventFromRecord
// reverses.
func reReplayAudit(operator string) adminForce {
	return adminForce{action: adminActionReReplay, operator: operator}
}

func (s *PostgresStore) AdminReReplay(ctx context.Context, workflowID string, generation int64, operator string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("admin %s: begin: %w", adminActionReReplay, err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL,
		    completed_at = NULL, error_msg = NULL, error_code = NULL, error_op = NULL,
		    next_wake_at = now(), generation = generation + 1
		WHERE id = $1 AND tenant_id = $2 AND generation = $3
		  AND status = ANY($4)
	`, workflowID, s.tenantID, generation, pq.Array(reReplayableStatuses))
	if err != nil {
		return fmt.Errorf("admin %s: %w", adminActionReReplay, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("admin %s: rows affected: %w", adminActionReReplay, err)
	}
	if n == 0 {
		var status string
		var stored int64
		err := tx.QueryRowContext(ctx,
			`SELECT status, generation FROM workflow_instances WHERE id = $1 AND tenant_id = $2`,
			workflowID, s.tenantID).Scan(&status, &stored)
		if errors.Is(err, sql.ErrNoRows) {
			return adminReReplayMiss("", 0, generation, workflowID, false)
		}
		if err != nil {
			return fmt.Errorf("admin %s: resolve miss for workflow %s: %w", adminActionReReplay, workflowID, err)
		}
		return adminReReplayMiss(status, stored, generation, workflowID, true)
	}

	if err := s.adminAppendAudit(ctx, tx, workflowID, reReplayAudit(operator)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *MySQLStore) AdminReReplay(ctx context.Context, workflowID string, generation int64, operator string) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("admin %s: begin: %w", adminActionReReplay, err)
	}
	defer tx.Rollback()

	// IN (?,?,?) expanded from the shared list rather than written out, so the
	// three dialects cannot drift on which statuses are re-replayable.
	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL,
		    completed_at = NULL, error_msg = NULL, error_code = NULL, error_op = NULL,
		    next_wake_at = NOW(6), generation = generation + 1
		WHERE id = ? AND tenant_id = ? AND generation = ?
		  AND status IN (`+sqlPlaceholders(len(reReplayableStatuses), "?")+`)
	`, append([]any{workflowID, s.tenantID, generation}, statusArgs()...)...)
	if err != nil {
		return fmt.Errorf("admin %s: %w", adminActionReReplay, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("admin %s: rows affected: %w", adminActionReReplay, err)
	}
	if n == 0 {
		var status string
		var stored int64
		err := tx.QueryRowContext(ctx,
			`SELECT status, generation FROM workflow_instances WHERE id = ? AND tenant_id = ?`,
			workflowID, s.tenantID).Scan(&status, &stored)
		if errors.Is(err, sql.ErrNoRows) {
			return adminReReplayMiss("", 0, generation, workflowID, false)
		}
		if err != nil {
			return fmt.Errorf("admin %s: resolve miss for workflow %s: %w", adminActionReReplay, workflowID, err)
		}
		return adminReReplayMiss(status, stored, generation, workflowID, true)
	}

	if err := s.adminAppendAudit(ctx, tx, workflowID, reReplayAudit(operator)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *MSSQLStore) AdminReReplay(ctx context.Context, workflowID string, generation int64, operator string) error {
	return withRollbackGuaranteedRetry(ctx, "admin "+adminActionReReplay,
		mssqlTxRetries, mssqlTxRetryDelay, func() error {
			return s.adminReReplayOnce(ctx, workflowID, generation, operator)
		})
}

func (s *MSSQLStore) adminReReplayOnce(ctx context.Context, workflowID string, generation int64, operator string) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("admin %s: begin: %w", adminActionReReplay, err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL,
		    completed_at = NULL, error_msg = NULL, error_code = NULL, error_op = NULL,
		    next_wake_at = SYSUTCDATETIME(), generation = generation + 1
		WHERE id = @p1 AND tenant_id = @p2 AND generation = @p3
		  AND status IN (`+sqlPlaceholders(len(reReplayableStatuses), "@p")+`)
	`, append([]any{workflowID, s.tenantID, generation}, statusArgs()...)...)
	if err != nil {
		return fmt.Errorf("admin %s: %w", adminActionReReplay, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("admin %s: rows affected: %w", adminActionReReplay, err)
	}
	if n == 0 {
		var status string
		var stored int64
		err := tx.QueryRowContext(ctx,
			`SELECT status, generation FROM workflow_instances WHERE id = @p1 AND tenant_id = @p2`,
			workflowID, s.tenantID).Scan(&status, &stored)
		if errors.Is(err, sql.ErrNoRows) {
			return adminReReplayMiss("", 0, generation, workflowID, false)
		}
		if err != nil {
			return fmt.Errorf("admin %s: resolve miss for workflow %s: %w", adminActionReReplay, workflowID, err)
		}
		return adminReReplayMiss(status, stored, generation, workflowID, true)
	}

	if err := s.adminAppendAudit(ctx, tx, workflowID, reReplayAudit(operator)); err != nil {
		return err
	}
	return tx.Commit()
}

// statusArgs returns reReplayableStatuses as driver args.
func statusArgs() []any {
	out := make([]any, len(reReplayableStatuses))
	for i, s := range reReplayableStatuses {
		out[i] = s
	}
	return out
}

// sqlPlaceholders builds "?,?,?" or "@p4,@p5,@p6" for an IN list. MSSQL's
// named parameters are positional here: the three fixed args above occupy
// @p1-@p3, so the list starts at @p4.
func sqlPlaceholders(n int, style string) string {
	out := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			out += ","
		}
		if style == "?" {
			out += "?"
		} else {
			out += fmt.Sprintf("@p%d", i+4)
		}
	}
	return out
}
