package plugin

import (
	"context"
	"strings"
	"testing"
)

// TestApplyTenantScopingEmitsTwoPolicies asserts what applyTenantScoping
// actually installs, by running it and reading pg_policies back.
//
// WHY NOT ASSERT THIS FROM THE ENGINE TEST that covers the same shape
// (engine/a_tenant_policy_keeps_its_index_and_its_boundary_test.go): that one
// builds its fixture BY HAND, with a CREATE POLICY written to match what this
// function emits. It proves the shape behaves correctly; it cannot notice if
// this function stops emitting that shape. The two together cover the pair,
// and neither covers it alone.
func TestApplyTenantScopingEmitsTwoPolicies(t *testing.T) {
	db := downTestDB(t, "cleat_scope_two_policies")
	ctx := context.Background()

	const table = "tenant_scope_probe"
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE `+table+` (tenant_id uuid NOT NULL, k text)`); err != nil {
		t.Fatalf("fixture table: %v", err)
	}
	t.Cleanup(func() { db.ExecContext(ctx, `DROP TABLE IF EXISTS `+table) })

	if err := applyTenantScoping(ctx, db.ExecContext, DialectPostgres, []string{table}); err != nil {
		t.Fatalf("applyTenantScoping: %v", err)
	}

	rows, err := db.QueryContext(ctx,
		`SELECT policyname, roles::text, qual FROM pg_policies WHERE tablename = $1 ORDER BY policyname`,
		table)
	if err != nil {
		t.Fatalf("reading pg_policies: %v", err)
	}
	defer rows.Close()

	got := map[string]struct{ roles, qual string }{}
	for rows.Next() {
		var name, roles, qual string
		if err := rows.Scan(&name, &roles, &qual); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[name] = struct{ roles, qual string }{roles, qual}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("got %d policies %v, want exactly 2 -- one tenant, one sweep", len(got), got)
	}

	tenant, ok := got[table+"_tenant_isolation"]
	if !ok {
		t.Fatalf("no %s_tenant_isolation policy; got %v", table, got)
	}
	// The equality, not the CASE. A CASE here lands as a Filter rather than an
	// Index Cond and walks every index entry -- 603.654 ms against 0.415 ms on
	// 400000 rows over 400 tenants. cleat#1490.
	if !strings.Contains(tenant.qual, "assert_tenant_set()") {
		t.Errorf("tenant policy qual is %q, want a cleat.assert_tenant_set() equality", tenant.qual)
	}
	if strings.Contains(tenant.qual, "tenant_row_is_visible") || strings.Contains(tenant.qual, "CASE") {
		t.Errorf("tenant policy qual is %q, which is the pre-1490 CASE: it cannot use the "+
			"tenant index", tenant.qual)
	}
	// TO PUBLIC, not to a role. Per-tenant roles are created NOINHERIT
	// (001_schema.sql), and a NOINHERIT member does not match a `TO <role>`
	// policy -- measured, such a role reads 0 rows, silently.
	if !strings.Contains(tenant.roles, "public") {
		t.Errorf("tenant policy applies to %s, want public: a role-scoped tenant policy "+
			"is invisible to the NOINHERIT per-tenant roles and returns 0 rows without "+
			"raising", tenant.roles)
	}

	sweep, ok := got[table+"_cross_tenant"]
	if !ok {
		t.Fatalf("no %s_cross_tenant policy; a sweep entering cleat_sweep would match "+
			"nothing and read 0 rows", table)
	}
	if !strings.Contains(sweep.roles, "cleat_sweep") {
		t.Errorf("sweep policy applies to %s, want cleat_sweep", sweep.roles)
	}
	if strings.TrimSpace(sweep.qual) != "true" {
		t.Errorf("sweep policy qual is %q, want true", sweep.qual)
	}
}
