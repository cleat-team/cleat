package migration

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/microsoft/go-mssqldb"

	"github.com/cleat-team/cleat/engine"
)

// 010_idempotency_keys_tenant_id.sql is a migration whose entire point is what
// it does to a database that already has rows in it, so testing it against a
// fresh schema would test the half that does not matter.
//
// IMPROVEMENT-PLAN 3.10 records two candidate fixes for the global
// idempotency key. Folding the tenant into the sha256 needs no migration and
// is one line per dialect -- and silently invalidates every key already
// stored, so the first retry after an upgrade starts a second workflow.
// Adding a tenant_id column keyed alongside key_hash keeps those rows
// matching, provided existing rows land on the tenant a single-tenant
// deployment already writes under. That proviso is the assertion below.
//
// The runner is driven over a staged directory holding one file, then two, so
// the "before" state is the real 001_schema.sql rather than a hand-written
// approximation of it -- the failure mode of engine/testutil's schema copies
// (IMPROVEMENT-PLAN 1.9, 2.60b) is precisely a test fixture that agrees with
// nothing shipped.
func TestIdempotencyTenantMigrationPreservesExistingKeys(t *testing.T) {
	for _, d := range []idempotencyDialect{postgresDialect(), mysqlDialect(), mssqlDialect()} {
		d := d
		t.Run(string(d.dialect), func(t *testing.T) {
			if d.skipReason != "" {
				t.Skip(d.skipReason)
			}
			db := d.scratchDB(t)
			ctx := context.Background()

			// Before: the schema as it shipped, with no tenant_id on
			// idempotency_keys at all.
			before := stageMigrations(t, d.dialect, "001_schema.sql")
			if err := NewRunner(db, d.dialect, before).Run(ctx); err != nil {
				t.Fatalf("apply 001_schema.sql: %v", err)
			}

			// A key written by the deployment before the upgrade.
			legacyHash := []byte("0123456789abcdef0123456789abcdef")
			if _, err := db.ExecContext(ctx, d.rebind(
				`INSERT INTO idempotency_keys (key_hash, workflow_id, expires_at)
				 VALUES (?, ?, `+d.futureTimestamp+`)`),
				legacyHash, "wf-before-upgrade"); err != nil {
				t.Fatalf("insert pre-upgrade idempotency key: %v", err)
			}

			// After.
			after := stageMigrations(t, d.dialect, "001_schema.sql", "010_idempotency_keys_tenant_id.sql")
			if err := NewRunner(db, d.dialect, after).Run(ctx); err != nil {
				t.Fatalf("apply 010_idempotency_keys_tenant_id.sql: %v", err)
			}

			// 1. The row survived, and it belongs to the default tenant --
			//    which is the tenant every write from a single-tenant
			//    deployment carries, so its keys keep matching.
			var workflowID, tenantID string
			err := db.QueryRowContext(ctx, d.rebind(
				`SELECT workflow_id, `+d.tenantIDText+` FROM idempotency_keys WHERE key_hash = ?`),
				legacyHash).Scan(&workflowID, &tenantID)
			if err != nil {
				t.Fatalf("read the pre-upgrade key back after migrating: %v\n"+
					"A key that does not survive the upgrade means the first retry "+
					"after it starts a second workflow.", err)
			}
			if workflowID != "wf-before-upgrade" {
				t.Errorf("pre-upgrade key resolves to workflow %q, want wf-before-upgrade", workflowID)
			}
			if tenantID != engine.DefaultTenantUUID {
				t.Errorf("pre-upgrade key landed on tenant %q, want the default tenant %q: "+
					"a single-tenant deployment's existing keys no longer match after the upgrade",
					tenantID, engine.DefaultTenantUUID)
			}

			// 2. The same key is now insertable for a second tenant. This is
			//    the defect itself: before 010 the primary key was key_hash
			//    alone, so tenant B's "order-123" was tenant A's row.
			const otherTenant = "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb"
			if _, err := db.ExecContext(ctx, d.rebind(
				`INSERT INTO idempotency_keys (key_hash, workflow_id, expires_at, tenant_id)
				 VALUES (?, ?, `+d.futureTimestamp+`, ?)`),
				legacyHash, "wf-other-tenant", otherTenant); err != nil {
				t.Fatalf("a second tenant cannot hold the same idempotency key: %v", err)
			}

			// 3. And within one tenant the key still deduplicates, which is
			//    the property the whole table exists for.
			if _, err := db.ExecContext(ctx, d.rebind(
				`INSERT INTO idempotency_keys (key_hash, workflow_id, expires_at, tenant_id)
				 VALUES (?, ?, `+d.futureTimestamp+`, ?)`),
				legacyHash, "wf-duplicate", otherTenant); err == nil {
				t.Error("the same key was inserted twice for one tenant: the composite " +
					"primary key is not enforcing uniqueness within a tenant")
			}
		})
	}
}

// idempotencyDialect carries the per-dialect knowledge the test above needs:
// how to get an empty database, and the three places where the SQL genuinely
// differs.
type idempotencyDialect struct {
	dialect engine.Dialect
	// scratchDB returns a handle to an empty database, skipping the subtest
	// when this dialect is not configured.
	scratchDB func(t *testing.T) *sql.DB
	// rebind rewrites ? placeholders into the dialect's own form.
	rebind func(string) string
	// futureTimestamp is an expires_at expression comfortably in the future.
	futureTimestamp string
	// tenantIDText selects tenant_id as a string; SQL Server's
	// UNIQUEIDENTIFIER scans as a byte slice with reordered fields otherwise.
	tenantIDText string
	// skipReason, when set, skips this dialect with an explanation.
	skipReason string
}

func postgresDialect() idempotencyDialect {
	return idempotencyDialect{
		dialect:         engine.DialectPostgres,
		scratchDB:       func(t *testing.T) *sql.DB { return newScratchDB(t, "cleat_migration_idem_tenant_test") },
		rebind:          rebindNumbered,
		futureTimestamp: "now() + INTERVAL '1 day'",
		tenantIDText:    "tenant_id::text",
	}
}

func mysqlDialect() idempotencyDialect {
	return idempotencyDialect{
		dialect:         engine.DialectMySQL,
		scratchDB:       newMySQLScratchDB,
		rebind:          func(q string) string { return q },
		futureTimestamp: "DATE_ADD(NOW(6), INTERVAL 1 DAY)",
		tenantIDText:    "tenant_id",

		// The Runner cannot apply migrations/mysql/001_schema.sql at all, so
		// there is no "before" state to migrate from. splitSQL splits the file
		// on every ';' with no regard for what it is inside, and line 7 of that
		// file is the comment
		//
		//	-- CREATE INDEX has no IF NOT EXISTS in MySQL 8.0; re-runs error harmlessly.
		//
		// which becomes a statement reading "re-runs error harmlessly." and
		// fails with Error 1064. Every cleat-worker runs this Runner at boot
		// and exits on failure, so this is not only a test-harness problem:
		// see IMPROVEMENT-PLAN 3.13, which is where this skip is removed.
		//
		// The MySQL half of 010 is still exercised behaviourally by
		// engine.TestIdempotencyKeyIsScopedToTenant, via the equivalent
		// statements in engine/testutil's MySQL schema. What is untested here
		// is the shipped .sql file, which is the gap 3.13 closes.
		skipReason: "blocked on IMPROVEMENT-PLAN 3.13: the Runner cannot parse " +
			"migrations/mysql/001_schema.sql (splitSQL cuts a comment on its embedded ';')",
	}
}

func mssqlDialect() idempotencyDialect {
	return idempotencyDialect{
		dialect:         engine.DialectMSSQL,
		scratchDB:       newMSSQLScratchDB,
		rebind:          rebindAtP,
		futureTimestamp: "DATEADD(DAY, 1, SYSUTCDATETIME())",
		tenantIDText:    "LOWER(CONVERT(NVARCHAR(36), tenant_id))",
	}
}

func rebindNumbered(q string) string {
	var b []byte
	n := 0
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			n++
			b = append(b, []byte(fmt.Sprintf("$%d", n))...)
			continue
		}
		b = append(b, q[i])
	}
	return string(b)
}

func rebindAtP(q string) string {
	var b []byte
	n := 0
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			n++
			b = append(b, []byte(fmt.Sprintf("@p%d", n))...)
			continue
		}
		b = append(b, q[i])
	}
	return string(b)
}

// stageMigrations builds a migrations root containing only the named files for
// one dialect, so a Runner can be pointed at a chosen prefix of the real
// history. The files are the shipped ones, copied rather than rewritten.
func stageMigrations(t *testing.T, dialect engine.Dialect, names ...string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, string(dialect))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("stage migrations: mkdir: %v", err)
	}
	src := filepath.Join(migrationsRoot(t), string(dialect))
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatalf("stage migrations: read %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatalf("stage migrations: write %s: %v", name, err)
		}
	}
	return root
}

const scratchDBName = "cleat_migration_idem_tenant_test"

// newMySQLScratchDB creates an empty MySQL database and returns a handle to it.
//
// Unlike the PostgreSQL helper this skips when CLEAT_TEST_MYSQL is unset,
// matching engine's MySQLBackend.Enabled: there is no default MySQL in CI's
// support matrix, so an unset variable means "not configured here" rather
// than "broken environment".
func newMySQLScratchDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("CLEAT_TEST_MYSQL")
	if dsn == "" {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL migration test")
	}

	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open MySQL admin connection: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Fatalf("configured MySQL is unreachable: %v", err)
	}
	if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + scratchDBName); err != nil {
		t.Fatalf("drop MySQL scratch database: %v", err)
	}
	if _, err := admin.Exec(`CREATE DATABASE ` + scratchDBName); err != nil {
		t.Fatalf("create MySQL scratch database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("mysql", dsn)
		if err != nil {
			return
		}
		defer cleanup.Close()
		if _, err := cleanup.Exec(`DROP DATABASE IF EXISTS ` + scratchDBName); err != nil {
			t.Logf("drop MySQL scratch database: %v", err)
		}
	})

	scratchDSN, err := swapMySQLDB(dsn, scratchDBName)
	if err != nil {
		t.Fatalf("derive MySQL scratch DSN: %v", err)
	}
	db, err := sql.Open("mysql", scratchDSN)
	if err != nil {
		t.Fatalf("open MySQL scratch database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping MySQL scratch database: %v", err)
	}
	return db
}

// swapMySQLDB replaces the database in a go-sql-driver DSN
// (user:pass@tcp(host:port)/dbname?params). Split on the last '/' before the
// query string rather than parsed as a URL, which the DSN is not.
func swapMySQLDB(dsn, name string) (string, error) {
	params := ""
	if i := lastIndexByte(dsn, '?'); i >= 0 {
		params = dsn[i:]
		dsn = dsn[:i]
	}
	i := lastIndexByte(dsn, '/')
	if i < 0 {
		return "", fmt.Errorf("no database component in MySQL DSN")
	}
	return dsn[:i+1] + name + params, nil
}

func lastIndexByte(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// newMSSQLScratchDB creates an empty SQL Server database and returns a handle
// to it. Skips when CLEAT_TEST_MSSQL is unset, for the reason in
// newMySQLScratchDB.
func newMSSQLScratchDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("CLEAT_TEST_MSSQL")
	if dsn == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server migration test")
	}

	masterDSN, err := swapMSSQLDB(dsn, "master")
	if err != nil {
		t.Fatalf("derive SQL Server master DSN: %v", err)
	}
	admin, err := sql.Open("sqlserver", masterDSN)
	if err != nil {
		t.Fatalf("open SQL Server admin connection: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Fatalf("configured SQL Server is unreachable: %v", err)
	}
	dropMSSQLScratchDB(t, admin)
	if _, err := admin.Exec(`CREATE DATABASE ` + scratchDBName); err != nil {
		t.Fatalf("create SQL Server scratch database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("sqlserver", masterDSN)
		if err != nil {
			return
		}
		defer cleanup.Close()
		dropMSSQLScratchDB(t, cleanup)
	})

	scratchDSN, err := swapMSSQLDB(dsn, scratchDBName)
	if err != nil {
		t.Fatalf("derive SQL Server scratch DSN: %v", err)
	}
	db, err := sql.Open("sqlserver", scratchDSN)
	if err != nil {
		t.Fatalf("open SQL Server scratch database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping SQL Server scratch database: %v", err)
	}
	return db
}

// dropMSSQLScratchDB drops the scratch database, evicting whatever a killed
// previous run left connected: SQL Server refuses to drop a database that has
// sessions on it, and SINGLE_USER WITH ROLLBACK IMMEDIATE is how you take them
// off it.
func dropMSSQLScratchDB(t *testing.T, admin *sql.DB) {
	t.Helper()
	_, _ = admin.Exec(`IF DB_ID('` + scratchDBName + `') IS NOT NULL
		ALTER DATABASE ` + scratchDBName + ` SET SINGLE_USER WITH ROLLBACK IMMEDIATE`)
	if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + scratchDBName); err != nil {
		t.Logf("drop SQL Server scratch database: %v", err)
	}
}

// swapMSSQLDB replaces the database query parameter in a sqlserver:// URL.
func swapMSSQLDB(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("database", name)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
