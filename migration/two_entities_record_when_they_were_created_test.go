package migration_test

// package migration_test (external), not migration: see runner_test.go's file
// header for why -- engine/testutil now depends on this package.

import (
	"context"
	"database/sql"
	"testing"

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
// TestCreatedAtIsBackfilledFromUpdatedAtNotFromTheMigrationsClock was removed
// by cleat#2434, and the reason is worth recording rather than leaving a gap
// where a test used to be.
//
// It staged every migration for a dialect EXCEPT the one that adds created_at,
// seeded a row, then applied that migration and asserted the row's created_at
// landed near its updated_at rather than near the present. That is a real
// property -- a column added with DEFAULT now() claims every existing row was
// created when the migration ran -- and it needs a database to test.
//
// It needs something else too, which the compaction took away: the excluded
// migration has to exist as a file. Postgres's was
// 086_two_entities_record_when_they_were_created.sql, folded into that
// dialect's three-file baseline by cleat#2059; MSSQL's was 078_..., folded in
// by cleat#2434. With no such file stageAllMigrations excludes nothing, its own
// assertion catches that, and the "before" state cannot be built from the
// shipped tree -- which is what a frozen baseline MEANS: the pre-migration
// schema is no longer something this repo ships.
//
// Reconstructing it from git history was considered and rejected: CI checks out
// with fetch-depth 1, so `git show <pre-baseline-commit>:migrations/...` is not
// available where this test would need it.
//
// The property is not left unguarded, only guarded differently. Once a
// migration is folded into a frozen baseline it can never be applied again --
// existing databases already record its name in schema_migrations, and fresh
// ones are built at the end state directly -- so the upgrade it performed
// cannot regress. What a baseline CAN get wrong is the end state, and that is
// what scripts/gen-mssql-baseline -mode=diff and -mode=supplementary assert,
// catalogue to catalogue, against a database built from the pre-compaction
// chain, backed by a known-positive battery.
//
// NOTE, and it predates this change: the Postgres leg of this test was already
// dead. 086 has not existed since cleat#2059. It went unnoticed because no job
// sets CLEAT_TEST_POSTGRES for this package, so the leg SKIPPED rather than
// failing -- an absent DSN and a missing file look the same from here.

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
//
// SCOPED TO THE CURRENT DATABASE, and on MySQL that is not optional. A MySQL
// "schema" IS a database, so information_schema.COLUMNS spans every database on
// the server -- including the fully-migrated one the rest of the suite uses.
// Unscoped, this reports `tenant_settings.created_at` as already present in a
// scratch database that has never seen the migration, and the precondition
// below turns a working test into a false UNMEASURED.
//
// Postgres and SQL Server both scope information_schema to the connected
// database already, so only the MySQL arm can fail this way -- which is exactly
// why it passed locally and on two dialects in CI. Every dialect carries the
// predicate regardless: a check that is correct only because two of three
// engines are forgiving is one engine change away from being wrong everywhere.
func columnExists(t *testing.T, ctx context.Context, db *sql.DB, d idempotencyDialect, table, column string) bool {
	t.Helper()
	var scope string
	switch d.dialect {
	case migration.DialectMySQL:
		scope = "TABLE_SCHEMA = DATABASE()"
	case migration.DialectMSSQL:
		scope = "TABLE_CATALOG = DB_NAME()"
	default:
		scope = "TABLE_CATALOG = CURRENT_DATABASE()"
	}
	var n int
	q := `SELECT COUNT(*) FROM information_schema.COLUMNS
	      WHERE ` + scope + ` AND TABLE_NAME = ? AND COLUMN_NAME = ?`
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
