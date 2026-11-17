package typescript

// The cron bindings (cleat#3827, group F): scheduleCron, deleteCron, listCrons.
//
// Two layers. In-process, with NO workflow store (the engine's schedules need a WorkflowStore, a large interface this package does
// not reimplement): the argument checks the engine makes before it consults a store, the "no workflow store configured" answer for a
// valid call, and the guard. On a real worker against each dialect's real store (typescript_schedules_worker_test.go): that a
// schedule is created, listed, deleted and idempotently deleted again, which no in-process double can say.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

const scheduleEntries = "schedules_bad_args,schedules_no_store,schedules_round_trip,schedules_late"

func buildSchedules(t *testing.T) []byte {
	t.Helper()
	root := needToolchain(t)
	src := "tests/typescript/testdata/workflows/schedules.ts"
	res := build(t, root, src, scheduleEntries)
	if res.exit != 0 {
		t.Fatalf("building %s failed (exit %d):\n%s", src, res.exit, res.output)
	}
	wasm, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	return wasm
}

func TestTypeScriptScheduleCronRefusesBadArgumentsWithTheHostsOwnReason(t *testing.T) {
	wasm := buildSchedules(t)
	e, closeRT := newEngineWith(t)
	defer closeRT()
	raw, history, susp, _, _, err := e.Execute(context.Background(), wasm, "schedules_bad_args", json.RawMessage(`{}`))
	if err != nil || susp != nil {
		t.Fatalf("susp=%v err=%v", susp, err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"badExpr": "schedule_cron \"wf\":",
		"badZone": "Mars/Olympus_Mons",
		// An EMPTY name is refused by the ABI wrapper before the scheduler runs, so the host writes no reason; the plugin says so
		// rather than hand the guest the NUL bytes the SDK reads from the unwritten buffer.
		"noName":   "refused a parameter and gave no reason",
		"badInput": "unsupported value: NaN",
	}
	for k, w := range want {
		if !strings.Contains(got[k], w) {
			t.Errorf("%s rejected with %q, want the host's reason containing %q (a bare error code would lose it)", k, got[k], w)
		}
	}
	// (A NaN in the input never reaches the host: refused in the guest as a REJECTION, so it records no event. It is in `want` above.)
	// The two that reached the scheduler each recorded a schedule_cron event carrying the host's error.
	var recorded int
	for _, ev := range history {
		if ev.EventType == engine.EventTypeScheduleCron {
			recorded++
			if ev.Err == "" {
				t.Errorf("a schedule_cron event with no error was recorded for a call that must have been refused: %+v", ev)
			}
		}
	}
	if recorded != 2 {
		t.Errorf("%d schedule_cron events recorded, want 2 (the bad expression and the bad zone; the empty name is refused by the ABI wrapper and the NaN in the guest, so neither records one)", recorded)
	}
	if strings.ContainsRune(got["noName"], 0) {
		t.Errorf("noName's rejection carries NUL bytes: %q", got["noName"])
	}
}

// With no WorkflowStore a VALID call is refused by name, for all three calls. This is what the in-process harness and an embedder
// without a store see, and it is why the harness has no `schedules` store: the refusal passes through rather than being papered over.
func TestTypeScriptCronCallsSayWhenThereIsNoWorkflowStore(t *testing.T) {
	wasm := buildSchedules(t)
	e, closeRT := newEngineWith(t)
	defer closeRT()
	raw, _, _, _, _, err := e.Execute(context.Background(), wasm, "schedules_no_store", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"create", "list"} {
		if !strings.Contains(got[k], "no workflow store configured") {
			t.Errorf("%s: %q, want the engine's \"no workflow store configured\"", k, got[k])
		}
	}
	// delete_cron is the odd one out and the test says so rather than hiding it: the host writes NO reason for a failed delete, so
	// the SDK reports a bare code with a hint ("Check that the schedule ID exists") that is wrong for this cause. Every SDK has the
	// same message; pinned here so that a change to either side is noticed.
	if !strings.Contains(got["remove"], "host error code 1") {
		t.Errorf("remove: %q, want the SDK's bare-code message for a delete the host refused", got["remove"])
	}
}

// What `guarded` buys on all three calls: nothing of any type is recorded after the call that suspended the segment.
func TestTypeScriptACronCallAfterASuspensionRecordsNothing(t *testing.T) {
	wasm := buildSchedules(t)
	e, closeRT := newEngineWith(t)
	defer closeRT()
	_, history, susp, _, _, err := e.Execute(context.Background(), wasm, "schedules_late", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if susp == nil || !strings.Contains(susp.Reason, "cleat_sleep(60000ms)") {
		t.Fatalf("the workflow did not suspend at the sleep (%v); the ordering below is then unmeasured", susp)
	}
	if len(history) != 0 {
		var types []string
		for _, ev := range history {
			types = append(types, string(ev.EventType))
		}
		t.Fatalf("the segment recorded %v AFTER the call that suspended it; schedule_cron, delete_cron and list_crons must be guarded", types)
	}
}
