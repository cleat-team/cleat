package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PublishQueryState: see the interface. PostgreSQL keeps the fence on the LEASE row and query_state on the PAYLOAD row (cleat#3245), so this takes the lease row FOR SHARE first: a
// reclaim bumps the generation under an exclusive lock on that row, so it either committed before (the predicate matches nothing, ErrFenceLost) or has to wait for this commit. That is
// the same serialisation Heartbeat gets from its single UPDATE of the lease row; here the write lands on a different row, so the lock is taken explicitly.
func (s *PostgresStore) PublishQueryState(ctx context.Context, runID, workerID string, generation int64, queryState map[string]string) error {
	if queryState == nil {
		return nil
	}
	// Sealed exactly as FinalizeWorkflowSegment seals it (finalizeWorkflowSegmentInner): the same marshal, the same tenant cipher, the same column encoder. A deployment that seals
	// query_state must not have a plaintext copy written here by a path that skipped the encoder.
	tc, err := s.tenantCipherForWrite()
	if err != nil {
		return fmt.Errorf("publish query state: derive tenant key: %w", err)
	}
	sealed, err := encryptJSONColumnForStorage(string(marshalQueryState(queryState)), tc)
	if err != nil {
		return fmt.Errorf("publish query state: encrypt query_state: %w", err)
	}

	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("publish query state: begin: %w", err)
	}
	defer tx.Rollback()

	var held int
	err = tx.QueryRowContext(ctx, `
		SELECT 1 FROM workflow_leases
		WHERE id = $1 AND assigned_to = $2 AND generation = $3
		FOR SHARE
	`, runID, workerID, generation).Scan(&held)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrFenceLost
	}
	if err != nil {
		return fmt.Errorf("publish query state: fence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_payloads SET query_state = $2 WHERE id = $1`, runID, sealed); err != nil {
		return fmt.Errorf("publish query state: %w", err)
	}
	return tx.Commit()
}
