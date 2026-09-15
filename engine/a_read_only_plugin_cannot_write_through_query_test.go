package engine_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/internal/tenantctx"
	"github.com/cleat-team/cleat/plugin"
)

// TestAReadOnlyPluginCannotWriteThroughQuery is cleat#1621.
//
// ReadOnlyDB.Query and QueryRow ran the statement on the BARE POOL whenever
// beginTenantTx declined to supply a tenant-scoped transaction -- which is any
// non-PostgreSQL dialect, and PostgreSQL with no tenant in context. Nothing on
// the bare pool is read-only, so a mutating statement reached the database.
//
// Exec being refused in Go does not cover this. Exec is the route a caller
// uses deliberately; Query takes any statement string, and
// "INSERT ... RETURNING" is an ordinary thing to send through it.
//
// Measured before the fix, with the row count taken on a separate connection:
//
//	postgres, tenant in ctx : refused (25006)
//	postgres, NO tenant     : wrote
//	mysql                   : wrote
//	mssql                   : wrote
//
// The PostgreSQL-without-a-tenant row is the one that makes this more than a
// portability bug: the dialect every CI job runs, silently unprotected on the
// path plugins actually take.
func TestAReadOnlyPluginCannotWriteThroughQuery(t *testing.T) {
	const tbl = "ro_query_write_probe_1621"

	cases := []struct {
		name      string
		td        testutil.Dialect
		pd        plugin.Dialect
		withTen   bool
		dbRefuses bool
	}{
		{"postgres_no_tenant", testutil.DialectPostgres, plugin.DialectPostgres, false, true},
		{"postgres_with_tenant", testutil.DialectPostgres, plugin.DialectPostgres, true, true},
		{"mysql", testutil.DialectMySQL, plugin.DialectMySQL, false, true},
		// SQL Server has no read-only transaction, so the database enforces
		// nothing here and the guarantee is the connection's privileges. This
		// row is a MEASUREMENT, not an aspiration: if it ever starts refusing,
		// this fails and says to update the type comment with it.
		{"mssql", testutil.DialectMSSQL, plugin.DialectMSSQL, false, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := testutil.TestDB(t, c.td)
			ctx := context.Background()
			if c.withTen {
				ctx = tenantctx.With(ctx, uuid.New())
			}

			db.ExecContext(ctx, "DROP TABLE "+tbl)
			if _, err := db.ExecContext(ctx, "CREATE TABLE "+tbl+" (id int)"); err != nil {
				t.Fatalf("UNMEASURED: could not create the probe table: %v", err)
			}
			defer db.ExecContext(ctx, "DROP TABLE "+tbl)

			// CONTROL: the write must land on a plain connection, or "refused"
			// below would be indistinguishable from a broken fixture.
			if _, err := db.ExecContext(ctx, "INSERT INTO "+tbl+" (id) VALUES (1)"); err != nil {
				t.Fatalf("UNMEASURED: the control write failed: %v", err)
			}
			var seeded int
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tbl).Scan(&seeded); err != nil || seeded != 1 {
				t.Fatalf("UNMEASURED: control seeded %d rows, err=%v", seeded, err)
			}
			if _, err := db.ExecContext(ctx, "DELETE FROM "+tbl); err != nil {
				t.Fatalf("UNMEASURED: could not clear the probe table: %v", err)
			}

			ro := &engine.ReadOnlyDB{Inner: db, Dialect: c.pd}

			stmt := "INSERT INTO " + tbl + " (id) VALUES (2)"
			if c.pd == plugin.DialectPostgres {
				stmt += " RETURNING id"
			}
			rows, qErr := ro.Query(ctx, stmt)
			if rows != nil {
				for rows.Next() {
				}
				rows.Close()
			}

			// Count on a SEPARATE connection, after the read transaction has
			// ended. Counting inside it would measure the rollback rather than
			// the read-only property.
			var n int
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tbl).Scan(&n); err != nil {
				t.Fatalf("UNMEASURED: could not count: %v", err)
			}

			if c.dbRefuses {
				if qErr == nil {
					t.Errorf("%s: Query accepted a write (%d row(s) landed)", c.name, n)
				}
				if n != 0 {
					t.Errorf("%s: a write reached the database through a READ-ONLY db: %d row(s)", c.name, n)
				}
			} else {
				if qErr == nil && n == 0 {
					t.Errorf("%s: the write neither errored nor landed -- the probe measured nothing", c.name)
				}
				if qErr != nil {
					t.Logf("%s now refuses the write (%v). If that is a real change, "+
						"update ReadOnlyDB's type comment, which says the database enforces "+
						"nothing here.", c.name, qErr)
				}
			}
		})
	}
}

// TestAReadOnlyQueryStillReads is the floor assertion: the fix wraps every
// read in a transaction, so an ordinary SELECT must still work -- and must
// still work when there is no tenant in context, which is the path that
// changed.
func TestAReadOnlyQueryStillReads(t *testing.T) {
	const tbl = "ro_query_read_probe_1621"
	for _, c := range []struct {
		name string
		td   testutil.Dialect
		pd   plugin.Dialect
	}{
		{"postgres", testutil.DialectPostgres, plugin.DialectPostgres},
		{"mysql", testutil.DialectMySQL, plugin.DialectMySQL},
		{"mssql", testutil.DialectMSSQL, plugin.DialectMSSQL},
	} {
		t.Run(c.name, func(t *testing.T) {
			db := testutil.TestDB(t, c.td)
			ctx := context.Background()

			db.ExecContext(ctx, "DROP TABLE "+tbl)
			if _, err := db.ExecContext(ctx, "CREATE TABLE "+tbl+" (id int)"); err != nil {
				t.Fatalf("UNMEASURED: %v", err)
			}
			defer db.ExecContext(ctx, "DROP TABLE "+tbl)
			if _, err := db.ExecContext(ctx, "INSERT INTO "+tbl+" (id) VALUES (7)"); err != nil {
				t.Fatalf("UNMEASURED: %v", err)
			}

			ro := &engine.ReadOnlyDB{Inner: db, Dialect: c.pd}

			rows, err := ro.Query(ctx, "SELECT id FROM "+tbl)
			if err != nil {
				t.Fatalf("Query failed on an ordinary SELECT: %v", err)
			}
			got := 0
			for rows.Next() {
				var id int
				if err := rows.Scan(&id); err != nil {
					t.Fatalf("Scan: %v", err)
				}
				got = id
			}
			rows.Close()
			if got != 7 {
				t.Errorf("Query read %d, want 7", got)
			}

			var one int
			if err := ro.QueryRow(ctx, "SELECT id FROM "+tbl).Scan(&one); err != nil {
				t.Fatalf("QueryRow failed: %v", err)
			}
			if one != 7 {
				t.Errorf("QueryRow read %d, want 7", one)
			}
		})
	}
}
