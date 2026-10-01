package oauthprovider

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestV2V4V5MigrationsAreIdempotentOnMySQL is the regression test for
// cleat#2880: v2 (oauth_sessions.state, code_verifier, token_hash), v4
// (oauth_config.issuer, oauth_sessions.nonce) and v5 (dropping
// oauth_config.client_secret) each had bare `ALTER TABLE ... ADD/DROP
// COLUMN` statements for their MySQL arm, not idempotent the way their
// PostgreSQL (`IF NOT EXISTS`/`IF EXISTS`) and SQL Server (a sys.columns
// guard) arms already were. MySQL DDL is not transactional, so a crash
// between one of those ALTERs and plugin_migrations recording the version
// leaves a worker that re-runs it on its next start and gets `ERROR 1060`
// (duplicate column) or `ERROR 1091` (no such column to drop) -- fatal to
// boot, on every start thereafter.
//
// Mirrors plugins/webhookingest's TestWebhookSourcesDeletedAtMigrationIsIdempotentOnMySQL
// (cleat#2221) exact shape: plugin.RunMigrations' version-tracking skip
// cannot exercise this, since calling it twice just skips the version the
// second time -- the mechanism a crash defeats. This re-runs each
// migration's real UpMySQL text, read from p.Migrations() rather than
// hand-copied, directly after RunMigrations has already applied it once.
func TestV2V4V5MigrationsAreIdempotentOnMySQL(t *testing.T) {
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

			upMySQL := map[int]string{}
			for _, m := range p.Migrations() {
				upMySQL[m.Version] = m.UpMySQL
			}
			for _, v := range []int{2, 4, 5} {
				if upMySQL[v] == "" {
					t.Fatalf("migration v%d's UpMySQL is empty -- nothing to re-run", v)
				}
			}

			// v2's block also carries an unguarded DROP INDEX and two
			// unguarded CREATE INDEX statements -- a real, separate hazard
			// (cleat#2880's own "CREATE INDEX" list), scoped to a follow-up
			// PR rather than this one. Re-running the FULL block text a
			// second time would fail on the DROP INDEX ("check that column/key
			// exists") or a CREATE INDEX ("Duplicate key name"), for a reason
			// unrelated to the column guards this test exists to check -- so
			// they are stripped here, not fixed, keeping this test scoped to
			// exactly the hazard this PR addresses.
			runStatements(t, ctx, be, withoutIndexDDL(upMySQL[2]))
			runStatements(t, ctx, be, withoutIndexDDL(upMySQL[2]))

			runStatements(t, ctx, be, upMySQL[4])
			runStatements(t, ctx, be, upMySQL[4])

			runStatements(t, ctx, be, upMySQL[5])
			runStatements(t, ctx, be, upMySQL[5])

			assertSessionsCol := func(column string, want int) {
				t.Helper()
				var got int
				if err := be.DB.QueryRowContext(ctx, `
					SELECT COUNT(*) FROM information_schema.columns
					WHERE table_schema = DATABASE() AND table_name = 'oauth_sessions' AND column_name = ?
				`, column).Scan(&got); err != nil {
					t.Fatalf("count oauth_sessions.%s columns: %v", column, err)
				}
				if got != want {
					t.Errorf("oauth_sessions.%s column count after two extra re-runs: got %d, want %d", column, got, want)
				}
			}
			assertConfigCol := func(column string, want int) {
				t.Helper()
				var got int
				if err := be.DB.QueryRowContext(ctx, `
					SELECT COUNT(*) FROM information_schema.columns
					WHERE table_schema = DATABASE() AND table_name = 'oauth_config' AND column_name = ?
				`, column).Scan(&got); err != nil {
					t.Fatalf("count oauth_config.%s columns: %v", column, err)
				}
				if got != want {
					t.Errorf("oauth_config.%s column count after two extra re-runs: got %d, want %d", column, got, want)
				}
			}
			// v2
			assertSessionsCol("state", 1)
			assertSessionsCol("code_verifier", 1)
			assertSessionsCol("token_hash", 1)
			// v4
			assertConfigCol("issuer", 1)
			assertSessionsCol("nonce", 1)
			// v5
			assertConfigCol("client_secret", 0)
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
