package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestCheckCrossTenantCapability_ReportsAGrantedDeploymentAsAvailable is the
// false-positive half, and it comes first deliberately: every assertion below
// is about detecting a broken deployment, and a check that reported everything
// broken would satisfy all of them.
func TestCheckCrossTenantCapability_ReportsAGrantedDeploymentAsAvailable(t *testing.T) {
	_, appDB, teardown := crossTenantClaimDB(t)
	defer teardown()

	got := NewPostgresStore(appDB).CheckCrossTenantCapability(context.Background())
	if !got.Claim {
		t.Errorf("claim reported unavailable on a correctly provisioned database: %s", got.ClaimReason)
	}
	if !got.Schedules {
		t.Errorf("schedules reported unavailable on a correctly provisioned database: %s", got.SchedulesReason)
	}
}

// TestCheckCrossTenantCapability_DetectsAMissingGrant covers the failure the
// runtime paths already detect, to confirm the startup check agrees with them
// rather than reporting a rosier answer than the first tick will.
func TestCheckCrossTenantCapability_DetectsAMissingGrant(t *testing.T) {
	adminDB, appDB, teardown := crossTenantClaimDB(t)
	defer teardown()
	store := NewPostgresStore(appDB)

	if _, err := adminDB.Exec(`REVOKE EXECUTE ON FUNCTION admin.get_due_schedules() FROM ` +
		testutil.PostgresRLSTestRole); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	defer func() {
		if _, err := adminDB.Exec(`GRANT EXECUTE ON FUNCTION admin.get_due_schedules() TO ` +
			testutil.PostgresRLSTestRole); err != nil {
			t.Errorf("restoring the grant failed, leaking into the next test: %v", err)
		}
	}()

	got := store.CheckCrossTenantCapability(context.Background())
	if got.Schedules {
		t.Error("a revoked EXECUTE was reported as available")
	}
	if !strings.Contains(got.SchedulesReason, "024_cross_tenant_schedules.sql") {
		t.Errorf("reason does not name the migration that fixes it: %s", got.SchedulesReason)
	}
	// The two grants are independent, and reporting them together would send an
	// operator to fix something that is not broken.
	if !got.Claim {
		t.Errorf("revoking the schedule grant also reported the claim as unavailable: %s", got.ClaimReason)
	}
}

// TestCheckCrossTenantCapability_DetectsALostBypassRLS covers the failure mode
// with no useful runtime diagnosis.
//
// It DOES raise -- that was measured rather than assumed, and the first version
// of this test asserted the opposite, which is how the error in 023's header
// was found. 001_schema.sql's policies are fail-closed through
// assert_tenant_set(), and these functions are called outside beginTxWithRLS,
// so a caller with no exemption gets P0001 rather than a quietly narrowed
// result set.
//
// What P0001 does not do is say why. It names the unset tenant GUC, which is a
// symptom of a role having lost a privilege three layers away, and it does not
// map to the provisioning-gap sentinel, so it propagates as a hard error on
// every tick. The check turns that into a named cause before the first tick.
func TestCheckCrossTenantCapability_DetectsALostBypassRLS(t *testing.T) {
	adminDB, appDB, teardown := crossTenantClaimDB(t)
	defer teardown()
	deployCrossTenantDef(t, adminDB)
	ctx := context.Background()
	store := NewPostgresStore(appDB)

	seedSchedule(t, adminDB, "xtc-bypass", xtcTenantA)

	// The exemption is doing the work before anything is changed. Without this
	// the assertions below could pass on a database where the read never
	// worked.
	before, err := store.GetDueSchedulesAcrossTenants(ctx)
	if err != nil {
		t.Fatalf("GetDueSchedulesAcrossTenants: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("fixture did not reach the cross-tenant read; the rest of this test would be vacuous")
	}
	if got := store.CheckCrossTenantCapability(ctx); !got.Claim || !got.Schedules {
		t.Fatalf("check reported unavailable before anything was revoked: %+v", got)
	}

	if _, err := adminDB.Exec(`ALTER ROLE cleat_dispatcher NOBYPASSRLS`); err != nil {
		t.Fatalf("drop BYPASSRLS: %v", err)
	}
	defer func() {
		if _, err := adminDB.Exec(`ALTER ROLE cleat_dispatcher BYPASSRLS`); err != nil {
			t.Errorf("restoring BYPASSRLS failed, so every later cross-tenant test in this "+
				"package sees a broken exemption: %v", err)
		}
	}()

	// What the runtime actually does. Asserted, not assumed: if this ever
	// changes to a silent narrowing, the check below becomes the ONLY thing
	// that can detect the failure, and whoever changed it should be told here.
	_, rErr := store.GetDueSchedulesAcrossTenants(ctx)
	if rErr == nil {
		t.Fatal("the read succeeded with the exemption removed. That means the policies stopped " +
			"being fail-closed, the failure is now silent, and 023's header and " +
			"CheckCrossTenantCapability's doc comment both need rewriting")
	}
	if !strings.Contains(rErr.Error(), "cleat.tenant_id is not set") {
		t.Errorf("runtime error changed shape: %v", rErr)
	}
	// The point: that message names neither the function nor the attribute.
	if strings.Contains(rErr.Error(), "BYPASSRLS") {
		t.Errorf("the runtime error now names the cause, so this check is redundant: %v", rErr)
	}

	got := store.CheckCrossTenantCapability(ctx)
	if got.Claim {
		t.Error("claim reported available with the owner's BYPASSRLS removed")
	}
	if got.Schedules {
		t.Error("schedules reported available with the owner's BYPASSRLS removed")
	}
	for _, reason := range []string{got.ClaimReason, got.SchedulesReason} {
		if !strings.Contains(reason, "BYPASSRLS") {
			t.Errorf("reason does not name the actual cause, which is the only thing it adds "+
				"over the runtime error: %s", reason)
		}
		if !strings.Contains(reason, "ALTER ROLE cleat_dispatcher BYPASSRLS") {
			t.Errorf("reason does not carry the remediation: %s", reason)
		}
	}
}
