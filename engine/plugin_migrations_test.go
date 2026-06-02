package engine_test

import (
	"context"
	"database/sql"
	"os"
	"regexp"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/microsoft/go-mssqldb"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"

	_ "github.com/cleat-team/cleat/plugins/auditlog"
	_ "github.com/cleat-team/cleat/plugins/blobstore"
	_ "github.com/cleat-team/cleat/plugins/datadogexport"
	_ "github.com/cleat-team/cleat/plugins/eventstore"
	_ "github.com/cleat-team/cleat/plugins/eventtriggers"
	_ "github.com/cleat-team/cleat/plugins/featureflags"
	_ "github.com/cleat-team/cleat/plugins/jobqueue"
	_ "github.com/cleat-team/cleat/plugins/kafkaconnect"
	_ "github.com/cleat-team/cleat/plugins/kvstore"
	_ "github.com/cleat-team/cleat/plugins/notifications"
	_ "github.com/cleat-team/cleat/plugins/oauthprovider"
	_ "github.com/cleat-team/cleat/plugins/pagerdutyalert"
	// pgvector is imported but explicitly excluded from migration testing:
	// it requires the pgvector PostgreSQL extension and cannot run on MySQL/MSSQL.
	_ "github.com/cleat-team/cleat/plugins/pgvector"
	_ "github.com/cleat-team/cleat/plugins/ratelimiter"
	_ "github.com/cleat-team/cleat/plugins/scheduledbackup"
	_ "github.com/cleat-team/cleat/plugins/scheduler"
	_ "github.com/cleat-team/cleat/plugins/slacknotify"
	_ "github.com/cleat-team/cleat/plugins/webhookingest"
)

// pluginTestBackend describes a database backend for plugin migration testing.
// Each backend provides a dialect, a setup function that returns a *sql.DB
// and a teardown, and an enabled check.
type pluginTestBackend struct {
	name    string
	dialect plugin.Dialect
	setup   func(t *testing.T) (*sql.DB, func())
	enabled func() bool
}

// pluginTestBackends is the set of backends tested by
// TestPluginMigrations_AllDialects.
var pluginTestBackends = []pluginTestBackend{
	{
		name:    "postgres",
		dialect: plugin.DialectPostgres,
		setup: func(t *testing.T) (*sql.DB, func()) {
			t.Helper()
			db := testutil.TestDB(t, testutil.DialectPostgres)
			return db, func() { db.Close() }
		},
		enabled: func() bool { return true },
	},
	{
		name:    "mysql",
		dialect: plugin.DialectMySQL,
		setup: func(t *testing.T) (*sql.DB, func()) {
			t.Helper()
			db := testutil.MySQLTestDB(t)
			return db, func() { db.Close() }
		},
		enabled: func() bool { return os.Getenv("CLEAT_TEST_MYSQL") != "" },
	},
	{
		name:    "mssql",
		dialect: plugin.DialectMSSQL,
		setup: func(t *testing.T) (*sql.DB, func()) {
			t.Helper()
			db := testutil.MSSQLTestDB(t)
			return db, func() { db.Close() }
		},
		enabled: func() bool { return os.Getenv("CLEAT_TEST_MSSQL") != "" },
	},
}

// createTableRE matches CREATE TABLE statements (with optional IF NOT EXISTS,
// optional schema-qualified names).  The table name is capture group 1.
var createTableRE = regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w[\w.]*)`)

// extractTableNames parses all CREATE TABLE statements from a slice of
// migrations and returns the deduplicated list of bare table names (schema
// prefixes such as "public." or "dbo." are stripped).
func extractTableNames(migrations []plugin.Migration) []string {
	seen := make(map[string]bool)
	var tables []string
	for _, m := range migrations {
		matches := createTableRE.FindAllStringSubmatch(m.Up, -1)
		for _, match := range matches {
			name := match[1]
			// Strip schema prefix (e.g. "public.kv_store" -> "kv_store").
			if idx := strings.LastIndex(name, "."); idx >= 0 {
				name = name[idx+1:]
			}
			if !seen[name] {
				seen[name] = true
				tables = append(tables, name)
			}
		}
	}
	return tables
}

// tableExists checks whether a table exists in the database for a given
// dialect, querying information_schema.tables.
func tableExists(ctx context.Context, db *sql.DB, dialect plugin.Dialect, tableName string) (bool, error) {
	var query string
	switch dialect {
	case plugin.DialectPostgres:
		query = `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1)`
	case plugin.DialectMySQL:
		query = `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?)`
	case plugin.DialectMSSQL:
		query = `SELECT CASE WHEN EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema = 'dbo' AND table_name = @p1) THEN 1 ELSE 0 END`
	default:
		return false, nil
	}

	switch dialect {
	case plugin.DialectMSSQL:
		var n int
		err := db.QueryRowContext(ctx, query, tableName).Scan(&n)
		if err != nil {
			return false, err
		}
		return n == 1, nil
	default:
		var exists bool
		err := db.QueryRowContext(ctx, query, tableName).Scan(&exists)
		if err != nil {
			return false, err
		}
		return exists, nil
	}
}

// TestPluginMigrations_AllDialects runs plugin migrations against every
// available database backend and verifies that the expected tables were
// created.
//
// For PostgreSQL  all Up SQL is PostgreSQL DDL, so every plugin table should
// be created.  For MySQL and MSSQL the dialect-specific fields (UpMySQL /
// UpMSSQL) are currently empty -- RunMigrations skips those versions and
// records them as applied.  Once Stream B (MySQL DDL) and Stream C (MSSQL
// DDL) add the required SQL, this test will begin passing for those backends
// as well.
func TestPluginMigrations_AllDialects(t *testing.T) {
	for _, backend := range pluginTestBackends {
		t.Run(backend.name, func(t *testing.T) {
			if !backend.enabled() {
				t.Skipf("%s not available: set CLEAT_TEST_%s or start a local instance",
					backend.name, strings.ToUpper(backend.name))
			}

			db, teardown := backend.setup(t)
			defer teardown()

			ctx := context.Background()

			plugins, err := plugin.Discover()
			if err != nil {
				t.Fatalf("plugin.Discover: %v", err)
			}

			// Filter: only healthy plugins implementing HasMigrations.
			// pgvector is PostgreSQL-only and must be excluded.
			var migratable []*plugin.LoadedPlugin
			for _, lp := range plugins {
				if !lp.Healthy {
					continue
				}
				if _, ok := lp.Plugin.(plugin.HasMigrations); !ok {
					continue
				}
				if lp.Plugin.Info().Name == "pgvector" {
					continue
				}
				migratable = append(migratable, lp)
			}

			// Run migrations — coreMigrations is nil because plugins
			// register their own migrations via Migrations().
			if err := plugin.RunMigrations(ctx, db, backend.dialect, nil, migratable); err != nil {
				t.Fatalf("%s: RunMigrations failed: %v", backend.name, err)
			}

			// Verify that every CREATE TABLE in every plugin's
			// migration set now exists in the database.
			for _, lp := range migratable {
				p, ok := lp.Plugin.(plugin.HasMigrations)
				if !ok {
					continue // should not happen given the filter above
				}
				tables := extractTableNames(p.Migrations())
				for _, table := range tables {
					exists, err := tableExists(ctx, db, backend.dialect, table)
					if err != nil {
						t.Errorf("%s/%s: failed to check table %q existence: %v\n"+
							"  This may be a query syntax issue for the dialect. Verify\n"+
							"  information_schema query is correct for %s.",
							backend.name, lp.Plugin.Info().Name, table, err, backend.name)
						continue
					}
					if !exists {
						t.Errorf("%s/%s: table %q was not created after RunMigrations.\n"+
							"  Plugin migrations for %s currently contain only PostgreSQL DDL.\n"+
							"  To fix: add UpMySQL / UpMSSQL to the plugin's Migration struct\n"+
							"  in plugins/%s/migrations.go.",
							backend.name, lp.Plugin.Info().Name, table,
							backend.name, lp.Plugin.Info().Name)
					}
				}
			}
		})
	}
}
