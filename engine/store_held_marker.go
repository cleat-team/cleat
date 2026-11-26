package engine

import (
	"context"
	"fmt"
)

// MarkRunHeld and ClearRunHeld: see HeldRunMarker. PostgreSQL keeps the fence and the marker on the same LEASE row, so each is one fenced UPDATE of it. Rows MATCHED is what
// PostgreSQL reports, so a zero count is a lost fence, including for a clear of a marker that is already clear.
func (s *PostgresStore) MarkRunHeld(ctx context.Context, runID, workerID string, generation int64) error {
	return s.setRunHeld(ctx, "mark run held", runID, workerID, generation, true)
}

func (s *PostgresStore) ClearRunHeld(ctx context.Context, runID, workerID string, generation int64) error {
	return s.setRunHeld(ctx, "clear run held", runID, workerID, generation, false)
}

func (s *PostgresStore) setRunHeld(ctx context.Context, what, runID, workerID string, generation int64, held bool) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("%s: begin: %w", what, err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_leases
		SET held_generation = CASE WHEN $4::boolean THEN generation ELSE NULL END
		WHERE id = $1 AND assigned_to = $2 AND generation = $3
	`, runID, workerID, generation, held)
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
