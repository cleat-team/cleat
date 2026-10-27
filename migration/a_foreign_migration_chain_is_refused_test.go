package migration_test

// cleat#3405, owner decision D18: 0.5.0 requires a fresh database and refuses
// one built by a different migration chain, before changing anything.
//
// Every case runs on all three dialects, against a two-file chain held in
// memory, so what is being tested is the runner's bookkeeping and nothing
// about the real schema. The cases that refuse also take a fingerprint of the
// database before and after -- every column of every table, and every row --
// because "refused" is not enough: a refusal that came after the CREATE or
// ALTER in ensureMigrationsTable would still report the right error, on a
// database it had already changed.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/cleat-team/cleat/migration"
)

const (
	chainFileA = "CREATE TABLE chain_a (id INT)"
	chainFileB = "CREATE TABLE chain_b (id INT)"
)

func chainFS(d migration.Dialect, files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[string(d)+"/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

func twoFileChain(d migration.Dialect) fstest.MapFS {
	return chainFS(d, map[string]string{"001_a.sql": chainFileA, "002_b.sql": chainFileB})
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

type chainDialect struct {
	d    migration.Dialect
	open func(t *testing.T, name string) *sql.DB
	// oldTrackingTable is schema_migrations as every cleat before cleat#3405
	// created it: no identity columns.
	oldTrackingTable string
	// placeholder for a one-argument statement.
	ph string
}

func chainDialects() []chainDialect {
	other := func(c otherDialect) func(t *testing.T, name string) *sql.DB {
		return func(t *testing.T, name string) *sql.DB {
			t.Helper()
			if os.Getenv(c.env) == "" {
				t.Skipf("%s not set, skipping %s", c.env, c.d)
			}
			db, err := sql.Open(c.driver, scratchDSN(t, c, name))
			if err != nil {
				t.Fatalf("open scratch database: %v", err)
			}
			t.Cleanup(func() { db.Close() })
			return db
		}
	}
	return []chainDialect{
		{migration.DialectPostgres, newScratchDB,
			`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`, "$1"},
		{migration.DialectMySQL, other(otherDialect{"CLEAT_TEST_MYSQL", "mysql", migration.DialectMySQL}),
			`CREATE TABLE schema_migrations (version VARCHAR(255) PRIMARY KEY, applied_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6))`, "?"},
		{migration.DialectMSSQL, other(otherDialect{"CLEAT_TEST_MSSQL", "sqlserver", migration.DialectMSSQL}),
			`CREATE TABLE schema_migrations (version NVARCHAR(255) PRIMARY KEY, applied_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME())`, "@p1"},
	}
}

// fingerprint is every column of every table in the scratch database and every
// row of every table, as text, in a fixed order.
func fingerprint(t *testing.T, db *sql.DB, d migration.Dialect) string {
	t.Helper()
	var colsQ string
	switch d {
	case migration.DialectPostgres:
		colsQ = `SELECT table_name, column_name, data_type FROM information_schema.columns WHERE table_schema = current_schema()`
	case migration.DialectMySQL:
		colsQ = `SELECT table_name, column_name, data_type FROM information_schema.columns WHERE table_schema = DATABASE()`
	case migration.DialectMSSQL:
		colsQ = `SELECT TABLE_NAME, COLUMN_NAME, DATA_TYPE FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = 'dbo'`
	}
	rows, err := db.Query(colsQ)
	if err != nil {
		t.Fatalf("fingerprint: list columns: %v", err)
	}
	var cols []string
	tables := map[string]bool{}
	for rows.Next() {
		var tbl, col, typ string
		if err := rows.Scan(&tbl, &col, &typ); err != nil {
			t.Fatalf("fingerprint: scan column: %v", err)
		}
		cols = append(cols, tbl+"."+col+" "+typ)
		tables[tbl] = true
	}
	rows.Close()
	sort.Strings(cols)

	var names []string
	for tbl := range tables {
		names = append(names, tbl)
	}
	sort.Strings(names)
	var data []string
	for _, tbl := range names {
		r, err := db.Query("SELECT * FROM " + tbl)
		if err != nil {
			t.Fatalf("fingerprint: read %s: %v", tbl, err)
		}
		n, _ := r.Columns()
		var lines []string
		for r.Next() {
			vals := make([]sql.NullString, len(n))
			ptrs := make([]any, len(n))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := r.Scan(ptrs...); err != nil {
				t.Fatalf("fingerprint: scan %s: %v", tbl, err)
			}
			parts := make([]string, len(vals))
			for i, v := range vals {
				parts[i] = fmt.Sprintf("%v", v)
			}
			lines = append(lines, tbl+": "+strings.Join(parts, "|"))
		}
		r.Close()
		sort.Strings(lines)
		data = append(data, lines...)
	}
	return strings.Join(cols, "\n") + "\n--\n" + strings.Join(data, "\n")
}

func chainRunner(db *sql.DB, d migration.Dialect, fsys fstest.MapFS) *migration.Runner {
	return migration.NewRunner(db, d, "unused").WithFS(fsys).WithLockTimeout(10 * time.Second)
}

// requireForeign asserts err is the foreign-chain refusal and carries the
// marker and every given fragment.
func requireForeign(t *testing.T, what string, err error, fragments ...string) {
	t.Helper()
	if !migration.IsForeignChain(err) {
		t.Fatalf("%s: err = %v, want a ForeignChainError", what, err)
	}
	for _, f := range append([]string{migration.ForeignChainMarker}, fragments...) {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("%s: the refusal does not say %q:\n%v", what, f, err)
		}
	}
}

func scratchName(d migration.Dialect, label string) string {
	return fmt.Sprintf("chain_%s_%s_%d", d, label, time.Now().UnixNano()%1e9)
}

func TestAFreshDatabaseRecordsEachMigrationsNameAndChecksum(t *testing.T) {
	for _, c := range chainDialects() {
		t.Run(string(c.d), func(t *testing.T) {
			ctx := context.Background()
			db := c.open(t, scratchName(c.d, "fresh"))
			r := chainRunner(db, c.d, twoFileChain(c.d))
			if err := runMigrations(t, ctx, r, c.d); err != nil {
				t.Fatalf("Run on a fresh database: %v", err)
			}
			want := map[string][2]string{"1": {"001_a.sql", sha(chainFileA)}, "2": {"002_b.sql", sha(chainFileB)}}
			rows, err := db.Query("SELECT version, name, checksum FROM schema_migrations")
			if err != nil {
				t.Fatal(err)
			}
			got := 0
			for rows.Next() {
				var v string
				var name, sum sql.NullString
				if err := rows.Scan(&v, &name, &sum); err != nil {
					t.Fatal(err)
				}
				got++
				w, ok := want[v]
				if !ok || name.String != w[0] || strings.TrimSpace(sum.String) != w[1] {
					t.Errorf("version %s recorded (%v, %v), want %v", v, name, sum, w)
				}
			}
			rows.Close()
			if got != 2 {
				t.Errorf("recorded %d versions, want 2", got)
			}

			// The second boot: nothing to apply, nothing to refuse.
			before := fingerprint(t, db, c.d)
			if err := runMigrations(t, ctx, r, c.d); err != nil {
				t.Fatalf("second Run: %v", err)
			}
			if err := r.CheckChain(ctx); err != nil {
				t.Fatalf("CheckChain after the first run: %v", err)
			}
			st, err := r.Verify(ctx)
			if err != nil || st.Behind() || len(st.Ahead) != 0 {
				t.Fatalf("Verify after the first run = %+v, %v; want current", st, err)
			}
			if after := fingerprint(t, db, c.d); after != before {
				t.Errorf("the second Run changed the database:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// The v0.4.0 shape: schema_migrations has no identity columns and records more
// versions than this chain ships, under the same low numbers.
func TestADatabaseFromAnEarlierChainIsRefusedAndLeftUnchanged(t *testing.T) {
	for _, c := range chainDialects() {
		t.Run(string(c.d), func(t *testing.T) {
			ctx := context.Background()
			db := c.open(t, scratchName(c.d, "old"))
			if _, err := db.Exec(c.oldTrackingTable); err != nil {
				t.Fatalf("create the old tracking table: %v", err)
			}
			for _, v := range []string{"1", "2", "3", "14"} {
				if _, err := db.Exec("INSERT INTO schema_migrations (version) VALUES ("+c.ph+")", v); err != nil {
					t.Fatalf("record version %s: %v", v, err)
				}
			}
			if _, err := db.Exec("CREATE TABLE workflow_instances (id INT)"); err != nil {
				t.Fatalf("create a table the earlier chain owns: %v", err)
			}
			before := fingerprint(t, db, c.d)
			r := chainRunner(db, c.d, twoFileChain(c.d))

			requireForeign(t, "Run", runMigrations(t, ctx, r, c.d), "version 1: database has no identity",
				"this binary ships 001_a.sql ("+sha(chainFileA)+")", "version 14", "no file with this number")
			_, verr := r.Verify(ctx)
			requireForeign(t, "Verify", verr, "version 2")
			requireForeign(t, "CheckChain", r.CheckChain(ctx))

			if after := fingerprint(t, db, c.d); after != before {
				t.Errorf("a refused run changed the database:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

func TestARecordedVersionWithNoIdentityIsRefused(t *testing.T) {
	for _, c := range chainDialects() {
		t.Run(string(c.d), func(t *testing.T) {
			ctx := context.Background()
			db := c.open(t, scratchName(c.d, "null"))
			r := chainRunner(db, c.d, twoFileChain(c.d))
			if err := runMigrations(t, ctx, r, c.d); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if _, err := db.Exec("UPDATE schema_migrations SET checksum = NULL WHERE version = "+c.ph, "2"); err != nil {
				t.Fatal(err)
			}
			before := fingerprint(t, db, c.d)
			requireForeign(t, "Run", runMigrations(t, ctx, r, c.d), "version 2: database has no identity (name or checksum is NULL)")
			if after := fingerprint(t, db, c.d); after != before {
				t.Errorf("a refused run changed the database:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// The same version number, a different file: what the database recorded is
// not what this binary ships.
func TestATamperedChecksumIsRefusedNamingBothSides(t *testing.T) {
	for _, c := range chainDialects() {
		t.Run(string(c.d), func(t *testing.T) {
			ctx := context.Background()
			db := c.open(t, scratchName(c.d, "tamper"))
			if err := runMigrations(t, ctx, chainRunner(db, c.d, twoFileChain(c.d)), c.d); err != nil {
				t.Fatalf("Run: %v", err)
			}
			const edited = chainFileB + " -- edited"
			other := chainRunner(db, c.d, chainFS(c.d, map[string]string{"001_a.sql": chainFileA, "002_b.sql": edited}))
			before := fingerprint(t, db, c.d)
			requireForeign(t, "Run", runMigrations(t, ctx, other, c.d),
				"version 2: database has 002_b.sql ("+sha(chainFileB)+"); this binary ships 002_b.sql ("+sha(edited)+")")
			if err := other.CheckChain(ctx); !migration.IsForeignChain(err) {
				t.Errorf("CheckChain = %v, want a ForeignChainError", err)
			}
			if after := fingerprint(t, db, c.d); after != before {
				t.Errorf("a refused run changed the database:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// cleat#2117's ahead case: an N binary on a database the N+1 deploy step
// migrated. The later file carries an identity this binary cannot check, and
// it starts.
func TestALaterChainsVersionWithAnIdentityIsAllowed(t *testing.T) {
	for _, c := range chainDialects() {
		t.Run(string(c.d), func(t *testing.T) {
			ctx := context.Background()
			db := c.open(t, scratchName(c.d, "ahead"))
			if err := runMigrations(t, ctx, chainRunner(db, c.d, twoFileChain(c.d)), c.d); err != nil {
				t.Fatalf("Run: %v", err)
			}
			older := chainRunner(db, c.d, chainFS(c.d, map[string]string{"001_a.sql": chainFileA}))
			if err := older.CheckChain(ctx); err != nil {
				t.Fatalf("CheckChain from the older binary = %v, want nil", err)
			}
			st, err := older.Verify(ctx)
			if err != nil || len(st.Ahead) != 1 || st.Ahead[0] != 2 {
				t.Fatalf("Verify from the older binary = %+v, %v; want ahead by version 2", st, err)
			}
		})
	}
}

// A tracking table from before cleat#3405 that records nothing is not a chain:
// Run adds the identity columns and migrates.
func TestAnEmptyTrackingTableFromBeforeIdentityIsMigrated(t *testing.T) {
	for _, c := range chainDialects() {
		t.Run(string(c.d), func(t *testing.T) {
			ctx := context.Background()
			db := c.open(t, scratchName(c.d, "empty"))
			if _, err := db.Exec(c.oldTrackingTable); err != nil {
				t.Fatal(err)
			}
			r := chainRunner(db, c.d, twoFileChain(c.d))
			if err := runMigrations(t, ctx, r, c.d); err != nil {
				t.Fatalf("Run: %v", err)
			}
			var n int
			if err := db.QueryRow("SELECT count(*) FROM schema_migrations WHERE checksum IS NOT NULL").Scan(&n); err != nil {
				t.Fatalf("read the identity columns Run added: %v", err)
			}
			if n != 2 {
				t.Errorf("%d versions carry a checksum, want 2", n)
			}
		})
	}
}
