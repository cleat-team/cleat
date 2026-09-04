package engine

// Regression coverage for finding S10: three indexes
// (idx_instances_ready, idx_defs_active, idx_instances_tenant_ready) exist on
// PostgreSQL and MySQL and, before
// migrations/mssql/035_ready_and_active_indexes.sql, did not exist on SQL
// Server at all.
//
// Two of the three were renamed and rewidened by
// migrations/mssql/043_claim_terminating_workflows.sql: the claim now accepts
// 'terminating' as well as 'ready' (IMPROVEMENT-PLAN 3.112), and a FILTERED
// index is only usable when the query's predicate implies its filter -- so a
// filter of `status = 'ready'` would have taken every claim index out of play
// on the hottest query in the system. idx_instances_ready became
// idx_instances_claimable and idx_instances_tenant_ready became
// idx_instances_tenant_claimable, with the filter covering both statuses.
// idx_defs_active is untouched. See that migration's header for how the three dialects'
// index sets were re-derived (grep, not the review's list taken on faith)
// and for the SET SHOWPLAN_ALL ON evidence of which of the three the planner
// actually chooses under the mandatory tenant security policy.
//
// Needs a real SQL Server (CLEAT_TEST_MSSQL).

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"

	_ "github.com/microsoft/go-mssqldb"
)

// mssqlIndexDef describes one index as sys.indexes/sys.index_columns report
// it, for comparing against what 035 is supposed to have created.
type mssqlIndexDef struct {
	table  string
	name   string
	cols   string // comma-separated, key-ordinal order
	filter string // filter_definition, "" if none
}

func readMSSQLIndexDef(t *testing.T, db *sql.DB, table, name string) (mssqlIndexDef, bool) {
	t.Helper()
	var filt sql.NullString
	var exists int
	if err := db.QueryRow(`
		SELECT COUNT(*), MAX(filter_definition) FROM sys.indexes
		WHERE object_id = OBJECT_ID(@p1) AND name = @p2
	`, "dbo."+table, name).Scan(&exists, &filt); err != nil {
		t.Fatalf("query sys.indexes for %s: %v", name, err)
	}
	if exists == 0 {
		return mssqlIndexDef{}, false
	}

	rows, err := db.Query(`
		SELECT c.name
		FROM sys.indexes i
		JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id AND ic.is_included_column = 0
		JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
		WHERE i.object_id = OBJECT_ID(@p1) AND i.name = @p2
		ORDER BY ic.key_ordinal
	`, "dbo."+table, name)
	if err != nil {
		t.Fatalf("query index columns for %s: %v", name, err)
	}
	defer rows.Close()
	var cols string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan index column: %v", err)
		}
		if cols != "" {
			cols += ","
		}
		cols += c
	}
	return mssqlIndexDef{table: table, name: name, cols: cols, filter: filt.String}, true
}

// TestMSSQLIndexes_ReadyAndActiveExist proves the three indexes S10 flagged
// as present on PostgreSQL/MySQL and absent on SQL Server now exist there
// too, with the expected columns and filter predicate -- not merely that
// *some* index by that name exists (a same-named index with the wrong
// columns would still answer this query slowly, so the shape is checked, not
// just the name).
func TestMSSQLIndexes_ReadyAndActiveExist(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, db)

	cases := []struct {
		table, name, wantCols string
		// wantFilterHas is a set of substrings rather than the whole
		// filter_definition. SQL Server rewrites what it stores -- an IN list
		// comes back as an OR chain, with its own bracketing and spacing --
		// so an exact string here would be asserting a normalisation rather
		// than an index. The statuses are what the assertion is about.
		wantFilterHas []string
	}{
		{"workflow_instances", "idx_instances_claimable", "status,next_wake_at",
			[]string{"'ready'", "'terminating'"}},
		{"workflow_defs", "idx_defs_active", "name,version", nil},
		{"workflow_instances", "idx_instances_tenant_claimable", "tenant_id,status,next_wake_at",
			[]string{"'ready'", "'terminating'"}},
	}
	for _, tc := range cases {
		def, ok := readMSSQLIndexDef(t, db, tc.table, tc.name)
		if !ok {
			t.Fatalf("%s: no such index on dbo.%s -- migrations/mssql/035_ready_and_active_indexes.sql "+
				"and 043_claim_terminating_workflows.sql did not create it, or something dropped it",
				tc.name, tc.table)
		}
		if def.cols != tc.wantCols {
			t.Errorf("%s columns = %q, want %q", tc.name, def.cols, tc.wantCols)
		}
		if len(tc.wantFilterHas) == 0 {
			if def.filter != "" {
				t.Errorf("%s filter = %q, want none", tc.name, def.filter)
			}
			continue
		}
		for _, want := range tc.wantFilterHas {
			if !strings.Contains(def.filter, want) {
				t.Errorf("%s filter = %q, want it to mention %s -- a filtered index is only "+
					"usable when the query's predicate implies its filter, and the claim's "+
					"predicate is status IN ('ready', 'terminating')", tc.name, def.filter, want)
			}
		}
	}

	// The old names must be gone, not merely superseded. Two filtered indexes
	// over overlapping row sets is the shape that makes the planner choose a
	// BitmapOr-equivalent and lose the claim's ordering -- see
	// migrations/postgres/040 for why the predicate is widened rather than a
	// second index added.
	for _, name := range []string{"idx_instances_ready", "idx_instances_tenant_ready",
		"idx_instances_tenant_queue_ready", "idx_workflow_instances_ready_claim"} {
		if _, ok := readMSSQLIndexDef(t, db, "workflow_instances", name); ok {
			t.Errorf("%s still exists; 043 was supposed to drop it once its widened "+
				"replacement was in place", name)
		}
	}
}

// TestMSSQLIndexes_TenantReadyUsedByPlanner is the "confirm the plan actually
// uses it" half of S10. idx_instances_ready and idx_defs_active are shown by
// 035's header (measured, not asserted here -- see its comment) to be
// bypassed by the optimizer once SQL Server's mandatory tenant security
// policy is in play, for the two query shapes this schema's own code
// actually runs. idx_instances_tenant_claimable (idx_instances_tenant_ready
// before 043 widened and renamed it) is the one of the three that
// *is* chosen naturally, because it is the only one of the three whose
// leading column is tenant_id -- which both the query's own predicate and
// the RLS residual predicate need. This test is the regression for that: it
// fails if a future index change (a reorder of columns, a dropped filter)
// stops the planner from choosing it for the query the tenant-scoped read
// path actually issues.
func TestMSSQLIndexes_TenantReadyUsedByPlanner(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	if testing.Short() {
		t.Skip("Skipping MSSQL integration test in short mode")
	}
	dsn := os.Getenv("CLEAT_TEST_MSSQL")
	ctx := context.Background()

	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, db)
	testutil.CleanupMSSQLTestData(t, db)
	t.Cleanup(func() { testutil.CleanupMSSQLTestData(t, db) })

	factory := NewMSSQLStoreFactory(dsn)
	defer factory.Close()
	pool, err := factory.getOrCreateTenantPool(ctx, DefaultTenantUUID)
	if err != nil {
		t.Fatalf("getOrCreateTenantPool: %v", err)
	}
	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()

	// workflow_instances.def_name/def_version FK into workflow_defs.
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO dbo.workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
		VALUES ('idxparity-nodef', 1, 0x0061736d, 1, 1, '`+DefaultTenantUUID+`')
	`); err != nil {
		t.Fatalf("seed workflow_defs: %v", err)
	}

	// Enough rows that the optimizer has a real choice -- a handful of rows
	// gets scanned regardless of what indexes exist, and that would make this
	// test pass for the wrong reason (see CLAUDE.md on watching which layer
	// holds a test up).
	const n = 1500
	if _, err := conn.ExecContext(ctx, `
		DECLARE @j INT = 1;
		WHILE @j <= `+fmt.Sprint(n)+`
		BEGIN
			INSERT INTO dbo.workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id)
			VALUES (
				CONCAT('idxparity-', @j), 'idxparity-nodef', 1,
				CASE WHEN @j % 20 = 0 THEN 'ready' ELSE 'running' END,
				DATEADD(SECOND, -@j, SYSUTCDATETIME()),
				'{}', 'default', '`+DefaultTenantUUID+`'
			);
			SET @j = @j + 1;
		END
	`); err != nil {
		t.Fatalf("seed rows: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE STATISTICS dbo.workflow_instances`); err != nil {
		t.Fatalf("update statistics: %v", err)
	}

	if _, err := conn.ExecContext(ctx, "SET SHOWPLAN_ALL ON"); err != nil {
		t.Fatalf("showplan on: %v", err)
	}
	rows, err := conn.QueryContext(ctx, `
		SELECT id FROM dbo.workflow_instances
		WHERE tenant_id = '`+DefaultTenantUUID+`' AND status = 'ready' AND next_wake_at <= SYSUTCDATETIME()
	`)
	if err != nil {
		t.Fatalf("query under showplan: %v", err)
	}
	var sawIndex bool
	var operators []string
	for rows.Next() {
		cols, _ := rows.Columns()
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		colIdx := map[string]int{}
		for i, c := range cols {
			colIdx[c] = i
		}
		get := func(name string) string {
			i, ok := colIdx[name]
			if !ok {
				return ""
			}
			switch v := vals[i].(type) {
			case []byte:
				return string(v)
			case string:
				return v
			default:
				return ""
			}
		}
		op, arg := get("PhysicalOp"), get("Argument")
		if op != "" {
			operators = append(operators, op)
		}
		if arg != "" && strings.Contains(arg, "idx_instances_tenant_claimable") {
			sawIndex = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	rows.Close()
	if _, err := conn.ExecContext(ctx, "SET SHOWPLAN_ALL OFF"); err != nil {
		t.Fatalf("showplan off: %v", err)
	}

	if !sawIndex {
		t.Fatalf("plan for the tenant-scoped ready-instance query does not reference "+
			"idx_instances_tenant_claimable (physical operators seen: %v) -- the index "+
			"exists but the optimizer is not using it for the query this schema's own "+
			"tenant-scoped reads issue", operators)
	}
}
