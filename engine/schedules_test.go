package engine

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The cron host calls are the first guest-facing door into the schedule
// subsystem. These tests cover the three things that door can get wrong:
// what it writes, what a retry of it writes, and what a replay of it returns.

// cronTestStore is Setup plus the removal of the schedule setupTestData seeds.
// These tests count schedules, and a fixture nobody asked for makes every
// count off by one.
func cronTestStore(t *testing.T, backend StoreBackend) (WorkflowStore, func()) {
	t.Helper()
	store, teardown := backend.Setup(t)
	setupTestData(t, store)
	if err := store.DeleteSchedule(context.Background(), "test-schedule"); err != nil {
		teardown()
		t.Fatalf("clearing the seeded schedule: %v", err)
	}
	return store, teardown
}

// sameJSON compares two JSON documents by value.
//
// Byte comparison does not survive the trip: Postgres and MySQL store the
// input in a JSON column and hand it back reformatted ({"depth": 2}), while
// SQL Server keeps the bytes it was given. A guest gets back what it meant,
// not what it typed.
func sameJSON(t *testing.T, got, want string) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("got is not JSON: %v (%s)", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want is not JSON: %v (%s)", err, want)
	}
	return reflect.DeepEqual(g, w)
}

func cronSession(t *testing.T, store WorkflowStore, opts ...EngineOption) *execSession {
	t.Helper()
	opts = append([]EngineOption{WithWorkflowStore(store)}, opts...)
	return &execSession{
		engine:     NewEngine(nil, nil, opts...),
		workflowID: "wf-cron-1",
		tenantID:   "default",
	}
}

// TestScheduleCron_WritesTheScheduleTheGuestAskedFor is the happy path, and
// pins the fields the guest does not get to see afterwards except through
// ListCrons.
func TestScheduleCron_WritesTheScheduleTheGuestAskedFor(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := cronTestStore(t, backend)
			defer teardown()

			ctx := context.Background()
			s := cronSession(t, store)

			id, err := s.createCronSchedule(ctx, "nightly-report", "0 3 * * *", "America/New_York", `{"depth":2}`)
			if err != nil {
				t.Fatalf("createCronSchedule: %v", err)
			}
			if !strings.HasPrefix(id, "cron-") {
				t.Errorf("schedule ID = %q, want a cron- prefix", id)
			}

			got := findSchedule(t, store, ctx, id)
			if got.DefName != "nightly-report" {
				t.Errorf("def_name = %q, want nightly-report", got.DefName)
			}
			if got.CronExpression != "0 3 * * *" {
				t.Errorf("cron_expression = %q", got.CronExpression)
			}
			if got.Timezone != "America/New_York" {
				t.Errorf("timezone = %q, want America/New_York", got.Timezone)
			}
			if !got.Enabled {
				t.Error("schedule was created disabled; a guest that asked for a cron expects it to run")
			}
			if !got.NextRunAt.After(time.Now().Add(-time.Minute)) {
				t.Errorf("next_run_at = %s, want a future instant", got.NextRunAt)
			}
			if !sameJSON(t, string(got.Input), `{"depth":2}`) {
				t.Errorf("input = %s", got.Input)
			}
		})
	}
}

// TestScheduleCron_ARetryAfterACrashDoesNotCreateASecondSchedule is the
// reason scheduleIDFor derives the name instead of generating a random one.
//
// The failure it guards against: a workflow calls ScheduleCron, the store
// commits, and the worker dies before the event reaches the journal. The
// workflow replays, reaches this same call at this same step, and creates a
// SECOND schedule. Nobody holds the first one's ID, so nothing can ever
// delete it and it fires forever.
//
// The second session here is that retry: same workflow, same step, empty
// history.
func TestScheduleCron_ARetryAfterACrashDoesNotCreateASecondSchedule(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := cronTestStore(t, backend)
			defer teardown()

			ctx := context.Background()

			first := cronSession(t, store)
			if code := decodeCronResult(first.ScheduleCron(ctx, nil, "hourly", "0 * * * *", "UTC", `{}`, 0, 0)); code != 0 {
				t.Fatalf("first ScheduleCron: errCode %d (%s)", code, first.history[0].Err)
			}
			firstID := first.history[0].CronScheduleID

			// The crash: nothing of the above survived except the row in the
			// store. The retry starts from an empty history at step 0.
			retry := cronSession(t, store)
			if code := decodeCronResult(retry.ScheduleCron(ctx, nil, "hourly", "0 * * * *", "UTC", `{}`, 0, 0)); code != 0 {
				t.Fatalf("retry ScheduleCron: errCode %d (%s)", code, retry.history[0].Err)
			}
			retryID := retry.history[0].CronScheduleID

			if retryID != firstID {
				t.Errorf("retry produced schedule ID %q, first produced %q; a retry must address the same schedule", retryID, firstID)
			}

			all, err := store.ListSchedules(ctx)
			if err != nil {
				t.Fatalf("ListSchedules: %v", err)
			}
			if len(all) != 1 {
				names := make([]string, 0, len(all))
				for i := range all {
					names = append(names, all[i].Name)
				}
				t.Errorf("%d schedules exist after one logical create (%v), want 1", len(all), names)
			}
		})
	}
}

// TestScheduleCron_QuotaCapsTheTenantNotTheRun checks both halves of the cap:
// that it stops the tenant, and that it counts schedules the tenant already
// holds rather than ones this run created. A cap counted per run would let a
// workflow create its limit, exit, and be started again.
func TestScheduleCron_QuotaCapsTheTenantNotTheRun(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := cronTestStore(t, backend)
			defer teardown()

			ctx := context.Background()

			// Two schedules already exist, created by some other run.
			for _, step := range []int{0, 1} {
				s := cronSession(t, store, WithMaxQuotaSchedules(2))
				s.stepCount = step
				if _, err := s.createCronSchedule(ctx, "filler", "* * * * *", "UTC", `{}`); err != nil {
					t.Fatalf("filler %d: %v", step, err)
				}
			}

			// A fresh run, having created nothing itself, is still refused.
			fresh := cronSession(t, store, WithMaxQuotaSchedules(2))
			fresh.stepCount = 7
			_, err := fresh.createCronSchedule(ctx, "one-too-many", "* * * * *", "UTC", `{}`)
			if err == nil {
				t.Fatal("a third schedule was accepted under a quota of 2")
			}
			if !strings.Contains(err.Error(), "quota exceeded") {
				t.Errorf("error = %v, want it to name the quota", err)
			}
		})
	}
}

// TestCronRoundTrip_ListThenDelete covers ListCrons' shape and DeleteCron's
// effect, including the second delete a retry would issue.
func TestCronRoundTrip_ListThenDelete(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := cronTestStore(t, backend)
			defer teardown()

			ctx := context.Background()
			s := cronSession(t, store)

			id, err := s.createCronSchedule(ctx, "report", "30 4 * * 1", "Europe/Paris", `{"n":1}`)
			if err != nil {
				t.Fatalf("createCronSchedule: %v", err)
			}

			listJSON, err := s.listCronSchedules(ctx)
			if err != nil {
				t.Fatalf("listCronSchedules: %v", err)
			}
			var views []cronScheduleView
			if err := json.Unmarshal([]byte(listJSON), &views); err != nil {
				t.Fatalf("ListCrons produced JSON a guest cannot parse: %v\n%s", err, listJSON)
			}
			if len(views) != 1 {
				t.Fatalf("%d entries, want 1: %s", len(views), listJSON)
			}
			want := cronScheduleView{
				ScheduleID:   id,
				WorkflowName: "report",
				CronExpr:     "30 4 * * 1",
				Timezone:     "Europe/Paris",
				Enabled:      true,
			}
			gotView := views[0]
			if !sameJSON(t, gotView.Input, `{"n":1}`) {
				t.Errorf("input = %s, want {\"n\":1}", gotView.Input)
			}
			gotView.Input, want.Input = "", ""
			if gotView != want {
				t.Errorf("entry = %+v\nwant   %+v", gotView, want)
			}

			if code := decodeCronResult(s.DeleteCron(ctx, nil, id)); code != 0 {
				t.Fatalf("DeleteCron: errCode %d", code)
			}
			after, err := store.ListSchedules(ctx)
			if err != nil {
				t.Fatalf("ListSchedules: %v", err)
			}
			if len(after) != 0 {
				t.Errorf("%d schedules survived the delete", len(after))
			}

			// The retry of a delete that already succeeded. At-least-once
			// means this happens, and it must not be an error.
			retry := cronSession(t, store)
			if code := decodeCronResult(retry.DeleteCron(ctx, nil, id)); code != 0 {
				t.Errorf("deleting an already-deleted schedule returned errCode %d, want 0", code)
			}
		})
	}
}

// TestScheduleCron_RejectsWhatTheSchedulerCouldNotActdOn keeps unusable values
// out of the store. The scheduler loop runs in the background with nobody to
// report a bad value to, so the moment to refuse one is here.
func TestScheduleCron_RejectsWhatTheSchedulerCouldNotActOn(t *testing.T) {
	s := cronSession(t, nil)
	ctx := context.Background()

	for _, tc := range []struct {
		name                             string
		wf, cronExpr, timezone, inputStr string
		wantIn                           string
	}{
		{"unparseable cron", "wf", "not a cron", "UTC", `{}`, "cron"},
		{"unknown timezone", "wf", "* * * * *", "Mars/Olympus", `{}`, "timezone"},
		{"input is not JSON", "wf", "* * * * *", "UTC", `{oops`, "JSON"},
		{"empty workflow name", "", "* * * * *", "UTC", `{}`, "workflow name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.createCronSchedule(ctx, tc.wf, tc.cronExpr, tc.timezone, tc.inputStr)
			if err == nil {
				t.Fatal("accepted a value the scheduler could not act on")
			}
			// The session has no store, so reaching the store check means
			// validation did not run first and the guest was told the wrong
			// thing about its own argument.
			if strings.Contains(err.Error(), "no workflow store") {
				t.Fatalf("validation ran after the store check: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantIn)
			}
		})
	}
}

// TestListCrons_ReplayReturnsTheRecordedListNotALiveOne is the determinism
// property. The set of schedules changes over time; a replay that re-read it
// would hand the workflow a different answer than the run recorded, and every
// decision downstream of it would diverge.
//
// The session has no store at all, so a live read cannot succeed. Getting the
// recorded array back is proof the store was never consulted.
func TestListCrons_ReplayReturnsTheRecordedListNotALiveOne(t *testing.T) {
	recorded := `[{"schedule_id":"cron-abc","workflow_name":"w","cron_expr":"* * * * *","timezone":"UTC","input":"{}","enabled":true}]`

	s := &execSession{
		engine:   NewEngine(nil, nil),
		isReplay: true,
		history: []EventRecord{
			{Step: 0, EventType: EventTypeListCrons, CronResult: recorded},
		},
	}

	h := newTestHostFuncHarness(t, "cleat_list_crons",
		[]byte{wasmI32, wasmI32}, []byte{wasmI64}, true, s)

	const outPtr, outMax = 256, 1024
	got, err := h.call(outPtr, outMax)
	if err != nil {
		t.Fatalf("call cleat_list_crons: %v", err)
	}
	errCode, written := decodeExportResult(got)
	if errCode != 0 {
		data, _ := h.mem.Read(outPtr, written)
		t.Fatalf("errCode = %d, want 0: %s", errCode, data)
	}
	data, ok := h.mem.Read(outPtr, written)
	if !ok {
		t.Fatal("read from memory failed")
	}
	if string(data) != recorded {
		t.Errorf("replay returned  %s\nwant the recorded %s", data, recorded)
	}
}

// TestScheduleCron_ReplayReturnsTheRecordedIDWithoutCreatingAnything is the
// same property for the call that has a side effect: replaying must not
// create a second schedule.
func TestScheduleCron_ReplayReturnsTheRecordedIDWithoutCreatingAnything(t *testing.T) {
	s := &execSession{
		engine:   NewEngine(nil, nil),
		isReplay: true,
		history: []EventRecord{{
			Step:             0,
			EventType:        EventTypeScheduleCron,
			CronWorkflowName: "nightly",
			CronExpr:         "0 3 * * *",
			CronTimezone:     "UTC",
			CronInput:        `{}`,
			CronScheduleID:   "cron-deadbeef01",
		}},
	}

	code, out := callScheduleCron(t, s, "nightly", "0 3 * * *", "UTC", `{}`)
	if code != 0 {
		t.Fatalf("errCode = %d, want 0: %s", code, out)
	}
	if out != "cron-deadbeef01" {
		t.Errorf("replay returned %q, want the recorded cron-deadbeef01", out)
	}
}

// TestScheduleCron_ReplayReportsDivergence: a workflow that scheduled a
// different cron on this run than the history records is non-deterministic,
// and must be told so rather than quietly getting the old schedule's ID.
func TestScheduleCron_ReplayReportsDivergence(t *testing.T) {
	s := &execSession{
		engine:   NewEngine(nil, nil),
		isReplay: true,
		history: []EventRecord{{
			Step:             0,
			EventType:        EventTypeScheduleCron,
			CronWorkflowName: "nightly",
			CronExpr:         "0 3 * * *",
			CronTimezone:     "UTC",
			CronInput:        `{}`,
			CronScheduleID:   "cron-deadbeef01",
		}},
	}

	// Every argument is checked, not just the two that name the schedule: a
	// run that changed only its timezone would otherwise be handed the
	// recorded ID and carry on believing it scheduled somewhere else.
	for _, tc := range []struct {
		name                             string
		wf, cronExpr, timezone, inputStr string
	}{
		{"cron expression", "nightly", "0 4 * * *", "UTC", `{}`},
		{"workflow name", "weekly", "0 3 * * *", "UTC", `{}`},
		{"timezone", "nightly", "0 3 * * *", "Europe/Paris", `{}`},
		{"input", "nightly", "0 3 * * *", "UTC", `{"changed":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replay := &execSession{engine: s.engine, isReplay: true, history: s.history}
			code, out := callScheduleCron(t, replay, tc.wf, tc.cronExpr, tc.timezone, tc.inputStr)
			if code == 0 {
				t.Fatalf("a changed %s replayed as a success, returning %q", tc.name, out)
			}
			if !strings.Contains(out, "replay divergence") {
				t.Errorf("message = %q, want it to name the divergence", out)
			}
		})
	}
}

// TestCronEventsSurviveThePayloadRoundTrip covers the two arms in
// store_events.go. A missing arm is silent on both sides: the event writes an
// empty payload and loads back zero-valued, so the damage only shows up as a
// replay divergence in some later run.
func TestCronEventsSurviveThePayloadRoundTrip(t *testing.T) {
	for _, rec := range []EventRecord{
		{
			Step: 0, EventType: EventTypeScheduleCron,
			CronWorkflowName: "nightly", CronExpr: "0 3 * * *",
			CronTimezone: "America/New_York", CronInput: `{"a":1}`,
			CronScheduleID: "cron-abc123",
		},
		{Step: 1, EventType: EventTypeDeleteCron, CronScheduleID: "cron-abc123", Err: "gone"},
		{Step: 2, EventType: EventTypeListCrons, CronResult: `[{"schedule_id":"cron-abc123"}]`},
	} {
		t.Run(string(rec.EventType), func(t *testing.T) {
			payload, err := eventRecordToPayload(rec)
			if err != nil {
				t.Fatalf("eventRecordToPayload: %v", err)
			}
			var got EventRecord
			got.EventType = rec.EventType
			populateFromPayload(&got, payload)

			got.Step = rec.Step
			if got != rec {
				t.Errorf("round trip lost fields\n got %+v\nwant %+v\npayload: %s", got, rec, payload)
			}
		})
	}
}

// TestCronEventsSurviveCompaction is the same question for the other
// serialisation. Two event types already in the code/type maps have no arm in
// buildFullHistoryFromCompaction, so map membership is not evidence that a
// type round-trips.
func TestCronEventsSurviveCompaction(t *testing.T) {
	original := []EventRecord{
		{
			Step: 0, EventType: EventTypeScheduleCron,
			CronWorkflowName: "nightly", CronExpr: "0 3 * * *",
			CronTimezone: "America/New_York", CronInput: `{"a":1}`,
			CronScheduleID: "cron-abc123",
		},
		{Step: 1, EventType: EventTypeDeleteCron, CronScheduleID: "cron-abc123"},
		{Step: 2, EventType: EventTypeListCrons, CronResult: `[{"schedule_id":"cron-abc123"}]`},
	}

	state := extractCompactionState(original)
	rebuilt := buildFullHistoryFromCompaction(nil, state)
	if len(rebuilt) != len(original) {
		t.Fatalf("rebuilt %d events, want %d", len(rebuilt), len(original))
	}
	for i := range original {
		if rebuilt[i].EventType != original[i].EventType {
			t.Errorf("event %d type = %q, want %q", i, rebuilt[i].EventType, original[i].EventType)
			continue
		}
		if rebuilt[i].CronWorkflowName != original[i].CronWorkflowName ||
			rebuilt[i].CronExpr != original[i].CronExpr ||
			rebuilt[i].CronTimezone != original[i].CronTimezone ||
			rebuilt[i].CronInput != original[i].CronInput ||
			rebuilt[i].CronScheduleID != original[i].CronScheduleID ||
			rebuilt[i].CronResult != original[i].CronResult {
			t.Errorf("event %d lost cron fields\n got %+v\nwant %+v", i, rebuilt[i], original[i])
		}
	}
}

// TestCronScheduleViewWireShape pins the JSON keys ListCrons emits. The SDK's
// cleat.CronSchedule is a separate declaration of the same contract, so this
// is where the contract is written down on the engine side.
func TestCronScheduleViewWireShape(t *testing.T) {
	out, err := json.Marshal(cronScheduleView{
		ScheduleID: "cron-1", WorkflowName: "w", CronExpr: "* * * * *",
		Timezone: "UTC", Input: "{}", Enabled: true,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"schedule_id":"cron-1","workflow_name":"w","cron_expr":"* * * * *","timezone":"UTC","input":"{}","enabled":true}`
	if string(out) != want {
		t.Errorf("wire shape changed\n got %s\nwant %s", out, want)
	}
}

// callScheduleCron drives cleat_schedule_cron through the real wazero closure
// and returns the error code plus whatever was written to the out buffer.
func callScheduleCron(t *testing.T, s *execSession, workflowName, cronExpr, timezone, inputJSON string) (uint32, string) {
	t.Helper()
	h := newTestHostFuncHarness(t, "cleat_schedule_cron",
		[]byte{wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32},
		[]byte{wasmI64}, true, s)

	offsets := make([]uint64, 0, 8)
	at := uint32(64)
	for _, arg := range []string{workflowName, cronExpr, timezone, inputJSON} {
		if !h.mem.Write(at, []byte(arg)) {
			t.Fatalf("write %q to memory failed", arg)
		}
		offsets = append(offsets, uint64(at), uint64(len(arg)))
		at += uint32(len(arg)) + 8
	}
	const outPtr, outMax = 2048, 1024

	got, err := h.call(append(offsets, outPtr, outMax)...)
	if err != nil {
		t.Fatalf("call cleat_schedule_cron: %v", err)
	}
	errCode, written := decodeExportResult(got)
	data, ok := h.mem.Read(outPtr, written)
	if !ok {
		t.Fatal("read from memory failed")
	}
	return errCode, string(data)
}

func decodeCronResult(result int64) uint32 {
	errCode, _ := decodeExportResult(uint64(result))
	return errCode
}
