package cleattest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/cleat"
)

// TestScheduleCron_RoundTrip covers the three calls together, because they are
// only useful together: a schedule you cannot list or delete is a leak.
func TestScheduleCron_RoundTrip(t *testing.T) {
	env := NewTestEnv()

	id, err := env.H().ScheduleCron("nightly-report", "0 3 * * *", "America/New_York", `{"depth":2}`)
	if err != nil {
		t.Fatalf("ScheduleCron: %v", err)
	}

	listJSON, err := env.H().ListCrons()
	if err != nil {
		t.Fatalf("ListCrons: %v", err)
	}
	var got []cleat.CronSchedule
	if err := json.Unmarshal([]byte(listJSON), &got); err != nil {
		t.Fatalf("ListCrons produced JSON a workflow cannot parse: %v\n%s", err, listJSON)
	}
	want := []cleat.CronSchedule{{
		ScheduleID:   id,
		WorkflowName: "nightly-report",
		CronExpr:     "0 3 * * *",
		Timezone:     "America/New_York",
		Input:        `{"depth":2}`,
		Enabled:      true,
	}}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("ListCrons = %+v\nwant           %+v", got, want)
	}

	if err := env.H().DeleteCron(id); err != nil {
		t.Fatalf("DeleteCron: %v", err)
	}
	if crons := env.Crons(); len(crons) != 0 {
		t.Errorf("%d schedules survived the delete: %+v", len(crons), crons)
	}

	// The retry of a delete that already succeeded. The host call treats this
	// as success because at-least-once delivery means it happens.
	if err := env.H().DeleteCron(id); err != nil {
		t.Errorf("deleting an already-deleted schedule: %v, want nil", err)
	}
}

// TestScheduleCron_RejectsWhatTheEngineRejects is why cleattest imports the
// engine's validators instead of writing its own.
//
// A harness that accepted an expression the engine refuses would turn a
// production failure into a green test -- and this call was left unwired for
// exactly that reason until the engine had a specification to borrow.
func TestScheduleCron_RejectsWhatTheEngineRejects(t *testing.T) {
	env := NewTestEnv()

	for _, tc := range []struct {
		name                             string
		wf, cronExpr, timezone, inputStr string
		wantIn                           string
	}{
		{"six fields", "wf", "0 3 * * * *", "UTC", `{}`, "cron"},
		{"minute out of range", "wf", "99 3 * * *", "UTC", `{}`, "cron"},
		{"unknown timezone", "wf", "0 3 * * *", "Mars/Olympus", `{}`, "timezone"},
		{"input is not JSON", "wf", "0 3 * * *", "UTC", `{oops`, "JSON"},
		{"empty workflow name", "", "0 3 * * *", "UTC", `{}`, "workflow name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, err := env.H().ScheduleCron(tc.wf, tc.cronExpr, tc.timezone, tc.inputStr)
			if err == nil {
				t.Fatalf("accepted %q, returning schedule ID %q", tc.cronExpr, id)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantIn)
			}
			if len(env.Crons()) != 0 {
				t.Errorf("a rejected schedule was stored anyway: %+v", env.Crons())
			}
		})
	}
}

// TestListCrons_IsOrdered: map iteration order is not stable, and a workflow
// that walked an unordered list would be non-deterministic -- the one thing a
// durable workflow must never be.
func TestListCrons_IsOrdered(t *testing.T) {
	env := NewTestEnv()
	for _, name := range []string{"c", "a", "b", "e", "d"} {
		if _, err := env.H().ScheduleCron(name, "* * * * *", "UTC", `{}`); err != nil {
			t.Fatalf("ScheduleCron(%s): %v", name, err)
		}
	}

	first := env.Crons()
	for i := 0; i < 20; i++ {
		again := env.Crons()
		for j := range first {
			if again[j].ScheduleID != first[j].ScheduleID {
				t.Fatalf("ListCrons order changed between calls: %v then %v",
					idsOf(first), idsOf(again))
			}
		}
	}

	ids := idsOf(first)
	for i := 1; i < len(ids); i++ {
		if ids[i-1] >= ids[i] {
			t.Errorf("ListCrons is not sorted by schedule ID: %v", ids)
			break
		}
	}
}

// TestScheduleCron_ResetClearsSchedules: a TestEnv reused across subtests must
// not leak one test's schedules into the next.
func TestScheduleCron_ResetClearsSchedules(t *testing.T) {
	env := NewTestEnv()
	if _, err := env.H().ScheduleCron("wf", "* * * * *", "UTC", `{}`); err != nil {
		t.Fatalf("ScheduleCron: %v", err)
	}
	env.Reset()
	if crons := env.Crons(); len(crons) != 0 {
		t.Errorf("Reset left %d schedules behind: %+v", len(crons), crons)
	}
}

func idsOf(list []cleat.CronSchedule) []string {
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, c.ScheduleID)
	}
	return out
}
