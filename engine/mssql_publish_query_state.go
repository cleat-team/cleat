package engine

import (
	"context"
	"fmt"
)

// PublishQueryState: see the interface. SQL Server reports rows MATCHED, so a zero count is a lost fence.
func (s *MSSQLStore) PublishQueryState(ctx context.Context, runID, workerID string, generation int64, queryState map[string]string) error {
	if queryState == nil {
		return nil
	}
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("publish query state: begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances SET query_state = @p4
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p3 AND tenant_id = @p5
	`, runID, workerID, generation, queryStateUpdateParam(queryState), s.tenantID)
	if err != nil {
		return fmt.Errorf("publish query state: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("publish query state: rows affected: %w", err)
	}
	if n == 0 {
		return ErrFenceLost
	}
	return tx.Commit()
}
