package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/lib/pq"
)

// TestTenantIDForSchema covers the mapping on its own, including the cases
// where a schema does not name a tenant at all.
func TestTenantIDForSchema(t *testing.T) {
	tests := []struct {
		schema string
		want   string
		ok     bool
	}{
		{"tenant_cccccccc_cccc_4ccc_cccc_cccccccccccc", "cccccccc-cccc-4ccc-cccc-cccccccccccc", true},
		{"tenant_00000000_0000_0000_0000_000000000000", "00000000-0000-0000-0000-000000000000", true},
		// Operator-chosen peer schema names carry no tenant.
		{"svc_billing", "", false},
		{"public", "", false},
		{"tenant_not_a_uuid", "", false},
		{"tenant_", "", false},
		{"", "", false},
		// The prefix alone is not enough -- the remainder must parse.
		{"tenant_cccccccc_cccc_4ccc_cccc", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.schema, func(t *testing.T) {
			got, ok := tenantIDForSchema(tt.schema)
			if ok != tt.ok {
				t.Fatalf("tenantIDForSchema(%q) ok = %v, want %v", tt.schema, ok, tt.ok)
			}
			if got != tt.want {
				t.Errorf("tenantIDForSchema(%q) = %q, want %q", tt.schema, got, tt.want)
			}
		})
	}
}

// Note: nothing in the repo provisions a peer schema, so the test below builds
// one by hand. migrations/postgres/001_schema.sql pins `SET search_path = public`
// and creates every table there, and admin.create_tenant_role creates
// `tenant_<uuid>` as an empty namespace whose grants all point back at public.*.
// So the cross-schema feature writes to `<schema>.workflow_instances`, a table
// the project never creates -- which is a large part of why the tenant
// attribution defect survived: there was nothing to run it against.

// TestStartChildWorkflowInSchemaAttributesToTargetTenant is the regression test
// for IMPROVEMENT-PLAN §2.23.
//
// A cross-schema child belongs to the *target* schema's tenant: the target
// schema is a separate microservice, and the child runs as part of it. The
// insert used to name no tenant_id column at all, so the row landed on the
// destination table's DEFAULT (the zero UUID) regardless of who it was really
// for, and under the destination's fail-closed RLS policy it was refused
// outright.
//
// Note what this does NOT assert: that the parent's tenant is used. Writing the
// parent's tenant would be a false fix -- it makes the insert succeed while
// filing one service's workflow under another service's tenant.
func TestStartChildWorkflowInSchemaAttributesToTargetTenant(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)
	defer testutil.CleanupPostgresTestData(t, adminDB)

	ctx := context.Background()
	const parentTenant = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
	const targetTenant = "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb"
	peerSchema := "tenant_bbbbbbbb_bbbb_4bbb_bbbb_bbbbbbbbbbbb"

	// Stand up the peer deployment's tables, with the same fail-closed policy
	// the real migration installs on public.
	qs := pq.QuoteIdentifier(peerSchema)
	for _, stmt := range []string{
		fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, qs),
		fmt.Sprintf(`CREATE SCHEMA %s`, qs),
		fmt.Sprintf(`CREATE TABLE %s.workflow_defs (
			name TEXT NOT NULL,
			version INTEGER NOT NULL,
			deprecated BOOLEAN NOT NULL DEFAULT false,
			tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
			PRIMARY KEY (name, version)
		)`, qs),
		fmt.Sprintf(`CREATE TABLE %s.workflow_instances (
			id TEXT PRIMARY KEY,
			def_name TEXT NOT NULL,
			def_version INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'ready',
			input JSONB NOT NULL DEFAULT '{}',
			parent_workflow_id TEXT,
			parent_close_policy TEXT DEFAULT 'ABANDON',
			task_queue TEXT NOT NULL DEFAULT 'default',
			priority INTEGER NOT NULL DEFAULT 0,
			tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
			FOREIGN KEY (def_name, def_version) REFERENCES `+qs+`.workflow_defs(name, version)
		)`, qs),
		fmt.Sprintf(`ALTER TABLE %s.workflow_instances ENABLE ROW LEVEL SECURITY`, qs),
		fmt.Sprintf(`ALTER TABLE %s.workflow_instances FORCE ROW LEVEL SECURITY`, qs),
		fmt.Sprintf(`CREATE POLICY tenant_isolation_peer ON %s.workflow_instances
			FOR ALL USING (tenant_id = cleat.assert_tenant_set())`, qs),
		fmt.Sprintf(`INSERT INTO %s.workflow_defs (name, version, tenant_id) VALUES ('peer-child', 1, '%s')`, qs, targetTenant),
	} {
		if _, err := adminDB.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("peer schema setup (%.60s...): %v", stmt, err)
		}
	}
	defer adminDB.ExecContext(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, qs))

	// The store is the *parent's* -- a different tenant from the target.
	appDB := testutil.OpenPostgresRLSTestDB(t, adminDB)
	defer appDB.Close()
	if _, err := adminDB.ExecContext(ctx,
		fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO `+testutil.PostgresRLSTestRole, qs)); err != nil {
		t.Fatalf("grant usage on peer schema: %v", err)
	}
	if _, err := adminDB.ExecContext(ctx,
		fmt.Sprintf(`GRANT SELECT, INSERT ON ALL TABLES IN SCHEMA %s TO `+testutil.PostgresRLSTestRole, qs)); err != nil {
		t.Fatalf("grant tables on peer schema: %v", err)
	}

	store := NewPostgresStore(appDB).WithTenant(parentTenant)

	parentID := fmt.Sprintf("xschema-parent-%d", time.Now().UnixNano())
	runID, err := store.StartChildWorkflowInSchema(ctx, peerSchema, parentID, "peer-child", `{"k":"v"}`, 1, "ABANDON", 0)
	if err != nil {
		t.Fatalf("StartChildWorkflowInSchema: %v -- the insert names no tenant_id, "+
			"so the row falls to the destination default and is refused by the "+
			"destination's tenant_isolation policy", err)
	}
	if runID == "" {
		t.Fatal("StartChildWorkflowInSchema returned an empty run id")
	}

	// Read back through the superuser connection, which is exempt from RLS and
	// so reports what was actually stored rather than what this tenant can see.
	var gotTenant string
	if err := adminDB.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT tenant_id::text FROM %s.workflow_instances WHERE id = $1`, qs), runID,
	).Scan(&gotTenant); err != nil {
		t.Fatalf("reading peer-schema child row: %v", err)
	}
	if gotTenant == parentTenant {
		t.Fatalf("child was attributed to the PARENT tenant %q -- a cross-schema child "+
			"belongs to the target schema's tenant, because the child runs as part of "+
			"the destination microservice", gotTenant)
	}
	if gotTenant != targetTenant {
		t.Errorf("child tenant_id = %q, want the target schema's tenant %q", gotTenant, targetTenant)
	}
}
