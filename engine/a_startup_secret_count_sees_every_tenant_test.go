package engine

// TestCountSecretsSeesEveryTenantsRowsOnEveryDialect is the regression test for
// cleat#2123, on all three dialects.
//
// CountSecrets is the worker's startup question "does this deployment hold any
// secrets at all", asked before any request exists. It used to run one
// unscoped read under plugin.AcrossAllTenants, and that read could not see
// tenant_secrets on two of three dialects: PostgreSQL raised
// (cleat.assert_tenant_set never reads the cross-tenant marker) and SQL Server
// returned zero rows without an error (fn_tenant_filter's default form ignores
// it). Its only caller read an error as "cannot tell" and a zero as "none", so
// the check went quiet exactly where it had something to say.
//
// WHAT MAKES THIS TEST ABLE TO FAIL, which is the point of each choice:
//
//   - PostgreSQL runs as cleat_app, the role that ships. A superuser bypasses
//     row-level security entirely, so on the connection the other tests use the
//     original implementation returned the right answer for the wrong reason
//     -- the same empty green as connecting as a superuser that CLAUDE.md
//     records for read-side policy assertions.
//   - One row belongs to a SUSPENDED tenant. TenantLister.ListTenantIDs skips
//     suspended tenants on purpose, so reusing it would pass every check that
//     seeds only live tenants and still miss ciphertext that needs a key.
//   - The assertion is a DELTA against what the same function returned before
//     seeding, so rows left by another test cannot make a broken read look
//     right or a right one look broken.
//
// MySQL is single-tenant by constraint, so it can hold only the default
// tenant's row; the suspended-tenant half applies to the other two.

import (
	"context"
	"database/sql"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/internal/tenantctx"
	"github.com/google/uuid"
)

func TestCountSecretsSeesEveryTenantsRowsOnEveryDialect(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMySQL, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			owner := testutil.TestDB(t, dialect)
			t.Cleanup(func() { owner.Close() })
			testutil.SetupFullSchema(t, owner, dialect)

			// The store under test connects the way a worker does.
			workerDB := owner
			if dialect == testutil.DialectPostgres {
				applyPostgresProcedures(t, owner)
				applyAppRoleMigration(t, owner)
				workerDB = appRoleDB(t, owner)
			}
			master := testMaster(t)
			counter, err := NewSecretStore(workerDB, string(dialect), master)
			if err != nil {
				t.Fatalf("NewSecretStore: %v", err)
			}
			// Seeding goes through the owner connection, which is allowed to
			// write; the row is what matters, not who wrote it.
			writer, err := NewSecretStore(owner, string(dialect), master)
			if err != nil {
				t.Fatalf("NewSecretStore (writer): %v", err)
			}

			def := uuid.MustParse(DefaultTenantUUID)
			type seed struct {
				tenant uuid.UUID
				name   string
			}
			seeds := []seed{{def, "cleat-2123-default-tenant"}}

			if dialect != testutil.DialectMySQL {
				suspended := uuid.New()
				ins := map[testutil.Dialect]string{
					testutil.DialectPostgres: `INSERT INTO admin.tenants (tenant_id, name, suspended) VALUES ($1, $2, true)`,
					testutil.DialectMSSQL:    `INSERT INTO admin.tenants (tenant_id, name, suspended) VALUES (@p1, @p2, 1)`,
				}[dialect]
				if _, err := owner.Exec(ins, suspended.String(), "cleat-2123-"+suspended.String()[:8]); err != nil {
					t.Fatalf("seeding a suspended tenant: %v", err)
				}
				seeds = append(seeds, seed{suspended, "cleat-2123-suspended-tenant"})
			}

			for _, sd := range seeds {
				deleteSecretRowForTest(t, owner, dialect, sd.tenant, sd.name)
			}
			before, err := counter.CountSecrets(context.Background())
			if err != nil {
				t.Fatalf("CountSecrets before seeding: %v", err)
			}

			for _, sd := range seeds {
				// A row left by an earlier run turns PutSecret's insert into an
				// update, the count does not move, and the delta below reads as a
				// broken CountSecrets. Remove it first.
				deleteSecretRowForTest(t, owner, dialect, sd.tenant, sd.name)
				ctx := tenantctx.With(context.Background(), sd.tenant)
				// Registered first so it runs last, wherever this test fails.
				t.Cleanup(func() {
					deleteSecretRowForTest(t, owner, dialect, sd.tenant, sd.name)
					if sd.tenant != def {
						// The foreign key cascades the tenant's rows with it.
						owner.ExecContext(ctx, deleteTenantStmtForTest(dialect), sd.tenant.String()) //nolint:errcheck // best-effort cleanup
					}
				})
				if err := writer.PutSecret(ctx, sd.tenant.String(), sd.name, "sk-live-"+sd.name); err != nil {
					t.Fatalf("PutSecret %s: %v", sd.name, err)
				}
			}

			got, err := counter.CountSecrets(context.Background())
			if err != nil {
				t.Fatalf("CountSecrets: %v -- an error here is the PostgreSQL half of cleat#2123", err)
			}
			if want := before + len(seeds); got != want {
				t.Fatalf("CountSecrets = %d, want %d (%d before + %d seeded): a startup check that "+
					"reads fewer rows than exist is the SQL Server half of cleat#2123",
					got, want, before, len(seeds))
			}
		})
	}
}

func deleteTenantStmtForTest(dialect testutil.Dialect) string {
	switch dialect {
	case testutil.DialectMSSQL:
		return `DELETE FROM admin.tenants WHERE tenant_id = @p1`
	case testutil.DialectMySQL:
		return `DELETE FROM tenants WHERE tenant_id = ?`
	default:
		return `DELETE FROM admin.tenants WHERE tenant_id = $1`
	}
}

// deleteSecretRowForTest removes one secret row, and must be able to.
//
// On SQL Server the row is invisible unless SESSION_CONTEXT('tenant_id') carries
// its tenant, and database/sql may give each statement a different pooled
// connection -- so the setting and the DELETE share one *sql.Conn. A plain
// db.ExecContext does neither: the filter hides the row, the DELETE affects zero
// rows, and it reports success. That is how this test's own first version left a
// row behind on every run and skewed the next run's delta.
func deleteSecretRowForTest(t *testing.T, db *sql.DB, dialect testutil.Dialect, tenant uuid.UUID, name string) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Errorf("cleanup: acquire a connection: %v", err)
		return
	}
	defer conn.Close()
	if dialect == testutil.DialectMSSQL {
		if _, err := conn.ExecContext(ctx,
			`EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, tenant.String()); err != nil {
			t.Errorf("cleanup: scope the connection to tenant %s: %v", tenant, err)
			return
		}
	}
	if _, err := conn.ExecContext(ctx, deleteSecretStmtForTest(dialect), tenant.String(), name); err != nil {
		t.Errorf("cleanup: delete %q for tenant %s: %v", name, tenant, err)
	}
}
