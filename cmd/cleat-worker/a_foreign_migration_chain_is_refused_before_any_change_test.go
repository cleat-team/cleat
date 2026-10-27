package main

// cleat#3405, owner decision D18: cleat 0.5.0 requires a fresh database, and a
// worker pointed at one built by an earlier chain refuses -- at --migrate-only
// and at boot -- before it changes anything.
//
// migration/a_foreign_migration_chain_is_refused_test.go proves the runner
// refuses before its own DDL. This proves the PROCESS does: the real binary,
// every plugin linked, against a database shaped the way a v0.4.0 one is to
// the check (a schema_migrations table with no identity columns, recording
// versions that overlap ours). The fingerprint is taken across the whole
// database -- schemas, tables, columns, indexes, routines, and on MySQL the set
// of databases on the server -- so it would also catch startup code that ran
// before the check, which is the case a runner-level test cannot see.
//
// The end-to-end v0.4.0 path, seeded by the published v0.4.0 worker, is
// scripts/upgrade_from_v040.py's CI job; it matches on the same marker.

import (
	"database/sql"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/migration"
)

// wholeDatabaseFingerprint is every catalog fact a startup could change, plus
// every schema_migrations row, as sorted text.
func wholeDatabaseFingerprint(t *testing.T, db *sql.DB, dialect string) string {
	t.Helper()
	var queries []string
	switch dialect {
	case "postgres":
		queries = []string{
			`SELECT nspname FROM pg_namespace WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'`,
			`SELECT n.nspname || '.' || c.relname || ':' || c.relkind::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			  WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'`,
			`SELECT table_schema || '.' || table_name || '.' || column_name || ':' || data_type FROM information_schema.columns
			  WHERE table_schema NOT IN ('pg_catalog', 'information_schema')`,
			`SELECT n.nspname || '.' || p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
			  WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'`,
			`SELECT extname FROM pg_extension`,
		}
	case "mysql":
		queries = []string{
			`SELECT schema_name FROM information_schema.schemata`,
			`SELECT CONCAT(table_name, ':', table_type) FROM information_schema.tables WHERE table_schema = DATABASE()`,
			`SELECT CONCAT(table_name, '.', column_name, ':', column_type) FROM information_schema.columns WHERE table_schema = DATABASE()`,
			`SELECT CONCAT(table_name, '.', index_name, '.', column_name) FROM information_schema.statistics WHERE table_schema = DATABASE()`,
			`SELECT CONCAT(routine_type, ':', routine_name) FROM information_schema.routines WHERE routine_schema = DATABASE()`,
		}
	default:
		queries = []string{
			`SELECT CONCAT(s.name COLLATE DATABASE_DEFAULT, '.', o.name COLLATE DATABASE_DEFAULT, ':', o.type COLLATE DATABASE_DEFAULT) FROM sys.objects o JOIN sys.schemas s ON s.schema_id = o.schema_id WHERE o.is_ms_shipped = 0`,
			`SELECT CONCAT(OBJECT_NAME(c.object_id) COLLATE DATABASE_DEFAULT, '.', c.name COLLATE DATABASE_DEFAULT) FROM sys.columns c JOIN sys.objects o ON o.object_id = c.object_id WHERE o.is_ms_shipped = 0`,
			`SELECT CONCAT(OBJECT_NAME(i.object_id) COLLATE DATABASE_DEFAULT, '.', ISNULL(i.name COLLATE DATABASE_DEFAULT, '<heap>')) FROM sys.indexes i JOIN sys.objects o ON o.object_id = i.object_id WHERE o.is_ms_shipped = 0`,
			`SELECT name FROM sys.schemas`,
		}
	}
	queries = append(queries, `SELECT version FROM schema_migrations`)
	var out []string
	for _, q := range queries {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("fingerprint: %s: %v", q, err)
		}
		var section []string
		for rows.Next() {
			var s sql.NullString
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("fingerprint: scan: %v", err)
			}
			section = append(section, s.String)
		}
		rows.Close()
		sort.Strings(section)
		out = append(out, strings.Join(section, "\n"))
	}
	return strings.Join(out, "\n--\n")
}

// seedAnEarlierChain shapes an empty database the way a pre-identity cleat
// left it, as far as the chain check can see: schema_migrations without the
// identity columns, recording v0.4.0's version numbers, beside a table that
// chain owned.
func seedAnEarlierChain(t *testing.T, db *sql.DB, dialect string) {
	t.Helper()
	tracking := map[string]string{
		"postgres": `CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		"mysql":    `CREATE TABLE schema_migrations (version VARCHAR(255) PRIMARY KEY, applied_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6))`,
		"mssql":    `CREATE TABLE schema_migrations (version NVARCHAR(255) PRIMARY KEY, applied_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME())`,
	}[dialect]
	if _, err := db.Exec(tracking); err != nil {
		t.Fatalf("create the earlier chain's tracking table: %v", err)
	}
	ins := map[string]string{
		"postgres": `INSERT INTO schema_migrations (version) VALUES ($1)`,
		"mysql":    `INSERT INTO schema_migrations (version) VALUES (?)`,
		"mssql":    `INSERT INTO schema_migrations (version) VALUES (@p1)`,
	}[dialect]
	for v := 1; v <= 14; v++ {
		if _, err := db.Exec(ins, fmt.Sprint(v)); err != nil {
			t.Fatalf("record version %d: %v", v, err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE workflow_instances (id VARCHAR(64) PRIMARY KEY)`); err != nil {
		t.Fatalf("create a table the earlier chain owned: %v", err)
	}
}

func TestAForeignMigrationChainIsRefusedBeforeAnyChange(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the worker binary")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "cleat-worker")
	build := exec.Command("go", "build", "-o", bin, "./cmd/cleat-worker")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/cleat-worker: %v\n%s", err, out)
	}

	for _, c := range []deployDialect{
		{"postgres", "CLEAT_TEST_POSTGRES", "postgres"},
		{"mysql", "CLEAT_TEST_MYSQL", "mysql"},
		{"mssql", "CLEAT_TEST_MSSQL", "sqlserver"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.admin() == "" {
				t.Skipf("%s not set, skipping %s", c.env, c.name)
			}
			dsn, db := deployScratchNamed(t, c, "chain_3405_")
			seedAnEarlierChain(t, db, c.name)
			before := wholeDatabaseFingerprint(t, db, c.name)
			base := []string{"--driver=" + c.name, "--db=" + dsn}

			for _, mode := range []struct {
				name string
				args []string
			}{
				{"--migrate-only", append(append([]string{}, base...), "--migrate-only")},
				{"boot", base},
				{"--migrate-on-start", append(append([]string{}, base...), "--migrate-on-start")},
			} {
				code, out := runWorker(t, bin, nil, mode.args...)
				if code == 0 {
					t.Fatalf("%s on a database from an earlier chain exited 0; it must refuse.\n%s", mode.name, out)
				}
				for _, want := range []string{migration.ForeignChainMarker, "version 1: database has no identity",
					"001_schema.sql", "docs/operations/upgrading.md"} {
					if !strings.Contains(out, want) {
						t.Errorf("%s: the refusal does not say %q:\n%s", mode.name, want, out)
					}
				}
				if after := wholeDatabaseFingerprint(t, db, c.name); after != before {
					t.Fatalf("%s refused but changed the database:\nbefore:\n%s\nafter:\n%s\nworker output:\n%s", mode.name, before, after, out)
				}
			}
		})
	}
}
