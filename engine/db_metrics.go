package engine

import (
	"context"
	"fmt"
	"time"
)

// CountStalledWorkflows counts running workflows without recent progress.
func (s *PostgresStore) CountStalledWorkflows(ctx context.Context, threshold time.Duration) (int, error) {
	cutoff := time.Now().Add(-threshold)
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_instances
		WHERE status = 'running'
		  AND (heartbeat_at IS NULL OR heartbeat_at < $1)
		  AND created_at < $2
		  AND tenant_id = $3
	`, cutoff, cutoff, s.tenantID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count stalled workflows: %w", err)
	}
	return count, nil
}

// CountEventHistoryTotal returns total rows in event_history.
func (s *PostgresStore) CountEventHistoryTotal(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_history WHERE tenant_id = $1`, s.tenantID).Scan(&count)
	return count, err
}

// EstimateEventHistorySize returns the estimated size of event_history in bytes.
func (s *PostgresStore) EstimateEventHistorySize(ctx context.Context) (int64, error) {
	var size int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(pg_total_relation_size('event_history'), 0)`).Scan(&size)
	return size, err
}
