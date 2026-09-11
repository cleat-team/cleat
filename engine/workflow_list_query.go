package engine

import (
	"context"
	"fmt"
)

// applyWorkflowFilters appends every WHERE clause a WorkflowFilter implies, and
// nothing else -- no ORDER BY, no LIMIT -- so that the listing and the count
// that accompanies it are guaranteed to select the same rows.
//
// # Why this is one function and not three
//
// PostgresStore.ListWorkflows, MySQLStore.ListWorkflows and
// MSSQLStore.ListWorkflows carried byte-identical filter logic, differing only
// in their preamble (PostgreSQL scopes by RLS and passes no tenant argument;
// the other two add one). Three copies of one rule is where divergence starts:
// this repository has already paid for that in the jobqueue reaper, whose
// PostgreSQL arm was repaired by copying a column name from an MSSQL arm that
// had never executed (cleat#1133, #1134, #1141).
//
// A new filter added to two arms out of three is not a visible failure. It is a
// listing that answers differently depending on which backend a tenant is on,
// and nothing fails.
func applyWorkflowFilters(qb *QueryBuilder, d Dialect, filter WorkflowFilter) {
	if filter.Status != "" {
		qb.AddCondition("status = %s", filter.Status)
	}
	if filter.DefName != "" {
		qb.AddCondition("def_name = %s", filter.DefName)
	}
	if filter.ErrorCode != "" {
		qb.AddCondition("error_code = %s", filter.ErrorCode)
	}
	if filter.IDPrefix != "" {
		// `id` is text on all three dialects, so no cast. Case-sensitive: run
		// ids are lowercase-hex UUIDs, and an ILIKE here would widen the match
		// for no caller's benefit.
		qb.AddLikeCondition("id", filter.IDPrefix+"%", false)
	}
	if !filter.StartedAfter.IsZero() {
		qb.AddCondition("created_at >= %s", filter.StartedAfter)
	}
	if !filter.StartedBefore.IsZero() {
		// Exclusive, so adjacent windows tile without a row on the boundary
		// appearing in both.
		qb.AddCondition("created_at < %s", filter.StartedBefore)
	}
	if filter.InputContains != "" {
		qb.AddLikeCondition(d.castExpr("input"), "%"+filter.InputContains+"%", true)
	}
	if filter.ErrorContains != "" {
		qb.AddLikeCondition("error_msg", "%"+filter.ErrorContains+"%", true)
	}
	if filter.Search != "" {
		pattern := "%" + filter.Search + "%"
		icol := d.castExpr("input")
		rcol := d.castExpr("result")
		n := qb.NextPos()
		// Search matches def_name in addition to input/result/error content: a
		// general "Search" box, as opposed to the targeted filters above, is
		// most often used to find workflows of a given type by name. DefName is
		// the precise form of that question and this stays the loose one.
		qb.AddRaw(fmt.Sprintf("AND (%s OR %s OR %s OR %s)",
			d.likeExpr(icol, n, true),
			d.likeExpr(rcol, n+1, true),
			d.likeExpr("error_msg", n+2, true),
			d.likeExpr("def_name", n+3, true)))
		qb.AddArgs(pattern, pattern, pattern, pattern)
	}
}

// workflowListOrder is the sort the listing pages over, and it is a TOTAL
// order. That is the whole point of the `, id DESC`.
//
// `ORDER BY created_at DESC` alone is not a total order, and the ties are
// structural rather than rare: created_at defaults to now(), which in
// PostgreSQL is TRANSACTION START time, so every row written by one
// transaction shares a timestamp to the microsecond. Measured on a scratch
// database -- 2006 rows inserted in a few transactions carried 3 distinct
// created_at values.
//
// Paging an untotally-ordered set with LIMIT/OFFSET is unsound: the database
// may return tied rows in any order, and it is free to choose differently for
// the query that fetches page 1 and the query that fetches page 2. Demonstrated
// before this line existed -- page 1, insert one row, page 2, and a row from
// page 1 came back on page 2 while another was never returned at all.
//
// `id` is the primary key, so appending it makes the order total on every
// dialect at the cost of one more sort key.
const workflowListOrder = "ORDER BY created_at DESC, id DESC"

// clampWorkflowListLimit is the store's own ceiling, applied regardless of what
// a caller asks for. 0 means "unspecified", not "none".
func clampWorkflowListLimit(limit int) int {
	if limit <= 0 {
		return 100
	}
	if limit > 1000 {
		return 1000
	}
	return limit
}

// applyWorkflowListPaging appends the order and the limit/offset.
func applyWorkflowListPaging(qb *QueryBuilder, d Dialect, filter WorkflowFilter) {
	qb.AddRaw(workflowListOrder)
	limit := clampWorkflowListLimit(filter.Limit)
	if filter.Offset > 0 {
		qb.AddRaw(d.limitOffset(qb.NextPos(), qb.NextPos()+1, true))
		qb.AddArgs(limit, filter.Offset)
	} else {
		qb.AddRaw(d.limitOffset(qb.NextPos(), 0, false))
		qb.AddArgs(limit)
	}
}

// CountWorkflows returns how many rows ListWorkflows would return for the same
// filter with no limit. It exists so a caller can tell a full page from the end
// of the data -- GET /api/workflows previously returned at most 100 rows in a
// bare array, and nothing distinguished that from a tenant that has exactly
// 100 (cleat#1182).
//
// It shares applyWorkflowFilters with the listing rather than restating the
// predicate, so "the count and the page disagree" is not a state this code can
// reach. A count assembled separately from the query it describes is the same
// defect as a summary table maintained by hand from the documents it
// summarises.
func (s *PostgresStore) CountWorkflows(ctx context.Context, filter WorkflowFilter) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("count workflows: begin: %w", err)
	}
	defer tx.Rollback()

	d := s.dialect
	qb := NewQueryBuilder(d, "SELECT COUNT(*) FROM workflow_instances WHERE 1=1")
	applyWorkflowFilters(qb, d, filter)

	query, args := qb.SQL()
	var n int
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count workflows: %w", err)
	}
	return n, tx.Commit()
}

// CountWorkflows: MySQL. Tenant scoping is an explicit predicate here, as it is
// in this store's ListWorkflows -- MySQL has no row-level security.
func (s *MySQLStore) CountWorkflows(ctx context.Context, filter WorkflowFilter) (int, error) {
	d := s.dialect
	qb := NewQueryBuilder(d, "SELECT COUNT(*) FROM workflow_instances WHERE tenant_id = ?")
	qb.AddArgs(s.tenantID)
	applyWorkflowFilters(qb, d, filter)

	query, args := qb.SQL()
	var n int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count workflows: %w", err)
	}
	return n, nil
}

// CountWorkflows: SQL Server.
func (s *MSSQLStore) CountWorkflows(ctx context.Context, filter WorkflowFilter) (int, error) {
	d := s.dialect
	qb := NewQueryBuilder(d, "SELECT COUNT(*) FROM workflow_instances WHERE tenant_id = @p1")
	qb.AddArgs(s.tenantID)
	applyWorkflowFilters(qb, d, filter)

	query, args := qb.SQL()
	var n int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count workflows: %w", err)
	}
	return n, nil
}
