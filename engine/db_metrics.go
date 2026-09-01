package engine

import (
	"context"
	"fmt"
	"time"
)

// The two counts below read RLS-forced tables (workflow_instances and
// event_history), so they go through beginTxWithRLS rather than s.db. On the
// raw pool nothing has called set_config, cleat.assert_tenant_set() raises, and
// under a non-superuser role -- the NOSUPERUSER/NOBYPASSRLS cleat_app the
// cluster compose file uses -- these return
// "cleat.tenant_id is not set" instead of a count.
//
// They differ from the defect in §3.44 in one way that matters: they check
// their errors, so they failed loudly rather than reporting a confident wrong
// number. Broken metrics, not lying metrics.
//
// Guarded by TestMetricsQueriesWorkUnderRLS.

// CountStalledWorkflows counts running workflows without recent progress.
func (s *PostgresStore) CountStalledWorkflows(ctx context.Context, threshold time.Duration) (int, error) {
	cutoff := time.Now().Add(-threshold)

	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("count stalled workflows: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only tx; Rollback returns ErrTxDone after Commit

	var count int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_instances
		WHERE status = 'running'
		  AND (heartbeat_at IS NULL OR heartbeat_at < $1)
		  AND created_at < $2
		  AND tenant_id = $3
	`, cutoff, cutoff, s.tenantID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count stalled workflows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("count stalled workflows: commit: %w", err)
	}
	return count, nil
}

// CountEventHistoryTotal returns total rows in event_history.
func (s *PostgresStore) CountEventHistoryTotal(ctx context.Context) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("count event history: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only tx; Rollback returns ErrTxDone after Commit

	var count int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM event_history WHERE tenant_id = $1`, s.tenantID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count event history: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("count event history: commit: %w", err)
	}
	return count, nil
}

// EstimateEventHistorySize returns the estimated size of event_history in bytes.
//
// Stays on s.db deliberately: pg_total_relation_size reads catalog metadata and
// touches no rows, so no RLS policy is ever evaluated and a tenant context would
// buy nothing. Verified in TestMetricsQueriesWorkUnderRLS, which asserts it
// works on a non-superuser connection so that a future change making it read
// rows does not slip through unnoticed.
func (s *PostgresStore) EstimateEventHistorySize(ctx context.Context) (int64, error) {
	var size int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(pg_total_relation_size('event_history'), 0)`).Scan(&size)
	return size, err
}
