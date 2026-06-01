package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// ─────────────────────────────────────────────────────────────
// Dialect-aware SQL fragments
// ─────────────────────────────────────────────────────────────

// placeholder returns a dialect-correct placeholder for a 1-indexed position.
func (d Dialect) placeholder(pos int) string {
	switch d {
	case DialectPostgres:
		return fmt.Sprintf("$%d", pos)
	case DialectMySQL:
		return "?"
	case DialectMSSQL:
		return fmt.Sprintf("@p%d", pos)
	default:
		panic("unknown dialect: " + d)
	}
}

// nowExpr returns the dialect's current-timestamp expression.
func (d Dialect) nowExpr() string {
	switch d {
	case DialectPostgres:
		return "now()"
	case DialectMySQL:
		return "NOW(6)"
	case DialectMSSQL:
		return "SYSUTCDATETIME()"
	default:
		panic("unknown dialect: " + d)
	}
}

// intervalExpr returns a dialect-correct interval expression for
// "now - <secondsParamPos> * seconds". Used by reap-stale queries.
func (d Dialect) intervalExpr(secondsParamPos int) string {
	ph := d.placeholder(secondsParamPos)
	switch d {
	case DialectPostgres:
		return fmt.Sprintf("now() - interval '1 second' * %s", ph)
	case DialectMySQL:
		return fmt.Sprintf("NOW(6) - INTERVAL %s SECOND", ph)
	case DialectMSSQL:
		return fmt.Sprintf("DATEADD(SECOND, -%s, SYSUTCDATETIME())", ph)
	default:
		panic("unknown dialect: " + d)
	}
}

// timestampDiffExpr returns an expression for heartbeat_at < now - N seconds.
func (d Dialect) timestampDiffExpr(col string, secondsParamPos int) string {
	switch d {
	case DialectPostgres:
		return fmt.Sprintf("%s < now() - interval '1 second' * %s", col, d.placeholder(secondsParamPos))
	case DialectMySQL:
		return fmt.Sprintf("%s < NOW(6) - INTERVAL %s SECOND", col, d.placeholder(secondsParamPos))
	case DialectMSSQL:
		return fmt.Sprintf("%s < DATEADD(SECOND, -%s, SYSUTCDATETIME())", col, d.placeholder(secondsParamPos))
	default:
		panic("unknown dialect: " + d)
	}
}

// likeExpr returns a dialect-correct LIKE/ILIKE expression.
// Postgres uses ILIKE for case-insensitive matching; all dialects use LIKE
// for case-sensitive.
func (d Dialect) likeExpr(column string, pos int, caseInsensitive bool) string {
	ph := d.placeholder(pos)
	if caseInsensitive && d == DialectPostgres {
		return fmt.Sprintf("%s ILIKE %s", column, ph)
	}
	return fmt.Sprintf("%s LIKE %s", column, ph)
}

// castExpr returns a dialect-specific cast expression for text matching.
func (d Dialect) castExpr(column string) string {
	switch d {
	case DialectPostgres:
		return fmt.Sprintf("%s::text", column)
	case DialectMySQL:
		return fmt.Sprintf("CAST(%s AS CHAR)", column)
	case DialectMSSQL:
		return fmt.Sprintf("CAST(%s AS NVARCHAR(MAX))", column)
	default:
		panic("unknown dialect: " + d)
	}
}

// limitOffset returns a dialect-correct LIMIT/OFFSET clause.
func (d Dialect) limitOffset(limitPos, offsetPos int, hasOffset bool) string {
	limitPH := d.placeholder(limitPos)
	switch d {
	case DialectPostgres, DialectMySQL:
		if hasOffset {
			return fmt.Sprintf("LIMIT %s OFFSET %s", limitPH, d.placeholder(offsetPos))
		}
		return fmt.Sprintf("LIMIT %s", limitPH)
	case DialectMSSQL:
		if hasOffset {
			return fmt.Sprintf("OFFSET %s ROWS FETCH NEXT %s ROWS ONLY",
				d.placeholder(offsetPos), limitPH)
		}
		return fmt.Sprintf("OFFSET 0 ROWS FETCH NEXT %s ROWS ONLY", limitPH)
	default:
		panic("unknown dialect: " + d)
	}
}

// batchLimit returns a dialect-correct LIMIT clause for batch deletes/updates.
func (d Dialect) batchLimit(limitPos int) string {
	ph := d.placeholder(limitPos)
	switch d {
	case DialectPostgres, DialectMySQL:
		return fmt.Sprintf("LIMIT %s", ph)
	case DialectMSSQL:
		return fmt.Sprintf("OFFSET 0 ROWS FETCH NEXT %s ROWS ONLY", ph)
	default:
		panic("unknown dialect: " + d)
	}
}

// workflowInstanceColumns returns the column list for SELECT queries that
// return WorkflowInstance rows. Includes id, def_name, def_version, status,
// input, assigned_to, next_wake_at, error_code, error_op, error_msg, created_at,
// and generation.
func (d Dialect) workflowInstanceColumns() string {
	switch d {
	case DialectPostgres:
		return "id, def_name, def_version, status, input, assigned_to, next_wake_at, error_code, error_op, error_msg, created_at, generation, COALESCE(priority, 0) AS priority, COALESCE(trace_id, '') AS trace_id"
	case DialectMySQL, DialectMSSQL:
		return "id, def_name, def_version, status, input, COALESCE(assigned_to, ''), next_wake_at, error_code, error_op, error_msg, created_at, generation, COALESCE(priority, 0) AS priority, COALESCE(trace_id, '') AS trace_id"
	default:
		panic("unknown dialect: " + d)
	}
}

// workflowInstanceColumnsExtra returns the column list plus tenant_id and created_at.
func (d Dialect) workflowInstanceColumnsExtra() string {
	switch d {
	case DialectPostgres:
		return "id, def_name, def_version, status, input, assigned_to, next_wake_at, tenant_id, created_at, error_code, error_op, generation, COALESCE(priority, 0) AS priority, COALESCE(trace_id, '') AS trace_id"
	case DialectMySQL, DialectMSSQL:
		return "id, def_name, def_version, status, input, COALESCE(assigned_to, ''), next_wake_at, tenant_id, created_at, error_code, error_op, generation, COALESCE(priority, 0) AS priority, COALESCE(trace_id, '') AS trace_id"
	default:
		panic("unknown dialect: " + d)
	}
}

// ─────────────────────────────────────────────────────────────
// QueryBuilder — dynamic WHERE clause construction
// ─────────────────────────────────────────────────────────────

// QueryBuilder accumulates a SQL query with automatic dialect-correct
// placeholder numbering. Use for methods with optional WHERE filters
// (ListWorkflows, ListWorkflowDefs, etc.).
type QueryBuilder struct {
	d       Dialect
	buf     strings.Builder
	args    []interface{}
	nextPos int
}

// NewQueryBuilder returns a QueryBuilder initialized with a base SQL fragment.
// The base should end at a point where WHERE conditions can be appended
// (e.g. "SELECT ... FROM t WHERE 1=1").
func NewQueryBuilder(d Dialect, baseSQL string) *QueryBuilder {
	qb := &QueryBuilder{d: d, nextPos: 1}
	qb.buf.WriteString(baseSQL)
	return qb
}

// AddCondition appends " AND <cond>" with one auto-numbered placeholder.
// condFmt must contain exactly one "%s" verb for the placeholder.
func (qb *QueryBuilder) AddCondition(condFmt string, arg interface{}) {
	ph := qb.d.placeholder(qb.nextPos)
	qb.nextPos++
	qb.buf.WriteString(" AND ")
	qb.buf.WriteString(fmt.Sprintf(condFmt, ph))
	qb.args = append(qb.args, arg)
}

// AddLikeCondition appends " AND <column> LIKE/ILIKE <placeholder>" with
// the given pattern. Dialect-aware: Postgres uses ILIKE for
// case-insensitive matching; MySQL and MSSQL use LIKE (their default
// collations are case-insensitive).
func (qb *QueryBuilder) AddLikeCondition(column string, pattern string, caseInsensitive bool) {
	expr := qb.d.likeExpr(column, qb.nextPos, caseInsensitive)
	qb.nextPos++
	qb.buf.WriteString(" AND ")
	qb.buf.WriteString(expr)
	qb.args = append(qb.args, pattern)
}

// AddRaw appends a raw SQL fragment (no placeholders auto-managed).
// The fragment is appended after the current builder content.
func (qb *QueryBuilder) AddRaw(sql string) {
	qb.buf.WriteByte(' ')
	qb.buf.WriteString(sql)
}

// AddArgs appends arguments directly (for use with AddRaw when you manually
// wrote placeholders). Increments nextPos by len(args).
func (qb *QueryBuilder) AddArgs(args ...interface{}) {
	qb.args = append(qb.args, args...)
	qb.nextPos += len(args)
}

// NextPos returns the next placeholder index (for callers that need to
// write raw SQL with placeholders and then sync the counter).
func (qb *QueryBuilder) NextPos() int {
	return qb.nextPos
}

// SQL returns the built query string and argument slice.
func (qb *QueryBuilder) SQL() (string, []interface{}) {
	return qb.buf.String(), qb.args
}

// ─────────────────────────────────────────────────────────────
// Scan helpers
// ─────────────────────────────────────────────────────────────

// scanner abstracts *sql.Rows and *sql.Row for scanWorkflowInstance.
type scanner interface {
	Scan(dest ...interface{}) error
}

// scanWorkflowInstance scans a row into a WorkflowInstance, handling
// NullString/NullTime boilerplate and the MSSQL input-as-string quirk.
// Expects columns: id, def_name, def_version, status, input, assigned_to,
// next_wake_at, error_code, error_op, error_msg, created_at, generation.
func (d Dialect) scanWorkflowInstance(row scanner, wf *WorkflowInstance) error {
	var nextWakeAt, createdAt sql.NullTime
	var errorCode, errorOp, errorMsg sql.NullString

	if d == DialectMSSQL {
		var inputStr string
		if err := row.Scan(
			&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status,
			&inputStr, &wf.AssignedTo, &nextWakeAt, &errorCode, &errorOp,
			&errorMsg, &createdAt, &wf.Generation, &wf.Priority, &wf.TraceID,
		); err != nil {
			return err
		}
		wf.Input = json.RawMessage(inputStr)
	} else {
		if err := row.Scan(
			&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status,
			&wf.Input, &wf.AssignedTo, &nextWakeAt, &errorCode, &errorOp,
			&errorMsg, &createdAt, &wf.Generation, &wf.Priority, &wf.TraceID,
		); err != nil {
			return err
		}
	}

	if nextWakeAt.Valid {
		wf.NextWakeAt = nextWakeAt.Time
	}
	wf.ErrorCode = errorCode.String
	wf.ErrorOp = errorOp.String
	wf.Error = errorMsg.String
	if createdAt.Valid {
		wf.CreatedAt = createdAt.Time
	}
	return nil
}

// scanWorkflowInstanceExtra is like scanWorkflowInstance but also scans
// tenant_id (sql.NullString) and created_at (sql.NullTime).
func (d Dialect) scanWorkflowInstanceExtra(row scanner, wf *WorkflowInstance) error {
	var nextWakeAt, createdAt sql.NullTime
	var tenantID sql.NullString
	var errorCode, errorOp sql.NullString

	if d == DialectMSSQL {
		var inputStr string
		if err := row.Scan(
			&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status,
			&inputStr, &wf.AssignedTo, &nextWakeAt,
			&tenantID, &createdAt, &errorCode, &errorOp, &wf.Generation, &wf.Priority, &wf.TraceID,
		); err != nil {
			return err
		}
		wf.Input = json.RawMessage(inputStr)
	} else {
		if err := row.Scan(
			&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status,
			&wf.Input, &wf.AssignedTo, &nextWakeAt,
			&tenantID, &createdAt, &errorCode, &errorOp, &wf.Generation, &wf.Priority, &wf.TraceID,
		); err != nil {
			return err
		}
	}

	if nextWakeAt.Valid {
		wf.NextWakeAt = nextWakeAt.Time
	}
	if tenantID.Valid {
		wf.TenantID = tenantID.String
	}
	if createdAt.Valid {
		wf.CreatedAt = createdAt.Time
	}
	wf.ErrorCode = errorCode.String
	wf.ErrorOp = errorOp.String
	return nil
}
