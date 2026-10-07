package engine

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

// AllowsPublicExposure answers for one tenant, and a tenant with no row is
// NOT opted in -- the same reasoning as IsTenantSuspended (admin.tenants is a
// registry a deployment can run without populating), but with the opposite
// stakes: here "absent means permissive" would be the exact fail-open default
// cleat#1986's owner decision refuses ("public ... is refused at deploy
// otherwise").
func TestATenantWithNoGrantMayNotDeployPublic(t *testing.T) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			owner := testutil.TestDB(t, dialect)
			testutil.SetupFullSchema(t, owner, dialect)
			if dialect == testutil.DialectPostgres {
				applyPostgresProcedures(t, owner)
				testutil.CleanupPostgresTestData(t, owner)
			}
			defer owner.Close()

			ctx := context.Background()
			granted, denied := uuid.NewString(), uuid.NewString()
			t.Cleanup(func() {
				_, _ = owner.Exec(deleteTenantsStmt(dialect), granted, denied)
			})
			for _, tc := range []struct {
				id      string
				name    string
				allowed bool
			}{
				{granted, "pub-grant-" + granted[:8], true},
				{denied, "pub-deny-" + denied[:8], false},
			} {
				if _, err := owner.ExecContext(ctx, insertTenantWithExposureStmt(dialect), tc.id, tc.name, tc.allowed); err != nil {
					t.Fatalf("seeding tenant %s: %v", tc.name, err)
				}
			}

			var store TenantExposurePolicyReader
			switch dialect {
			case testutil.DialectPostgres:
				store = NewPostgresStore(owner)
			case testutil.DialectMSSQL:
				store = NewMSSQLStore(owner)
			}

			for _, tc := range []struct {
				name   string
				tenant string
				want   bool
			}{
				{"explicitly granted", granted, true},
				{"explicitly denied", denied, false},
				{"no row at all", "11111111-1111-4111-8111-111111111111", false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					got, err := store.AllowsPublicExposure(ctx, tc.tenant)
					if err != nil {
						t.Fatalf("AllowsPublicExposure: %v", err)
					}
					if got != tc.want {
						t.Errorf("AllowsPublicExposure(%s) = %v, want %v", tc.tenant, got, tc.want)
					}
				})
			}
		})
	}
}

func insertTenantWithExposureStmt(dialect testutil.Dialect) string {
	if dialect == testutil.DialectMSSQL {
		return `INSERT INTO admin.tenants (tenant_id, name, allow_public_exposure) VALUES (@p1, @p2, @p3)`
	}
	return `INSERT INTO admin.tenants (tenant_id, name, allow_public_exposure) VALUES ($1, $2, $3)`
}

func deleteTenantsStmt(dialect testutil.Dialect) string {
	if dialect == testutil.DialectMSSQL {
		return `DELETE FROM admin.tenants WHERE tenant_id IN (@p1, @p2)`
	}
	return `DELETE FROM admin.tenants WHERE tenant_id IN ($1, $2)`
}
