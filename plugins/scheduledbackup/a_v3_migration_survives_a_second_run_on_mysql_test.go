package scheduledbackup

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestBackupHistoryConfigIdCascadeMigrationIsIdempotentOnMySQL pins
// migrations.go v3's own comment: MySQL raises
// ER_DUP_KEYNAME/ER_FK_DUP_NAME on a re-add rather than silently no-op-ing,
// and there is no ADD CONSTRAINT IF NOT EXISTS to lean on -- so v3's UpMySQL
// guards the ADD with an information_schema.table_constraints check inside a
// PREPARE/EXECUTE/DEALLOCATE sequence, the same shape
// plugins/webhookingest/migrations.go's v8 (deleted_at) and
// plugins/notifications/migrations.go's v7 (webhook_delivery's identical FK
// fix) already use for the same reason.
//
// That is exactly what plugin.RunMigrations' own version-tracking skip
// cannot exercise: calling it twice just skips v3 the second time, which is
// the mechanism a crash between the ALTER and plugin_migrations recording
// version 3 would defeat. This runs the migration's real UpMySQL text --
// read from p.Migrations(), not hand-copied -- directly, twice, after
// RunMigrations has already applied it once, the same state a crashed-then-
// restarted worker would find the constraint in.
//
// THE EXPLICIT os.Getenv CHECK, RATHER THAN LOOPING OVER
// testutil.NewPluginTestBackends AND continue-ing PAST NON-MYSQL BACKENDS:
// that shape (still present in webhookingest's own a_v8_migration_survives_
// a_second_run_on_mysql_test.go, an earlier, unflagged instance of the same
// thing) is CLAUDE.md's "silent no-op" trap -- when CLEAT_TEST_MYSQL is
// unset, NewPluginTestBackends simply omits mysql from its slice, so the
// loop body never runs, no t.Run ever starts, and the outer test reports a
// trivial PASS with no skip event at all. Measured directly against this
// file's first draft: with CLEAT_TEST_MYSQL unset, `go test -json` printed
// `pass TestBackupHistoryConfigIdCascadeMigrationIsIdempotentOnMySQL` in
// 0.02s -- no "run .../mysql" line ever appeared. Same trap cleat-review
// flagged for #2242's ingest-race test, same fix: check the env var and
// t.Skip explicitly before opening any connection, so CI's test-go/plugins
// job (PostgreSQL only) reports a real skip event rather than an
// indistinguishable-from-passing no-op.
func TestBackupHistoryConfigIdCascadeMigrationIsIdempotentOnMySQL(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MYSQL") == "" {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
	}

	mysqlDB := testutil.MySQLTestDB(t)
	defer mysqlDB.Close()

	ctx := context.Background()
	testutil.SetupFullSchema(t, mysqlDB, testutil.DialectMySQL)

	p := &Plugin{dialect: plugin.DialectMySQL}
	if err := plugin.RunMigrations(ctx, mysqlDB, plugin.DialectMySQL, nil,
		[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	var v3SQL string
	for _, m := range p.Migrations() {
		if m.Version == 3 {
			v3SQL = m.UpMySQL
		}
	}
	if v3SQL == "" {
		t.Fatal("migration v3's UpMySQL is empty -- nothing to re-run")
	}

	// The constraint already exists at this point (RunMigrations applied v3
	// above, inside its own transaction). Re-running the same text against
	// an ordinary connection -- not the migration framework's
	// tx.ExecContext, and not a multi-statement DSN -- is the crash
	// scenario, and also the more exacting one: the
	// PREPARE/EXECUTE/DEALLOCATE sequence's session variables (@fk, @ddl)
	// have to survive being split into separate Exec calls the same way
	// plugin.RunMigrations' own execSQLStatements would split them, which
	// only holds if they land on one connection.
	runStatements(t, ctx, mysqlDB, v3SQL)

	// And a second extra run, for good measure -- the guard should not be
	// order- or count-sensitive.
	runStatements(t, ctx, mysqlDB, v3SQL)

	var fkCount int
	if err := mysqlDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.table_constraints
		WHERE constraint_schema = DATABASE() AND table_name = 'backup_history'
		  AND constraint_name = 'backup_history_config_id_fkey'
	`).Scan(&fkCount); err != nil {
		t.Fatalf("count backup_history_config_id_fkey constraints: %v", err)
	}
	if fkCount != 1 {
		t.Errorf("backup_history_config_id_fkey constraint count after two extra re-runs: got %d, want 1", fkCount)
	}
}

// runStatements executes sqlText one statement at a time, on one pinned
// connection, mirroring plugin.RunMigrations' own execSQLStatements closely
// enough to exercise the same hazard it would: MySQL user-defined session
// variables (@fk, @ddl) and a PREPAREd statement do not survive being sent on
// different pooled connections. A naive split on ';' is safe for this
// specific text -- checked by eye, and it is short and entirely this
// package's own -- but is not a general-purpose SQL splitter.
func runStatements(t *testing.T, ctx context.Context, db *sql.DB, sqlText string) {
	t.Helper()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Close()

	for _, stmt := range strings.Split(sqlText, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}
