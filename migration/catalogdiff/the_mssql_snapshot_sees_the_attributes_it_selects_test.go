package catalogdiff

// cleat#2447, the MSSQL foreign-key sibling of
// the_mysql_snapshot_sees_the_attributes_it_selects_test.go. Same method,
// same reason it is written this way: an attribute the comparator does not
// SELECT is an attribute it cannot disagree about, and a control without a
// live-difference precondition is indistinguishable from one that never ran.
//
// mssql.go's FOREIGN KEY rendering carried no delete/update action and no
// trust bit -- the generalised form of cleat#2438's CHECK-constraint finding,
// one branch over in the same UNION ALL. Measured on the shipped
// migrations/mssql/001_schema.sql: 19 FOREIGN KEY occurrences, 13 ON DELETE
// CASCADE, 6 with no action clause -- both forms deliberate, and rendered
// identically before this fix.

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/migration"
	_ "github.com/microsoft/go-mssqldb"
)

// mssqlScratchDB opens a freshly-created EMPTY database for the named schema
// -- not the full migration chain scratchMSSQLDB (other_dialects_test.go)
// applies, which is unnecessary weight for a two-table control and would tie
// every case here to migrations/mssql/'s current contents. Skips when
// unconfigured, the same precondition convention as mysqlScratchDB above.
func mssqlScratchDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	dsn := mssqlAdminDSN()
	if dsn == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server catalogdiff test")
	}
	admin, err := sql.Open("sqlserver", dsn)
	if err != nil {
		t.Fatalf("open SQL Server admin connection: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Fatalf("configured SQL Server is unreachable: %v", err)
	}
	drop := fmt.Sprintf("IF DB_ID('%[1]s') IS NOT NULL BEGIN ALTER DATABASE [%[1]s] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE [%[1]s] END", name)
	if _, err := admin.Exec(drop); err != nil {
		t.Fatalf("drop %s: %v", name, err)
	}
	if _, err := admin.Exec("CREATE DATABASE [" + name + "]"); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		a, err := sql.Open("sqlserver", dsn)
		if err != nil {
			return
		}
		defer a.Close()
		_, _ = a.Exec(drop)
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parsing admin DSN: %v", err)
	}
	q := u.Query()
	q.Set("database", name)
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlserver", u.String())
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping %s: %v", name, err)
	}
	return db
}

// mssqlDiffCase mirrors mysqlDiffCase exactly: build A and B from the two DDL
// statements given, assert the catalogue difference INDEPENDENTLY of the
// snapshot, then require Diff to report it and to name the wanted substring.
func mssqlDiffCase(t *testing.T, name, ddlA, ddlB, want string,
	establish func(t *testing.T, a, b *sql.DB) string) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		scratchName := "cleat_2447_" + strings.ReplaceAll(name, "-", "_") + "_" +
			fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)
		a := mssqlScratchDB(t, scratchName+"_a")
		b := mssqlScratchDB(t, scratchName+"_b")
		ctx := context.Background()
		if _, err := a.ExecContext(ctx, ddlA); err != nil {
			t.Fatalf("build A: %v", err)
		}
		if _, err := b.ExecContext(ctx, ddlB); err != nil {
			t.Fatalf("build B: %v", err)
		}

		if got := establish(t, a, b); got == "" {
			t.Fatalf("the two databases do not actually differ in the attribute " +
				"this case is about, so a zero diff would prove nothing")
		} else {
			t.Logf("established independently: %s", got)
		}

		ca, err := Snapshot(ctx, a, migration.DialectMSSQL)
		if err != nil {
			t.Fatalf("snapshot A: %v", err)
		}
		cb, err := Snapshot(ctx, b, migration.DialectMSSQL)
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

func TestTheMSSQLSnapshotSeesTheAttributesItSelects(t *testing.T) {
	const parent = `CREATE TABLE parent (id INT PRIMARY KEY);`

	// 1. ON DELETE CASCADE vs no action -- the live gap: 13 of 19 shipped
	// foreign keys carry this today, and it rendered identically to the 6
	// that carry no action at all.
	mssqlDiffCase(t, "fk-on-delete-cascade",
		parent+`CREATE TABLE child (id INT PRIMARY KEY, parent_id INT,
			CONSTRAINT fk_child_parent FOREIGN KEY (parent_id) REFERENCES parent(id) ON DELETE CASCADE);`,
		parent+`CREATE TABLE child (id INT PRIMARY KEY, parent_id INT,
			CONSTRAINT fk_child_parent FOREIGN KEY (parent_id) REFERENCES parent(id));`,
		"ON DELETE CASCADE",
		func(t *testing.T, a, b *sql.DB) string {
			var da, db_ string
			q := `SELECT delete_referential_action_desc FROM sys.foreign_keys WHERE name = 'fk_child_parent'`
			if err := a.QueryRow(q).Scan(&da); err != nil {
				t.Fatalf("read A's delete action: %v", err)
			}
			if err := b.QueryRow(q).Scan(&db_); err != nil {
				t.Fatalf("read B's delete action: %v", err)
			}
			if da != "CASCADE" || db_ != "NO_ACTION" {
				return ""
			}
			return "A is CASCADE, B is NO_ACTION"
		})

	// 2. ON UPDATE SET NULL vs no action -- the update-side sibling. refAction
	// takes the verb as a parameter specifically so DELETE and UPDATE are not
	// confused (scripts/gen-mssql-baseline/emit.go's own doc comment), so this
	// checks the comparator keeps them apart too rather than only proving one
	// of the two columns reaches the rendered line.
	mssqlDiffCase(t, "fk-on-update-set-null",
		parent+`CREATE TABLE child (id INT PRIMARY KEY, parent_id INT,
			CONSTRAINT fk_child_parent FOREIGN KEY (parent_id) REFERENCES parent(id) ON UPDATE SET NULL);`,
		parent+`CREATE TABLE child (id INT PRIMARY KEY, parent_id INT,
			CONSTRAINT fk_child_parent FOREIGN KEY (parent_id) REFERENCES parent(id));`,
		"ON UPDATE SET NULL",
		func(t *testing.T, a, b *sql.DB) string {
			var ua, ub string
			q := `SELECT update_referential_action_desc FROM sys.foreign_keys WHERE name = 'fk_child_parent'`
			if err := a.QueryRow(q).Scan(&ua); err != nil {
				t.Fatalf("read A's update action: %v", err)
			}
			if err := b.QueryRow(q).Scan(&ub); err != nil {
				t.Fatalf("read B's update action: %v", err)
			}
			if ua != "SET_NULL" || ub != "NO_ACTION" {
				return ""
			}
			return "A is SET_NULL, B is NO_ACTION"
		})

	// 3. is_not_trusted -- not live (no shipped FK is WITH NOCHECK today), the
	// same "worth taking now" reasoning as the CHECK constraint's own trust
	// bit: a WITH NOCHECK FK still enforces new writes and only skips
	// validating existing rows, so a diff that could not see it would let a
	// migration silently swap a validated FK for an unvalidated one.
	mssqlDiffCase(t, "fk-not-trusted",
		parent+`CREATE TABLE child (id INT PRIMARY KEY, parent_id INT NOT NULL);
			ALTER TABLE child WITH NOCHECK ADD CONSTRAINT fk_child_parent FOREIGN KEY (parent_id) REFERENCES parent(id);`,
		parent+`CREATE TABLE child (id INT PRIMARY KEY, parent_id INT NOT NULL);
			ALTER TABLE child WITH CHECK ADD CONSTRAINT fk_child_parent FOREIGN KEY (parent_id) REFERENCES parent(id);`,
		"WITH NOCHECK",
		func(t *testing.T, a, b *sql.DB) string {
			var ta, tb bool
			q := `SELECT is_not_trusted FROM sys.foreign_keys WHERE name = 'fk_child_parent'`
			if err := a.QueryRow(q).Scan(&ta); err != nil {
				t.Fatalf("read A's is_not_trusted: %v", err)
			}
			if err := b.QueryRow(q).Scan(&tb); err != nil {
				t.Fatalf("read B's is_not_trusted: %v", err)
			}
			if !ta || tb {
				return ""
			}
			return "A is_not_trusted=1, B is_not_trusted=0"
		})
}
