package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestSchedulePolicies_RoundTripAndDefaults(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			ctx := context.Background()

			// Explicit, non-default values on every policy, so a store that
			// wrote a constant would be caught rather than agreeing by luck.
			if err := store.CreateSchedule(ctx, Schedule{
				Name:           "policy-explicit",
				DefName:        "test-workflow",
				CronExpression: "* * * * *",
				Input:          json.RawMessage(`{}`),
				Enabled:        true,
				NextRunAt:      time.Now().Add(time.Hour),
				MisfirePolicy:  MisfireSkip,
				CatchUpLimit:   7,
				OverlapPolicy:  OverlapSkip,
			}); err != nil {
				t.Fatalf("CreateSchedule: %v", err)
			}
			got := findSchedule(t, store, ctx, "policy-explicit")
			if got.MisfirePolicy != MisfireSkip {
				t.Errorf("misfire = %q, want %q", got.MisfirePolicy, MisfireSkip)
			}
			if got.CatchUpLimit != 7 {
				t.Errorf("catch_up_limit = %d, want 7", got.CatchUpLimit)
			}
			if got.OverlapPolicy != OverlapSkip {
				t.Errorf("overlap = %q, want %q", got.OverlapPolicy, OverlapSkip)
			}

			// Unset fields normalise on the way in, so the column never holds
			// an empty string a reader has to know means something.
			if err := store.CreateSchedule(ctx, Schedule{
				Name:           "policy-default",
				DefName:        "test-workflow",
				CronExpression: "* * * * *",
				Input:          json.RawMessage(`{}`),
				Enabled:        true,
				NextRunAt:      time.Now().Add(time.Hour),
			}); err != nil {
				t.Fatalf("CreateSchedule: %v", err)
			}
			got = findSchedule(t, store, ctx, "policy-default")
			if got.MisfirePolicy != MisfireCatchUp {
				t.Errorf("default misfire = %q, want %q", got.MisfirePolicy, MisfireCatchUp)
			}
			if got.OverlapPolicy != OverlapAllow {
				t.Errorf("default overlap = %q, want %q", got.OverlapPolicy, OverlapAllow)
			}
			if got.CatchUpLimit != DefaultCatchUpLimit {
				t.Errorf("default catch_up_limit = %d, want %d", got.CatchUpLimit, DefaultCatchUpLimit)
			}
		})
	}
}

// TestClaimDueSchedule_RecordsTheRunItStarted: last_run_id is what makes
// overlap "skip" answerable. Without it the scheduler cannot tell a run it
// started from any other run of the same definition.
func TestClaimDueSchedule_RecordsTheRunItStarted(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			ctx := context.Background()

			due := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
			createDueSchedule(t, store, ctx, "policy-lastrun", due)
			got := findSchedule(t, store, ctx, "policy-lastrun")

			claimed, err := store.ClaimDueSchedule(ctx, "policy-lastrun", got.NextRunAt, got.NextRunAt.Add(time.Minute), "run-abc")
			if err != nil || !claimed {
				t.Fatalf("ClaimDueSchedule: claimed=%v err=%v", claimed, err)
			}
			after := findSchedule(t, store, ctx, "policy-lastrun")
			if after.LastRunID != "run-abc" {
				t.Errorf("last_run_id = %q, want run-abc", after.LastRunID)
			}

			// An empty runID leaves it alone: the overlap-skip and misfire-skip
			// paths advance the schedule without starting anything, and must
			// not erase the run they are skipping on account of.
			claimed, err = store.ClaimDueSchedule(ctx, "policy-lastrun", after.NextRunAt, after.NextRunAt.Add(time.Minute), "")
			if err != nil || !claimed {
				t.Fatalf("second ClaimDueSchedule: claimed=%v err=%v", claimed, err)
			}
			after2 := findSchedule(t, store, ctx, "policy-lastrun")
			if after2.LastRunID != "run-abc" {
				t.Errorf("last_run_id = %q after an empty-runID claim, want it unchanged at run-abc", after2.LastRunID)
			}
		})
	}
}

func TestValidateSchedulePolicies(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fn    func(string) error
		ok    []string
		notOK []string
	}{
		{"misfire", ValidateMisfirePolicy, []string{"", MisfireCatchUp, MisfireSkip}, []string{"CATCH_UP", "catchup", "nope", "allow"}},
		{"overlap", ValidateOverlapPolicy, []string{"", OverlapAllow, OverlapSkip}, []string{"ALLOW", "queue", "nope", "catch_up"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, v := range tc.ok {
				if err := tc.fn(v); err != nil {
					t.Errorf("%q should be valid: %v", v, err)
				}
			}
			for _, v := range tc.notOK {
				if err := tc.fn(v); err == nil {
					t.Errorf("%q should be rejected", v)
				}
			}
		})
	}
}

// TestSchedulePolicies_DatabaseRejectsUnknownValues: the scheduler reads these
// in a background loop with nobody to report a bad value to, so a value it
// cannot interpret has to be impossible to store rather than handled at 03:00.
// Application validation is the friendly path; the CHECK constraint is the one
// that holds when something bypasses it.
func TestSchedulePolicies_DatabaseRejectsUnknownValues(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			ctx := context.Background()

			err := store.CreateSchedule(ctx, Schedule{
				Name:           "policy-bad",
				DefName:        "test-workflow",
				CronExpression: "* * * * *",
				Input:          json.RawMessage(`{}`),
				Enabled:        true,
				NextRunAt:      time.Now().Add(time.Hour),
				MisfirePolicy:  "definitely-not-a-policy",
			})
			if err == nil {
				t.Error("CreateSchedule accepted an unknown misfire policy; the CHECK constraint " +
					"is missing, so the scheduler could read a value it cannot interpret")
			}
		})
	}
}
