package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// dialectsUnderTest pairs the testutil dialect with the plugin one, so a new
// dialect has to be added here rather than being silently uncovered.
var readOnlyDialects = []struct {
	name string
	td   testutil.Dialect
	pd   plugin.Dialect
}{
	{"postgres", testutil.DialectPostgres, plugin.DialectPostgres},
	{"mysql", testutil.DialectMySQL, plugin.DialectMySQL},
	{"mssql", testutil.DialectMSSQL, plugin.DialectMSSQL},
}

// TestReadOnlyDBBeginsOnEveryDialect is cleat#1615.
//
// Begin issued "SET TRANSACTION READ ONLY" after opening the transaction.
// That statement is redundant on PostgreSQL, a syntax error on SQL Server,
// and refused by MySQL, which will not change transaction characteristics
// once a transaction is open. So Begin failed on TWO of the three dialects:
//
//	mssql: readOnlyDB begin tx: read-only transactions are not supported
//	mysql: readOnlyDB set transaction read only: Error 1568 (25001):
//	       Transaction characteristics can't be changed while a transaction
//	       is in progress
//
// The MySQL half was not in the original report -- it was filed as a SQL
// Server defect -- and would have survived a fix aimed only at SQL Server.
func TestReadOnlyDBBeginsOnEveryDialect(t *testing.T) {
	for _, d := range readOnlyDialects {
		t.Run(d.name, func(t *testing.T) {
			db := testutil.TestDB(t, d.td)
			ro := &engine.ReadOnlyDB{Inner: db, Dialect: d.pd}

			tx, err := ro.Begin(context.Background())
			if err != nil {
				t.Fatalf("ReadOnlyDB.Begin failed on %s: %v", d.name, err)
			}
			defer tx.Rollback()

			// Whatever the database does or does not enforce, the Go refusal
			// is unconditional and is the same on every dialect.
			if _, err := tx.Exec(context.Background(), "INSERT INTO probe_should_not_exist (id) VALUES (1)"); err == nil {
				t.Fatal("readOnlyTx.Exec allowed a write")
			} else if !strings.Contains(err.Error(), "Exec denied") {
				t.Errorf("expected the Go-level refusal, got: %v", err)
			}
		})
	}
}

// TestReadOnlyDBEnforcementIsNotUniform pins what the DATABASE enforces, which
// is not the same on all three, so that the asymmetry is a checked property
// rather than a thing someone discovers.
//
// PostgreSQL and MySQL refuse a write inside the transaction. SQL Server has
// no read-only transaction, so a mutating statement sent through Query --
// which has no refusal of its own -- reaches the database.
//
// If SQL Server ever grows one, this test fails and says so, which is the
// point: the row below is a measurement, not an aspiration.
func TestReadOnlyDBEnforcementIsNotUniform(t *testing.T) {
	const tbl = "ro_enforcement_probe_1615"
	for _, d := range readOnlyDialects {
		t.Run(d.name, func(t *testing.T) {
			db := testutil.TestDB(t, d.td)
			ctx := context.Background()

			db.ExecContext(ctx, "DROP TABLE "+tbl)
			if _, err := db.ExecContext(ctx, "CREATE TABLE "+tbl+" (id int)"); err != nil {
				t.Fatalf("UNMEASURED: could not create the probe table: %v", err)
			}
			defer db.ExecContext(ctx, "DROP TABLE "+tbl)

			// CONTROL: the write must succeed on a plain connection, or a
			// "refused" below would prove nothing about read-only.
			if _, err := db.ExecContext(ctx, "INSERT INTO "+tbl+" (id) VALUES (1)"); err != nil {
				t.Fatalf("UNMEASURED: the control write failed: %v", err)
			}
			if _, err := db.ExecContext(ctx, "DELETE FROM "+tbl); err != nil {
				t.Fatalf("UNMEASURED: could not clear the probe table: %v", err)
			}

			ro := &engine.ReadOnlyDB{Inner: db, Dialect: d.pd}
			tx, err := ro.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin failed on %s: %v", d.name, err)
			}
			// Query, not Exec: Exec is refused in Go on every dialect, so it
			// cannot tell us what the database would have done.
			rows, qErr := tx.Query(ctx, "INSERT INTO "+tbl+" (id) VALUES (2)")
			if rows != nil {
				rows.Close()
			}

			// Only ask whether a row landed when the statement was NOT
			// refused. Two reasons, both found by measuring:
			//
			//  - counting AFTER the rollback measures the rollback. SQL Server
			//    accepted the write and then lost it, which reads exactly like
			//    PostgreSQL refusing it.
			//  - counting after a REFUSED write is impossible on PostgreSQL:
			//    the transaction is aborted (25P02) and every later statement
			//    in it fails, so the count reports the abort, not the table.
			//
			// A refusal is the measurement. The count only settles the other
			// branch: did an accepted statement actually write?
			n := -1
			if qErr == nil {
				if err := tx.QueryRow(ctx, "SELECT COUNT(*) FROM "+tbl).Scan(&n); err != nil {
					t.Fatalf("UNMEASURED: the write was accepted but could not be counted: %v", err)
				}
			}
			tx.Rollback()

			dbEnforces := d.pd != plugin.DialectMSSQL
			if dbEnforces {
				if qErr == nil {
					t.Errorf("%s must refuse the write in the DATABASE, and did not (rows=%d)", d.name, n)
				}
			} else {
				if qErr != nil || n <= 0 {
					t.Errorf("SQL Server is documented as NOT enforcing read-only in the database "+
						"(readOnlyTxOptions returns nil for it, and readOnlyDB's doc comment says so). "+
						"This now behaves differently -- err=%v rows=%d -- so update both.", qErr, n)
				}
			}
		})
	}
}
