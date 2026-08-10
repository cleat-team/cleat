package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestScheduleTenantID_IsPopulatedOnRead: the scheduler loop reads the owning
// tenant off the row and passes it to StartNewRun. If the stores did not select
// the column, every scheduled run would be attributed to whatever constant the
// loop fell back to.
func TestScheduleTenantID_IsPopulatedOnRead(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()
			if err := store.CreateSchedule(ctx, Schedule{
				Name:           "tenant-id-read",
				DefName:        "test-workflow",
				CronExpression: "* * * * *",
				Input:          json.RawMessage(`{}`),
				Enabled:        true,
				NextRunAt:      time.Now().Add(-time.Hour),
			}); err != nil {
				t.Fatalf("CreateSchedule: %v", err)
			}

			got := findSchedule(t, store, ctx, "tenant-id-read")
			if got.TenantID == "" {
				t.Error("ListSchedules left TenantID empty; the column is not being selected")
			}

			due, err := store.GetDueSchedules(ctx)
			if err != nil {
				t.Fatalf("GetDueSchedules: %v", err)
			}
			for _, s := range due {
				if s.Name == "tenant-id-read" && s.TenantID == "" {
					t.Error("GetDueSchedules left TenantID empty; the scheduler would " +
						"attribute this run to a fallback constant")
				}
			}
		})
	}
}

// TestScheduleTenantID_IsTheStoresOwnNotTheCallers: CreateSchedule writes the
// store's tenant, ignoring whatever the caller put in the struct. Otherwise a
// caller could create a schedule that fires as a tenant it is not scoped to,
// which for a host call reachable from guest workflow code would be a
// privilege escalation rather than a mistake.
func TestScheduleTenantID_IsTheStoresOwnNotTheCallers(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()
			const someoneElse = "99999999-9999-9999-9999-999999999999"

			if err := store.CreateSchedule(ctx, Schedule{
				Name:           "tenant-id-spoof",
				DefName:        "test-workflow",
				CronExpression: "* * * * *",
				Input:          json.RawMessage(`{}`),
				Enabled:        true,
				NextRunAt:      time.Now().Add(time.Hour),
				TenantID:       someoneElse, // ignored on write
			}); err != nil {
				t.Fatalf("CreateSchedule: %v", err)
			}

			got := findSchedule(t, store, ctx, "tenant-id-spoof")
			if got.TenantID == someoneElse {
				t.Errorf("CreateSchedule honoured the caller-supplied TenantID %q; "+
					"a caller must not be able to create a schedule for another tenant", someoneElse)
			}
		})
	}
}

// TestScheduleLoop_OnlySeesItsOwnTenantsSchedules records a property of the
// system that is easy to mistake for a bug in the scheduler, and that cost a
// session to establish.
//
// A store returns only ITS OWN tenant's schedules from GetDueSchedules --
// enforced by RLS on Postgres and SQL Server, and by an explicit tenant_id
// predicate on MySQL. cmd/cleat-worker opens exactly one store, scoped to the
// default tenant, and polls it.
//
// The consequence: a schedule created by a non-default tenant through
// POST /api/schedules (which uses a request-scoped store) is stored, listed in
// the dashboard, shown as enabled with a next_run_at -- and never fires,
// because the loop that would fire it cannot see it.
//
// This is NOT specific to schedules. The dispatch loop claims workflows through
// the same single default-tenant store, so a non-default tenant's workflows do
// not execute either. It is the worker's execution scope, not a defect in this
// file, and widening it is an architectural change well beyond the scheduler.
//
// The test asserts the isolation rather than the absence of firing, because
// isolation is the part that is deliberate and must not regress.
//
// WHICH LAYER HOLDS THIS UP. On MySQL and (as of this change) PostgreSQL, an
// explicit `tenant_id = ?` predicate in the store's own SQL. On SQL Server,
// the RLS session context the factory's connector sets per connection.
//
// The Postgres predicate is here because this test FAILED on Postgres when it
// was first written correctly. The table has RLS enabled and FORCEd --
// `relrowsecurity` and `relforcerowsecurity` are both true -- but the test
// connects as `postgres`, and a superuser bypasses row security
// unconditionally, FORCE included. GetDueSchedules had no predicate of its
// own, so a deployment connecting as a superuser rather than as the cleat_app
// role from migration 005 had a scheduler that fired every tenant's schedules.
// The predicate makes that independent of which role connects.
//
// The consequence for this test is worth stating plainly: it now passes on
// Postgres because of the SQL, not because of RLS. Whether RLS itself blocks
// cross-tenant access is a different question, tested separately against a
// non-superuser role -- see testutil.OpenPostgresRLSTestDB.
func TestScheduleLoop_OnlySeesItsOwnTenantsSchedules(t *testing.T) {
	for _, backend := range registeredBackends {
		tb, ok := backend.(interface {
			SetupForTenant(t *testing.T, tenantID string) (WorkflowStore, func())
		})
		if !ok {
			continue
		}
		t.Run(backend.Name(), func(t *testing.T) {
			const otherTenant = "22222222-2222-2222-2222-222222222222"

			// ORDER IS LOAD-BEARING. Both Setup and SetupForTenant run
			// CleanupTestData, an unqualified DELETE across the schedule table
			// among others. An earlier version of this test opened the
			// default-tenant store LAST, so its setup deleted the very fixture
			// the assertion was about -- and the test then passed while MySQL's
			// tenant predicate was deliberately broken to `tenant_id = ? OR
			// 1=1`. It was asserting that an empty table contains nothing.
			//
			// Both stores are therefore opened before any fixture is written.
			defaultStore, defaultTeardown := backend.Setup(t)
			defer defaultTeardown()

			otherStore, otherTeardown := tb.SetupForTenant(t, otherTenant)
			defer otherTeardown()

			ctx := context.Background()
			if err := otherStore.CreateSchedule(ctx, Schedule{
				Name:           "other-tenant-schedule",
				DefName:        "test-workflow",
				CronExpression: "* * * * *",
				Input:          json.RawMessage(`{}`),
				Enabled:        true,
				NextRunAt:      time.Now().Add(-time.Hour), // already due
			}); err != nil {
				t.Fatalf("CreateSchedule as the other tenant: %v", err)
			}
			defer otherStore.DeleteSchedule(ctx, "other-tenant-schedule")

			// The other tenant can see its own schedule -- otherwise this test
			// would pass for the wrong reason, having created nothing.
			ownDue, err := otherStore.GetDueSchedules(ctx)
			if err != nil {
				t.Fatalf("GetDueSchedules as the other tenant: %v", err)
			}
			if !containsSchedule(ownDue, "other-tenant-schedule") {
				t.Fatal("the other tenant cannot see its own due schedule; " +
					"the fixture did not take, so the assertion below would be vacuous")
			}

			// The default-tenant store -- the one the worker daemon polls with
			// -- must not. (Opened above, before the fixture was written.)
			defaultDue, err := defaultStore.GetDueSchedules(ctx)
			if err != nil {
				t.Fatalf("GetDueSchedules as the default tenant: %v", err)
			}
			if containsSchedule(defaultDue, "other-tenant-schedule") {
				t.Error("the default-tenant store returned another tenant's schedule; " +
					"tenant isolation on GetDueSchedules has regressed")
			}
		})
	}
}

func containsSchedule(ss []Schedule, name string) bool {
	for _, s := range ss {
		if s.Name == name {
			return true
		}
	}
	return false
}
