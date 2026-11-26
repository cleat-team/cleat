package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// MarkRunHeld and ClearRunHeld: see HeldRunMarker. MySQL reports rows CHANGED, not rows MATCHED, so an UPDATE that sets held_generation to what it already holds (a clear of a
// marker that is already clear, which is the ordinary case for a hold that was never marked) reports zero rows, exactly what a lost fence looks like. A zero count is therefore
// not read as a lost fence; the fence is asked separately, with a read, as PublishQueryState does.
func (s *MySQLStore) MarkRunHeld(ctx context.Context, runID, workerID string, generation int64) error {
	return s.setRunHeld(ctx, "mark run held", runID, workerID, generation, true)
}

func (s *MySQLStore) ClearRunHeld(ctx context.Context, runID, workerID string, generation int64) error {
	return s.setRunHeld(ctx, "clear run held", runID, workerID, generation, false)
}

func (s *MySQLStore) setRunHeld(ctx context.Context, what, runID, workerID string, generation int64, held bool) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("%s: begin: %w", what, err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances SET held_generation = IF(?, generation, NULL)
		WHERE id = ? AND assigned_to = ? AND tenant_id = ? AND generation = ?
	`, held, runID, workerID, s.tenantID, generation)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: rows affected: %w", what, err)
	}
	if n == 0 {
		var still int
		err := tx.QueryRowContext(ctx, `
			SELECT 1 FROM workflow_instances
			WHERE id = ? AND assigned_to = ? AND tenant_id = ? AND generation = ?
		`, runID, workerID, s.tenantID, generation).Scan(&still)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrFenceLost
		}
		if err != nil {
			return fmt.Errorf("%s: fence: %w", what, err)
		}
	}
	return tx.Commit()
}
