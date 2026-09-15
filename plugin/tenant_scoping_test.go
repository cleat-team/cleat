package plugin

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// recordingExec captures the DDL applyTenantScoping emits without needing a
// database, so the dialect gating and the identifier check can be asserted
// directly rather than inferred from a migration run.
func recordingExec(out *[]string) func(context.Context, string, ...any) (sql.Result, error) {
	return func(_ context.Context, query string, _ ...any) (sql.Result, error) {
		*out = append(*out, query)
		return nil, nil
	}
}

func TestTenantScopingEmitsEnableForceAndPolicy(t *testing.T) {
	var got []string
	err := applyTenantScoping(context.Background(), recordingExec(&got),
		DialectPostgres, []string{"kv_store"})
	if err != nil {
		t.Fatalf("applyTenantScoping: %v", err)
	}

	joined := strings.Join(got, "\n")
	for _, want := range []string{
		"ALTER TABLE kv_store ENABLE ROW LEVEL SECURITY",
		// Without FORCE the policy is silently bypassed for the table's
		// owner, which is whoever ran the migration -- so a suite connecting
		// as the owner would pass against an unprotected table.
		"ALTER TABLE kv_store FORCE ROW LEVEL SECURITY",
		// TWO policies now, not the single cleat.tenant_row_is_visible CASE
		// #1278 introduced. That CASE answered for both callers with one
		// predicate, and a CASE cannot become an Index Cond: measured on
		// 400000 rows over 400 tenants, 603.654 ms with 399000 rows removed
		// by the filter, against 0.415 ms for the equality. cleat#1490.
		"CREATE POLICY kv_store_tenant_isolation ON kv_store FOR ALL TO PUBLIC " +
			"USING (tenant_id = cleat.assert_tenant_set())",
		// TO PUBLIC and not a role: per-tenant roles are NOINHERIT
		// (001_schema.sql), and a NOINHERIT member does not match a
		// `TO <role>` policy -- measured, such a role reads 0 rows without
		// raising, which is the failure assert_tenant_set exists to prevent.
		"CREATE POLICY kv_store_cross_tenant ON kv_store FOR ALL TO cleat_sweep USING (true)",
		// The sweep role is entered with SET LOCAL ROLE and so needs
		// privileges of its own; membership does not lend them.
		"GRANT SELECT, INSERT, UPDATE, DELETE ON kv_store TO cleat_sweep",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("emitted DDL does not contain %q:\n%s", want, joined)
		}
	}
}

// The policy is replaced rather than added to. Without the DROP, re-running a
// migration against a database that already has the policy fails on the
// duplicate name, which would make the whole migration un-rerunnable.
func TestTenantScopingDropsAnExistingPolicyFirst(t *testing.T) {
	var got []string
	if err := applyTenantScoping(context.Background(), recordingExec(&got),
		DialectPostgres, []string{"kv_store"}); err != nil {
		t.Fatalf("applyTenantScoping: %v", err)
	}

	dropAt, createAt := -1, -1
	for i, stmt := range got {
		if strings.HasPrefix(stmt, "DROP POLICY") {
			dropAt = i
		}
		if strings.HasPrefix(stmt, "CREATE POLICY") {
			createAt = i
		}
	}
	if dropAt < 0 || createAt < 0 {
		t.Fatalf("expected a DROP POLICY and a CREATE POLICY, got %v", got)
	}
	if dropAt > createAt {
		t.Errorf("DROP POLICY is emitted after CREATE POLICY (%d > %d)", dropAt, createAt)
	}
}

// MySQL has no row-level security, so it emits nothing and always will.
//
// THIS TEST USED TO COVER SQL SERVER TOO, and its own comment said what to do
// when that changed -- "if either ever gains an arm, this test is where it is
// declared". cleat#1552 gave SQL Server an arm, and this failing is how the
// change announced itself, which is the guard working. The SQL Server side now
// has its own assertions in
// a_plugin_table_has_a_policy_on_sql_server_test.go: what the statements say,
// and what they do against a real server.
//
// The MySQL half is kept as a SEPARATE test rather than folded in, because it
// is a different claim. MySQL emits nothing because the database cannot express
// the policy at all; SQL Server used to emit nothing because nobody had written
// the arm. The first is permanent and the second was a gap, and a single test
// covering both made them look like one fact.
func TestTenantScopingEmitsNothingOnMySQL(t *testing.T) {
	var got []string
	if err := applyTenantScoping(context.Background(), recordingExec(&got),
		DialectMySQL, []string{"kv_store"}); err != nil {
		t.Fatalf("applyTenantScoping: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("emitted %d statements on mysql, want none:\n%s",
			len(got), strings.Join(got, "\n"))
	}
}

// And the control for the test above: it must be asserting something MySQL
// specific rather than "applyTenantScoping never emits anything". If SQL Server
// ever went quiet again the arm would be gone, and the MySQL assertion alone
// would still pass.
func TestTenantScopingDoesEmitOnSQLServer(t *testing.T) {
	var got []string
	if err := applyTenantScoping(context.Background(), recordingExec(&got),
		DialectMSSQL, []string{"kv_store"}); err != nil {
		t.Fatalf("applyTenantScoping: %v", err)
	}
	if len(got) == 0 {
		t.Error("applyTenantScoping emitted nothing on mssql; the SQL Server arm " +
			"(cleat#1552) has gone missing, and every plugin table there is unprotected")
	}
}

// A table name is interpolated into DDL rather than bound, so a name that is
// not a plain identifier must stop the migration instead of being executed.
// The names come from plugin source, not from a request -- this exists so that
// a mistake surfaces as an error naming the table.
func TestTenantScopingRefusesATableNameThatIsNotAnIdentifier(t *testing.T) {
	for _, bad := range []string{
		"kv_store; DROP TABLE workflow_instances",
		"kv store",
		`"kv_store"`,
		"1kv_store",
		"",
	} {
		t.Run(bad, func(t *testing.T) {
			var got []string
			err := applyTenantScoping(context.Background(), recordingExec(&got),
				DialectPostgres, []string{bad})
			if err == nil {
				t.Fatalf("accepted %q and emitted %v", bad, got)
			}
			if len(got) != 0 {
				t.Errorf("refused %q but had already emitted %v", bad, got)
			}
		})
	}
}

func TestTenantScopingAcceptsOrdinaryTableNames(t *testing.T) {
	for _, ok := range []string{"kv_store", "KvStore", "t1", "a_b_c_9"} {
		if !isPlainIdentifier(ok) {
			t.Errorf("rejected the ordinary table name %q", ok)
		}
	}
}
