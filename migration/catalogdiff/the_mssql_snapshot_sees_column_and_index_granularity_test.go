package catalogdiff

// cleat#2882, the remaining third of this issue (MySQL's trigger gap and
// roles are elsewhere). mssql.go compared a column's type NAME but not its
// width, precision or scale, and an index's key columns but not its filter,
// INCLUDE list or sort order -- scripts/gen-mssql-baseline/supplementary.go's
// "column shape" and "index attributes" checks exist because of exactly
// this, and are the proven queries this change ports rather than re-derives.
//
// Same method as the_mssql_snapshot_sees_the_attributes_it_selects_test.go
// (cleat#2447) and the_mysql_snapshot_sees_a_trigger_test.go (cleat#2882's
// first third): establish the difference independently of Snapshot, THEN
// require Diff to report it. Uses the mssqlDiffCase/mssqlScratchDB helpers
// already defined in that sibling file rather than duplicating them.

import (
	"database/sql"
	"testing"
)

func TestTheMSSQLSnapshotSeesColumnAndIndexGranularity(t *testing.T) {
	// 1. Column width. Live on this dialect: a migration narrowing a
	// varchar column is exactly the compaction-silently-narrows-a-column
	// scenario cleat#2882's own issue text names.
	mssqlDiffCase(t, "column-width",
		`CREATE TABLE t (v NVARCHAR(100) NULL);`,
		`CREATE TABLE t (v NVARCHAR(50) NULL);`,
		"len=100",
		func(t *testing.T, a, b *sql.DB) string {
			var la, lb int
			q := `SELECT max_length FROM sys.columns WHERE object_id = OBJECT_ID('t') AND name = 'v'`
			if err := a.QueryRow(q).Scan(&la); err != nil {
				t.Fatalf("read A's max_length: %v", err)
			}
			if err := b.QueryRow(q).Scan(&lb); err != nil {
				t.Fatalf("read B's max_length: %v", err)
			}
			if la == lb {
				return ""
			}
			return "A max_length=200 (NVARCHAR(100), UTF-16), B max_length=100"
		})

	// 2. Column precision/scale.
	mssqlDiffCase(t, "column-precision-scale",
		`CREATE TABLE t (v DECIMAL(10,2) NULL);`,
		`CREATE TABLE t (v DECIMAL(12,4) NULL);`,
		"precision=10,scale=2",
		func(t *testing.T, a, b *sql.DB) string {
			var pa, sa, pb, sb int
			q := `SELECT precision, scale FROM sys.columns WHERE object_id = OBJECT_ID('t') AND name = 'v'`
			if err := a.QueryRow(q).Scan(&pa, &sa); err != nil {
				t.Fatalf("read A's precision/scale: %v", err)
			}
			if err := b.QueryRow(q).Scan(&pb, &sb); err != nil {
				t.Fatalf("read B's precision/scale: %v", err)
			}
			if pa == pb && sa == sb {
				return ""
			}
			return "A is DECIMAL(10,2), B is DECIMAL(12,4)"
		})

	// 3. IDENTITY. Not merely a width-shaped attribute: an identity column
	// generates its own values, so losing the flag changes INSERT behaviour
	// that no column-type comparison would otherwise catch.
	mssqlDiffCase(t, "column-identity",
		`CREATE TABLE t (id INT IDENTITY(1,1) PRIMARY KEY, v INT);`,
		`CREATE TABLE t (id INT PRIMARY KEY, v INT);`,
		"IDENTITY",
		func(t *testing.T, a, b *sql.DB) string {
			var ia, ib bool
			q := `SELECT is_identity FROM sys.columns WHERE object_id = OBJECT_ID('t') AND name = 'id'`
			if err := a.QueryRow(q).Scan(&ia); err != nil {
				t.Fatalf("read A's is_identity: %v", err)
			}
			if err := b.QueryRow(q).Scan(&ib); err != nil {
				t.Fatalf("read B's is_identity: %v", err)
			}
			if ia == ib {
				return ""
			}
			return "A is_identity=1, B is_identity=0"
		})

	// 4. Index filter. mssql-baseline-known-positive.sh's own "change a
	// filtered index's filter" case moves from "supplementary catches it"
	// to "catalogdiff catches it" with this fix (updated alongside).
	mssqlDiffCase(t, "index-filter",
		`CREATE TABLE t (id INT PRIMARY KEY, v INT NULL);
		 CREATE NONCLUSTERED INDEX ix_t_v ON t(v) WHERE v IS NOT NULL;`,
		`CREATE TABLE t (id INT PRIMARY KEY, v INT NULL);
		 CREATE NONCLUSTERED INDEX ix_t_v ON t(v);`,
		"filter(true)=([v] IS NOT NULL)",
		func(t *testing.T, a, b *sql.DB) string {
			var fa, fb bool
			q := `SELECT has_filter FROM sys.indexes WHERE object_id = OBJECT_ID('t') AND name = 'ix_t_v'`
			if err := a.QueryRow(q).Scan(&fa); err != nil {
				t.Fatalf("read A's has_filter: %v", err)
			}
			if err := b.QueryRow(q).Scan(&fb); err != nil {
				t.Fatalf("read B's has_filter: %v", err)
			}
			if fa == fb {
				return ""
			}
			return "A has_filter=1, B has_filter=0"
		})

	// 5. INCLUDE column. The PRIOR query mixed key and included columns
	// into one STRING_AGG ordered by key_ordinal (which is 0 for every
	// included column) -- this proves the fix separates them, not merely
	// that an included column's existence reaches the line.
	mssqlDiffCase(t, "index-include-column",
		`CREATE TABLE t (id INT PRIMARY KEY, k INT NOT NULL, inc INT NULL);
		 CREATE NONCLUSTERED INDEX ix_t_k ON t(k) INCLUDE (inc);`,
		`CREATE TABLE t (id INT PRIMARY KEY, k INT NOT NULL, inc INT NULL);
		 CREATE NONCLUSTERED INDEX ix_t_k ON t(k);`,
		"include=inc",
		func(t *testing.T, a, b *sql.DB) string {
			var na, nb int
			q := `SELECT COUNT(*) FROM sys.index_columns WHERE object_id = OBJECT_ID('t')
			        AND index_id = (SELECT index_id FROM sys.indexes WHERE object_id = OBJECT_ID('t') AND name = 'ix_t_k')
			        AND is_included_column = 1`
			if err := a.QueryRow(q).Scan(&na); err != nil {
				t.Fatalf("read A's included-column count: %v", err)
			}
			if err := b.QueryRow(q).Scan(&nb); err != nil {
				t.Fatalf("read B's included-column count: %v", err)
			}
			if na == nb {
				return ""
			}
			return "A has 1 INCLUDE column, B has 0"
		})

	// 6. Sort order (DESC). MySQL's sibling control
	// (the_mysql_snapshot_sees_the_attributes_it_selects_test.go's
	// "descending-index" case) already covers this class on that dialect;
	// this is the MSSQL analogue cleat#2882 names explicitly.
	mssqlDiffCase(t, "index-sort-order",
		`CREATE TABLE t (id INT PRIMARY KEY, k INT NOT NULL);
		 CREATE NONCLUSTERED INDEX ix_t_k ON t(k DESC);`,
		`CREATE TABLE t (id INT PRIMARY KEY, k INT NOT NULL);
		 CREATE NONCLUSTERED INDEX ix_t_k ON t(k ASC);`,
		"columns=k DESC",
		func(t *testing.T, a, b *sql.DB) string {
			var da, db_ bool
			q := `SELECT is_descending_key FROM sys.index_columns WHERE object_id = OBJECT_ID('t')
			        AND index_id = (SELECT index_id FROM sys.indexes WHERE object_id = OBJECT_ID('t') AND name = 'ix_t_k')`
			if err := a.QueryRow(q).Scan(&da); err != nil {
				t.Fatalf("read A's is_descending_key: %v", err)
			}
			if err := b.QueryRow(q).Scan(&db_); err != nil {
				t.Fatalf("read B's is_descending_key: %v", err)
			}
			if da == db_ {
				return ""
			}
			return "A is DESC, B is ASC"
		})
}
