package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PublishQueryState: see the interface. MySQL reports rows CHANGED, not rows MATCHED, so an UPDATE that sets query_state to what it already holds reports zero rows, which is
// exactly what a lost fence looks like. That is not hypothetical: a run that is replayed after a suspension sets the same state again. A zero count is therefore NOT read as a lost
// fence; the fence is asked separately, with a read.
func (s *MySQLStore) PublishQueryState(ctx context.Context, runID, workerID string, generation int64, queryState map[string]string) error {
	if queryState == nil {
		return nil
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("publish query state: begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances SET query_state = ?
		WHERE id = ? AND assigned_to = ? AND tenant_id = ? AND generation = ?
	`, queryStateUpdateParam(queryState), runID, workerID, s.tenantID, generation)
	if err != nil {
		return fmt.Errorf("publish query state: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("publish query state: rows affected: %w", err)
	}
	if n == 0 {
		var held int
		err := tx.QueryRowContext(ctx, `
			SELECT 1 FROM workflow_instances
			WHERE id = ? AND assigned_to = ? AND tenant_id = ? AND generation = ?
		`, runID, workerID, s.tenantID, generation).Scan(&held)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrFenceLost
		}
		if err != nil {
			return fmt.Errorf("publish query state: fence: %w", err)
		}
	}
	return tx.Commit()
}
