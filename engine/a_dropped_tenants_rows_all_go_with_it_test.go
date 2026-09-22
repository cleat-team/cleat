package engine

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// admin.drop_tenant deletes from a list of table names, and the list has
// drifted three times: tenant_settings (missing for "twenty migrations", per
// cmd/cleatctl/droptenant.go's own comment), workflow_defs (cleat#1201), and
// workflow_memory_{stats,samples} (cleat#1644, which this file is the durable
// half of).
//
// migration 056 named the structural reason, about a different guard:
//
//	so it answers "is every statement against a KNOWN tenant-scoped table
//	scoped?" and cannot answer "is every table that should be tenant-scoped
//	actually one?"
//
// admin.drop_tenant has that blind spot with the roles reversed. A test
// written from the same list as the function cannot see what both of them
// missed -- which is why the universe below is read from
// information_schema.columns and not from anything in this repository.
//
// THE SEED MAP IS THE RATCHET, and it matters more than the emptiness check.
// "Zero rows afterwards" is satisfied by a table that was never seeded, which
// is how cleat#1265 came to publish "4 of 4 clean" off a run where two of six
// seeds had failed. So every member of the universe must be seeded here, and a
// member with no seed fails NAMING ITSELF -- so the next migration to add a
// tenant_id column cannot land without someone deciding what drop_tenant does
// with it.

// tenantOwnedTablesSQL is the universe: every table carrying a tenant_id
// column, in the two schemas a single install owns.
//
// RESTRICTED TO public AND admin, deliberately. Per-tenant `tenant_<uuid>`
// schemas (001_schema.sql) also contain tables with a tenant_id column, and
// they belong to OTHER tenants -- two were present on the database this was
// written against. admin.drop_tenant removes the dropped tenant's own schema
// with DROP SCHEMA ... CASCADE and must not touch anyone else's, so sweeping
// them here would assert the opposite of what the function should do.
//
// 'public' rather than the --schema value because that is what this test drops
// from; see the p_schema argument below.
const tenantOwnedTablesSQL = `
	SELECT table_schema, table_name
	FROM information_schema.columns
	WHERE column_name = 'tenant_id'
	  AND table_schema IN ('public', 'admin')
	ORDER BY table_schema, table_name`

// tenantOwnedTables reads the universe from the live catalogue.
func tenantOwnedTables(t *testing.T, ctx context.Context, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, tenantOwnedTablesSQL)
	if err != nil {
		t.Fatalf("enumerate tenant-owned tables: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var schema, name string
		if err := rows.Scan(&schema, &name); err != nil {
			t.Fatalf("scan tenant-owned table: %v", err)
		}
		out = append(out, schema+"."+name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("enumerate tenant-owned tables: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("no table in public or admin carries a tenant_id column, so every count " +
			"below would be zero for a reason that has nothing to do with drop_tenant")
	}
	sort.Strings(out)
	return out
}

// seededByDropTenantFixture names the tables dropTenantFixture already seeds,
// including the two behind its withAPIKey and withRole switches, both of which
// this test passes true.
//
// A list rather than an inspection because dropTenantFixture is Go: there is
// nothing to read it out of. It is checked, not trusted -- every name here is
// asserted non-empty before the drop, so a fixture that stops seeding one of
// them fails the precondition rather than quietly weakening the test.
var seededByDropTenantFixture = []string{
	"admin.tenant_api_keys",
	"admin.tenant_roles",
	"admin.tenants",
	"public.concurrency_keys",
	"public.event_history",
	"public.idempotency_keys",
	"public.queue_holders",
	"public.workflow_defs",
	"public.workflow_instances",
	"public.workflow_promises",
	"public.workflow_routing",
	"public.workflow_schedules",
	"public.workflow_signals",
	"public.workflow_tags",
	"public.workflow_update_requests",
}

// seedRemainingTenantTables covers the universe members dropTenantFixture does
// not: the five that reach a tenant by ON DELETE CASCADE from admin.tenants,
// and the two from cleat#1644 that reach it by nothing at all.
//
// The cascade ones are seeded even though no DELETE in admin.drop_tenant names
// them. That is the point: "the row went away" is the property under test, and
// whether a DELETE or a foreign key removed it is not something an operator
// distinguishes -- the same reasoning droptenant.go gives for listing them in
// its preview.
func seedRemainingTenantTables(t *testing.T, ctx context.Context, db *sql.DB, tenant, tag string) []string {
	t.Helper()
	seeds := []struct {
		table string
		stmt  string
		args  []any
	}{
		{"public.tenant_settings",
			`INSERT INTO tenant_settings (tenant_id) VALUES ($1) ON CONFLICT DO NOTHING`,
			[]any{tenant}},
		{"public.tenant_domains",
			`INSERT INTO tenant_domains (hostname, tenant_id) VALUES ($1, $2)`,
			[]any{"host-" + tag + ".example.test", tenant}},
		{"public.tenant_secrets",
			`INSERT INTO tenant_secrets (tenant_id, name, ciphertext) VALUES ($1, $2, $3)`,
			[]any{tenant, "secret-" + tag, []byte("ciphertext-" + tag)}},
		{"admin.tenant_egress_allow",
			`INSERT INTO admin.tenant_egress_allow (tenant_id, host) VALUES ($1, $2)`,
			[]any{tenant, "egress-" + tag + ".example.test"}},
		// cleat#1116. ON DELETE CASCADE from admin.tenants, like tenant_settings
		// and tenant_domains above -- droptenant.go lists it in its preview for
		// the same reason.
		{"public.queues",
			`INSERT INTO queues (tenant_id, name, concurrency_limit) VALUES ($1, $2, 3)`,
			[]any{tenant, "queue-" + tag}},
		// cleat#1918. ON DELETE CASCADE from admin.tenants directly (not
		// chained through workflow_instances the way queue_holders is -- see
		// migration 097's header), so like queues above it reaches the
		// tenant by a foreign key no DELETE in admin.drop_tenant names.
		{"public.queue_rate_tokens",
			`INSERT INTO queue_rate_tokens (tenant_id, queue_name, workflow_id, expires_at)
			 VALUES ($1, $2, $3, now() + interval '1 hour')`,
			[]any{tenant, "queue-" + tag, "wf-" + tag}},
		// cleat#1644. Neither has a foreign key to anything, so neither
		// cascaded, and neither was named by any DELETE.
		{"public.workflow_memory_stats",
			`INSERT INTO workflow_memory_stats (tenant_id, def_name, mean_bytes, sample_count)
			 VALUES ($1, $2, 1048576, 1)`,
			[]any{tenant, "memprofile-" + tag}},
		{"public.workflow_memory_samples",
			`INSERT INTO workflow_memory_samples (tenant_id, def_name, sample_bytes)
			 VALUES ($1, $2, 1048576)`,
			[]any{tenant, "memprofile-" + tag}},
	}
	var names []string
	for _, s := range seeds {
		if _, err := db.ExecContext(ctx, s.stmt, s.args...); err != nil {
			t.Fatalf("seed %s(%s): %v", s.table, tag, err)
		}
		names = append(names, s.table)
	}
	return names
}

func countTenantRowsIn(t *testing.T, ctx context.Context, db *sql.DB, table, tenant string) int {
	t.Helper()
	var n int
	// The name comes from information_schema, not from a request. Quoted so a
	// table needing quotes is a quoted identifier rather than a syntax error.
	schema, name := splitQualified(table)
	stmt := fmt.Sprintf(`SELECT count(*) FROM %s.%s WHERE tenant_id = $1`,
		quoteIdentPG(schema), quoteIdentPG(name))
	if err := db.QueryRowContext(ctx, stmt, tenant).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func splitQualified(qualified string) (string, string) {
	for i := 0; i < len(qualified); i++ {
		if qualified[i] == '.' {
			return qualified[:i], qualified[i+1:]
		}
	}
	return "public", qualified
}

func quoteIdentPG(ident string) string {
	out := make([]byte, 0, len(ident)+2)
	out = append(out, '"')
	for i := 0; i < len(ident); i++ {
		if ident[i] == '"' {
			out = append(out, '"')
		}
		out = append(out, ident[i])
	}
	return string(append(out, '"'))
}

// TestEveryTenantOwnedTableIsEmptiedByDropTenant is the durable half of
// cleat#1644: it asserts the property the three drifts each violated, rather
// than the two table names the latest one happened to be about.
func TestEveryTenantOwnedTableIsEmptiedByDropTenant(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	apply032DropTenantMigration(t, adminDB)
	testutil.CleanupPostgresTestData(t, adminDB)
	defer testutil.CleanupPostgresTestData(t, adminDB)

	ctx := context.Background()
	const (
		victim    = "01d01644-0000-4000-8000-0000000016a4"
		bystander = "01d01644-0000-4000-8000-0000000016b4"
	)
	defer cleanupDropTenantExtras(t, ctx, adminDB, victim)
	defer cleanupDropTenantExtras(t, ctx, adminDB, bystander)

	universe := tenantOwnedTables(t, ctx, adminDB)

	seeded := map[string]bool{}
	for _, tn := range []struct{ id, tag string }{{victim, "v1644"}, {bystander, "b1644"}} {
		defName := "drop-tenant-all-def-" + tn.tag
		// workflow_instances_def_fkey carries tenant_id since D7
		// (IMPROVEMENT-PLAN 3.77), so the definition is deployed for the
		// tenant whose instances the fixture seeds.
		deployDefForTenants(t, adminDB, defName, 1, tn.id)
		// Both switches true: this test needs admin.tenant_api_keys and
		// admin.tenant_roles populated, which are two members of the universe.
		dropTenantFixture(t, ctx, adminDB, tn.id, defName, tn.tag, true, true)
		for _, name := range seededByDropTenantFixture {
			seeded[name] = true
		}
		for _, name := range seedRemainingTenantTables(t, ctx, adminDB, tn.id, tn.tag) {
			seeded[name] = true
		}
	}

	// THE RATCHET. A tenant-owned table with no seed here would answer zero
	// after the drop whether or not admin.drop_tenant reaches it, so the
	// emptiness check below would pass it silently -- which is exactly how
	// workflow_memory_{stats,samples} went unnoticed from 056 to cleat#1644.
	var unseeded []string
	for _, table := range universe {
		if !seeded[table] {
			unseeded = append(unseeded, table)
		}
	}
	if len(unseeded) > 0 {
		t.Fatalf("%v carries a tenant_id column and is not seeded by this test.\n\n"+
			"That is a new tenant-owned table, and it needs a decision before it can land:\n"+
			"  * drop_tenant should delete it -- add it to the core array in a new migration "+
			"that redefines the function, and seed it in seedRemainingTenantTables;\n"+
			"  * it already reaches the tenant by ON DELETE CASCADE from admin.tenants -- seed "+
			"it here anyway, as tenant_settings and three others are, because the property "+
			"under test is that the row went away and not which mechanism removed it;\n"+
			"  * it is genuinely NOT tenant-owned despite the column -- say so here, in a "+
			"named exemption with the reason, the way plugin_defs is excluded in "+
			"admin.drop_tenant's own comment. There are none today, which is why there is no "+
			"exemption list to add to yet; write one.\n\n"+
			"What is not an option is leaving it out: a table that is never seeded counts "+
			"zero after the drop whether or not anything deleted it, so the assertion below "+
			"would pass it silently.", unseeded)
	}

	// Every seeded table genuinely holds a row for both tenants, checked before
	// anything is deleted. cleat#1265 published "4 of 4 clean" off a run where
	// two of six seeds had silently failed.
	for _, tn := range []struct{ id, label string }{{victim, "victim"}, {bystander, "bystander"}} {
		var empty []string
		for _, table := range universe {
			if countTenantRowsIn(t, ctx, adminDB, table, tn.id) == 0 {
				empty = append(empty, table)
			}
		}
		if len(empty) > 0 {
			t.Fatalf("PRECONDITION FAILED: the %s has no rows in %v before the drop. "+
				"A zero count afterwards would mean nothing for those tables", tn.label, empty)
		}
	}

	if _, err := adminDB.ExecContext(ctx,
		`SELECT admin.drop_tenant($1, 'public')`, victim); err != nil {
		t.Fatalf("admin.drop_tenant(victim): %v", err)
	}

	// Did it run at all? A function that silently did nothing leaves the same
	// surviving rows and reads as the same bug.
	if got := countTenantRowsIn(t, ctx, adminDB, "admin.tenants", victim); got != 0 {
		t.Fatalf("PRECONDITION FAILED: admin.drop_tenant left the victim's admin.tenants row "+
			"behind (count=%d), so it did not run to completion and every count below says "+
			"nothing", got)
	}

	var survived []string
	for _, table := range universe {
		if got := countTenantRowsIn(t, ctx, adminDB, table, victim); got != 0 {
			survived = append(survived, fmt.Sprintf("%s (%d rows)", table, got))
		}
	}
	if len(survived) > 0 {
		t.Errorf("admin.drop_tenant left the dropped tenant's rows in %v.\n\n"+
			"Every table listed carries a tenant_id column, so it holds rows owned by one "+
			"tenant. Either add it to the core array in a new migration that redefines "+
			"admin.drop_tenant, or establish an ON DELETE CASCADE from admin.tenants -- "+
			"the operator does not care which mechanism removed it, only that it is gone.",
			survived)
	}

	// The other direction, which is what stops a fix becoming "delete
	// everything". Counted over the whole universe rather than a sample,
	// because a DELETE that forgot its WHERE reaches every table equally.
	var collateral []string
	for _, table := range universe {
		if got := countTenantRowsIn(t, ctx, adminDB, table, bystander); got == 0 {
			collateral = append(collateral, table)
		}
	}
	if len(collateral) > 0 {
		t.Errorf("dropping one tenant emptied the bystander's rows in %v", collateral)
	}
}
