package kvstore

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/lib/pq"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/migration"
	"github.com/cleat-team/cleat/plugin"
)

// Plugin tables go where --schema says, not where plugin/migration.go used to
// say. cleat#1287.
//
// #1353 moved core migrations onto the configured schema and left this half
// deliberately undone, because it is only reachable once core migrations
// complete: before that fix a non-default --schema killed the worker at
// 020_event_intent.sql, so no plugin migration ever ran to be wrong.
//
// The failure this prevents is quiet. pluginMigrationSession pinned
// `SET search_path = public` unconditionally, while the runtime pool plugins
// are handed (getPluginDB, cmd/cleat-worker/server.go) carries
// search_path=<schema> from its DSN and every plugin's SQL is unqualified. So
// the DDL landed in one schema and the queries looked in another, and the
// migration run reported success either way -- the first sign is a plugin's
// first query failing with "relation kv_store does not exist" at runtime.
func TestPluginTablesFollowTheConfiguredSchema(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema string // as passed to plugin.WithSchema
		want   string
	}{
		{"default", "", "public"},
		{"non-default", "kvstore_prod", "kvstore_prod"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A database of its own, not the package's shared one. Both cases
			// create a table of the same name in different schemas, so on a
			// shared database the second reads the first's leftovers and the
			// placement assertion cannot say which run put it there.
			ctx := context.Background()

			// The availability gate, and nothing else: this test needs a
			// database of its own (see freshDatabase) but the decision about
			// whether PostgreSQL is there at all belongs in one place.
			gate := postgresBackend(t)
			gate.Cleanup()

			db := freshDatabase(t, "cleat_kvstore_schema_"+strings.ReplaceAll(tc.name, "-", "_"))
			if err := migration.NewRunner(db, migration.DialectPostgres, "../../migrations").
				Run(ctx); err != nil {
				t.Fatalf("core migrations: %v", err)
			}
			pg := struct{ DB *sql.DB }{DB: db}
			loaded := []*plugin.LoadedPlugin{{Plugin: &Plugin{}, Healthy: true}}

			var opts []plugin.MigrationOption
			if tc.schema != "" {
				opts = append(opts, plugin.WithSchema(tc.schema))
			}
			if err := plugin.RunMigrations(ctx, pg.DB, plugin.DialectPostgres,
				nil, loaded, opts...); err != nil {
				t.Fatalf("RunMigrations with schema %q: %v", tc.schema, err)
			}

			var landed string
			if err := pg.DB.QueryRowContext(ctx,
				`SELECT coalesce(string_agg(schemaname, ',' ORDER BY schemaname), '<nowhere>')
				   FROM pg_tables WHERE tablename = 'kv_store'`).Scan(&landed); err != nil {
				t.Fatalf("locating kv_store: %v", err)
			}
			if landed != tc.want {
				t.Fatalf("kv_store landed in %q, want %q.\n\n"+
					"The DDL and the plugin's own unqualified queries have to "+
					"agree about the schema, and nothing reports it when they "+
					"do not -- the migration succeeds and the first runtime "+
					"query fails.", landed, tc.want)
			}

			// The bookkeeping follows the tables. In the other schema it reads
			// as empty on the next boot and every plugin migration re-applies,
			// which for a CREATE TABLE IF NOT EXISTS is silent and for an
			// ALTER is not.
			var tracking string
			if err := pg.DB.QueryRowContext(ctx,
				`SELECT coalesce(string_agg(schemaname, ',' ORDER BY schemaname), '<nowhere>')
				   FROM pg_tables WHERE tablename = 'plugin_migrations'`).Scan(&tracking); err != nil {
				t.Fatalf("locating plugin_migrations: %v", err)
			}
			if tracking != tc.want {
				t.Errorf("plugin_migrations is in %q, kv_store is in %q", tracking, tc.want)
			}

			// And what a plugin's own statement sees: unqualified, resolved
			// through the search_path its runtime pool carries. This is the
			// half that has to be true for any of the above to matter.
			conn, err := pg.DB.Conn(ctx)
			if err != nil {
				t.Fatalf("connection: %v", err)
			}
			defer conn.Close()
			if _, err := conn.ExecContext(ctx, "SET search_path = "+tc.want); err != nil {
				t.Fatalf("set search_path: %v", err)
			}
			var n int
			if err := conn.QueryRowContext(ctx,
				`SELECT count(*) FROM kv_store`).Scan(&n); err != nil {
				t.Fatalf("unqualified read through search_path=%s: %v", tc.want, err)
			}
		})
	}
}

// freshDatabase creates an empty database and returns a handle to it, dropping
// it when the test ends.
//
// engine/testutil's backends hand out one database per package, which is right
// for almost everything and wrong here: this test asserts WHERE a table is,
// and two cases that both create kv_store would read each other's leftovers.
func freshDatabase(t *testing.T, name string) *sql.DB {
	t.Helper()
	// Whether PostgreSQL is available at all is decided by postgresBackend,
	// the same gate every other test in this package uses -- the caller runs
	// it before calling here. Duplicating that decision is what
	// scripts/check-skips.sh objected to, and it was right: a second,
	// independent "is it reachable" check is a second chance to turn an
	// unreachable service into a green that measured nothing. Everything below
	// is Fatal.
	adminDSN := testutil.PostgresTestDSN()
	admin, err := sql.Open("postgres", adminDSN)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Fatalf("postgres is unreachable, but postgresBackend accepted it: %v", err)
	}
	for _, stmt := range []string{
		`DROP DATABASE IF EXISTS ` + name,
		`CREATE DATABASE ` + name,
	} {
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("postgres", adminDSN)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(`DROP DATABASE IF EXISTS ` + name)
	})

	dsn, err := swapDatabaseName(adminDSN, name)
	if err != nil {
		t.Fatalf("derive scratch DSN: %v", err)
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open scratch database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping scratch database: %v", err)
	}
	return db
}

// swapDatabaseName replaces the database in a postgres:// URL.
func swapDatabaseName(dsn, name string) (string, error) {
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return "", errNoDatabaseInDSN
	}
	rest := dsn[slash+1:]
	if q := strings.IndexByte(rest, '?'); q >= 0 {
		return dsn[:slash+1] + name + rest[q:], nil
	}
	return dsn[:slash+1] + name, nil
}

var errNoDatabaseInDSN = errDSN("postgres DSN has no database component")

type errDSN string

func (e errDSN) Error() string { return string(e) }
