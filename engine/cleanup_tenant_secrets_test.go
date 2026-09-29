package engine

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/internal/tenantctx"
)

// cleat#2228: tenant_secrets was not in any of the three CleanupXTestData
// table lists, so rows seeded under the default tenant (as
// plugins/notifications and plugins/webhookingest do) accumulated across
// local `go test` processes. Each process derives its own random master key
// ring at startup, so a later process inherits an earlier process's
// key_version=1 row whose actual key material differs -- a row present but
// undecryptable. TestResealSecretsCommandConvergesAndIsIdempotent
// (cmd/cleatctl) is where that surfaced.
//
// Checking for siblings (the issue's own ask) turned up three more: queues,
// tenant_domains and tenant_settings all carry FORCE ROW LEVEL SECURITY
// (`grep -oE 'ALTER TABLE (ONLY )?[a-zA-Z_.]+ FORCE ROW LEVEL SECURITY'
// migrations/postgres/*.sql`) and were never in any cleanup list either.
//
// This is the known-positive the issue asks for, generalised to all four:
// seed a row, run the cleanup for each dialect, confirm it is gone. It does
// not need to reproduce tenant_secrets' decrypt failure itself -- that is a
// consequence of the row surviving, and the row surviving is the one thing
// to assert on, for every table with the same gap.
func TestCleanupTestDataClearsTenantScopedFixtureTables(t *testing.T) {
	tenant := uuid.MustParse(DefaultTenantUUID)

	for _, dialect := range []testutil.Dialect{
		testutil.DialectPostgres, testutil.DialectMySQL, testutil.DialectMSSQL,
	} {
		t.Run(string(dialect), func(t *testing.T) {
			db := testutil.TestDB(t, dialect)
			defer db.Close()
			testutil.SetupFullSchema(t, db, dialect)

			// AdminDB, not db: on MSSQL a plain pool with no session context is
			// filtered by these tables' own RLS predicates and sees zero rows
			// whether a fixture landed or not, which would make "before == 0"
			// mean nothing -- testutil.AdminDB bypasses that (and is a no-op on
			// the other two dialects).
			admin := testutil.AdminDB(t, db, dialect)

			seedTenantSecret(t, db, dialect, tenant)
			seedQueue(t, db, dialect, tenant)
			seedTenantDomain(t, db, dialect, tenant)
			seedTenantSettings(t, db, dialect, tenant)

			for _, table := range []string{"tenant_secrets", "queues", "tenant_domains", "tenant_settings"} {
				var before int
				if err := admin.QueryRow("SELECT count(*) FROM " + table).Scan(&before); err != nil { //nolint:gosec // G202: table is one of four literals in the range above.
					t.Fatalf("count before, %s: %v", table, err)
				}
				if before == 0 {
					t.Fatalf("fixture did not land in %s; the test would pass vacuously", table)
				}
			}

			switch dialect {
			case testutil.DialectPostgres:
				testutil.CleanupPostgresTestData(t, db)
			case testutil.DialectMySQL:
				testutil.CleanupMySQLTestData(t, db)
			case testutil.DialectMSSQL:
				testutil.CleanupMSSQLTestData(t, db)
			}

			for _, table := range []string{"tenant_secrets", "queues", "tenant_domains", "tenant_settings"} {
				var after int
				if err := admin.QueryRow("SELECT count(*) FROM " + table).Scan(&after); err != nil { //nolint:gosec // G202: table is one of four literals in the range above.
					t.Fatalf("count after, %s: %v", table, err)
				}
				if after != 0 {
					t.Fatalf("cleanup left %d row(s) in %s on %s. %s is missing from this "+
						"dialect's cleanup table list, so fixtures accumulate across local "+
						"test runs. See cleat#2228.", after, table, dialect, table)
				}
			}
		})
	}
}

// withMSSQLTenantSession runs fn on a connection with SESSION_CONTEXT set to
// tenant, when the dialect is MSSQL -- required for an INSERT to pass
// tenant_secrets/queues/tenant_domains/tenant_settings' BLOCK PREDICATE.
// A no-op single-statement Exec on the other two dialects.
func withMSSQLTenantSession(t *testing.T, db *sql.DB, dialect testutil.Dialect, tenant uuid.UUID, query string, args ...any) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Close()
	if dialect == testutil.DialectMSSQL {
		if _, err := conn.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, tenant.String()); err != nil {
			t.Fatalf("scope the connection: %v", err)
		}
	}
	if _, err := conn.ExecContext(ctx, query, args...); err != nil {
		// Tolerate a duplicate-key error: a row already present under this
		// name/tenant -- however it got there, including a PRIOR run of this
		// same test against a persistent local database, whose OWN cleanup
		// call is what this test exists to verify -- satisfies this seed
		// step just as well as one this call inserted itself. Any other
		// error is real and still fatal.
		if !isDuplicateKeyErr(err) {
			t.Fatalf("seed fixture: %v", err)
		}
	}
}

// isDuplicateKeyErr matches each dialect's own wording rather than a shared
// error type, because database/sql has none: PostgreSQL's pq error code
// 23505, MySQL's 1062, SQL Server's 2627.
func isDuplicateKeyErr(err error) bool {
	s := err.Error()
	return strings.Contains(s, "23505") ||
		strings.Contains(s, "1062") ||
		strings.Contains(s, "2627") ||
		strings.Contains(s, "Violation of PRIMARY KEY constraint") ||
		strings.Contains(s, "Duplicate entry")
}

func seedTenantSecret(t *testing.T, db *sql.DB, dialect testutil.Dialect, tenant uuid.UUID) {
	t.Helper()
	key := VersionedKey{Version: 1, Key: make([]byte, 32)}
	ring, err := NewKeyRing(key)
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}
	store := NewSecretStoreWithRing(db, string(dialect), ring)
	ctx := tenantctx.With(context.Background(), tenant)
	if err := store.PutSecret(ctx, tenant.String(), "cleat-2228-cleanup-fixture", "fixture-value"); err != nil {
		t.Fatalf("seed tenant_secrets row: %v", err)
	}
}

func seedQueue(t *testing.T, db *sql.DB, dialect testutil.Dialect, tenant uuid.UUID) {
	t.Helper()
	q := map[testutil.Dialect]string{
		testutil.DialectPostgres: `INSERT INTO queues (tenant_id, name, concurrency_limit) VALUES ($1, $2, 1)`,
		testutil.DialectMySQL:    `INSERT INTO queues (tenant_id, name, concurrency_limit) VALUES (?, ?, 1)`,
		testutil.DialectMSSQL:    `INSERT INTO queues (tenant_id, name, concurrency_limit) VALUES (@p1, @p2, 1)`,
	}[dialect]
	withMSSQLTenantSession(t, db, dialect, tenant, q, tenant.String(), "cleat-2228-cleanup-fixture")
}

func seedTenantDomain(t *testing.T, db *sql.DB, dialect testutil.Dialect, tenant uuid.UUID) {
	t.Helper()
	q := map[testutil.Dialect]string{
		testutil.DialectPostgres: `INSERT INTO tenant_domains (hostname, tenant_id) VALUES ($1, $2)`,
		testutil.DialectMySQL:    `INSERT INTO tenant_domains (hostname, tenant_id) VALUES (?, ?)`,
		testutil.DialectMSSQL:    `INSERT INTO tenant_domains (hostname, tenant_id) VALUES (@p1, @p2)`,
	}[dialect]
	withMSSQLTenantSession(t, db, dialect, tenant, q, "cleat-2228-cleanup-fixture.example.com", tenant.String())
}

func seedTenantSettings(t *testing.T, db *sql.DB, dialect testutil.Dialect, tenant uuid.UUID) {
	t.Helper()
	q := map[testutil.Dialect]string{
		testutil.DialectPostgres: `INSERT INTO tenant_settings (tenant_id) VALUES ($1)`,
		testutil.DialectMySQL:    `INSERT INTO tenant_settings (tenant_id) VALUES (?)`,
		testutil.DialectMSSQL:    `INSERT INTO tenant_settings (tenant_id) VALUES (@p1)`,
	}[dialect]
	withMSSQLTenantSession(t, db, dialect, tenant, q, tenant.String())
}
