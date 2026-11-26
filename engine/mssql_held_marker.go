package engine

import (
	"context"
	"fmt"
)

// MarkRunHeld and ClearRunHeld: see HeldRunMarker. SQL Server reports rows MATCHED, so a zero count is a lost fence, including for a clear of a marker that is already clear.
func (s *MSSQLStore) MarkRunHeld(ctx context.Context, runID, workerID string, generation int64) error {
	return s.setRunHeld(ctx, "mark run held", runID, workerID, generation, true)
}

func (s *MSSQLStore) ClearRunHeld(ctx context.Context, runID, workerID string, generation int64) error {
	return s.setRunHeld(ctx, "clear run held", runID, workerID, generation, false)
}

func (s *MSSQLStore) setRunHeld(ctx context.Context, what, runID, workerID string, generation int64, held bool) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("%s: begin: %w", what, err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances SET held_generation = CASE WHEN @p4 = 1 THEN generation ELSE NULL END
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p3 AND tenant_id = @p5
	`, runID, workerID, generation, held, s.tenantID)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: rows affected: %w", what, err)
	}
	if n == 0 {
		return ErrFenceLost
	}
	return tx.Commit()
}
