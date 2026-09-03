package migration_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

// D1 (tiers.yaml) says MySQL is single-tenant. Until migrations/mysql/038 that
// was enforced by breakage at the point of USE: a second tenant could be
// created without complaint, got an unmigrated database on first use, and
// failed every operation with "Table 'cleat_<uuid>.workflow_instances' doesn't
// exist" -- a message that names a missing table rather than an unsupported
// configuration.
//
// The pair below is the test, and neither half means anything alone. The MySQL
// half asserts the refusal; the PostgreSQL half asserts a second tenant still
// inserts fine there, so the refusal is MySQL's rule rather than something that
// broke tenants everywhere. A single-dialect version of this would pass equally
// well against a migration that made every backend single-tenant.
func TestMySQLRefusesASecondTenant(t *testing.T) {
	dsn := os.Getenv("CLEAT_TEST_MYSQL")
	if dsn == "" {
		t.Skip("CLEAT_TEST_MYSQL not set")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	// Not a skip: a DSN was supplied, so MySQL was asked for.
	if err := db.Ping(); err != nil {
		t.Fatalf("CLEAT_TEST_MYSQL is set but MySQL is unreachable: %v", err)
	}
	ctx := context.Background()

	// The default tenant must already be there -- 002_defaults seeds it. If it
	// is not, this test would "pass" by inserting the first tenant
	// successfully, which measures nothing.
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tenants`).Scan(&n); err != nil {
		t.Fatalf("counting tenants: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 tenant before this test, found %d.\n\n"+
			"With 0, the insert below succeeds and this test proves nothing. "+
			"With more than 1, migration 038's unique index could not have been "+
			"created and the guard is not in place at all.", n)
	}

	_, err = db.ExecContext(ctx,
		`INSERT INTO tenants (tenant_id, name, display_name) VALUES (?, ?, ?)`,
		"11111111-2222-3333-4444-555555555555", "second-tenant", "Second Tenant")
	if err == nil {
		_, _ = db.ExecContext(ctx, `DELETE FROM tenants WHERE name = 'second-tenant'`)
		t.Fatalf("a second tenant was created on MySQL.\n\n" +
			"D1 says MySQL is single-tenant, and migrations/mysql/038 is what " +
			"makes that true rather than merely stated. Without it the tenant " +
			"is created, gets an unmigrated database on first use, and fails " +
			"every operation with a missing-table error far from the cause.")
	}

	// The error must name the rule, not just fail. That is the whole point --
	// the pre-038 behaviour also produced an error eventually, just an
	// unhelpful one in an unrelated place.
	const wantKey = "uq_tenants_mysql_is_single_tenant_only_see_tiers_yaml_d1"
	if !strings.Contains(err.Error(), wantKey) {
		t.Fatalf("the insert failed, but not with the single-tenant guard: %v\n\n"+
			"Expected a duplicate-key error naming %q, which tells the reader "+
			"the rule and where it is written down. Some other failure means "+
			"this test is no longer measuring the guard.", err, wantKey)
	}
}

// The control. Same insert, PostgreSQL, must succeed -- 038 is a MySQL-only
// migration and multi-tenancy is granted there (D1).
func TestPostgresStillAcceptsASecondTenant(t *testing.T) {
	// postgresDSN, not os.Getenv("CLEAT_TEST_POSTGRES"): ci.yml's `support`
	// job -- the one that runs ./migration/... -- configures CLEAT_TEST_DB and
	// not CLEAT_TEST_POSTGRES. Reading only the latter would have made this
	// control SKIP in CI while passing locally, so the assertion above would
	// have run for weeks with nothing checking that the guard stayed inside
	// MySQL. That is the failure this whole file is about, one level up.
	dsn, configured := postgresDSN(t)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		if configured {
			t.Fatalf("configured postgres database is unreachable: %v", err)
		}
		t.Skipf("no postgres database configured: %v", err)
	}
	ctx := context.Background()

	const name = "second-tenant-control"
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM admin.tenants WHERE name = $1`, name)
	})
	_, _ = db.ExecContext(ctx, `DELETE FROM admin.tenants WHERE name = $1`, name)

	if _, err := db.ExecContext(ctx,
		`INSERT INTO admin.tenants (tenant_id, name, display_name) VALUES ($1, $2, $3)`,
		"11111111-2222-3333-4444-555555555555", name, "Second Tenant Control"); err != nil {
		t.Fatalf("PostgreSQL refused a second tenant: %v\n\n"+
			"D1 grants multi-tenancy on PostgreSQL. If this now fails, the "+
			"single-tenant guard leaked out of MySQL -- check that "+
			"migrations/mysql/038 has no PostgreSQL counterpart.", err)
	}
}
