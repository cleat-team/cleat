package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// cleat#3552, B3a. A provisioner may suspend and resume the tenants its principal created and no others.
// This is the store half; the routes and the credential matrix are cmd/cleat-worker's. It is measured
// against the table, not the return value: the question is whether admin.tenants.suspended changed.

func TestAProvisionerSuspendsAndResumesOnlyTheTenantsItsPrincipalCreated(t *testing.T) {
	for _, dialect := range []string{DialectPostgres, DialectMSSQL} {
		dialect := dialect
		t.Run(dialect, func(t *testing.T) {
			f := newProvisionFixture(t, dialect)
			ctx := context.Background()

			mine, err := f.provision(t, ProvisionRequest{TenantID: uuid.New(), Name: "b3a-mine-" + uuid.NewString(), OrgName: "b3a-org-" + uuid.NewString(), Creator: "clew-service"})
			if err != nil {
				t.Fatal(err)
			}
			theirs, err := f.provision(t, ProvisionRequest{TenantID: uuid.New(), Name: "b3a-theirs-" + uuid.NewString(), OrgName: "b3a-org-" + uuid.NewString(), Creator: "other-service"})
			if err != nil {
				t.Fatal(err)
			}
			oid, err := f.ts.CreateOrg(ctx, "b3a-offline-org-"+uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			f.orgs = append(f.orgs, oid)
			offline, err := f.ts.CreateTenant(ctx, "b3a-offline-"+uuid.NewString(), "", oid)
			if err != nil {
				t.Fatal(err)
			}
			f.tenants = append(f.tenants, offline)

			suspended := func(id uuid.UUID) bool {
				t.Helper()
				var v bool
				if err := f.db.QueryRow(`SELECT suspended FROM admin.tenants WHERE tenant_id = `+f.ph(1), id.String()).Scan(&v); err != nil {
					t.Fatal(err)
				}
				return v
			}
			if suspended(mine.Tenant.ID) || suspended(theirs.Tenant.ID) || suspended(offline) {
				t.Fatal("UNMEASURED: a fresh tenant starts suspended, so a suspend proves nothing")
			}

			// The known positive: its own tenant, both directions, and a repeat of each.
			for i, want := range []bool{true, true, false, false} {
				if err := f.ts.SetTenantSuspendedByCreator(ctx, mine.Tenant.ID, want, "clew-service"); err != nil {
					t.Fatalf("step %d: %v", i, err)
				}
				if got := suspended(mine.Tenant.ID); got != want {
					t.Errorf("step %d: suspended = %v, want %v", i, got, want)
				}
			}

			// Refusals, each leaving the tenant exactly as it was.
			for name, tc := range map[string]struct {
				id      uuid.UUID
				creator string
			}{
				"another principal's tenant":                        {theirs.Tenant.ID, "clew-service"},
				"an offline-made tenant":                            {offline, "clew-service"},
				"a tenant that does not exist":                      {uuid.New(), "clew-service"},
				"the right tenant, a different case of the creator": {mine.Tenant.ID, "Clew-Service"},
			} {
				err := f.ts.SetTenantSuspendedByCreator(ctx, tc.id, true, tc.creator)
				if !errors.Is(err, ErrTenantNotFound) {
					t.Errorf("%s: %v, want ErrTenantNotFound (one answer for all of them)", name, err)
				}
			}
			if suspended(theirs.Tenant.ID) || suspended(offline) || suspended(mine.Tenant.ID) {
				t.Errorf("a refused suspend changed a tenant (theirs=%v offline=%v mine=%v)", suspended(theirs.Tenant.ID), suspended(offline), suspended(mine.Tenant.ID))
			}

			// An empty creator is never "any creator".
			if err := f.ts.SetTenantSuspendedByCreator(ctx, mine.Tenant.ID, true, ""); err == nil || errors.Is(err, ErrTenantNotFound) {
				t.Errorf("an empty creator = %v, want a distinct error", err)
			}
			if suspended(mine.Tenant.ID) {
				t.Error("an empty creator suspended a tenant")
			}
		})
	}
}

func TestSuspendingByCreatorIsRefusedOnMySQL(t *testing.T) {
	ts := &TenantStore{dialect: DialectMySQL}
	if err := ts.SetTenantSuspendedByCreator(context.Background(), uuid.New(), true, "clew-service"); err == nil {
		t.Error("MySQL has no created_by column, so there is no creator to compare; it must refuse")
	}
}
