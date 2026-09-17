package migration_test

// package migration_test (external), not migration: see runner_test.go's file
// header for why -- engine/testutil now depends on this package.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/microsoft/go-mssqldb"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/migration"
)

// cleat#1702. `created_at` arrives on tenant_settings and tenant_secrets, the
// last two members of the entity contract that lacked it.
//
// WHAT NEEDS A DATABASE RATHER THAN A GUARD. scripts/check-entity-contract.py
// reads the migration FILES, so it can prove the column is declared and cannot
// prove the SQL runs, that the backfill matches any rows, or that it writes the
// right value. Those are the three ways this migration can be wrong while every
// file-level check passes.
//
// THE VALUE IS THE ASSERTION, NOT THE COLUMN'S EXISTENCE. These two tables are
// the INVERSE of the other eight members: they carry `updated_at` and no
// `created_at`, so the backfill runs the opposite way to migration 085's and
// reads `updated_at`. The failure mode worth catching is the obvious
// alternative -- `DEFAULT now()` applied at ADD COLUMN time, which stamps every
// existing row with the instant the migration ran. Both produce a NOT NULL
// TIMESTAMPTZ on every row; only the value tells them apart, which is why this
// test seeds a distinctive one far in the past and asserts on it rather than
// asserting the column is populated.
//
// The rows are seeded BEFORE the migration, because a migration whose entire
// point is what it does to a database that already has rows in it tests the
// half that does not matter when it is run against an empty schema.
func TestCreatedAtIsBackfilledFromUpdatedAtNotFromTheMigrationsClock(t *testing.T) {
	// Deliberately far in the past, and not a round now(). Any value the
	// migration invents for itself -- now(), SYSUTCDATETIME(), NOW(6) -- is
	// years away from this, so the two cannot be confused by clock skew,
	// timezone handling, or a test that happens to run at midnight.
	seeded := time.Date(2020, 3, 14, 15, 9, 26, 0, time.UTC)

	for _, d := range []idempotencyDialect{postgresDialect(), mysqlDialect(), mssqlDialect()} {
		d := d
		t.Run(string(d.dialect), func(t *testing.T) {
			if d.skipReason != "" {
				t.Skip(d.skipReason)
			}
			db := createdAtScratchDB(t, d)
			ctx := context.Background()

			// Before: every migration except the one under test, so the
			// "before" state is the real shipped schema rather than a
			// hand-written approximation of it.
			before := stageAllMigrations(t, d.dialect, createdAtMigration(d.dialect))
			if err := runMigrations(t, ctx,
				migration.NewRunner(db, d.dialect, before), d.dialect); err != nil {
				t.Fatalf("apply the migrations preceding the one under test: %v", err)
			}

			// PRECONDITION, reported as UNMEASURED rather than as a pass. If
			// the column is already here the seed below writes into the
			// finished shape and the assertion proves nothing.
			if columnExists(t, ctx, db, d, "tenant_settings", "created_at") {
				t.Fatalf("UNMEASURED: tenant_settings.created_at already exists before the " +
					"migration under test ran, so this test cannot observe the backfill")
			}

			conn := pinnedConn(t, ctx, db, d)

			if _, err := conn.ExecContext(ctx, d.rebind(
				`INSERT INTO tenant_settings (tenant_id, updated_at) VALUES (?, ?)`),
				engine.DefaultTenantUUID, seeded); err != nil {
				t.Fatalf("seed a pre-upgrade tenant_settings row: %v", err)
			}
			if _, err := conn.ExecContext(ctx, d.rebind(
				`INSERT INTO tenant_secrets (tenant_id, name, ciphertext, updated_at)
				 VALUES (?, ?, ?, ?)`),
				engine.DefaultTenantUUID, "pre-upgrade-secret", "Y2lwaGVy", seeded); err != nil {
				t.Fatalf("seed a pre-upgrade tenant_secrets row: %v", err)
			}

			// After.
			after := stageAllMigrations(t, d.dialect, "")
			if err := runMigrations(t, ctx,
				migration.NewRunner(db, d.dialect, after), d.dialect); err != nil {
				t.Fatalf("apply the migration under test: %v", err)
			}

			for _, tc := range []struct {
				table string
				where string
				arg   any
			}{
				{"tenant_settings", "tenant_id = ?", engine.DefaultTenantUUID},
				{"tenant_secrets", "name = ?", "pre-upgrade-secret"},
			} {
				var got time.Time
				err := conn.QueryRowContext(ctx, d.rebind(
					`SELECT created_at FROM `+tc.table+` WHERE `+tc.where), tc.arg).Scan(&got)
				if err != nil {
					t.Fatalf("%s: read created_at back after migrating: %v\n"+
						"A row the backfill could not see is the silent failure this "+
						"migration is written against.", tc.table, err)
				}

				// The assertion. A second of tolerance covers a dialect that
				// stores less precision than it is given; it is four orders of
				// magnitude tighter than the difference between this value and
				// any clock the migration could have read.
				if delta := got.UTC().Sub(seeded); delta > time.Second || delta < -time.Second {
					t.Errorf("%s.created_at is %v, want %v (its updated_at).\n"+
						"Off by %v. A value near the present means the column was added "+
						"with DEFAULT now() rather than backfilled from updated_at, which "+
						"claims every existing row was created when the migration ran.",
						tc.table, got.UTC(), seeded, delta)
				}
			}
		})
	}
}

// createdAtMigration is the file under test for a dialect. Named rather than
// derived from a number, because the numbers are allocated at push time and
// differ per dialect -- and a test that reasons about "the highest-numbered
// file" would silently start testing somebody else's migration.
func createdAtMigration(d migration.Dialect) string {
	switch d {
	case migration.DialectPostgres:
		return "086_two_entities_record_when_they_were_created.sql"
	case migration.DialectMySQL:
		return "074_two_entities_record_when_they_were_created.sql"
	default:
		return "078_two_entities_record_when_they_were_created.sql"
	}
}

// stageAllMigrations copies every migration for a dialect into a temp tree,
// optionally omitting one.
//
// stageMigrations next door takes an explicit file list, which is right for a
// test whose "before" state is two files. This one's before state is every
// migration but one -- eighty-odd for Postgres -- so naming them is not an
// option, and a list would go stale on every new migration anyway.
//
// IT ASSERTS THE EXCLUSION HAPPENED. Passing a name that matches nothing would
// stage the complete set, the "before" database would already have the column,
// and the test would pass while measuring nothing. That is the same shape as a
// `-run` pattern selecting no tests.
func stageAllMigrations(t *testing.T, dialect migration.Dialect, exclude string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, string(dialect))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("stage migrations: mkdir: %v", err)
	}
	src := filepath.Join(migrationsRoot(t), string(dialect))
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("stage migrations: read %s: %v", src, err)
	}
	staged, excluded := 0, false
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".sql" {
			continue
		}
		if exclude != "" && e.Name() == exclude {
			excluded = true
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatalf("stage migrations: read %s: %v", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o644); err != nil {
			t.Fatalf("stage migrations: write %s: %v", e.Name(), err)
		}
		staged++
	}
	if staged == 0 {
		t.Fatalf("UNMEASURED: staged no migrations from %s", src)
	}
	if exclude != "" && !excluded {
		t.Fatalf("UNMEASURED: %q matched no file in %s, so nothing was held back and "+
			"the 'before' state is really the 'after' state", exclude, src)
	}
	return root
}

// pinnedConn returns a single connection, with the tenant context set where the
// dialect needs one.
//
// ONE CONNECTION, NOT THE POOL. SQL Server's tenant context is
// sp_set_session_context, which is per-connection state; issued through a
// *sql.DB it lands on whichever connection the pool hands out and the next
// query may run on a different one. cmd/cleatctl/droptenant_mssql.go records
// the same hazard for the same reason.
//
// Postgres needs nothing here: migrations and this test connect as a superuser,
// which bypasses RLS unconditionally. MySQL has no row-level policy at all.
func pinnedConn(t *testing.T, ctx context.Context, db *sql.DB, d idempotencyDialect) *sql.Conn {
	t.Helper()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin a connection: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if d.dialect == migration.DialectMSSQL {
		if _, err := conn.ExecContext(ctx,
			`EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`,
			engine.DefaultTenantUUID); err != nil {
			t.Fatalf("set the SQL Server tenant context: %v", err)
		}
	}
	return conn
}

// columnExists answers from the live database rather than from the files, which
// is the whole reason this test exists.
func columnExists(t *testing.T, ctx context.Context, db *sql.DB, d idempotencyDialect, table, column string) bool {
	t.Helper()
	var n int
	q := `SELECT COUNT(*) FROM information_schema.COLUMNS
	      WHERE TABLE_NAME = ? AND COLUMN_NAME = ?`
	if err := db.QueryRowContext(ctx, d.rebind(q), table, column).Scan(&n); err != nil {
		t.Fatalf("query information_schema for %s.%s: %v", table, column, err)
	}
	return n > 0
}

// Each test in this package needs its own scratch database: they run in one
// package and a shared name would make them order-dependent.
const createdAtScratchDBName = "cleat_migration_created_at_test"

func createdAtScratchDB(t *testing.T, d idempotencyDialect) *sql.DB {
	t.Helper()
	switch d.dialect {
	case migration.DialectPostgres:
		return newScratchDB(t, createdAtScratchDBName)
	case migration.DialectMySQL:
		return newMySQLScratchDB(t, createdAtScratchDBName)
	default:
		return newMSSQLScratchDB(t, createdAtScratchDBName)
	}
}
