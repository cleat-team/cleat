package pluginharness

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/migration"
	"github.com/cleat-team/cleat/plugin"
)

// OpenTestDB opens a database connection and creates an isolated schema
// (or database for MySQL) for the test. It returns the *sql.DB handle and
// the generated unique schema name.
//
// For PostgreSQL: creates a schema and sets search_path.
// For MySQL: creates a database and issues USE.
// For MSSQL: creates a schema.
//
// The caller must call CleanupTestDB when done.
func OpenTestDB(t *testing.T, dialect plugin.Dialect, connStr string) (*sql.DB, string) {
	t.Helper()

	if testing.Short() {
		t.Skip("Skipping database test in short mode")
	}

	schemaName := uniqueSchemaName()

	var driverName string
	switch dialect {
	case plugin.DialectPostgres:
		driverName = "postgres"
	case plugin.DialectMySQL:
		driverName = "mysql"
	case plugin.DialectMSSQL:
		driverName = "sqlserver"
	default:
		t.Fatalf("OpenTestDB: unknown dialect: %s", dialect)
	}

	db, err := sql.Open(driverName, connStr)
	if err != nil {
		t.Fatalf("OpenTestDB: sql.Open: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("OpenTestDB: ping: %v", err)
	}

	ctx := context.Background()
	switch dialect {
	case plugin.DialectPostgres:
		// Use public schema — CI provides an isolated Postgres container
		// so there is no risk of cross-test pollution. The separate-schema
		// approach with SET search_path is unreliable across connection
		// pools because search_path is per-session.
		schemaName = "public"
	case plugin.DialectMySQL:
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE DATABASE IF NOT EXISTS %s`, quoteIdent(dialect, schemaName))); err != nil {
			t.Fatalf("OpenTestDB: create database: %v", err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`USE %s`, quoteIdent(dialect, schemaName))); err != nil {
			t.Fatalf("OpenTestDB: use database: %v", err)
		}
	case plugin.DialectMSSQL:
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA %s`, quoteIdent(dialect, schemaName))); err != nil {
			t.Fatalf("OpenTestDB: create schema (mssql): %v", err)
		}
	}

	return db, schemaName
}

// RunCoreMigrations reads SQL migration files from the migrations directory
// for the given dialect and executes them in order (sorted by filename).
//
// Migration files are expected at ../../migrations/{postgres,mysql,mssql}/.
// This function is a no-op if the migration directory cannot be found (the
// caller is expected to have created the schemas another way).
func RunCoreMigrations(t *testing.T, db *sql.DB, dialect plugin.Dialect, schemaName string) {
	t.Helper()

	dir := coreMigrationDir(t, dialect)
	if dir == "" {
		t.Log("RunCoreMigrations: no migration directory found, skipping")
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("RunCoreMigrations: read dir %s: %v", dir, err)
	}

	var files []fs.DirEntry
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e)
		}
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Name() < files[j].Name()
	})

	ctx := context.Background()

	// Use a single dedicated connection so that SET search_path / USE
	// database persists across all migration statements. Connection
	// pooling would otherwise distribute statement execution across
	// different connections, losing the schema/database context.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("RunCoreMigrations: get conn: %v", err)
	}
	defer conn.Close()

	if err := setSearchPath(ctx, conn, dialect, schemaName); err != nil {
		t.Fatalf("RunCoreMigrations: set context %s: %v", schemaName, err)
	}

	for _, f := range files {
		path := filepath.Join(dir, f.Name())
		sqlBytes, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("RunCoreMigrations: read %s: %v", path, err)
		}
		sqlStr := string(sqlBytes)
		if prefix := schemaPrefix(dialect, schemaName); prefix != "" {
			sqlStr = prefix + ";" + "\n" + sqlStr
		}
		statements := splitStatements(dialect, sqlStr)
		for _, stmt := range statements {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("RunCoreMigrations: execute %s: %v", f.Name(), err)
			}
		}
	}
}

// schemaPrefix returns a dialect-appropriate SET/USE statement to prepend
// before migration SQL so the correct schema/database is targeted.
func schemaPrefix(dialect plugin.Dialect, schemaName string) string {
	switch dialect {
	case plugin.DialectPostgres:
		return fmt.Sprintf(`SET search_path TO %s`, quoteIdent(dialect, schemaName))
	case plugin.DialectMySQL:
		return fmt.Sprintf(`USE %s`, quoteIdent(dialect, schemaName))
	default:
		return ""
	}
}

// setSearchPath ensures the connection targets the correct schema/database.
func setSearchPath(ctx context.Context, conn *sql.Conn, dialect plugin.Dialect, schemaName string) error {
	switch dialect {
	case plugin.DialectPostgres:
		_, err := conn.ExecContext(ctx, fmt.Sprintf(`SET search_path TO %s`, quoteIdent(dialect, schemaName)))
		return err
	case plugin.DialectMySQL:
		_, err := conn.ExecContext(ctx, fmt.Sprintf(`USE %s`, quoteIdent(dialect, schemaName)))
		return err
	default:
		return nil
	}
}

// RunPluginMigrations runs each loaded plugin's database migrations for the
// given dialect. Plugins that do not implement plugin.HasMigrations are
// skipped. The dialect-appropriate SQL is selected:
//
//   - postgres -> Migration.Up
//   - mysql -> Migration.UpMySQL (skipped if empty)
//   - mssql -> Migration.UpMSSQL (skipped if empty)
//
// A tracking table (plugin_migrations) is used so each migration version
// runs only once.
func RunPluginMigrations(t *testing.T, db *sql.DB, dialect plugin.Dialect, loadedPlugins []*plugin.LoadedPlugin) {
	t.Helper()

	// Ensure the tracking table exists.
	createTrackingTable(t, db, dialect)

	ctx := context.Background()
	for _, lp := range loadedPlugins {
		if !lp.Healthy {
			continue
		}
		pm, ok := lp.Plugin.(plugin.HasMigrations)
		if !ok {
			continue
		}

		pluginName := lp.Plugin.Info().Name
		migrations := pm.Migrations()
		sort.Slice(migrations, func(i, j int) bool {
			return migrations[i].Version < migrations[j].Version
		})

		for _, m := range migrations {
			if migrationApplied(t, ctx, db, dialect, pluginName, m.Version) {
				continue
			}

			sqlStr := selectMigrationSQL(dialect, m)
			if sqlStr == "" {
				// Record as applied so it won't block future migrations.
				recordMigration(t, ctx, db, dialect, pluginName, m.Version)
				continue
			}

			if err := execStatements(ctx, db, dialect, sqlStr); err != nil {
				t.Fatalf("RunPluginMigrations: plugin %s v%d: %v", pluginName, m.Version, err)
			}

			recordMigration(t, ctx, db, dialect, pluginName, m.Version)
		}
	}
}

// SeedPluginConfig inserts test configuration rows that plugins may depend on.
// The SQL uses dialect-appropriate placeholder syntax.
func SeedPluginConfig(t *testing.T, db *sql.DB, dialect plugin.Dialect) {
	t.Helper()
	ctx := context.Background()

	defaultTenant := "00000000-0000-0000-0000-000000000000"
	seedRows := []struct {
		table string
		sql   string
		args  []interface{}
	}{
		{
			table: "feature_flags",
			sql:   placeholderSQL(dialect, fmt.Sprintf("INSERT INTO feature_flags (tenant_id, id, %s, name, enabled) VALUES (%%s, %%s, %%s, %%s, %%s)", quoteIdent(dialect, "key"))),
			args:  []interface{}{defaultTenant, "00000000-0000-0000-0000-000000000010", "test-flag", "Test Flag", false},
		},
		{
			table: "kafka_config",
			sql:   placeholderSQL(dialect, "INSERT INTO kafka_config (tenant_id, id, name, brokers, topic, enabled) VALUES (%s, %s, %s, %s, %s, %s)"),
			args:  []interface{}{defaultTenant, "00000000-0000-0000-0000-000000000001", "test-kafka", "localhost:9092", "test-topic", true},
		},
		{
			table: "webhook_config",
			sql:   placeholderSQL(dialect, "INSERT INTO webhook_config (tenant_id, id, url) VALUES (%s, %s, %s)"),
			args:  []interface{}{defaultTenant, "00000000-0000-0000-0000-000000000002", "http://localhost/wh"},
		},
		{
			table: "slack_config",
			sql:   placeholderSQL(dialect, "INSERT INTO slack_config (tenant_id, id, name, webhook_url, enabled) VALUES (%s, %s, %s, %s, %s)"),
			args:  []interface{}{defaultTenant, "00000000-0000-0000-0000-000000000004", "test-slack", "https://hooks.slack.com/test", true},
		},
		{
			table: "pd_config",
			sql:   placeholderSQL(dialect, "INSERT INTO pd_config (tenant_id, id, name, routing_key, enabled) VALUES (%s, %s, %s, %s, %s)"),
			args:  []interface{}{defaultTenant, "00000000-0000-0000-0000-000000000003", "test-pd", "test-routing-key", true},
		},
	}

	for _, row := range seedRows {
		// Attempt the insert. If the table doesn't exist yet (the plugin
		// migration hasn't run), log a warning and continue.
		if _, err := db.ExecContext(ctx, row.sql, row.args...); err != nil {
			t.Logf("SeedPluginConfig: %s: %v (plugin migrations may not have run yet)", row.table, err)
		}
	}
}

// CleanupTestDB drops the schema/database created by OpenTestDB and closes
// the database connection.
func CleanupTestDB(t *testing.T, db *sql.DB, dialect plugin.Dialect, schemaName string) {
	t.Helper()

	if db == nil {
		return
	}
	defer db.Close()

	ctx := context.Background()
	switch dialect {
	case plugin.DialectPostgres:
		if schemaName == "public" {
			// Never drop the public schema.
			break
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, quoteIdent(dialect, schemaName))); err != nil {
			t.Logf("CleanupTestDB: drop schema: %v", err)
		}
	case plugin.DialectMySQL:
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, quoteIdent(dialect, schemaName))); err != nil {
			t.Logf("CleanupTestDB: drop database: %v", err)
		}
	case plugin.DialectMSSQL:
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s`, quoteIdent(dialect, schemaName))); err != nil {
			t.Logf("CleanupTestDB: drop schema (mssql): %v", err)
		}
	}
}

// --------------------------------------------------------------------------
// Internal helpers
// --------------------------------------------------------------------------

// uniqueSchemaName generates a name like "cleat_test_a1b2c3d4".
func uniqueSchemaName() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		// Fallback — should never happen on any real system.
		return fmt.Sprintf("cleat_test_%x", b)
	}
	return fmt.Sprintf("cleat_test_%x", b)
}

// quoteIdent returns a safely quoted identifier for the given dialect.
func quoteIdent(dialect plugin.Dialect, name string) string {
	switch dialect {
	case plugin.DialectPostgres:
		return `"` + name + `"`
	case plugin.DialectMySQL:
		return "`" + name + "`"
	case plugin.DialectMSSQL:
		return `"` + name + `"`
	default:
		return `"` + name + `"`
	}
}

// placeholderSQL replaces %s placeholders with the dialect-appropriate
// parameter marker ($1/$2, ?, or @p1/@p2).
func placeholderSQL(dialect plugin.Dialect, tmpl string) string {
	switch dialect {
	case plugin.DialectPostgres:
		// Replace each %s with $1, $2, etc.
		var result strings.Builder
		// The loop that used to be here scanned every rune to find '%' and did
		// nothing with it -- an `if` with an empty body inside a `for` with no
		// other statements. Removed rather than filled in: the split below has
		// been doing the actual work all along.
		parts := strings.Split(tmpl, "%s")
		for i, p := range parts {
			result.WriteString(p)
			if i < len(parts)-1 {
				result.WriteString(fmt.Sprintf("$%d", i+1))
			}
		}
		return result.String()
	case plugin.DialectMySQL:
		return strings.ReplaceAll(tmpl, "%s", "?")
	case plugin.DialectMSSQL:
		parts := strings.Split(tmpl, "%s")
		var result strings.Builder
		for i, p := range parts {
			result.WriteString(p)
			if i < len(parts)-1 {
				result.WriteString(fmt.Sprintf("@p%d", i+1))
			}
		}
		return result.String()
	default:
		return strings.ReplaceAll(tmpl, "%s", "?")
	}
}

// coreMigrationDir locates the migration directory for the given dialect.
// It tries common paths since the working directory varies across test runners.
func coreMigrationDir(t *testing.T, dialect plugin.Dialect) string {
	t.Helper()

	sub := string(dialect)
	candidates := []string{
		filepath.Join("..", "..", "migrations", sub),
		filepath.Join("migrations", sub),
	}

	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}
	return ""
}

// createTrackingTable creates the plugin_migrations tracking table.
func createTrackingTable(t *testing.T, db *sql.DB, dialect plugin.Dialect) {
	t.Helper()
	ctx := context.Background()

	var ddl string
	switch dialect {
	case plugin.DialectPostgres:
		ddl = `CREATE TABLE IF NOT EXISTS plugin_migrations (
			plugin_name TEXT NOT NULL,
			version INTEGER NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (plugin_name, version)
		)`
	case plugin.DialectMySQL:
		ddl = `CREATE TABLE IF NOT EXISTS plugin_migrations (
			plugin_name VARCHAR(255) NOT NULL,
			version INTEGER NOT NULL,
			applied_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
			PRIMARY KEY (plugin_name, version)
		)`
	case plugin.DialectMSSQL:
		ddl = `IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'plugin_migrations')
			CREATE TABLE plugin_migrations (
				plugin_name NVARCHAR(255) NOT NULL,
				version INTEGER NOT NULL,
				applied_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
				PRIMARY KEY (plugin_name, version)
			)`
	default:
		t.Fatalf("createTrackingTable: unknown dialect: %s", dialect)
	}

	if _, err := db.ExecContext(ctx, ddl); err != nil {
		t.Fatalf("createTrackingTable: %v", err)
	}
}

// migrationApplied checks whether a plugin migration version has been recorded.
func migrationApplied(t *testing.T, ctx context.Context, db *sql.DB, dialect plugin.Dialect, pluginName string, version int) bool {
	t.Helper()

	var checkSQL string
	switch dialect {
	case plugin.DialectPostgres:
		checkSQL = `SELECT EXISTS(SELECT 1 FROM plugin_migrations WHERE plugin_name = $1 AND version = $2)`
	case plugin.DialectMySQL:
		checkSQL = `SELECT EXISTS(SELECT 1 FROM plugin_migrations WHERE plugin_name = ? AND version = ?)`
	case plugin.DialectMSSQL:
		checkSQL = `SELECT CASE WHEN EXISTS(SELECT 1 FROM plugin_migrations WHERE plugin_name = @p1 AND version = @p2) THEN 1 ELSE 0 END`
	default:
		t.Fatalf("migrationApplied: unknown dialect: %s", dialect)
	}

	var exists bool
	err := db.QueryRowContext(ctx, checkSQL, pluginName, version).Scan(&exists)
	if err != nil {
		t.Fatalf("migrationApplied: check %s v%d: %v", pluginName, version, err)
	}
	return exists
}

// recordMigration records a plugin migration as applied.
func recordMigration(t *testing.T, ctx context.Context, db *sql.DB, dialect plugin.Dialect, pluginName string, version int) {
	t.Helper()

	var insertSQL string
	switch dialect {
	case plugin.DialectPostgres:
		insertSQL = `INSERT INTO plugin_migrations (plugin_name, version) VALUES ($1, $2)`
	case plugin.DialectMySQL:
		insertSQL = `INSERT INTO plugin_migrations (plugin_name, version) VALUES (?, ?)`
	case plugin.DialectMSSQL:
		insertSQL = `INSERT INTO plugin_migrations (plugin_name, version) VALUES (@p1, @p2)`
	default:
		t.Fatalf("recordMigration: unknown dialect: %s", dialect)
	}

	if _, err := db.ExecContext(ctx, insertSQL, pluginName, version); err != nil {
		t.Fatalf("recordMigration: insert %s v%d: %v", pluginName, version, err)
	}
}

// selectMigrationSQL returns the dialect-appropriate SQL for a migration.
// Returns "" if no dialect-specific SQL is available and the dialect is
// not PostgreSQL (Up is always used for PostgreSQL).
func selectMigrationSQL(dialect plugin.Dialect, m plugin.Migration) string {
	switch dialect {
	case plugin.DialectPostgres:
		return m.Up
	case plugin.DialectMySQL:
		return m.UpMySQL
	case plugin.DialectMSSQL:
		return m.UpMSSQL
	default:
		return m.Up
	}
}

// splitStatements splits a migration file's SQL text into individual
// statements to execute, using dialect-appropriate rules.
//
// Both MySQL and MSSQL delegate to the production splitters in
// migration.Runner, and for the same reason: this harness used to carry its
// own copy for each, and each copy was wrong in the dialect's own way.
//
// MySQL: the local copy did not understand the DELIMITER directive, so it cut
// migrations/mysql/003_procedures.sql's stored procedure body on the
// semicolons inside it and sent the fragments -- plus the bare word DELIMITER,
// which no MySQL server accepts -- to the server. Error 1064 near 'DELIMITER //'.
//
// MSSQL: the local copy split on every semicolon rather than only on GO batch
// separators, which broke migrations/mssql/003_procedures.sql's CREATE OR
// ALTER PROCEDURE into fragments the server rejected with "Incorrect syntax
// near 'ON'". That failure was invisible until the MySQL one above was fixed,
// because the mssql arm never got that far.
//
// GO is the only batch separator MSSQL recognises; everything else inside a
// batch, semicolons included, is the server's job to parse as one unit. That
// is the MSSQL analogue of what DELIMITER guards against in MySQL, and the
// production splitters already got both right (migration/runner.go,
// migration/split_test.go). The harness only needed to call them instead of
// maintaining two divergent copies.
//
// PostgreSQL keeps using the local splitSQL below, which additionally
// understands PostgreSQL's dollar-quoted strings. Production's
// migration.Runner does not split PostgreSQL SQL at all -- it executes the
// whole file in one statement, relying on the driver's native multi-statement
// support -- so it is not a drop-in replacement for what this harness needs.
func splitStatements(dialect plugin.Dialect, sql string) []string {
	switch dialect {
	case plugin.DialectMySQL:
		return migration.SplitSQL(sql)
	case plugin.DialectMSSQL:
		return migration.SplitMSSQL(sql)
	}
	return splitSQL(sql)
}

// splitSQL splits SQL text on semicolons, discarding empty fragments.
func splitSQL(sql string) []string {
	// Split on semicolons, but skip semicolons that appear inside
	// dollar-quoted strings (Postgres $$...$$ or $tag$...$tag$)
	// or inside SQL line comments (-- ... up to end of line).
	// Also handle MSSQL GO batch separators (GO on its own line).
	var stmts []string
	var buf strings.Builder
	inDollar := false
	dollarTag := ""
	i := 0
	n := len(sql)

	// isGO reports whether the text starting at i and ending at j (exclusive)
	// is a MSSQL GO batch separator (case-insensitive, on its own line).
	isGO := func(start, end int) bool {
		word := strings.TrimSpace(sql[start:end])
		return strings.EqualFold(word, "GO") || strings.EqualFold(word, "GO\n") || strings.EqualFold(word, "GO\r")
	}

	for i < n {
		c := sql[i]

		// Skip SQL line comments (-- to end of line).
		if !inDollar && c == '-' && i+1 < n && sql[i+1] == '-' {
			for i < n && sql[i] != '\n' && sql[i] != '\r' {
				i++
			}
			continue
		}

		// Split on newline-bounded GO batch separator (MSSQL).
		if !inDollar && c == '\n' {
			j := i + 1
			for j < n && (sql[j] == ' ' || sql[j] == '\t') {
				j++
			}
			k := j
			for k < n && sql[k] != '\n' && sql[k] != '\r' && sql[k] != ';' {
				k++
			}
			if isGO(j, k) {
				// Emit any buffered statement before the GO.
				trimmed := strings.TrimSpace(buf.String())
				if trimmed != "" {
					stmts = append(stmts, trimmed)
				}
				buf.Reset()
				i = k
				continue
			}
			buf.WriteByte(c)
			i++
			continue
		}

		// Detect start of dollar-quoted string.
		if !inDollar && c == '$' {
			// Find the matching closing $ to form the tag.
			j := i + 1
			for j < n && sql[j] != '$' {
				j++
			}
			if j < n {
				// Found closing $ — this is a dollar-quote tag.
				tag := sql[i : j+1]
				buf.WriteString(tag)
				i = j + 1
				inDollar = true
				dollarTag = tag
				continue
			}
		}

		// Detect end of dollar-quoted string.
		if inDollar && strings.HasPrefix(sql[i:], dollarTag) {
			buf.WriteString(dollarTag)
			i += len(dollarTag)
			inDollar = false
			dollarTag = ""
			continue
		}

		// Statement separator — only outside dollar quotes.
		if !inDollar && c == ';' {
			trimmed := strings.TrimSpace(buf.String())
			if trimmed != "" {
				stmts = append(stmts, trimmed)
			}
			buf.Reset()
			i++
			continue
		}

		buf.WriteByte(c)
		i++
	}

	trimmed := strings.TrimSpace(buf.String())
	if trimmed != "" {
		stmts = append(stmts, trimmed)
	}
	return stmts
}

func execStatements(ctx context.Context, db *sql.DB, dialect plugin.Dialect, sqlStr string) error {
	for _, stmt := range splitStatements(dialect, sqlStr) {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}
