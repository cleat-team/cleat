package engine

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Dialect method tests
// ---------------------------------------------------------------------------

func TestDialect_placeholder(t *testing.T) {
	tests := []struct {
		d    Dialect
		pos  int
		want string
	}{
		{DialectPostgres, 1, "$1"},
		{DialectPostgres, 3, "$3"},
		{DialectMySQL, 1, "?"},
		{DialectMySQL, 5, "?"},
		{DialectMSSQL, 1, "@p1"},
		{DialectMSSQL, 4, "@p4"},
	}

	for _, tt := range tests {
		got := tt.d.placeholder(tt.pos)
		if got != tt.want {
			t.Errorf("%s.placeholder(%d) = %q, want %q", tt.d, tt.pos, got, tt.want)
		}
	}
}

func TestDialect_placeholder_unknown_panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for unknown dialect")
		}
	}()
	Dialect("oracle").placeholder(1)
}

func TestDialect_nowExpr(t *testing.T) {
	tests := []struct {
		d    Dialect
		want string
	}{
		{DialectPostgres, "now()"},
		{DialectMySQL, "NOW(6)"},
		{DialectMSSQL, "SYSUTCDATETIME()"},
	}

	for _, tt := range tests {
		got := tt.d.nowExpr()
		if got != tt.want {
			t.Errorf("%s.nowExpr() = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestDialect_nowExpr_unknown_panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	Dialect("oracle").nowExpr()
}

func TestDialect_intervalExpr(t *testing.T) {
	tests := []struct {
		d       Dialect
		pos     int
		contains string
	}{
		{DialectPostgres, 2, "interval '1 second' * $2"},
		{DialectMySQL, 2, "INTERVAL ? SECOND"},
		{DialectMSSQL, 2, "DATEADD(SECOND, -@p2, SYSUTCDATETIME())"},
	}

	for _, tt := range tests {
		got := tt.d.intervalExpr(tt.pos)
		if !strings.Contains(got, tt.contains) {
			t.Errorf("%s.intervalExpr(%d) = %q, want containing %q", tt.d, tt.pos, got, tt.contains)
		}
	}
}

func TestDialect_intervalExpr_unknown_panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	Dialect("oracle").intervalExpr(1)
}

func TestDialect_timestampDiffExpr(t *testing.T) {
	tests := []struct {
		d        Dialect
		col      string
		pos      int
		contains string
	}{
		{DialectPostgres, "heartbeat_at", 3, "heartbeat_at < now() - interval '1 second' * $3"},
		{DialectMySQL, "heartbeat_at", 3, "heartbeat_at < NOW(6) - INTERVAL ? SECOND"},
		{DialectMSSQL, "heartbeat_at", 3, "heartbeat_at < DATEADD(SECOND, -@p3, SYSUTCDATETIME())"},
	}

	for _, tt := range tests {
		got := tt.d.timestampDiffExpr(tt.col, tt.pos)
		if !strings.Contains(got, tt.contains) {
			t.Errorf("%s.timestampDiffExpr(%q, %d) = %q, want containing %q", tt.d, tt.col, tt.pos, got, tt.contains)
		}
	}
}

func TestDialect_timestampDiffExpr_unknown_panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	Dialect("oracle").timestampDiffExpr("col", 1)
}

func TestDialect_likeExpr(t *testing.T) {
	tests := []struct {
		d               Dialect
		col             string
		pos             int
		caseInsensitive bool
		want            string
	}{
		{DialectPostgres, "name", 3, true, "name ILIKE $3"},
		{DialectPostgres, "name", 3, false, "name LIKE $3"},
		{DialectMySQL, "name", 3, true, "name LIKE ?"},
		{DialectMySQL, "name", 3, false, "name LIKE ?"},
		{DialectMSSQL, "name", 3, true, "name LIKE @p3"},
		{DialectMSSQL, "name", 3, false, "name LIKE @p3"},
	}

	for _, tt := range tests {
		got := tt.d.likeExpr(tt.col, tt.pos, tt.caseInsensitive)
		if got != tt.want {
			t.Errorf("%s.likeExpr(%q, %d, %v) = %q, want %q", tt.d, tt.col, tt.pos, tt.caseInsensitive, got, tt.want)
		}
	}
}

func TestDialect_castExpr(t *testing.T) {
	tests := []struct {
		d      Dialect
		col    string
		expect string
	}{
		{DialectPostgres, "payload", "payload::text"},
		{DialectMySQL, "payload", "CAST(payload AS CHAR)"},
		{DialectMSSQL, "payload", "CAST(payload AS NVARCHAR(MAX))"},
	}

	for _, tt := range tests {
		got := tt.d.castExpr(tt.col)
		if got != tt.expect {
			t.Errorf("%s.castExpr(%q) = %q, want %q", tt.d, tt.col, got, tt.expect)
		}
	}
}

func TestDialect_castExpr_unknown_panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	Dialect("oracle").castExpr("col")
}

func TestDialect_limitOffset(t *testing.T) {
	tests := []struct {
		d         Dialect
		limitPos  int
		offsetPos int
		hasOffset bool
		contains  string
	}{
		{DialectPostgres, 2, 3, true, "LIMIT $2 OFFSET $3"},
		{DialectPostgres, 2, 3, false, "LIMIT $2"},
		{DialectMySQL, 2, 3, true, "LIMIT ? OFFSET ?"},
		{DialectMySQL, 2, 3, false, "LIMIT ?"},
		{DialectMSSQL, 2, 3, true, "OFFSET @p3 ROWS FETCH NEXT @p2 ROWS ONLY"},
		{DialectMSSQL, 2, 3, false, "OFFSET 0 ROWS FETCH NEXT @p2 ROWS ONLY"},
	}

	for _, tt := range tests {
		got := tt.d.limitOffset(tt.limitPos, tt.offsetPos, tt.hasOffset)
		if !strings.Contains(got, tt.contains) {
			t.Errorf("%s.limitOffset(%d, %d, %v) = %q, want containing %q", tt.d, tt.limitPos, tt.offsetPos, tt.hasOffset, got, tt.contains)
		}
	}
}

func TestDialect_limitOffset_unknown_panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	Dialect("oracle").limitOffset(1, 2, true)
}

func TestDialect_batchLimit(t *testing.T) {
	tests := []struct {
		d    Dialect
		pos  int
		want string
	}{
		{DialectPostgres, 1, "LIMIT $1"},
		{DialectMySQL, 1, "LIMIT ?"},
		{DialectMSSQL, 1, "OFFSET 0 ROWS FETCH NEXT @p1 ROWS ONLY"},
	}

	for _, tt := range tests {
		got := tt.d.batchLimit(tt.pos)
		if got != tt.want {
			t.Errorf("%s.batchLimit(%d) = %q, want %q", tt.d, tt.pos, got, tt.want)
		}
	}
}

func TestDialect_batchLimit_unknown_panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	Dialect("oracle").batchLimit(1)
}

func TestDialect_workflowInstanceColumns(t *testing.T) {
	pg := DialectPostgres.workflowInstanceColumns()
	if !strings.Contains(pg, "COALESCE(priority, 0) AS priority") {
		t.Errorf("Postgres columns missing COALESCE: %s", pg)
	}

	mysql := DialectMySQL.workflowInstanceColumns()
	if !strings.Contains(mysql, "COALESCE(assigned_to, '')") {
		t.Errorf("MySQL columns missing COALESCE assigned_to: %s", mysql)
	}

	mssql := DialectMSSQL.workflowInstanceColumns()
	if !strings.Contains(mssql, "COALESCE(assigned_to, '')") {
		t.Errorf("MSSQL columns missing COALESCE assigned_to: %s", mssql)
	}
}

func TestDialect_workflowInstanceColumns_unknown_panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	Dialect("oracle").workflowInstanceColumns()
}

func TestDialect_workflowInstanceColumnsExtra(t *testing.T) {
	pg := DialectPostgres.workflowInstanceColumnsExtra()
	if !strings.Contains(pg, "tenant_id") {
		t.Errorf("Postgres extra columns missing tenant_id: %s", pg)
	}

	mysql := DialectMySQL.workflowInstanceColumnsExtra()
	if !strings.Contains(mysql, "tenant_id") {
		t.Errorf("MySQL extra columns missing tenant_id: %s", mysql)
	}

	mssql := DialectMSSQL.workflowInstanceColumnsExtra()
	if !strings.Contains(mssql, "tenant_id") {
		t.Errorf("MSSQL extra columns missing tenant_id: %s", mssql)
	}
}

func TestDialect_workflowInstanceColumnsExtra_unknown_panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	Dialect("oracle").workflowInstanceColumnsExtra()
}

// ---------------------------------------------------------------------------
// QueryBuilder tests
// ---------------------------------------------------------------------------

func TestQueryBuilder_AddLikeCondition(t *testing.T) {
	t.Run("case insensitive postgres", func(t *testing.T) {
		qb := NewQueryBuilder(DialectPostgres, "SELECT * FROM wf WHERE 1=1")
		qb.AddLikeCondition("def_name", "%test%", true)
		sql, args := qb.SQL()
		if !strings.Contains(sql, "ILIKE") {
			t.Errorf("expected ILIKE in SQL: %s", sql)
		}
		if len(args) != 1 || args[0] != "%test%" {
			t.Errorf("args = %v, want [%%test%%]", args)
		}
	})

	t.Run("case sensitive postgres", func(t *testing.T) {
		qb := NewQueryBuilder(DialectPostgres, "SELECT * FROM wf WHERE 1=1")
		qb.AddLikeCondition("def_name", "%test%", false)
		sql, args := qb.SQL()
		if strings.Contains(sql, "ILIKE") {
			t.Errorf("expected LIKE (not ILIKE) in SQL: %s", sql)
		}
		if len(args) != 1 || args[0] != "%test%" {
			t.Errorf("args = %v, want [%%test%%]", args)
		}
	})

	t.Run("case insensitive mysql", func(t *testing.T) {
		qb := NewQueryBuilder(DialectMySQL, "SELECT * FROM wf WHERE 1=1")
		qb.AddLikeCondition("def_name", "%test%", true)
		sql, args := qb.SQL()
		if !strings.Contains(sql, "LIKE") {
			t.Errorf("expected LIKE in SQL: %s", sql)
		}
		if len(args) != 1 || args[0] != "%test%" {
			t.Errorf("args = %v, want [%%test%%]", args)
		}
	})

	t.Run("case insensitive mssql", func(t *testing.T) {
		qb := NewQueryBuilder(DialectMSSQL, "SELECT * FROM wf WHERE 1=1")
		qb.AddLikeCondition("def_name", "%test%", true)
		sql, args := qb.SQL()
		if !strings.Contains(sql, "LIKE") {
			t.Errorf("expected LIKE in SQL: %s", sql)
		}
		if len(args) != 1 || args[0] != "%test%" {
			t.Errorf("args = %v, want [%%test%%]", args)
		}
	})
}

func TestQueryBuilder_MultipleConditions(t *testing.T) {
	qb := NewQueryBuilder(DialectPostgres, "SELECT * FROM wf WHERE 1=1")
	qb.AddCondition("status = %s", "running")
	qb.AddCondition("priority > %s", 5)
	sql, args := qb.SQL()

	if !strings.Contains(sql, "status = $1") {
		t.Errorf("missing status condition: %s", sql)
	}
	if !strings.Contains(sql, "priority > $2") {
		t.Errorf("missing priority condition: %s", sql)
	}
	if len(args) != 2 {
		t.Errorf("expected 2 args, got %d: %v", len(args), args)
	}
}

func TestQueryBuilder_AddRawAndAddArgs(t *testing.T) {
	qb := NewQueryBuilder(DialectPostgres, "SELECT * FROM wf WHERE 1=1")
	qb.AddCondition("status = %s", "running")
	qb.AddRaw("ORDER BY created_at DESC")
	qb.AddRaw("LIMIT $2")
	qb.AddArgs(10)
	sql, args := qb.SQL()

	if !strings.Contains(sql, "ORDER BY") {
		t.Errorf("missing ORDER BY: %s", sql)
	}
	if !strings.Contains(sql, "LIMIT $2") {
		t.Errorf("missing LIMIT $2: %s", sql)
	}
	if len(args) != 2 {
		t.Errorf("expected 2 args, got %d: %v", len(args), args)
	}
}

func TestQueryBuilder_NextPos(t *testing.T) {
	qb := NewQueryBuilder(DialectPostgres, "SELECT 1")
	if qb.NextPos() != 1 {
		t.Errorf("initial NextPos = %d, want 1", qb.NextPos())
	}
	qb.AddCondition("x = %s", 42)
	if qb.NextPos() != 2 {
		t.Errorf("NextPos after AddCondition = %d, want 2", qb.NextPos())
	}
}
