package engine

import (
	"context"
	"fmt"
	"time"
)

// The retention preview: what a sweep WOULD remove, without removing it.
// cleat#1457.
//
// Each Count method below pairs with exactly one Delete/Clear method and is
// built from the same predicate constant, so the two cannot drift. See
// retention_predicates.go for why that matters and for the one arm whose zero is
// structural rather than empty.
//
// BEST EFFORT, NOT A GUARANTEE, and the API says so. These counts are taken at
// one instant against a database other workers are writing to: between a preview
// and the sweep that follows it, workflows complete, rows age past the cutoff,
// and other retention runs may have removed some of what was counted. The number
// is the right order of magnitude for catching a mistyped `older_than` -- which
// is the whole reason the preview exists -- and is not a promise about which
// rows will be deleted.
//
// No LIMIT, deliberately. The sweep batches at 10000 because it is deleting; the
// count is a single read and reporting a batch size instead of a total would
// turn "you are about to delete five million rows" into "10000", which is the
// one answer that would not alarm anybody.

// CountExpiredEvents reports how many event_history rows DeleteExpiredEvents
// would remove.
//
// Counts event_history rows rather than workflows, because that is what the
// delete deletes -- a count of matching WORKFLOWS would be a different and much
// smaller number under the same predicate.
func (s *PostgresStore) CountExpiredEvents(ctx context.Context, olderThan time.Time) (int64, error) {
	return s.retentionCount(ctx, `
		SELECT count(*) FROM event_history
		WHERE workflow_id IN (
			SELECT id`+pgExpiredEventsWorkflows+`
		)
	`, "count expired events", olderThan)
}

// CountExpiredCompactionState reports how many workflow_instances rows
// ClearExpiredCompactionState would clear.
func (s *PostgresStore) CountExpiredCompactionState(ctx context.Context, olderThan time.Time) (int64, error) {
	return s.retentionCount(ctx, `
		SELECT count(*)`+pgExpiredCompactionState+`
	`, "count expired compaction state", olderThan)
}

// CountDeadLetteredWorkflows reports how many rows DeleteDeadLetteredWorkflows
// would remove.
func (s *PostgresStore) CountDeadLetteredWorkflows(ctx context.Context, olderThan time.Time) (int64, error) {
	return s.retentionCount(ctx, `
		SELECT count(*)`+pgDeadLetteredWorkflows+`
		  AND tenant_id = $2
	`, "count dead-lettered workflows", olderThan, s.tenantID)
}

// CountCompletedWorkflows reports how many rows DeleteCompletedWorkflows would
// remove.
func (s *PostgresStore) CountCompletedWorkflows(ctx context.Context, olderThan time.Time) (int64, error) {
	return s.retentionCount(ctx, `
		SELECT count(*)`+pgCompletedWorkflows+`
		  AND tenant_id = $2
	`, "count completed workflows", olderThan, s.tenantID)
}

// retentionCount runs one count through the same RLS-scoped transaction the
// corresponding delete uses.
//
// beginTxWithRLS rather than a bare query, and that is not ceremony: the delete
// paths are RLS-scoped, so a count taken outside that scope would report rows
// the sweep cannot see and cannot delete. The transaction is rolled back --
// there is nothing to commit, and a read does not need to hold.
func (s *PostgresStore) retentionCount(ctx context.Context, query, label string, args ...any) (int64, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("%s: begin: %w", label, err)
	}
	defer tx.Rollback()
	var n int64
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("%s: %w", label, err)
	}
	return n, nil
}

// ---- MySQL -----------------------------------------------------------------
//
// No RLS, so the tenant is a predicate argument rather than a session setting --
// see retention_predicates.go on why the two dialects' predicates differ here
// and why copying one to the other would be wrong in both directions.

func (s *MySQLStore) CountExpiredEvents(ctx context.Context, olderThan time.Time) (int64, error) {
	return s.retentionCount(ctx, `
		SELECT count(*) FROM event_history e
		INNER JOIN (
			SELECT id`+myExpiredEventsWorkflows+`
		  AND tenant_id = ?
		) AS w ON e.workflow_id = w.id
	`, "count expired events", olderThan, s.tenantID)
}

func (s *MySQLStore) CountExpiredCompactionState(ctx context.Context, olderThan time.Time) (int64, error) {
	return s.retentionCount(ctx, `
		SELECT count(*)`+myExpiredCompactionState+`
		  AND tenant_id = ?
	`, "count expired compaction state", olderThan, s.tenantID)
}

func (s *MySQLStore) CountDeadLetteredWorkflows(ctx context.Context, olderThan time.Time) (int64, error) {
	return s.retentionCount(ctx, `
		SELECT count(*)`+myDeadLetteredWorkflows+`
		  AND tenant_id = ?
	`, "count dead-lettered workflows", olderThan, s.tenantID)
}

func (s *MySQLStore) CountCompletedWorkflows(ctx context.Context, olderThan time.Time) (int64, error) {
	return s.retentionCount(ctx, `
		SELECT count(*)`+myCompletedWorkflows+`
		  AND tenant_id = ?
	`, "count completed workflows", olderThan, s.tenantID)
}

func (s *MySQLStore) retentionCount(ctx context.Context, query, label string, args ...any) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("%s: %w", label, err)
	}
	return n, nil
}

// ---- SQL Server ------------------------------------------------------------
//
// Positional arguments, which go-mssqldb maps onto @p1/@p2 -- the same form the
// inline sweep arms use. The batch constants use sql.Named for the same
// parameters; both spellings reach the same placeholders.

func (s *MSSQLStore) CountExpiredEvents(ctx context.Context, olderThan time.Time) (int64, error) {
	return s.retentionCount(ctx, `
		SELECT count(*) FROM event_history
		WHERE workflow_id IN (
			SELECT id`+msExpiredEventsWorkflows+`
		  AND tenant_id = @p2
		)
	`, "count expired events", olderThan, s.tenantID)
}

func (s *MSSQLStore) CountExpiredCompactionState(ctx context.Context, olderThan time.Time) (int64, error) {
	return s.retentionCount(ctx, `
		SELECT count(*)`+msExpiredCompactionState+`
		  AND tenant_id = @p2
	`, "count expired compaction state", olderThan, s.tenantID)
}

func (s *MSSQLStore) CountDeadLetteredWorkflows(ctx context.Context, olderThan time.Time) (int64, error) {
	return s.retentionCount(ctx, `
		SELECT count(*)`+msDeadLetteredWorkflows+`
		  AND tenant_id = @p2
	`, "count dead-lettered workflows", olderThan, s.tenantID)
}

func (s *MSSQLStore) CountCompletedWorkflows(ctx context.Context, olderThan time.Time) (int64, error) {
	return s.retentionCount(ctx, `
		SELECT count(*)`+msCompletedWorkflows+`
		  AND tenant_id = @p2
	`, "count completed workflows", olderThan, s.tenantID)
}

func (s *MSSQLStore) retentionCount(ctx context.Context, query, label string, args ...any) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("%s: %w", label, err)
	}
	return n, nil
}

// ---- sharded ---------------------------------------------------------------
//
// Summed across shards, mirroring how the delete arms report: one sweep spans
// every shard, so one preview must too. A per-shard error fails the whole count
// rather than returning a partial -- an undercount here is the one answer that
// would reassure an operator wrongly.

func (s *ShardedStore) CountExpiredEvents(ctx context.Context, olderThan time.Time) (int64, error) {
	return s.retentionCountAcrossShards(ctx, olderThan, WorkflowStore.CountExpiredEvents)
}

func (s *ShardedStore) CountExpiredCompactionState(ctx context.Context, olderThan time.Time) (int64, error) {
	return s.retentionCountAcrossShards(ctx, olderThan, WorkflowStore.CountExpiredCompactionState)
}

func (s *ShardedStore) CountDeadLetteredWorkflows(ctx context.Context, olderThan time.Time) (int64, error) {
	return s.retentionCountAcrossShards(ctx, olderThan, WorkflowStore.CountDeadLetteredWorkflows)
}

func (s *ShardedStore) CountCompletedWorkflows(ctx context.Context, olderThan time.Time) (int64, error) {
	return s.retentionCountAcrossShards(ctx, olderThan, WorkflowStore.CountCompletedWorkflows)
}

func (s *ShardedStore) retentionCountAcrossShards(ctx context.Context, olderThan time.Time,
	count func(WorkflowStore, context.Context, time.Time) (int64, error),
) (int64, error) {
	var total int64
	for _, sh := range s.shards {
		n, err := count(sh.Store, ctx, olderThan)
		if err != nil {
			return 0, fmt.Errorf("shard %s: %w", sh.Config.Name, err)
		}
		total += n
	}
	return total, nil
}
