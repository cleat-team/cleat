package engine

// cleat#4083: delete_cron had no defer-segment guard, and schedule_cron has one.
//
// A defer segment replays a TERMINATED workflow purely to run its outstanding
// cleanup. stopBeforeNewWork is how every host call that starts or changes
// durable work refuses to run in the workflow BODY past the end of its recorded
// history, while letting the defer bodies' own calls through (inDeferDrain).
// ScheduleCron consults it; DeleteCron did not, so a body that was terminated
// while waiting would, when its segment replayed past recorded history, delete
// a schedule the workflow had not yet reached.
//
// THE ISSUE'S OWN MEASUREMENT STOPPED SHORT OF THE EFFECT. It showed the guest
// continuing past the call -- on a test engine with no schedule store, so the
// call ran into the no-store branch and errored after the guest had already
// continued. That shows the host did not refuse the call; it does not show a
// row was deleted. This file runs against a real store and reads the row back.
//
// THREE CASES, because each one answers a different question:
//
//   - an ordinary session DOES delete: without it, "the row survived" below
//     could mean the call never reached the store for any reason at all;
//   - a defer segment's BODY does not: the defect;
//   - a defer BODY inside the segment still does: the guard must not remove the
//     one legitimate use, a defer that deletes the schedule its workflow made.

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func cronDeleteSession(store WorkflowStore, inDeferSegment, inDeferBody bool) *execSession {
	opts := []EngineOption{WithWorkflowStore(store), WithWorkflowID("wf-cron-delete")}
	if inDeferSegment {
		opts = append(opts, WithDeferPhase())
	}
	return &execSession{
		engine:       NewEngine(nil, nil, opts...),
		workflowID:   "wf-cron-delete",
		nowMs:        time.Now().UnixMilli(),
		deferrals:    map[string]string{},
		queryState:   map[string]string{},
		isReplay:     inDeferSegment, // a segment replays; with no history left it is past the frontier
		inDeferDrain: inDeferBody,
	}
}

func scheduleStillThere(t *testing.T, store WorkflowStore, name string) bool {
	t.Helper()
	all, err := store.ListSchedules(context.Background())
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	for _, s := range all {
		if s.Name == name {
			return true
		}
	}
	return false
}

func TestADeferSegmentDoesNotDeleteACron(t *testing.T) {
	cases := []struct {
		name                       string
		inDeferSegment, inDeferBod bool
		wantDeleted                bool
		why                        string
	}{
		{"an ordinary session deletes", false, false, true,
			"the control: with no defer segment the call reaches the store, so the other rows' " +
				"'survived' means the guard refused rather than the call never arriving"},
		{"a defer segment's body does not", true, false, false,
			"cleat#4083: the segment replays past recorded history, so the BODY is running work the " +
				"termination was meant to stop"},
		{"a defer body inside the segment still does", true, true, true,
			"cleanup that deletes the schedule its own workflow created is a legitimate defer, and " +
				"inDeferDrain is how the guard tells it from the body"},
	}
	for _, backend := range registeredBackends {
		for i, c := range cases {
			t.Run(backend.Name()+"/"+c.name, func(t *testing.T) {
				store, teardown := backend.Setup(t)
				defer teardown()
				setupTestData(t, store)
				ctx := context.Background()

				id := "cron-delete-guard-" + string(rune('a'+i))
				if err := store.CreateSchedule(ctx, Schedule{
					Name: id, DefName: "test-workflow", CronExpression: "* * * * *",
					Input: json.RawMessage(`{}`), NextRunAt: time.Now().Add(time.Hour),
				}); err != nil {
					t.Fatalf("CreateSchedule: %v", err)
				}
				// PRECONDITION. A row that was never there would make every case below read as deleted.
				if !scheduleStillThere(t, store, id) {
					t.Fatalf("UNMEASURED: the seeded schedule %q is not in the store, so nothing below was measured", id)
				}

				s := cronDeleteSession(store, c.inDeferSegment, c.inDeferBod)
				got := s.DeleteCron(ctx, nil, id)

				deleted := !scheduleStillThere(t, store, id)
				if deleted != c.wantDeleted {
					t.Errorf("DeleteCron(%q) left the schedule deleted=%v, want %v (returned %#x).\n\n%s",
						id, deleted, c.wantDeleted, got, c.why)
				}
				if !c.wantDeleted && got != callSuspendSentinel {
					t.Errorf("a refused delete returned %#x, want the stop sentinel %#x: the guest must "+
						"be told to stop, as it is for every other refused call in a segment", got, callSuspendSentinel)
				}
			})
		}
	}
}
