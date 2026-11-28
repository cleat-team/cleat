package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// EventChecksumReader is implemented by stores that can report the checksum
// stored with one event row. The worker uses it for a compaction state written
// before cleat#3837, which carries no PrevChecksum: the first retained row's own
// stored checksum is then the only start the chain has.
type EventChecksumReader interface {
	StoredEventChecksum(ctx context.Context, workflowID string, step int) (string, error)
}

// StoredEventChecksum returns the checksum stored with the event at step, or ""
// when there is no such row or it has none.
func (s *PostgresStore) StoredEventChecksum(ctx context.Context, workflowID string, step int) (string, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return "", fmt.Errorf("stored event checksum: begin: %w", err)
	}
	defer tx.Rollback()
	var checksum sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT checksum FROM event_history WHERE workflow_id = $1 AND step = $2
	`, workflowID, step).Scan(&checksum)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("stored event checksum: %w", err)
	}
	return checksum.String, tx.Commit()
}

func (s *MySQLStore) StoredEventChecksum(ctx context.Context, workflowID string, step int) (string, error) {
	var checksum sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT checksum FROM event_history WHERE workflow_id = ? AND tenant_id = ? AND step = ?
	`, workflowID, s.tenantID, step).Scan(&checksum)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("stored event checksum: %w", err)
	}
	return checksum.String, nil
}

func (s *MSSQLStore) StoredEventChecksum(ctx context.Context, workflowID string, step int) (string, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return "", fmt.Errorf("stored event checksum: begin: %w", err)
	}
	defer tx.Rollback()
	var checksum sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT checksum FROM event_history WHERE workflow_id = @p1 AND step = @p2 AND tenant_id = @p3
	`, workflowID, step, s.tenantID).Scan(&checksum)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("stored event checksum: %w", err)
	}
	return checksum.String, tx.Commit()
}

// StoredEventChecksum routes by workflow ID.
func (s *ShardedStore) StoredEventChecksum(ctx context.Context, workflowID string, step int) (string, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return "", fmt.Errorf("no shard available for workflow %s", workflowID)
	}
	r, ok := shard.Store.(EventChecksumReader)
	if !ok {
		return "", fmt.Errorf("shard store %T cannot report a stored event checksum", shard.Store)
	}
	return r.StoredEventChecksum(ctx, workflowID, step)
}
