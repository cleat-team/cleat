package engine

// IMPROVEMENT-PLAN 3.77 step 3, D7: schedule names are per-tenant.
//
// What this replaces. Before migrations postgres/036, mysql/035 and mssql/039,
// workflow_schedules keyed on (name) alone, so the second tenant to create a
// schedule called "nightly-report" got
//
//     mssql: Violation of PRIMARY KEY constraint 'pk_workflow_schedules'.
//            Cannot insert duplicate key ... The duplicate key value is
//            (nightly-report).
//
// with postgres and mysql failing the same way in their own wording. One
// tenant naming a schedule took that name away from every other tenant on the
// deployment, and the error named a constraint rather than the problem.
//
// Note this is the LOUD failure class. 3.77 step 2 had to worry about a silent
// one as well -- MAX(version) resolvers returning another tenant's number --
// because workflow_defs is read by aggregate. Nothing reads schedules that
// way: every lookup here is a whole row or a listing, so a missing predicate
// shows up as somebody else's row rather than as a plausible wrong number.
// That is why this file is short and def_lookup_tenant_property_test.go is not.
//
// The prerequisite is 3.86, not this migration. On SQL Server the isolation
// between two same-named schedules is the Go-level `AND tenant_id` on each
// statement, because dbo.fn_tenant_filter admits any dbo.cleat_admin
// connection outright and a multi-tenant deployment must grant that role.
// Those predicates landed first, deliberately: without them this change would
// convert "one tenant cannot use the name" into "one tenant's DELETE takes
// both rows".

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// sharedScheduleName is a name two tenants would plausibly both choose. That
// is the whole point of D7: the collision is ordinary use, not an attack.
const sharedScheduleName = "nightly-report"

func createNamedSchedule(t *testing.T, store WorkflowStore, name, defName string, nextRun time.Time) {
	t.Helper()
	if err := store.CreateSchedule(context.Background(), Schedule{
		Name:           name,
		DefName:        defName,
		CronExpression: "0 3 * * *",
		Input:          json.RawMessage(`{}`),
		Enabled:        true,
		NextRunAt:      nextRun,
		Timezone:       "UTC",
	}); err != nil {
		t.Fatalf("CreateSchedule(%s) for %s: %v", name, defName, err)
	}
}

func scheduleFromList(t *testing.T, store WorkflowStore, name string) *Schedule {
	t.Helper()
	all, err := store.ListSchedules(context.Background())
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

// TestTwoTenantsEachHoldTheirOwnScheduleOfOneName is the feature.
//
// Both tenants create "nightly-report", both succeed, and each reads back the
// one it created -- distinguished by def_name, so a test that merely counted
// rows could not pass by accident.
func TestTwoTenantsEachHoldTheirOwnScheduleOfOneName(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			storeA, storeB, _ := twoTenantStores(t, backend)
			at := time.Now().Add(time.Hour).UTC()

			createNamedSchedule(t, storeA, sharedScheduleName, "tenant-a-report", at)
			createNamedSchedule(t, storeB, sharedScheduleName, "tenant-b-report", at)

			gotA := scheduleFromList(t, storeA, sharedScheduleName)
			if gotA == nil {
				t.Fatalf("tenant A cannot see its own schedule %q", sharedScheduleName)
			}
			if gotA.DefName != "tenant-a-report" {
				t.Errorf("tenant A's %q points at def %q, want tenant-a-report -- "+
					"it is reading tenant B's row", sharedScheduleName, gotA.DefName)
			}

			gotB := scheduleFromList(t, storeB, sharedScheduleName)
			if gotB == nil {
				t.Fatalf("tenant B cannot see its own schedule %q", sharedScheduleName)
			}
			if gotB.DefName != "tenant-b-report" {
				t.Errorf("tenant B's %q points at def %q, want tenant-b-report", sharedScheduleName, gotB.DefName)
			}

			// Exactly one each. A key that admitted both rows to both tenants
			// would satisfy the two assertions above on whichever row happened
			// to sort first.
			for _, tc := range []struct {
				who   string
				store WorkflowStore
			}{{"A", storeA}, {"B", storeB}} {
				all, err := tc.store.ListSchedules(context.Background())
				if err != nil {
					t.Fatalf("ListSchedules(%s): %v", tc.who, err)
				}
				n := 0
				for _, s := range all {
					if s.Name == sharedScheduleName {
						n++
					}
				}
				if n != 1 {
					t.Errorf("tenant %s sees %d schedules named %q, want exactly 1", tc.who, n, sharedScheduleName)
				}
			}
		})
	}
}

// TestDeletingOneTenantsScheduleLeavesTheOtherNamesake is what 3.86 protects
// once the names can actually collide.
//
// Before this migration the two tenants could not both hold the name, so the
// worst an unqualified `DELETE ... WHERE name = ?` could do was reach a row
// belonging to whoever had claimed it. Now it would take both, and the
// per-statement tenant predicate is the only thing between the two.
func TestDeletingOneTenantsScheduleLeavesTheOtherNamesake(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			storeA, storeB, _ := twoTenantStores(t, backend)
			ctx := context.Background()
			at := time.Now().Add(time.Hour).UTC()

			createNamedSchedule(t, storeA, sharedScheduleName, "tenant-a-report", at)
			createNamedSchedule(t, storeB, sharedScheduleName, "tenant-b-report", at)

			if err := storeB.DeleteSchedule(ctx, sharedScheduleName); err != nil {
				t.Fatalf("tenant B DeleteSchedule: %v", err)
			}

			if got := scheduleFromList(t, storeA, sharedScheduleName); got == nil {
				t.Errorf("tenant B deleting its own %q took tenant A's schedule of the same name with it",
					sharedScheduleName)
			}
			if got := scheduleFromList(t, storeB, sharedScheduleName); got != nil {
				t.Errorf("tenant B's own %q survived its delete (def %q)", sharedScheduleName, got.DefName)
			}
		})
	}
}

// TestDisablingOneTenantsScheduleLeavesTheOtherNamesake is the same shape on
// the quieter statement. A schedule that is still listed, still shows a
// next_run_at and never fires is harder to attribute than one that is gone.
func TestDisablingOneTenantsScheduleLeavesTheOtherNamesake(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			storeA, storeB, _ := twoTenantStores(t, backend)
			ctx := context.Background()
			at := time.Now().Add(time.Hour).UTC()

			createNamedSchedule(t, storeA, sharedScheduleName, "tenant-a-report", at)
			createNamedSchedule(t, storeB, sharedScheduleName, "tenant-b-report", at)

			if err := storeB.SetScheduleEnabled(ctx, sharedScheduleName, false); err != nil {
				t.Fatalf("tenant B SetScheduleEnabled: %v", err)
			}

			gotA := scheduleFromList(t, storeA, sharedScheduleName)
			if gotA == nil {
				t.Fatalf("tenant A's schedule %q disappeared", sharedScheduleName)
			}
			if !gotA.Enabled {
				t.Errorf("tenant B disabling its own %q disabled tenant A's schedule of the same name",
					sharedScheduleName)
			}

			gotB := scheduleFromList(t, storeB, sharedScheduleName)
			if gotB == nil {
				t.Fatalf("tenant B's schedule %q disappeared", sharedScheduleName)
			}
			if gotB.Enabled {
				t.Errorf("tenant B's own %q is still enabled after it disabled it", sharedScheduleName)
			}
		})
	}
}

// TestClaimingOneTenantsScheduleLeavesTheOtherNamesakeDue covers the
// compare-and-swap the delivery guarantee rests on.
//
// Two tenants hold "nightly-report" due at the SAME instant, which is the
// realistic case -- both chose 03:00 -- and the one this migration creates.
// The scheduler loop reads the due set across tenants, re-scopes to
// Schedule.TenantID and claims by name. If that claim is not tenant-qualified
// it advances whichever row it reaches, and the other tenant's firing is
// consumed by a run started under someone else's definition.
func TestClaimingOneTenantsScheduleLeavesTheOtherNamesakeDue(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			storeA, storeB, _ := twoTenantStores(t, backend)
			ctx := context.Background()
			// Truncated to the second: SQL Server's DATETIMEOFFSET and
			// PostgreSQL's timestamptz do not agree about sub-second precision
			// on the round trip, and this test compares an instant it read back
			// against one it wrote.
			due := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)

			createNamedSchedule(t, storeA, sharedScheduleName, "tenant-a-report", due)
			createNamedSchedule(t, storeB, sharedScheduleName, "tenant-b-report", due)

			beforeA := scheduleFromList(t, storeA, sharedScheduleName)
			if beforeA == nil {
				t.Fatalf("tenant A cannot see its own schedule")
			}

			advanced := due.Add(24 * time.Hour)
			claimed, err := storeB.ClaimDueSchedule(ctx, sharedScheduleName, beforeA.NextRunAt, advanced, "run-b")
			if err != nil {
				t.Fatalf("tenant B ClaimDueSchedule: %v", err)
			}
			if !claimed {
				t.Fatalf("tenant B could not claim its OWN schedule; the fixture or the CAS is wrong, "+
					"not the tenancy -- expected next_run_at %s", beforeA.NextRunAt.UTC())
			}

			afterA := scheduleFromList(t, storeA, sharedScheduleName)
			if afterA == nil {
				t.Fatalf("tenant A's schedule %q disappeared", sharedScheduleName)
			}
			if !afterA.NextRunAt.UTC().Equal(beforeA.NextRunAt.UTC()) {
				t.Errorf("tenant B's claim advanced tenant A's namesake schedule from %s to %s; "+
					"A's firing has been consumed by B's run",
					beforeA.NextRunAt.UTC(), afterA.NextRunAt.UTC())
			}
		})
	}
}
