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

// CountActiveConcurrencyKeys counts concurrency keys that are still held.
//
// THE OTHER THREE METHODS IN THIS FILE WERE DEAD WITHOUT THIS ONE. ShardedStore
// reaches its shards through a single four-method `metricsStore` assertion
// (sharded_store.go), and PostgresStore satisfied only three of the four -- so
// the assertion failed, every shard hit the `continue`, and CountStalledWorkflows,
// CountEventHistoryTotal and CountActiveConcurrencyKeys each returned (0, nil).
// Zero with no error, on every sharded deployment: not a broken metric but a
// lying one, which is the distinction the comment at the top of this file draws.
//
// Counts HELD keys, not rows. A row whose expires_at has passed is a lease
// nobody has collected yet, and reporting it as active would make the gauge
// read high exactly when the key sweep falls behind -- turning a sweep-lag
// signal into a phantom contention signal. The sweep's own backlog is a
// different question and wants its own metric (cleat#1317's
// SetConcurrencyKeysExpiringSoon).
//
// RLS-forced table, so beginTxWithRLS rather than s.db -- see the file header.
func (s *PostgresStore) CountActiveConcurrencyKeys(ctx context.Context) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("count active concurrency keys: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only tx; Rollback returns ErrTxDone after Commit

	var count int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM concurrency_keys
		WHERE expires_at > now() AND tenant_id = $1
	`, s.tenantID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count active concurrency keys: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("count active concurrency keys: commit: %w", err)
	}
	return count, nil
}

// CountConcurrencyKeysExpiringSoon counts held keys whose lease runs out within
// `within` (cleat#1317).
//
// SEPARATE FROM CountActiveConcurrencyKeys, and the pair is the point. The total
// says how much contention exists; this says how much of it is about to be
// released whether or not its holder is finished. A sweep that falls behind
// shows up here FIRST -- keys pile into the window before they expire -- which
// is what makes it a leading indicator rather than a second way of counting the
// same thing.
//
// Bounded at both ends: `expires_at > now()` excludes leases already expired
// (those are CountActiveConcurrencyKeys' exclusion too, and counting them here
// would mix sweep lag into a contention signal), and `< now() + within` is the
// window.
//
// RLS-forced table, so beginTxWithRLS rather than s.db -- see the file header.
func (s *PostgresStore) CountConcurrencyKeysExpiringSoon(ctx context.Context, within time.Duration) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("count concurrency keys expiring soon: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only tx; Rollback returns ErrTxDone after Commit

	var count int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM concurrency_keys
		WHERE expires_at > now() AND expires_at < now() + $1::interval
		  AND tenant_id = $2
	`, fmt.Sprintf("%d seconds", int(within.Seconds())), s.tenantID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count concurrency keys expiring soon: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("count concurrency keys expiring soon: commit: %w", err)
	}
	return count, nil
}
