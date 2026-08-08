package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestScheduleTimezone_RoundTrips checks that a schedule's timezone survives a
// write and a read, on every dialect.
//
// This is the assertion that catches a store where the column was added to the
// schema but not to the INSERT or the SELECT -- which is a plausible way to get
// this wrong three times over, once per dialect, with the Go code compiling
// perfectly and the tests passing everywhere except the one that reads the
// value back.
func TestScheduleTimezone_RoundTrips(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()
			const zone = "America/New_York"

			sch := Schedule{
				Name:           "tz-round-trip",
				DefName:        "test-workflow",
				EntryPoint:     "main",
				CronExpression: "0 7 * * *",
				Input:          json.RawMessage(`{}`),
				Enabled:        true,
				NextRunAt:      time.Now().Add(time.Hour),
				Timezone:       zone,
			}
			if err := store.CreateSchedule(ctx, sch); err != nil {
				t.Fatalf("CreateSchedule: %v", err)
			}

			got := findSchedule(t, store, ctx, "tz-round-trip")
			if got.Timezone != zone {
				t.Errorf("ListSchedules timezone = %q, want %q", got.Timezone, zone)
			}
		})
	}
}

// TestScheduleTimezone_EmptyBecomesUTC: the column is NOT NULL and an empty
// string would be a third state, indistinguishable on read from a schedule
// whose author meant UTC. The stores normalise on the way in.
func TestScheduleTimezone_EmptyBecomesUTC(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()
			sch := Schedule{
				Name:           "tz-empty",
				DefName:        "test-workflow",
				CronExpression: "0 7 * * *",
				Input:          json.RawMessage(`{}`),
				Enabled:        true,
				NextRunAt:      time.Now().Add(time.Hour),
				// Timezone deliberately left empty.
			}
			if err := store.CreateSchedule(ctx, sch); err != nil {
				t.Fatalf("CreateSchedule: %v", err)
			}

			got := findSchedule(t, store, ctx, "tz-empty")
			if got.Timezone != DefaultScheduleTimezone {
				t.Errorf("timezone = %q, want %q", got.Timezone, DefaultScheduleTimezone)
			}
		})
	}
}

// TestScheduleTimezone_ReachesGetDueSchedules is the one that matters
// operationally. The worker's scheduler loop reads schedules through
// GetDueSchedules, not ListSchedules, and computes the next firing from
// sch.Timezone. A GetDueSchedules that selected every other column but not
// this one would hand the scheduler an empty zone, which resolves to UTC --
// so every schedule would quietly fire at the UTC wall clock and the bug would
// look exactly like the one this change is fixing.
func TestScheduleTimezone_ReachesGetDueSchedules(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()
			const zone = "Asia/Tokyo"

			sch := Schedule{
				Name:           "tz-due",
				DefName:        "test-workflow",
				CronExpression: "0 7 * * *",
				Input:          json.RawMessage(`{}`),
				Enabled:        true,
				NextRunAt:      time.Now().Add(-time.Hour), // already due
				Timezone:       zone,
			}
			if err := store.CreateSchedule(ctx, sch); err != nil {
				t.Fatalf("CreateSchedule: %v", err)
			}

			due, err := store.GetDueSchedules(ctx)
			if err != nil {
				t.Fatalf("GetDueSchedules: %v", err)
			}
			var found *Schedule
			for i := range due {
				if due[i].Name == "tz-due" {
					found = &due[i]
					break
				}
			}
			if found == nil {
				t.Fatalf("GetDueSchedules did not return tz-due (returned %d schedules)", len(due))
			}
			if found.Timezone != zone {
				t.Errorf("GetDueSchedules timezone = %q, want %q", found.Timezone, zone)
			}
		})
	}
}

func findSchedule(t *testing.T, store WorkflowStore, ctx context.Context, name string) Schedule {
	t.Helper()
	schedules, err := store.ListSchedules(ctx)
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	for _, s := range schedules {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("ListSchedules did not return %q (returned %d schedules)", name, len(schedules))
	return Schedule{}
}
