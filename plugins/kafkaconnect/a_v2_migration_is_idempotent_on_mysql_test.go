package kafkaconnect

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestV2MigrationIsIdempotentOnMySQL is the regression test for cleat#2880:
// migration v2 (kafka_config.event_type) had a bare `ALTER TABLE ... ADD
// COLUMN` for its MySQL arm, not idempotent the way its PostgreSQL (`IF NOT
// EXISTS`) and SQL Server (a sys.columns guard) arms already were. MySQL DDL
// is not transactional, so a crash between that ALTER and plugin_migrations
// recording version 2 leaves a worker that re-runs it on its next start and
// gets `ERROR 1060 (42S21): Duplicate column name 'event_type'` -- fatal to
// boot, on every start thereafter.
//
// Mirrors plugins/webhookingest's TestWebhookSourcesDeletedAtMigrationIsIdempotentOnMySQL
// (cleat#2221) exact shape: plugin.RunMigrations' version-tracking skip
// cannot exercise this, since calling it twice just skips v2 the second
// time -- the mechanism a crash defeats. This re-runs the migration's real
// UpMySQL text, read from p.Migrations() rather than hand-copied, directly
// after RunMigrations has already applied it once.
func TestV2MigrationIsIdempotentOnMySQL(t *testing.T) {
	// cleat#2880 (same shape fixed in cleat#2249/#2883): testutil.NewPluginTestBackends
	// only includes a MySQL backend when CLEAT_TEST_MYSQL is set -- PostgreSQL is the
	// only one it attempts unconditionally. So when CLEAT_TEST_MYSQL is unset, the
	// returned slice has no MySQL entry, this loop's `continue` fires on every
	// iteration, the t.Run below is never reached, and the function returns having
	// asserted nothing -- a genuine PASS with no subtests and no t.Skip anywhere to
	// make that visible to the skip-budget/skip-ledger guards, which only see t.Skip.
	if os.Getenv("CLEAT_TEST_MYSQL") == "" {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
	}
	for _, be := range testutil.NewPluginTestBackends(t) {
		if be.Dialect != testutil.DialectMySQL {
			continue
		}
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			p := &Plugin{dialect: plugin.DialectMySQL}
			if err := plugin.RunMigrations(ctx, be.DB, plugin.DialectMySQL, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("migrations: %v", err)
			}

			var v2SQL string
			for _, m := range p.Migrations() {
				if m.Version == 2 {
					v2SQL = m.UpMySQL
				}
			}
			if v2SQL == "" {
				t.Fatal("migration v2's UpMySQL is empty -- nothing to re-run")
			}

			// v2's block also carries an unguarded `CREATE INDEX` -- a real,
			// separate hazard (cleat#2880's own "CREATE INDEX" list), scoped
			// to a follow-up PR rather than this one. Re-running the FULL
			// block text a second time would fail on that CREATE INDEX with
			// "Duplicate key name", for a reason unrelated to the column
			// guard this test exists to check -- so it is stripped here, not
			// fixed, keeping this test scoped to exactly the hazard this PR
			// addresses.
			columnOnly := withoutIndexDDL(v2SQL)
			runStatements(t, ctx, be, columnOnly)
			runStatements(t, ctx, be, columnOnly)

			var got int
			if err := be.DB.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM information_schema.columns
				WHERE table_schema = DATABASE() AND table_name = 'kafka_config' AND column_name = 'event_type'
			`).Scan(&got); err != nil {
				t.Fatalf("count kafka_config.event_type columns: %v", err)
			}
			if got != 1 {
				t.Errorf("kafka_config.event_type column count after two extra re-runs: got %d, want 1", got)
			}
		})
	}
}

// runStatements executes sqlText one statement at a time, on one pinned
// connection, mirroring plugin.RunMigrations' own execSQLStatements closely
// enough to exercise the same hazard it would: MySQL user-defined session
// variables (@col, @ddl) and a PREPAREd statement do not survive being sent
// on different pooled connections. A naive split on ';' is safe for this
// specific text -- checked by eye, and it is short and entirely this
// package's own -- but is not a general-purpose SQL splitter.
func runStatements(t *testing.T, ctx context.Context, be testutil.PluginTestBackend, sqlText string) {
	t.Helper()
	conn, err := be.DB.Conn(ctx)
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

// withoutIndexDDL drops any CREATE [UNIQUE] INDEX or DROP INDEX statement
// from sqlText, keeping everything else (including the guarded ADD/DROP
// COLUMN sequences this file tests) in its original order. Index DDL in
// these migrations is not yet guarded -- that is cleat#2880's separate,
// later "CREATE INDEX" PR -- so re-running a block containing it a second
// time fails for a reason unrelated to the column guards under test here.
func withoutIndexDDL(sqlText string) string {
	var kept []string
	for _, stmt := range strings.Split(sqlText, ";") {
		trimmed := strings.TrimSpace(stmt)
		if trimmed == "" {
			continue
		}
		upper := strings.ToUpper(trimmed)
		if strings.HasPrefix(upper, "CREATE INDEX") || strings.HasPrefix(upper, "CREATE UNIQUE INDEX") || strings.HasPrefix(upper, "DROP INDEX") {
			continue
		}
		kept = append(kept, trimmed)
	}
	return strings.Join(kept, ";\n")
}
