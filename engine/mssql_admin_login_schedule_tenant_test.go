package engine

// The SQL Server tenant filter is disabled for the whole connection when the
// login is a member of dbo.cleat_admin, and four schedule statements have no
// tenant predicate of their own. On a multi-tenant SQL Server deployment those
// two facts meet, and one tenant can delete, disable and reschedule another
// tenant's cron schedules over the ordinary HTTP API.
//
// Why the login is a cleat_admin member rather than might be. A non-default
// tenant's workflows only run if the dispatch loop can see across tenants:
// that is what migrations 023 and 024 exist for, and on SQL Server the
// exemption is IS_ROLEMEMBER(N'cleat_admin') = 1 inside dbo.fn_tenant_filter
// itself (012_admin_role.sql). GetDueSchedulesAcrossTenants and
// ClaimReadyAcrossTenants both call requireCleatAdminMembership and fail
// loudly without it. So a working multi-tenant SQL Server deployment has
// granted the role, and MSSQLStore.WithTenant returns a copy sharing s.db --
// one pool, one login. Every tenant-scoped store the worker hands a request is
// therefore unfiltered.
//
// This is where SQL Server and PostgreSQL diverge, and it is worth naming
// because the PostgreSQL side got it right. There the exemption is a separate
// role, cleat_dispatcher, owning a SECURITY DEFINER function; the application
// role keeps BYPASSRLS off, so widening is scoped to one function body. SQL
// Server puts the exemption in the predicate, keyed on the connection's role
// membership, and a predicate cannot tell which function is asking.
// mssql_schedules.go records "a connection either sees across tenants for both
// or for neither" as a simplification over PostgreSQL's two grants. It is the
// same fact these tests read as a hazard.
//
// Scoped to schedules deliberately. The same reasoning covers every SQL Server
// statement that leans on the policy instead of carrying a predicate; that
// sweep is IMPROVEMENT-PLAN 3.86, and one PR does one thing.
//
// Independent of 3.77 and true today, which is why it is not folded into the
// key change. It gets worse there: under PRIMARY KEY (name) a name-only
// statement at least matches one row, so an attacker has to name a schedule
// that exists. Under (tenant_id, name) two tenants can hold "nightly-report"
// and a single unqualified DELETE takes both.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// adminLoginSchedulingStores returns two tenant-scoped stores over one
// cleat_admin pool: the shape a multi-tenant SQL Server worker actually runs.
//
// Note what this does NOT do. It does not grant anything, weaken a policy or
// reach past the store API. The connection is the one the product requires for
// cross-tenant dispatch and the calls are the ones the HTTP handler makes
// (cmd/cleat-worker/server.go:993-1005, through scopedStore).
func adminLoginSchedulingStores(t *testing.T) (a, b *MSSQLStore) {
	t.Helper()
	backend := &MSSQLBackend{}
	if !backend.Enabled() {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, db)
	testutil.CleanupMSSQLTestData(t, db)
	t.Cleanup(func() {
		testutil.CleanupMSSQLTestData(t, db)
		db.Close()
	})

	adminPool := testutil.MSSQLAdminDB(t, db)

	// Fatal, not Skip, and scripts/check-skips.sh was right to insist.
	//
	// MSSQLAdminDB returns the plain pool unchanged only when the schema
	// carries no security policies -- and SetupMSSQLFullSchema above applies
	// the shipped migrations and then runs requireMSSQLPoliciesIntact, which
	// already fails when they are missing. So by the time this line runs an
	// admin pool is guaranteed, and MSSQLAdminDB t.Fatalf's itself if the GRANT
	// did not take. That makes this the guard's case (c): a precondition always
	// satisfiable in this repo, which must not be a skip.
	//
	// It matters more here than the rule alone suggests. On a filtered
	// connection every assertion in this file passes trivially, because the
	// policy does the work the missing predicates would otherwise expose. A
	// skip here would not merely hide a failure, it would hide the fact that
	// the test had stopped testing anything.
	var isMember int
	if err := adminPool.QueryRow(`SELECT ISNULL(IS_ROLEMEMBER(N'cleat_admin'), 0)`).Scan(&isMember); err != nil {
		t.Fatalf("check cleat_admin membership: %v", err)
	}
	if isMember != 1 {
		t.Fatalf("this pool reads IS_ROLEMEMBER('cleat_admin') = %d, so dbo.fn_tenant_filter "+
			"still applies and every assertion in this file would pass without measuring "+
			"anything; SetupMSSQLFullSchema should have made this impossible", isMember)
	}

	base := NewMSSQLStore(adminPool)
	return base.WithTenant(unscopedTenantA), base.WithTenant(unscopedTenantB)
}

func mustCreateSchedule(t *testing.T, s *MSSQLStore, name string) {
	t.Helper()
	if err := s.CreateSchedule(context.Background(), Schedule{
		Name:           name,
		DefName:        "some-workflow",
		CronExpression: "0 3 * * *",
		Input:          json.RawMessage(`{}`),
		Enabled:        true,
		NextRunAt:      time.Now().Add(time.Hour).UTC(),
		Timezone:       "UTC",
	}); err != nil {
		t.Fatalf("CreateSchedule(%s): %v", name, err)
	}
}

// scheduleNamed returns tenant A's schedule of that name, or nil.
func scheduleNamed(t *testing.T, s *MSSQLStore, name string) *Schedule {
	t.Helper()
	all, err := s.ListSchedules(context.Background())
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i]
		}
	}
	return nil
}

// TestAdminLoginDeleteScheduleCannotCrossTenants — the destructive one.
//
// The names differ, so this does not depend on 3.77's key change at all: B is
// naming a schedule it does not own and the statement finds it anyway.
func TestAdminLoginDeleteScheduleCannotCrossTenants(t *testing.T) {
	storeA, storeB := adminLoginSchedulingStores(t)
	const name = "tenant-a-nightly-report"
	mustCreateSchedule(t, storeA, name)

	if scheduleNamed(t, storeA, name) == nil {
		t.Fatalf("fixture is broken: tenant A cannot see the schedule it just created")
	}

	if err := storeB.DeleteSchedule(context.Background(), name); err != nil {
		t.Fatalf("tenant B DeleteSchedule: %v", err)
	}

	if scheduleNamed(t, storeA, name) == nil {
		t.Errorf("tenant B deleted tenant A's schedule %q over the ordinary store API; "+
			"a cron schedule that silently stops firing is the kind of outage nobody "+
			"attributes to another tenant", name)
	}
}

// TestAdminLoginSetScheduleEnabledCannotCrossTenants — quieter than deletion
// and therefore worse to diagnose: the row is still listed, still shows a
// next_run_at, and never fires.
func TestAdminLoginSetScheduleEnabledCannotCrossTenants(t *testing.T) {
	storeA, storeB := adminLoginSchedulingStores(t)
	const name = "tenant-a-billing-sweep"
	mustCreateSchedule(t, storeA, name)

	if err := storeB.SetScheduleEnabled(context.Background(), name, false); err != nil {
		t.Fatalf("tenant B SetScheduleEnabled: %v", err)
	}

	got := scheduleNamed(t, storeA, name)
	if got == nil {
		t.Fatalf("tenant A's schedule %q disappeared entirely", name)
	}
	if !got.Enabled {
		t.Errorf("tenant B disabled tenant A's schedule %q", name)
	}
}

// TestAdminLoginUpdateScheduleNextRunCannotCrossTenants — the same statement
// the scheduler loop uses to advance a firing, pointed at a name it does not
// own. Moving another tenant's next_run_at backwards fires their workflow
// early; forwards suppresses it.
func TestAdminLoginUpdateScheduleNextRunCannotCrossTenants(t *testing.T) {
	storeA, storeB := adminLoginSchedulingStores(t)
	const name = "tenant-a-reconcile"
	mustCreateSchedule(t, storeA, name)

	before := scheduleNamed(t, storeA, name)
	if before == nil {
		t.Fatalf("fixture is broken: tenant A cannot see %q", name)
	}

	moved := before.NextRunAt.Add(72 * time.Hour)
	if err := storeB.UpdateScheduleNextRun(context.Background(), name, moved); err != nil {
		t.Fatalf("tenant B UpdateScheduleNextRun: %v", err)
	}

	after := scheduleNamed(t, storeA, name)
	if after == nil {
		t.Fatalf("tenant A's schedule %q disappeared entirely", name)
	}
	if !after.NextRunAt.Equal(before.NextRunAt) {
		t.Errorf("tenant B moved tenant A's schedule %q from %s to %s",
			name, before.NextRunAt.UTC(), after.NextRunAt.UTC())
	}
}

// TestAdminLoginGetDueSchedulesStaysWithinItsTenant covers the read.
//
// GetDueSchedules is the tenant-scoped method -- the one whose whole contract
// is "my tenant's due schedules". GetDueSchedulesAcrossTenants is the
// deliberate cross-tenant read and is not under test here. If the scoped one
// answers with every tenant's rows on this connection, a worker configured for
// per-tenant dispatch fires other tenants' workflows under its own tenant.
func TestAdminLoginGetDueSchedulesStaysWithinItsTenant(t *testing.T) {
	storeA, storeB := adminLoginSchedulingStores(t)
	ctx := context.Background()

	due := Schedule{
		Name: "tenant-a-due-now", DefName: "some-workflow",
		CronExpression: "* * * * *", Input: json.RawMessage(`{}`), Enabled: true,
		NextRunAt: time.Now().Add(-time.Minute).UTC(), Timezone: "UTC",
	}
	if err := storeA.CreateSchedule(ctx, due); err != nil {
		t.Fatalf("tenant A CreateSchedule: %v", err)
	}

	got, err := storeB.GetDueSchedules(ctx)
	if err != nil {
		t.Fatalf("tenant B GetDueSchedules: %v", err)
	}
	for _, s := range got {
		if s.Name == due.Name {
			t.Errorf("tenant B's GetDueSchedules returned tenant A's schedule %q "+
				"(tenant_id %s); the scoped reader is answering across tenants",
				s.Name, s.TenantID)
		}
	}
}

// TestAdminLoginClaimDueScheduleCannotCrossTenants covers the compare-and-swap
// the scheduler loop uses to make delivery at-least-once.
//
// Not reachable from the HTTP API the way the three above are, so it is the
// weakest of the five on today's schema -- but it is the one 3.77 makes worst.
// The loop reads a due schedule cross-tenant, re-scopes to Schedule.TenantID
// and claims by name; once two tenants can hold "nightly-report" that claim
// advances whichever row the unqualified predicate reaches first, and the CAS
// on next_run_at is what the delivery guarantee rests on.
func TestAdminLoginClaimDueScheduleCannotCrossTenants(t *testing.T) {
	storeA, storeB := adminLoginSchedulingStores(t)
	ctx := context.Background()
	const name = "tenant-a-claimable"
	mustCreateSchedule(t, storeA, name)

	before := scheduleNamed(t, storeA, name)
	if before == nil {
		t.Fatalf("fixture is broken: tenant A cannot see %q", name)
	}

	claimed, err := storeB.ClaimDueSchedule(ctx, name, before.NextRunAt,
		before.NextRunAt.Add(24*time.Hour), "run-from-tenant-b")
	if err != nil {
		t.Fatalf("tenant B ClaimDueSchedule: %v", err)
	}
	if claimed {
		t.Errorf("tenant B claimed tenant A's schedule %q and was told the claim succeeded", name)
	}

	after := scheduleNamed(t, storeA, name)
	if after == nil {
		t.Fatalf("tenant A's schedule %q disappeared entirely", name)
	}
	if !after.NextRunAt.Equal(before.NextRunAt) {
		t.Errorf("tenant B advanced tenant A's schedule %q from %s to %s",
			name, before.NextRunAt.UTC(), after.NextRunAt.UTC())
	}
}
