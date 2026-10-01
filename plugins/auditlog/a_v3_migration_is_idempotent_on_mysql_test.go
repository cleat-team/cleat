package auditlog

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestV3MigrationIsIdempotentOnMySQL is the regression test for cleat#2880:
// migration v3's three-column ADD (audit_events.seq, prev_hash, row_hash)
// had a bare `ALTER TABLE ... ADD COLUMN` for its MySQL arm, not idempotent
// the way its PostgreSQL (`IF NOT EXISTS`) and SQL Server (a COL_LENGTH
// guard) arms already were. MySQL DDL is not transactional, so a crash
// between that ALTER and plugin_migrations recording version 3 leaves a
// worker that re-runs it on its next start and gets `ERROR 1060 (42S21):
// Duplicate column name 'seq'` -- fatal to boot, on every start thereafter.
//
// Mirrors plugins/webhookingest's TestWebhookSourcesDeletedAtMigrationIsIdempotentOnMySQL
// (cleat#2221) exact shape: plugin.RunMigrations' version-tracking skip
// cannot exercise this, since calling it twice just skips v3 the second
// time -- the mechanism a crash defeats. This re-runs the migration's real
// UpMySQL text, read from p.Migrations() rather than hand-copied, directly
// after RunMigrations has already applied it once -- the same state a
// crashed-then-restarted worker would find the columns in.
func TestV3MigrationIsIdempotentOnMySQL(t *testing.T) {
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

			var v3SQL string
			for _, m := range p.Migrations() {
				if m.Version == 3 {
					v3SQL = m.UpMySQL
				}
			}
			if v3SQL == "" {
				t.Fatal("migration v3's UpMySQL is empty -- nothing to re-run")
			}

			// v3's block also carries an unguarded `CREATE UNIQUE INDEX` --
			// a real, separate hazard (cleat#2880's own "CREATE INDEX" list),
			// scoped to a follow-up PR rather than this one. Re-running the
			// FULL block text a second time would fail on that CREATE INDEX
			// with "Duplicate key name", for a reason unrelated to the column
			// guards this test exists to check -- so it is stripped here,
			// not fixed, keeping this test scoped to exactly the hazard this
			// PR addresses.
			columnOnly := withoutIndexDDL(v3SQL)
			runStatements(t, ctx, be, columnOnly)
			runStatements(t, ctx, be, columnOnly)

			assertColumnCount := func(table, column string, want int) {
				t.Helper()
				var got int
				if err := be.DB.QueryRowContext(ctx, `
					SELECT COUNT(*) FROM information_schema.columns
					WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?
				`, table, column).Scan(&got); err != nil {
					t.Fatalf("count %s.%s columns: %v", table, column, err)
				}
				if got != want {
					t.Errorf("%s.%s column count after two extra re-runs: got %d, want %d", table, column, got, want)
				}
			}
			assertColumnCount("audit_events", "seq", 1)
			assertColumnCount("audit_events", "prev_hash", 1)
			assertColumnCount("audit_events", "row_hash", 1)
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
// from sqlText, keeping everything else (including MODIFY COLUMN, which is
// idempotent, and the guarded ADD/DROP COLUMN sequences this file tests) in
// its original order. Index DDL in these migrations is not yet guarded --
// that is cleat#2880's separate, later "CREATE INDEX" PR -- so re-running a
// block containing it a second time fails for a reason unrelated to the
// column guards under test here.
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
