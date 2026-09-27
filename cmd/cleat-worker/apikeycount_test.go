package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/google/uuid"
)

// TestLiveAPIKeyCountQuery_ExcludesDisabledAndExpired proves the startup
// key-count query counts only live keys: not disabled, not expired. The point
// of extracting liveAPIKeyCountQuery (cleat#2352) was that a bare COUNT(*)
// counted disabled and expired rows too, so a tenant whose every key had
// lapsed still read keyCount > 0 forever and never got its one-time startup
// key. This runs against a real database on every dialect because the two
// things it guards are dialect-specific SQL: the table name (the cleat#1963
// bug read an always-empty dbo.tenant_api_keys on SQL Server) and the "now"
// spelling that the expiry comparison uses.
//
// The count is over the whole tenant_api_keys table -- no tenant filter, since
// main() is asking "does any live key exist" -- and TestDB runs against a
// shared, persistent database that already holds other tests' keys. So the
// assertion is on the DELTA: seeding one expired, one live and one disabled
// key must raise the count by exactly one. An absolute "== 1" would measure
// other tests' leftovers, not this test's seeding.
func TestLiveAPIKeyCountQuery_ExcludesDisabledAndExpired(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMySQL, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			db := testutil.TestDB(t, dialect)
			ctx := context.Background()
			driver := string(dialect)

			tenantID := seedCountTenant(t, db, driver)

			live := func() int {
				var n int
				if err := db.QueryRowContext(ctx, liveAPIKeyCountQuery(driver)).Scan(&n); err != nil {
					t.Fatalf("live API key count (%s): %v", driver, err)
				}
				return n
			}

			before := live()
			seedCountKey(t, db, driver, tenantID, sql.NullTime{Time: time.Now().Add(-time.Hour), Valid: true}, sql.NullTime{}) // expired
			seedCountKey(t, db, driver, tenantID, sql.NullTime{}, sql.NullTime{})                                              // live
			seedCountKey(t, db, driver, tenantID, sql.NullTime{}, sql.NullTime{Time: time.Now(), Valid: true})                 // disabled
			after := live()

			if after != before+1 {
				t.Errorf("live API key count went %d -> %d after seeding expired+live+disabled, want +1", before, after)
			}
		})
	}
}

// seedCountTenant ensures a tenant row exists for seedCountKey to reference and
// returns its id, mirroring auth/a_key_expiry_test.go's seedTenant: MySQL is
// single-tenant (tiers.yaml's decision, a singleton unique constraint), so a
// second INSERT there fails and it returns engine.DefaultTenantUUID instead.
func seedCountTenant(t *testing.T, db *sql.DB, driver string) string {
	t.Helper()
	if driver == "mysql" {
		return engine.DefaultTenantUUID
	}
	id := uuid.New().String()
	switch driver {
	case "mssql":
		if _, err := db.Exec(`INSERT INTO admin.tenants (tenant_id, name) VALUES (@p1, @p2)`, id, "apikeycount-"+id); err != nil {
			t.Fatalf("seed tenant (%s): %v", driver, err)
		}
	default:
		if _, err := db.Exec(`INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`, id, "apikeycount-"+id); err != nil {
			t.Fatalf("seed tenant (%s): %v", driver, err)
		}
	}
	return id
}

// seedCountKey inserts one API key row with the given expiry/disable state.
// The key_hash is a fresh sha256 so it can never collide with a previous run's
// row even though the count query itself ignores it.
func seedCountKey(t *testing.T, db *sql.DB, driver, tenantID string, expiresAt, disabledAt sql.NullTime) {
	t.Helper()
	hash := sha256.Sum256([]byte(uuid.New().String()))
	switch driver {
	case "mysql":
		if _, err := db.Exec(
			`INSERT INTO tenant_api_keys (key_id, tenant_id, key_hash, description, expires_at, disabled_at) VALUES (?, ?, ?, ?, ?, ?)`,
			uuid.New().String(), tenantID, hash[:], "apikeycount", expiresAt, disabledAt); err != nil {
			t.Fatalf("seed key (%s): %v", driver, err)
		}
	case "mssql":
		if _, err := db.Exec(
			`INSERT INTO admin.tenant_api_keys (tenant_id, key_hash, description, expires_at, disabled_at) VALUES (@p1, @p2, @p3, @p4, @p5)`,
			tenantID, hash[:], "apikeycount", expiresAt, disabledAt); err != nil {
			t.Fatalf("seed key (%s): %v", driver, err)
		}
	default:
		if _, err := db.Exec(
			`INSERT INTO admin.tenant_api_keys (tenant_id, key_hash, description, expires_at, disabled_at) VALUES ($1, $2, $3, $4, $5)`,
			tenantID, hash[:], "apikeycount", expiresAt, disabledAt); err != nil {
			t.Fatalf("seed key (%s): %v", driver, err)
		}
	}
}
