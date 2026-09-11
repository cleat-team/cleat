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
		"cleat.assert_tenant_set()",
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

// MySQL has no row-level security and SQL Server scopes a tenant per pool, so
// both emit nothing. This asserts the documented gap rather than leaving it to
// be discovered: if either ever gains an arm, this test is where it is
// declared.
func TestTenantScopingEmitsNothingOnDialectsWithoutRowLevelSecurity(t *testing.T) {
	for _, dialect := range []Dialect{DialectMySQL, DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			var got []string
			if err := applyTenantScoping(context.Background(), recordingExec(&got),
				dialect, []string{"kv_store"}); err != nil {
				t.Fatalf("applyTenantScoping: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("emitted %d statements on %s, want none:\n%s",
					len(got), dialect, strings.Join(got, "\n"))
			}
		})
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
