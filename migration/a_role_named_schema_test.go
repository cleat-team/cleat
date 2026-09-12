package migration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/migration"
)

// The default search_path is `"$user", public`, so a schema named after the
// connecting role captures every unqualified name. That is not hypothetical
// here: 001_schema.sql creates a schema called "cleat" and
// docker-compose.cluster.yml connects as POSTGRES_USER=cleat, so the shipped
// cluster deployment runs in exactly this configuration.
//
// Nineteen migration files carried `SET search_path = public;` for that reason
// until cleat#1287. The runner sets it now, and this is the test that the
// substitution is equivalent where it matters.
//
// WHY IT EXISTS SEPARATELY FROM TestMigrationsHonourANonDefaultSchema. That one
// varies the configured schema, which is the feature. This one varies the
// SHAPE OF THE DATABASE -- whether a role-named schema exists to capture things
// -- which is the hazard, and no amount of exercising the feature reaches it.
// The first attempt at #1287 passed every local test and failed in CI on this
// configuration alone, with
//
//	create_tenant_role(a): pq: relation "cleat.workflow_defs" does not exist
//
// because a plpgsql function with no search_path of its own resolves names, and
// evaluates current_schema(), with the CALLER's -- and the caller is not the
// migration connection. The body had just moved from a public.* literal to
// current_schema(), which is correct at migration time and wrong at call time.
// `SET search_path FROM CURRENT` on the function moves the answer back.
//
// Reproducing it needs no special role: a schema named after whoever the test
// connects as has the same effect as one named "cleat" has for the cluster.
func TestARoleNamedSchemaDoesNotCaptureTheMigrations(t *testing.T) {
	ctx := context.Background()
	db := newScratchDB(t, "cleat_role_named_schema_test")

	var role string
	if err := db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role); err != nil {
		t.Fatalf("current_user: %v", err)
	}
	if !strings.HasPrefix(role, "pg_") {
		if _, err := db.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS "`+role+`"`); err != nil {
			t.Fatalf("create schema %q: %v", role, err)
		}
	}

	// The control for the whole test. If "$user" does not actually resolve
	// here, nothing below is measuring the hazard and everything passes for
	// the wrong reason.
	var firstSchema string
	if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&firstSchema); err != nil {
		t.Fatalf("current_schema: %v", err)
	}
	if firstSchema != role {
		t.Fatalf("created schema %q but current_schema() is %q; the "+
			"role-named-schema hazard is not reproduced and this test proves "+
			"nothing", role, firstSchema)
	}

	if err := migration.NewRunner(db, migration.DialectPostgres, migrationsRoot(t)).
		Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := tablesBySchema(t, db)
	if got["public"] < 10 {
		t.Errorf("only %d tables in public: %v", got["public"], got)
	}
	if got[role] != 0 {
		t.Errorf("%d table(s) landed in the role-named schema %q: %v", got[role], role, got)
	}

	// And the call-time half, which is the part CI caught and local runs did
	// not. This connection is not the runner's, and its search_path still puts
	// the role-named schema first.
	// Two arguments since cleat#1307: the password is derived by the caller
	// (plugin.TenantRolePassword) and no longer generated or stored, and the
	// one-argument form is dropped rather than overloaded. A literal of the
	// right length is enough here -- this test is about search_path, not about
	// the derivation, and the function only checks the length.
	var created string
	if err := db.QueryRowContext(ctx, `SELECT admin.create_tenant_role($1, $2)`,
		"00000000-0000-0000-0000-000000000000",
		strings.Repeat("0", 64)).Scan(&created); err != nil {
		t.Fatalf("create_tenant_role on a connection whose search_path is %q: %v\n\n"+
			"The function resolves names with the caller's search_path unless "+
			"it carries one of its own. `SET search_path FROM CURRENT` on the "+
			"function is what freezes the migration-time answer onto it.",
			firstSchema, err)
	}
	if created == "" {
		t.Error("create_tenant_role returned an empty role name")
	}
}
