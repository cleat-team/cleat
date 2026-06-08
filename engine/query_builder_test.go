package engine

import (
	"strings"
	"testing"
)

// helpers

func mustContain(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("expected %q to contain %q", got, want)
	}
}

func mustNotContain(t *testing.T, got, want string) {
	t.Helper()
	if strings.Contains(got, want) {
		t.Errorf("expected %q to NOT contain %q", got, want)
	}
}

func expectPanic(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("%s: expected panic, but did not panic", name)
		}
	}()
	f()
}

// Dialect methods

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

func TestDialect_placeholder_panics_unknown(t *testing.T) {
	expectPanic(t, "placeholder(unknown)", func() {
		Dialect("oracle").placeholder(1)
	})
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

func TestDialect_nowExpr_panics_unknown(t *testing.T) {
	expectPanic(t, "nowExpr(unknown)", func() {
		Dialect("oracle").nowExpr()
	})
}

func TestDialect_intervalExpr(t *testing.T) {
	tests := []struct {
		d    Dialect
		pos  int
		want string
	}{
		{DialectPostgres, 1, "now() - interval '1 second' * $1"},
		{DialectPostgres, 3, "now() - interval '1 second' * $3"},
		{DialectMySQL, 1, "NOW(6) - INTERVAL ? SECOND"},
		{DialectMySQL, 2, "NOW(6) - INTERVAL ? SECOND"},
		{DialectMSSQL, 1, "DATEADD(SECOND, -@p1, SYSUTCDATETIME())"},
		{DialectMSSQL, 5, "DATEADD(SECOND, -@p5, SYSUTCDATETIME())"},
	}
	for _, tt := range tests {
		got := tt.d.intervalExpr(tt.pos)
		if got != tt.want {
			t.Errorf("%s.intervalExpr(%d) = %q, want %q", tt.d, tt.pos, got, tt.want)
		}
	}
}

func TestDialect_intervalExpr_panics_unknown(t *testing.T) {
	expectPanic(t, "intervalExpr(unknown)", func() {
		Dialect("oracle").intervalExpr(1)
	})
}

func TestDialect_timestampDiffExpr(t *testing.T) {
	tests := []struct {
		d     Dialect
		col   string
		pos   int
		check []string
	}{
		{DialectPostgres, "heartbeat_at", 2, []string{"heartbeat_at < now() - interval '1 second' * $2"}},
		{DialectMySQL, "hb", 1, []string{"hb < NOW(6) - INTERVAL ? SECOND"}},
		{DialectMSSQL, "ts", 3, []string{"ts < DATEADD(SECOND, -@p3, SYSUTCDATETIME())"}},
	}
	for _, tt := range tests {
		got := tt.d.timestampDiffExpr(tt.col, tt.pos)
		for _, c := range tt.check {
			mustContain(t, got, c)
		}
	}
}

func TestDialect_timestampDiffExpr_panics_unknown(t *testing.T) {
	expectPanic(t, "timestampDiffExpr(unknown)", func() {
		Dialect("oracle").timestampDiffExpr("col", 1)
	})
}

func TestDialect_likeExpr(t *testing.T) {
	tests := []struct {
		name            string
		d               Dialect
		col             string
		pos             int
		caseInsensitive bool
		want            string
	}{
		{"postgres case-insensitive", DialectPostgres, "name", 1, true, "name ILIKE $1"},
		{"postgres case-sensitive", DialectPostgres, "name", 2, false, "name LIKE $2"},
		{"mysql case-insensitive", DialectMySQL, "name", 1, true, "name LIKE ?"},
		{"mysql case-sensitive", DialectMySQL, "name", 3, false, "name LIKE ?"},
		{"mssql case-insensitive", DialectMSSQL, "name", 1, true, "name LIKE @p1"},
		{"mssql case-sensitive", DialectMSSQL, "name", 2, false, "name LIKE @p2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.d.likeExpr(tt.col, tt.pos, tt.caseInsensitive)
			if got != tt.want {
				t.Errorf("likeExpr() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDialect_castExpr(t *testing.T) {
	tests := []struct {
		d    Dialect
		col  string
		want string
	}{
		{DialectPostgres, "payload", "payload::text"},
		{DialectMySQL, "payload", "CAST(payload AS CHAR)"},
		{DialectMSSQL, "payload", "CAST(payload AS NVARCHAR(MAX))"},
	}
	for _, tt := range tests {
		got := tt.d.castExpr(tt.col)
		if got != tt.want {
			t.Errorf("%s.castExpr(%q) = %q, want %q", tt.d, tt.col, got, tt.want)
		}
	}
}

func TestDialect_castExpr_panics_unknown(t *testing.T) {
	expectPanic(t, "castExpr(unknown)", func() {
		Dialect("oracle").castExpr("col")
	})
}

func TestDialect_limitOffset(t *testing.T) {
	tests := []struct {
		name       string
		d          Dialect
		limitPos   int
		offsetPos  int
		hasOffset  bool
		want       string
	}{
		{"postgres limit only", DialectPostgres, 1, 0, false, "LIMIT $1"},
		{"postgres limit+offset", DialectPostgres, 2, 3, true, "LIMIT $2 OFFSET $3"},
		{"mysql limit only", DialectMySQL, 1, 0, false, "LIMIT ?"},
		{"mysql limit+offset", DialectMySQL, 4, 5, true, "LIMIT ? OFFSET ?"},
		{"mssql limit only", DialectMSSQL, 1, 0, false, "OFFSET 0 ROWS FETCH NEXT @p1 ROWS ONLY"},
		{"mssql limit+offset", DialectMSSQL, 3, 2, true, "OFFSET @p2 ROWS FETCH NEXT @p3 ROWS ONLY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.d.limitOffset(tt.limitPos, tt.offsetPos, tt.hasOffset)
			if got != tt.want {
				t.Errorf("limitOffset() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDialect_limitOffset_panics_unknown(t *testing.T) {
	expectPanic(t, "limitOffset(unknown)", func() {
		Dialect("oracle").limitOffset(1, 2, false)
	})
}

func TestDialect_batchLimit(t *testing.T) {
	tests := []struct {
		d    Dialect
		pos  int
		want string
	}{
		{DialectPostgres, 1, "LIMIT $1"},
		{DialectPostgres, 5, "LIMIT $5"},
		{DialectMySQL, 1, "LIMIT ?"},
		{DialectMSSQL, 3, "OFFSET 0 ROWS FETCH NEXT @p3 ROWS ONLY"},
	}
	for _, tt := range tests {
		got := tt.d.batchLimit(tt.pos)
		if got != tt.want {
			t.Errorf("%s.batchLimit(%d) = %q, want %q", tt.d, tt.pos, got, tt.want)
		}
	}
}

func TestDialect_batchLimit_panics_unknown(t *testing.T) {
	expectPanic(t, "batchLimit(unknown)", func() {
		Dialect("oracle").batchLimit(1)
	})
}

func TestDialect_workflowInstanceColumns(t *testing.T) {
	pg := DialectPostgres.workflowInstanceColumns()
	mustContain(t, pg, "id, def_name, def_version, status, input, assigned_to")
	mustContain(t, pg, "COALESCE(priority, 0) AS priority")
	mustContain(t, pg, "COALESCE(trace_id, '') AS trace_id")
	mustNotContain(t, pg, "COALESCE(assigned_to, '')")

	mysql := DialectMySQL.workflowInstanceColumns()
	mustContain(t, mysql, "COALESCE(assigned_to, '')")

	mssql := DialectMSSQL.workflowInstanceColumns()
	mustContain(t, mssql, "COALESCE(assigned_to, '')")
}

func TestDialect_workflowInstanceColumns_panics_unknown(t *testing.T) {
	expectPanic(t, "workflowInstanceColumns(unknown)", func() {
		Dialect("oracle").workflowInstanceColumns()
	})
}

func TestDialect_workflowInstanceColumnsExtra(t *testing.T) {
	pg := DialectPostgres.workflowInstanceColumnsExtra()
	mustContain(t, pg, "tenant_id")
	mustContain(t, pg, "COALESCE(priority, 0) AS priority")
	mustNotContain(t, pg, "COALESCE(assigned_to, '')")

	mysql := DialectMySQL.workflowInstanceColumnsExtra()
	mustContain(t, mysql, "tenant_id")
	mustContain(t, mysql, "COALESCE(assigned_to, '')")

	mssql := DialectMSSQL.workflowInstanceColumnsExtra()
	mustContain(t, mssql, "tenant_id")
}

func TestDialect_workflowInstanceColumnsExtra_panics_unknown(t *testing.T) {
	expectPanic(t, "workflowInstanceColumnsExtra(unknown)", func() {
		Dialect("oracle").workflowInstanceColumnsExtra()
	})
}

// QueryBuilder methods

func TestNewQueryBuilder(t *testing.T) {
	qb := NewQueryBuilder(DialectPostgres, "SELECT * FROM t WHERE 1=1")
	if qb == nil {
		t.Fatal("NewQueryBuilder returned nil")
	}
	if qb.d != DialectPostgres {
		t.Errorf("dialect = %v, want postgres", qb.d)
	}
	if qb.nextPos != 1 {
		t.Errorf("nextPos = %d, want 1", qb.nextPos)
	}
	sql, args := qb.SQL()
	if sql != "SELECT * FROM t WHERE 1=1" {
		t.Errorf("SQL() = %q, want base SQL", sql)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want empty", args)
	}
}

func TestQueryBuilder_AddCondition(t *testing.T) {
	qb := NewQueryBuilder(DialectPostgres, "SELECT * FROM t WHERE 1=1")
	qb.AddCondition("col = %s", 42)
	qb.AddCondition("status = %s", "active")

	sql, args := qb.SQL()
	mustContain(t, sql, "AND col = $1")
	mustContain(t, sql, "AND status = $2")
	if len(args) != 2 {
		t.Fatalf("len(args) = %d, want 2", len(args))
	}
	if args[0] != 42 {
		t.Errorf("args[0] = %v, want 42", args[0])
	}
	if args[1] != "active" {
		t.Errorf("args[1] = %v, want active", args[1])
	}
	if qb.NextPos() != 3 {
		t.Errorf("NextPos() = %d, want 3", qb.NextPos())
	}
}

func TestQueryBuilder_AddLikeCondition(t *testing.T) {
	t.Run("postgres case insensitive", func(t *testing.T) {
		qb := NewQueryBuilder(DialectPostgres, "SELECT * FROM t WHERE 1=1")
		qb.AddLikeCondition("name", "%test%", true)
		sql, args := qb.SQL()
		mustContain(t, sql, "name ILIKE $1")
		if len(args) != 1 || args[0] != "%test%" {
			t.Errorf("unexpected args: %v", args)
		}
	})

	t.Run("postgres case sensitive", func(t *testing.T) {
		qb := NewQueryBuilder(DialectPostgres, "SELECT * FROM t WHERE 1=1")
		qb.AddLikeCondition("name", "%test%", false)
		sql, _ := qb.SQL()
		mustContain(t, sql, "name LIKE $1")
	})

	t.Run("mysql case insensitive", func(t *testing.T) {
		qb := NewQueryBuilder(DialectMySQL, "SELECT * FROM t WHERE 1=1")
		qb.AddLikeCondition("name", "pattern", true)
		sql, _ := qb.SQL()
		mustContain(t, sql, "name LIKE ?")
	})

	t.Run("mysql case sensitive", func(t *testing.T) {
		qb := NewQueryBuilder(DialectMySQL, "SELECT * FROM t WHERE 1=1")
		qb.AddLikeCondition("name", "pattern", false)
		sql, _ := qb.SQL()
		mustContain(t, sql, "name LIKE ?")
	})

	t.Run("mssql case insensitive", func(t *testing.T) {
		qb := NewQueryBuilder(DialectMSSQL, "SELECT * FROM t WHERE 1=1")
		qb.AddLikeCondition("name", "pattern", true)
		sql, _ := qb.SQL()
		mustContain(t, sql, "name LIKE @p1")
	})

	t.Run("mssql case sensitive", func(t *testing.T) {
		qb := NewQueryBuilder(DialectMSSQL, "SELECT * FROM t WHERE 1=1")
		qb.AddLikeCondition("name", "pattern", false)
		sql, _ := qb.SQL()
		mustContain(t, sql, "name LIKE @p1")
	})
}

func TestQueryBuilder_AddRaw(t *testing.T) {
	qb := NewQueryBuilder(DialectMySQL, "SELECT * FROM t")
	qb.AddRaw("ORDER BY id")
	sql, args := qb.SQL()
	mustContain(t, sql, "ORDER BY id")
	if len(args) != 0 {
		t.Errorf("args should be empty, got %v", args)
	}
}

func TestQueryBuilder_AddArgs(t *testing.T) {
	qb := NewQueryBuilder(DialectMySQL, "SELECT * FROM t")
	qb.AddRaw("LIMIT ? OFFSET ?")
	qb.AddArgs(10, 20)
	sql, args := qb.SQL()
	mustContain(t, sql, "LIMIT ? OFFSET ?")
	if len(args) != 2 {
		t.Fatalf("len(args) = %d, want 2", len(args))
	}
	if args[0] != 10 || args[1] != 20 {
		t.Errorf("args = %v, want [10, 20]", args)
	}
	if qb.NextPos() != 3 {
		t.Errorf("NextPos() = %d, want 3", qb.NextPos())
	}
}

func TestQueryBuilder_MultiConditionFlow(t *testing.T) {
	qb := NewQueryBuilder(DialectMSSQL, "SELECT * FROM workflows WHERE 1=1")
	qb.AddCondition("status = %s", "running")
	qb.AddLikeCondition("name", "%test%", true)
	qb.AddRaw("ORDER BY created_at DESC")

	sql, args := qb.SQL()
	mustContain(t, sql, "AND status = @p1")
	mustContain(t, sql, "AND name LIKE @p2")
	mustContain(t, sql, "ORDER BY created_at DESC")
	if len(args) != 2 {
		t.Fatalf("len(args) = %d, want 2", len(args))
	}
	if args[0] != "running" {
		t.Errorf("args[0] = %v, want running", args[0])
	}
	if args[1] != "%test%" {
		t.Errorf("args[1] = %v, want %%test%%", args[1])
	}
}
