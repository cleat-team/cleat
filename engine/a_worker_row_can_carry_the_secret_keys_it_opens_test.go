package engine

// The schema half of cleat#1991: admin.workers.secret_key_versions, added by
// migrations 102 (PostgreSQL) / 101 (MySQL) / 101 (SQL Server) ahead of the Go
// that writes it.
//
// WHAT THIS PINS, and why it is more than "the column exists":
//
//   - it is NULLABLE, because a worker from before the column reads as "unknown"
//     and a gate must never read unknown as "can open anything";
//   - a registration made by the CURRENT Register statement, which does not know
//     the column, still works and reads back NULL -- an upgrade must not break
//     the workers that are already running;
//   - NULL, "" and "1,2" are three DIFFERENT values on every dialect. "" means a
//     worker with no key, which blocks every write; NULL means unknown. A dialect
//     that folded the empty string into NULL (the way some databases do) would
//     make a keyless worker indistinguishable from an old one, and the gate would
//     have to guess.

import (
	"context"
	"database/sql"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/google/uuid"
)

func TestAWorkerRowCanCarryTheSecretKeysItOpens(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMySQL, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			db := testutil.TestDB(t, dialect)
			t.Cleanup(func() { db.Close() })
			testutil.SetupFullSchema(t, db, dialect)
			ctx := context.Background()
			reg := &WorkerRegistry{DB: db, Dialect: Dialect(string(dialect))}

			// The column exists and is nullable.
			var nullable string
			var q string
			switch dialect {
			case testutil.DialectMySQL:
				q = `SELECT is_nullable FROM information_schema.columns
				      WHERE table_schema = DATABASE() AND table_name = 'workers' AND column_name = 'secret_key_versions'`
			default:
				q = `SELECT is_nullable FROM information_schema.columns
				      WHERE table_schema = 'admin' AND table_name = 'workers' AND column_name = 'secret_key_versions'`
			}
			if err := db.QueryRowContext(ctx, q).Scan(&nullable); err != nil {
				t.Fatalf("the column is not there: %v", err)
			}
			if nullable != "YES" {
				t.Fatalf("secret_key_versions is_nullable = %q, want YES: a NOT NULL column would make every "+
					"existing worker's registration fail on upgrade", nullable)
			}

			// A registration made without the column still works, and reads NULL.
			id := "cleat-1991-" + uuid.New().String()
			if err := reg.Register(ctx, WorkerRegistration{WorkerID: id, Hostname: "h", PID: 1, Concurrency: 1}); err != nil {
				t.Fatalf("Register (the statement that predates the column): %v", err)
			}
			t.Cleanup(func() { _ = reg.Deregister(context.Background(), id) })

			read := func() sql.NullString {
				var v sql.NullString
				sel := map[testutil.Dialect]string{
					testutil.DialectPostgres: `SELECT secret_key_versions FROM admin.workers WHERE worker_id = $1`,
					testutil.DialectMySQL:    `SELECT secret_key_versions FROM workers WHERE worker_id = ?`,
					testutil.DialectMSSQL:    `SELECT secret_key_versions FROM admin.workers WHERE worker_id = @p1`,
				}[dialect]
				if err := db.QueryRowContext(ctx, sel, id).Scan(&v); err != nil {
					t.Fatalf("read secret_key_versions: %v", err)
				}
				return v
			}
			write := func(v any) {
				upd := map[testutil.Dialect]string{
					testutil.DialectPostgres: `UPDATE admin.workers SET secret_key_versions = $1 WHERE worker_id = $2`,
					testutil.DialectMySQL:    `UPDATE workers SET secret_key_versions = ? WHERE worker_id = ?`,
					testutil.DialectMSSQL:    `UPDATE admin.workers SET secret_key_versions = @p1 WHERE worker_id = @p2`,
				}[dialect]
				if _, err := db.ExecContext(ctx, upd, v, id); err != nil {
					t.Fatalf("write secret_key_versions: %v", err)
				}
			}

			if got := read(); got.Valid {
				t.Fatalf("a worker registered by the current code has secret_key_versions = %q, want NULL (unknown)", got.String)
			}
			write("1,2")
			if got := read(); !got.Valid || got.String != "1,2" {
				t.Fatalf("a set of versions did not round-trip: %+v", got)
			}
			// The EMPTY string is a worker with no key, and must not read as NULL.
			write("")
			if got := read(); !got.Valid || got.String != "" {
				t.Fatalf("an empty set read back as %+v, want a non-NULL empty string: a keyless worker "+
					"would be indistinguishable from one that predates the column", got)
			}
			write(nil)
			if got := read(); got.Valid {
				t.Fatalf("NULL read back as %+v", got)
			}
		})
	}
}
