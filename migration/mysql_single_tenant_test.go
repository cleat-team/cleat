package migration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/migration"
)

// D1 (tiers.yaml) says MySQL is single-tenant. Until migrations/mysql/038 that
// was enforced by breakage at the point of USE: a second tenant could be
// created without complaint, got an unmigrated database on first use, and
// failed every operation with "Table 'cleat_<uuid>.workflow_instances' doesn't
// exist" -- a message that names a missing table rather than an unsupported
// configuration, far from the cause.
//
// The pair below is the test, and neither half means anything alone. The MySQL
// half asserts the refusal; the PostgreSQL half asserts a second tenant still
// inserts fine there, so the refusal is MySQL's rule rather than something that
// broke tenants everywhere. A single-dialect version would pass equally well
// against a migration that made every backend single-tenant.
//
// Both build their own scratch database and run the shipped migrations into it,
// rather than using whatever database the environment points at. The first
// version of this file did the latter and **passed locally while failing in
// CI**: ci.yml's `support` job provides a bare PostgreSQL service, so
// `INSERT INTO admin.tenants` came back with
// `relation "admin.tenants" does not exist`. A test that assumes a migrated
// database is testing the developer's machine.

func TestMySQLRefusesASecondTenant(t *testing.T) {
	db := newMySQLScratchDB(t, "cleat_migration_mysql_single_tenant_test")
	ctx := context.Background()

	r := migration.NewRunner(db, migration.DialectMySQL, migrationsRoot(t))
	if err := r.Run(ctx); err != nil {
		t.Fatalf("applying the shipped MySQL migrations failed: %v", err)
	}

	// 002_defaults seeds exactly one tenant. Checked rather than assumed: with
	// zero, the insert below succeeds and this test proves nothing; with more
	// than one, migration 038's unique index could not have been created and
	// the guard is not in place at all.
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tenants`).Scan(&n); err != nil {
		t.Fatalf("counting tenants: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 tenant after migrating a fresh database, found %d", n)
	}

	_, err := db.ExecContext(ctx,
		`INSERT INTO tenants (tenant_id, name, display_name) VALUES (?, ?, ?)`,
		"11111111-2222-3333-4444-555555555555", "second-tenant", "Second Tenant")
	if err == nil {
		t.Fatalf("a second tenant was created on MySQL.\n\n" +
			"D1 says MySQL is single-tenant, and migrations/mysql/038 is what " +
			"makes that true rather than merely stated. Without it the tenant " +
			"is created, gets an unmigrated database on first use, and fails " +
			"every operation with a missing-table error far from the cause.")
	}

	// The error must name the rule, not merely be an error. That is the whole
	// point: the pre-038 behaviour also failed eventually, just unhelpfully and
	// somewhere else.
	const wantKey = "uq_tenants_mysql_is_single_tenant_only_see_tiers_yaml_d1"
	if !strings.Contains(err.Error(), wantKey) {
		t.Fatalf("the insert failed, but not with the single-tenant guard: %v\n\n"+
			"Expected a duplicate-key error naming %q, which tells the reader "+
			"the rule and where it is written down. Some other failure means "+
			"this test is no longer measuring the guard.", err, wantKey)
	}
}

// The control: the same insert on PostgreSQL must succeed. 038 is a MySQL-only
// migration and D1 grants multi-tenancy on PostgreSQL.
func TestPostgresStillAcceptsASecondTenant(t *testing.T) {
	db := newScratchDB(t, "cleat_migration_pg_second_tenant_test")
	ctx := context.Background()

	r := migration.NewRunner(db, migration.DialectPostgres, migrationsRoot(t))
	if err := r.Run(ctx); err != nil {
		t.Fatalf("applying the shipped PostgreSQL migrations failed: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO admin.tenants (tenant_id, name, display_name) VALUES ($1, $2, $3)`,
		"11111111-2222-3333-4444-555555555555", "second-tenant-control",
		"Second Tenant Control"); err != nil {
		t.Fatalf("PostgreSQL refused a second tenant: %v\n\n"+
			"D1 grants multi-tenancy on PostgreSQL. If this now fails, the "+
			"single-tenant guard leaked out of MySQL -- check that "+
			"migrations/mysql/038 has no PostgreSQL counterpart.", err)
	}
}
