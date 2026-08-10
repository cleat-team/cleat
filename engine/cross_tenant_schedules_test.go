package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/cleat-team/cleat/engine/testutil"
)

// PostgreSQL-only, for the same reason cross_tenant_claim_test.go is:
// admin.get_due_schedules (migrations/postgres/024_cross_tenant_schedules.sql)
// exists on no other dialect.

// seedSchedule inserts one due, enabled schedule directly through adminDB.
//
// Raw SQL rather than CreateSchedule, because CreateSchedule only ever writes
// the calling store's own tenant_id and these fixtures need to belong to
// tenants the store under test is NOT scoped to -- which is the entire point of
// the contrast below.
func seedSchedule(t *testing.T, adminDB *sql.DB, name, tenantID string) {
	t.Helper()
	if _, err := adminDB.Exec(`
		INSERT INTO workflow_schedules
			(name, def_name, entry_point, cron_expression, input, enabled,
			 next_run_at, timezone, tenant_id, misfire_policy, catch_up_limit, overlap_policy)
		VALUES ($1, $2, 'run', '* * * * *', '{}', true, $3, 'UTC', $4, 'catch_up', 0, 'allow')
	`, name, xtcDefName, time.Now().Add(-time.Minute).UTC(), tenantID); err != nil {
		t.Fatalf("seed schedule %s: %v", name, err)
	}
}

func scheduleNames(schedules []Schedule) []string {
	out := make([]string, len(schedules))
	for i, s := range schedules {
		out[i] = s.Name
	}
	return out
}

// TestGetDueSchedulesAcrossTenants_SeesAllTenants is the property migration 024
// exists for, proven by contrast rather than by trusting either half alone.
//
// A tenant-scoped GetDueSchedules can only ever see its own tenant's due
// schedules -- that is what RLS is for -- so a non-default tenant's cron never
// fires, because the loop that would fire it cannot see it. That is recorded in
// TestScheduleLoop_OnlySeesItsOwnTenantsSchedules, which asserts the isolation
// and must keep passing: this widens one read, it does not remove the policy.
func TestGetDueSchedulesAcrossTenants_SeesAllTenants(t *testing.T) {
	adminDB, appDB, teardown := crossTenantClaimDB(t)
	defer teardown()
	deployCrossTenantDef(t, adminDB)
	ctx := context.Background()

	seedSchedule(t, adminDB, "xts-a", xtcTenantA)
	seedSchedule(t, adminDB, "xts-b", xtcTenantB)

	store := NewPostgresStore(appDB)

	across, err := store.GetDueSchedulesAcrossTenants(ctx)
	if err != nil {
		t.Fatalf("GetDueSchedulesAcrossTenants: %v", err)
	}
	got := map[string]string{}
	for _, s := range across {
		got[s.Name] = s.TenantID
	}
	if _, ok := got["xts-a"]; !ok {
		t.Errorf("cross-tenant read returned %v, missing xts-a", scheduleNames(across))
	}
	if _, ok := got["xts-b"]; !ok {
		t.Errorf("cross-tenant read returned %v, missing xts-b", scheduleNames(across))
	}
	// The field the whole design turns on: cmd/cleat-worker re-scopes the
	// firing to this before it starts the run or advances the schedule, so a
	// wrong value fires one tenant's schedule under another's isolation.
	if got["xts-a"] != xtcTenantA {
		t.Errorf("xts-a TenantID = %q, want %q", got["xts-a"], xtcTenantA)
	}
	if got["xts-b"] != xtcTenantB {
		t.Errorf("xts-b TenantID = %q, want %q", got["xts-b"], xtcTenantB)
	}

	// The other half. Without this a read that returned everything to everyone
	// would satisfy the assertions above, and the isolation this feature is
	// carefully stepping around would already be gone.
	scoped := NewPostgresStore(appDB).WithTenant(xtcTenantA)
	only, err := scoped.GetDueSchedules(ctx)
	if err != nil {
		t.Fatalf("GetDueSchedules (tenant-scoped): %v", err)
	}
	for _, s := range only {
		if s.TenantID != xtcTenantA {
			t.Errorf("tenant-scoped GetDueSchedules returned %s owned by %s; RLS is not holding",
				s.Name, s.TenantID)
		}
	}
	if len(only) != 1 || only[0].Name != "xts-a" {
		t.Errorf("tenant-scoped read returned %v, want exactly [xts-a]", scheduleNames(only))
	}
}

// TestGetDueSchedulesAcrossTenants_UngrantedDeploymentFallsBack is the schedule
// half of the provisioning story, and it is deliberately separate from the
// claim's: 023 and 024 are different migrations, so a deployment can have the
// cross-tenant claim and not this. The sentinel is what lets cmd/cleat-worker
// fall back to its own tenant's schedules and warn, instead of failing the
// schedule loop on every tick because a GRANT is missing.
func TestGetDueSchedulesAcrossTenants_UngrantedDeploymentFallsBack(t *testing.T) {
	adminDB, appDB, teardown := crossTenantClaimDB(t)
	defer teardown()
	deployCrossTenantDef(t, adminDB)
	ctx := context.Background()

	seedSchedule(t, adminDB, "xts-ungranted", xtcTenantA)
	store := NewPostgresStore(appDB)

	// With the grant in place it works. Without this, a revoke that silently
	// did nothing would leave the assertion below passing for the wrong reason.
	if _, err := store.GetDueSchedulesAcrossTenants(ctx); err != nil {
		t.Fatalf("with EXECUTE granted the read should work, got: %v", err)
	}

	if _, err := adminDB.Exec(`REVOKE EXECUTE ON FUNCTION admin.get_due_schedules() FROM ` +
		testutil.PostgresRLSTestRole); err != nil {
		t.Fatalf("revoke EXECUTE: %v", err)
	}
	// defer, not t.Cleanup: t.Cleanup runs after this function's defers, and
	// teardown has closed adminDB by then. The grant is database state and
	// would leak into whatever runs next.
	defer func() {
		if _, err := adminDB.Exec(`GRANT EXECUTE ON FUNCTION admin.get_due_schedules() TO ` +
			testutil.PostgresRLSTestRole); err != nil {
			t.Errorf("restoring the EXECUTE grant failed, so the next test starts from a "+
				"revoked one: %v", err)
		}
	}()

	_, err := store.GetDueSchedulesAcrossTenants(ctx)
	if !errors.Is(err, ErrCrossTenantClaimUnsupported) {
		t.Fatalf("error is %v, want ErrCrossTenantClaimUnsupported -- without that sentinel the "+
			"schedule loop fails every tick on a missing GRANT instead of falling back to its "+
			"own tenant", err)
	}
	// The remediation must name 024, not 023. They are separate grants and an
	// operator handed the wrong filename applies a migration they already have.
	if !strings.Contains(err.Error(), "024_cross_tenant_schedules.sql") {
		t.Errorf("error does not name the migration that fixes it: %v", err)
	}
	if strings.Contains(err.Error(), "023_cross_tenant_claim.sql") {
		t.Errorf("error names 023, which grants a different function: %v", err)
	}
}

// TestCrossTenantScheduleProvisioningGap_MapsBothCodes pins the SQLSTATEs,
// including the negative case: treating an unanticipated error as a
// provisioning gap would silently downgrade a real failure into "fall back to
// one tenant", which is the class of bug this mechanism exists to prevent.
func TestCrossTenantScheduleProvisioningGap_MapsBothCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"undefined_function: migration 024 never applied", &pq.Error{Code: "42883"}, true},
		{"insufficient_privilege: EXECUTE never granted", &pq.Error{Code: "42501"}, true},
		{"deadlock: a real failure, must propagate", &pq.Error{Code: "40P01"}, false},
		{"not a pq error at all", errors.New("connection refused"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := crossTenantScheduleProvisioningGap(tc.err) != ""; got != tc.want {
				t.Errorf("treated as provisioning gap = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGetDueSchedulesAcrossTenants_ColumnsMatchTheGoScan is the drift guard.
//
// The column list lives in a migration and the scan lives in Go, and nothing
// but this connects them. A reordering there would not fail to compile; it
// would put the timezone in Timezone's neighbour and fire schedules in the
// wrong zone, or -- worse -- put a tenant in the wrong field and route a
// firing to the wrong tenant.
func TestGetDueSchedulesAcrossTenants_ColumnsMatchTheGoScan(t *testing.T) {
	adminDB, appDB, teardown := crossTenantClaimDB(t)
	defer teardown()
	deployCrossTenantDef(t, adminDB)
	ctx := context.Background()

	if _, err := adminDB.Exec(`
		INSERT INTO workflow_schedules
			(name, def_name, entry_point, cron_expression, input, enabled,
			 next_run_at, last_run_at, timezone, tenant_id, misfire_policy,
			 catch_up_limit, overlap_policy, last_run_id)
		VALUES ('xts-cols', $1, 'my-entry', '5 4 * * *', '{"depth":2}', true,
		        $2, $3, 'Europe/Berlin', $4, 'skip', 7, 'skip', 'run-xyz')
	`, xtcDefName, time.Now().Add(-time.Minute).UTC(), time.Now().Add(-time.Hour).UTC(),
		xtcTenantB); err != nil {
		t.Fatalf("seed distinguishable schedule: %v", err)
	}

	across, err := NewPostgresStore(appDB).GetDueSchedulesAcrossTenants(ctx)
	if err != nil {
		t.Fatalf("GetDueSchedulesAcrossTenants: %v", err)
	}
	var got *Schedule
	for i := range across {
		if across[i].Name == "xts-cols" {
			got = &across[i]
		}
	}
	if got == nil {
		t.Fatalf("seeded schedule not returned; got %v", scheduleNames(across))
	}

	// Every field distinct, so a swapped pair cannot pass.
	for _, c := range []struct {
		field string
		got   any
		want  any
	}{
		{"DefName", got.DefName, xtcDefName},
		{"EntryPoint", got.EntryPoint, "my-entry"},
		{"CronExpression", got.CronExpression, "5 4 * * *"},
		{"Enabled", got.Enabled, true},
		{"Timezone", got.Timezone, "Europe/Berlin"},
		{"TenantID", got.TenantID, xtcTenantB},
		{"MisfirePolicy", got.MisfirePolicy, "skip"},
		{"CatchUpLimit", got.CatchUpLimit, 7},
		{"OverlapPolicy", got.OverlapPolicy, "skip"},
		{"LastRunID", got.LastRunID, "run-xyz"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v -- the migration's column order and the Go scan disagree",
				c.field, c.got, c.want)
		}
	}
	if got.LastRunAt == nil {
		t.Error("LastRunAt is nil; a non-null last_run_at did not reach the scan")
	}
	var input map[string]int
	if err := json.Unmarshal(got.Input, &input); err != nil || input["depth"] != 2 {
		t.Errorf("Input = %s, want {\"depth\":2}: %v", got.Input, err)
	}
}
