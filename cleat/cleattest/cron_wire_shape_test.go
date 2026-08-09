package cleattest

import (
	"encoding/json"
	"testing"

	"github.com/cleat-team/cleat/cleat"
)

// TestCronScheduleWireShapeMatchesTheEngine is one half of a contract the
// compiler cannot see.
//
// engine.cronScheduleView is what ListCrons marshals; cleat.CronSchedule is
// what a workflow unmarshals into. They are two declarations of one wire
// format in two modules -- the SDK imports the engine, so the engine cannot
// import the SDK, and nothing connects the two types.
//
// The literal below is byte-identical to the one in
// engine.TestCronScheduleViewWireShape. Renaming a field on either side
// without the other fails one of these two tests; without them it would
// silently break every Go guest that calls ListCrons, because a JSON field
// that no longer matches unmarshals to the zero value rather than erroring.
func TestCronScheduleWireShapeMatchesTheEngine(t *testing.T) {
	const wire = `{"schedule_id":"cron-1","workflow_name":"w","cron_expr":"* * * * *","timezone":"UTC","input":"{}","enabled":true}`

	// Marshalling must produce exactly what the engine produces.
	out, err := json.Marshal(cleat.CronSchedule{
		ScheduleID: "cron-1", WorkflowName: "w", CronExpr: "* * * * *",
		Timezone: "UTC", Input: "{}", Enabled: true,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != wire {
		t.Errorf("cleat.CronSchedule marshals to\n  %s\nengine emits\n  %s", out, wire)
	}

	// And unmarshalling the engine's bytes must populate every field, which
	// is the direction a workflow actually exercises.
	var got cleat.CronSchedule
	if err := json.Unmarshal([]byte(wire), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := cleat.CronSchedule{
		ScheduleID: "cron-1", WorkflowName: "w", CronExpr: "* * * * *",
		Timezone: "UTC", Input: "{}", Enabled: true,
	}
	if got != want {
		t.Errorf("unmarshalling the engine's JSON dropped fields\n got %+v\nwant %+v", got, want)
	}
}
