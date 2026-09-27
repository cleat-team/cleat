package catalogdiff

// Controls for the three MySQL snapshot columns added in cleat#2446/#2445.
//
// WHY EACH CASE EXISTS, AND WHY IT IS WRITTEN THIS WAY. cleat#2445 found that
// an attribute the comparator does not SELECT is an attribute it cannot
// disagree about -- seven MSSQL constraints differed under a reported zero
// because the trust bit was not in the tuple. The same audit on MySQL found
// three, and this file is the control: **a comparator change without one is
// indistinguishable from a comparator that was already right.**
//
// So each case builds a pair of databases differing in EXACTLY ONE attribute,
// establishes the difference from the catalogue independently of the snapshot
// (a raw query in the test), and then requires Diff to report non-zero and to
// name the attribute. A case that returned zero would mean the column was
// added but never reaches the diff -- which is the failure mode this whole
// change is about, one level up: `canonicalize` renders only
// type/nullable/default for a column, so a new struct field would have been
// invisible. The assertion is on the RENDERED line, not on the struct.

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/migration"
	_ "github.com/go-sql-driver/mysql"
)

// mysqlScratchDB opens a freshly-created empty database for the named schema,
// skipping when the dialect is not configured -- the same precondition
// convention the migration package uses: unconfigured SKIPS, configured but
// unreachable FAILS.
func mysqlScratchDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	dsn := os.Getenv("CLEAT_TEST_MYSQL")
	if dsn == "" {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL catalogdiff test")
	}
	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open MySQL admin connection: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Fatalf("configured MySQL is unreachable: %v", err)
	}
	if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + name); err != nil {
		t.Fatalf("drop %s: %v", name, err)
	}
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		c, err := sql.Open("mysql", dsn)
		if err != nil {
			return
		}
		defer c.Close()
		if _, err := c.Exec(`DROP DATABASE IF EXISTS ` + name); err != nil {
			t.Logf("drop %s: %v", name, err)
		}
	})

	// Swap the database in the DSN the same way the migration package's helper
	// does, rather than string-splicing.
	i := strings.LastIndex(dsn, "/")
	j := strings.Index(dsn[i:], "?")
	if j < 0 {
		j = len(dsn) - i
	}
	scratch := dsn[:i+1] + name + dsn[i+j:]
	db, err := sql.Open("mysql", scratch)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping %s: %v", name, err)
	}
	return db
}

// mysqlDiffCase runs one control: build A and B from the two DDL statements
// given, assert the catalogue difference INDEPENDENTLY, then require Diff to
// report it and to name the wanted substring.
func mysqlDiffCase(t *testing.T, name, ddlA, ddlB, want string,
	establish func(t *testing.T, a, b *sql.DB) string) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		a := mysqlScratchDB(t, "cleat_cd_a_"+strings.ReplaceAll(name, "-", "_"))
		b := mysqlScratchDB(t, "cleat_cd_b_"+strings.ReplaceAll(name, "-", "_"))
		ctx := context.Background()
		if _, err := a.ExecContext(ctx, ddlA); err != nil {
			t.Fatalf("build A: %v", err)
		}
		if _, err := b.ExecContext(ctx, ddlB); err != nil {
			t.Fatalf("build B: %v", err)
		}

		// The difference is established from the catalogue directly, so the
		// snapshots below are not both the evidence and the subject.
		if got := establish(t, a, b); got == "" {
			t.Fatalf("the two databases do not actually differ in the attribute " +
				"this case is about, so a zero diff would prove nothing")
		} else {
			t.Logf("established independently: %s", got)
		}

		ca, err := Snapshot(ctx, a, migration.DialectMySQL)
		if err != nil {
			t.Fatalf("snapshot A: %v", err)
		}
		cb, err := Snapshot(ctx, b, migration.DialectMySQL)
		if err != nil {
			t.Fatalf("snapshot B: %v", err)
		}
		d := Diff(ca, cb)
		if len(d) == 0 {
			t.Fatalf("Diff reported ZERO differences over a pair that demonstrably differs "+
				"in %s.\n\nThe column is selected but does not reach the rendered line, so the "+
				"comparator is still blind -- which is the defect this control exists to catch, "+
				"one level up from the one it fixes.", name)
		}
		joined := strings.Join(d, "\n")
		if !strings.Contains(joined, want) {
			t.Fatalf("Diff reported differences but none names %q:\n%s", want, joined)
		}
		t.Logf("Diff reported %d line(s), including %q", len(d), want)
	})
}

func TestTheMySQLSnapshotSeesTheAttributesItSelects(t *testing.T) {
	// 1. STATISTICS.COLLATION -- a DESC index silently becoming ASC. Live on
	//    this dialect: workflow_memory_samples.idx_memory_samples_tenant_def
	//    carries recorded_at DESC in both the chain and the baseline.
	mysqlDiffCase(t, "descending-index",
		`CREATE TABLE t (a INT NOT NULL, b DATETIME NOT NULL, KEY ix (a, b DESC))`,
		`CREATE TABLE t (a INT NOT NULL, b DATETIME NOT NULL, KEY ix (a, b))`,
		// The whole token, not just "DESC". A bare "DESC" is satisfied by any
		// rendering that mentions the direction, including one that cannot be
		// read back unambiguously -- which was the state before the separator
		// changed. Pinning the exact form is what makes the join a decision
		// rather than a detail nothing observes.
		"columns=[a,b DESC]",
		func(t *testing.T, a, b *sql.DB) string {
			var n int
			if err := a.QueryRow(`SELECT COUNT(*) FROM information_schema.statistics
				WHERE table_schema = DATABASE() AND collation = 'D'`).Scan(&n); err != nil {
				t.Fatalf("read A's index direction: %v", err)
			}
			var m int
			if err := b.QueryRow(`SELECT COUNT(*) FROM information_schema.statistics
				WHERE table_schema = DATABASE() AND collation = 'D'`).Scan(&m); err != nil {
				t.Fatalf("read B's index direction: %v", err)
			}
			if n != 1 || m != 0 {
				return ""
			}
			return "A has 1 descending key, B has 0"
		})

	// 2. COLUMNS.EXTRA = auto_increment. Two columns carry it today, and
	//    COLUMN_TYPE reports `int` and COLUMN_DEFAULT NULL either way.
	mysqlDiffCase(t, "auto-increment",
		`CREATE TABLE t (id INT NOT NULL AUTO_INCREMENT PRIMARY KEY, v INT)`,
		`CREATE TABLE t (id INT NOT NULL PRIMARY KEY, v INT)`,
		"auto_increment",
		func(t *testing.T, a, b *sql.DB) string {
			var n, m int
			q := `SELECT COUNT(*) FROM information_schema.columns
				WHERE table_schema = DATABASE() AND extra LIKE '%auto_increment%'`
			if err := a.QueryRow(q).Scan(&n); err != nil {
				t.Fatalf("read A's extra: %v", err)
			}
			if err := b.QueryRow(q).Scan(&m); err != nil {
				t.Fatalf("read B's extra: %v", err)
			}
			if n != 1 || m != 0 {
				return ""
			}
			return "A has 1 auto_increment column, B has 0"
		})

	// 3. TABLE_CONSTRAINTS.ENFORCED -- the MSSQL is_not_trusted class on this
	//    dialect. Zero today, added so the instrument can see the state before
	//    someone writes it.
	mysqlDiffCase(t, "not-enforced-check",
		`CREATE TABLE t (v LONGTEXT NULL,
			CONSTRAINT ck CHECK (v IS NULL OR JSON_VALID(v)))`,
		`CREATE TABLE t (v LONGTEXT NULL,
			CONSTRAINT ck CHECK (v IS NULL OR JSON_VALID(v)) NOT ENFORCED)`,
		"NOT ENFORCED",
		func(t *testing.T, a, b *sql.DB) string {
			var ea, eb string
			q := `SELECT enforced FROM information_schema.table_constraints
				WHERE table_schema = DATABASE() AND constraint_name = 'ck'`
			if err := a.QueryRow(q).Scan(&ea); err != nil {
				t.Fatalf("read A's enforced: %v", err)
			}
			if err := b.QueryRow(q).Scan(&eb); err != nil {
				t.Fatalf("read B's enforced: %v", err)
			}
			if ea != "YES" || eb != "NO" {
				return ""
			}
			return "A is ENFORCED=YES, B is ENFORCED=NO"
		})
}
