//go:build cgo

package csharp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// Group F of cleat#4080's host-call coverage: durable-send, durable-schedule-invoke, durable-schedule-cron, durable-delete-cron and
// durable-list-crons, each driven through the real engine and wasmtime backend by a named entry of the conformance component
// (packages/cleat-csharp/conformance/GroupF.cs). Nothing here claims C# is supported.

// fakeScheduleStore is the part of the workflow store the cron calls use. The rest of the interface is the embedded nil value: a call to
// anything else panics, which is the right answer for a test that did not expect it.
type fakeScheduleStore struct {
	engine.WorkflowStore
	mu       sync.Mutex
	byName   map[string]engine.Schedule
	listErr  error
	createFn func(engine.Schedule) error
}

func newFakeScheduleStore() *fakeScheduleStore {
	return &fakeScheduleStore{byName: map[string]engine.Schedule{}}
}

func (f *fakeScheduleStore) CreateSchedule(_ context.Context, s engine.Schedule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createFn != nil {
		if err := f.createFn(s); err != nil {
			return err
		}
	}
	f.byName[s.Name] = s
	return nil
}

func (f *fakeScheduleStore) ListSchedules(context.Context) ([]engine.Schedule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]engine.Schedule, 0, len(f.byName))
	for _, s := range f.byName {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeScheduleStore) DeleteSchedule(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.byName[name]; !ok {
		return engine.ErrScheduleNotFound
	}
	delete(f.byName, name)
	return nil
}

func (f *fakeScheduleStore) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n []string
	for k := range f.byName {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

func withScheduleStore(f *fakeScheduleStore) []engine.EngineOption {
	return []engine.EngineOption{engine.WithWorkflowStore(f)}
}

func cronInput(workflow, expr, tz, input string) string {
	b, _ := json.Marshal(map[string]any{"workflow": workflow, "expr": expr, "tz": tz, "input": input})
	return string(b)
}

// recordOf returns the one history record of this type, failing if there is not exactly one.
func recordOf(t *testing.T, res conformanceResult, typ engine.EventType) engine.EventRecord {
	t.Helper()
	var got []engine.EventRecord
	for _, r := range res.History {
		if r.EventType == typ {
			got = append(got, r)
		}
	}
	if len(got) != 1 {
		t.Fatalf("history has %d %s record(s), want exactly 1: %+v", len(got), typ, res.History)
	}
	return got[0]
}

// The send is fire-and-forget: the HOST hands it to the service on a goroutine after the call returns, so what is deterministic and
// checkable is the RECORD, not the service's receipt (which a test could only see by sleeping).
func TestCSharpGroupFSendRecordsTheRequestAndReturnsAtOnce(t *testing.T) {
	res := runConformanceEntry(t, "f_send", entryRun{Input: `{"service":"mail","operation":"send","request":{"to":"a@b"}}`})
	if mustFinish(t, res)["sent"] != true {
		t.Fatal("Send did not return")
	}
	rec := recordOf(t, res, engine.EventTypeDurableSend)
	if rec.Service != "mail" || rec.Op != "send" || rec.Request != `{"to":"a@b"}` {
		t.Errorf("the recorded send is %+v, want mail/send with the request unchanged", rec)
	}
}

// A replay does not send again and answers from the record. The control is that the fresh run recorded one send.
func TestCSharpGroupFSendReplayReadsTheRecordAndSendsNothing(t *testing.T) {
	in := `{"service":"mail","operation":"send","request":{"n":1}}`
	fresh := runConformanceEntry(t, "f_send", entryRun{WorkflowID: "wf-f-1", Input: in})
	recordOf(t, fresh, engine.EventTypeDurableSend)
	replay := runConformanceEntry(t, "f_send", entryRun{WorkflowID: "wf-f-1", Input: in, Replay: fresh.History})
	if mustFinish(t, replay)["sent"] != true {
		t.Fatal("the replay did not complete")
	}
	if len(replay.Calls) != 0 {
		t.Errorf("the replay reached the service %v, want nothing sent", replay.Calls)
	}
}

// A replay of a send over a history whose record at that step is a different call is a divergence, which the host reports with status 1.
func TestCSharpGroupFSendReplayDivergenceIsAnError(t *testing.T) {
	in := `{"service":"mail","operation":"send","request":{}}`
	fresh := runConformanceEntry(t, "f_send", entryRun{WorkflowID: "wf-f-2", Input: in})
	hist := append([]engine.EventRecord(nil), fresh.History...)
	for i := range hist {
		if hist[i].EventType == engine.EventTypeDurableSend {
			hist[i].EventType = engine.EventTypeCall
		}
	}
	res := runConformanceEntry(t, "f_send", entryRun{WorkflowID: "wf-f-2", Input: in, Replay: hist})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "send failed: host status code 1") {
		t.Errorf("err = %v result = %q, want the run to fail with the send's host status named", res.Err, res.Result)
	}
}

func TestCSharpGroupFScheduleInvokeRecordsTheDelay(t *testing.T) {
	res := runConformanceEntry(t, "f_schedule_invoke", entryRun{Input: `{"service":"mail","operation":"digest","request":{"d":1},"delayMs":90000}`})
	if mustFinish(t, res)["scheduled"] != true {
		t.Fatal("ScheduleInvoke did not return")
	}
	rec := recordOf(t, res, engine.EventTypeDurableScheduleInvoke)
	if rec.Service != "mail" || rec.Op != "digest" || rec.Request != `{"d":1}` || rec.DurationMs != 90000 {
		t.Errorf("the recorded schedule is %+v, want mail/digest, the request unchanged and a delay of 90000 ms", rec)
	}
}

// A sub-millisecond delay is rounded UP, and zero is allowed.
func TestCSharpGroupFScheduleInvokeRoundsUpAndAllowsZero(t *testing.T) {
	for _, tc := range []struct {
		delay string
		want  int64
	}{{"0.2", 1}, {"0", 0}, {"1500.4", 1501}} {
		res := runConformanceEntry(t, "f_schedule_invoke", entryRun{Input: `{"service":"s","operation":"o","request":{},"delayMs":` + tc.delay + `}`})
		mustFinish(t, res)
		if got := recordOf(t, res, engine.EventTypeDurableScheduleInvoke).DurationMs; got != tc.want {
			t.Errorf("delay %s ms reached the host as %d, want %d", tc.delay, got, tc.want)
		}
	}
}

// A negative delay would be read by the host as a huge unsigned one, and a delay past about 292 years wraps the host's nanosecond arithmetic
// negative, which would deliver at once. Both are refused before the host.
func TestCSharpGroupFScheduleInvokeRefusesADelayTheHostCannotCount(t *testing.T) {
	for _, d := range []string{"-1", "1e13"} {
		res := runConformanceEntry(t, "f_schedule_invoke", entryRun{Input: `{"service":"s","operation":"o","request":{},"delayMs":` + d + `}`})
		if m := mustFinish(t, res); m["invalid"] != "delay" {
			t.Errorf("delay %s ms: got %v, want the SDK to refuse it", d, m)
		}
		for _, r := range res.History {
			if r.EventType == engine.EventTypeDurableScheduleInvoke {
				t.Errorf("delay %s ms reached the host and was recorded: %+v", d, r)
			}
		}
	}
}

func TestCSharpGroupFCronCreateReturnsAnIdAndStoresTheSchedule(t *testing.T) {
	st := newFakeScheduleStore()
	res := runConformanceEntry(t, "f_cron_create", entryRun{WorkflowID: "wf-f-3", Input: cronInput("nightly", "0 0 * * *", "America/New_York", `{"k":1}`), Options: withScheduleStore(st)})
	m := mustFinish(t, res)
	id, _ := m["id"].(string)
	if id == "" || m["failed"] != nil {
		t.Fatalf("got %v, want a schedule id", m)
	}
	got, ok := st.byName[id]
	if !ok || len(st.byName) != 1 {
		t.Fatalf("the store holds %v, want exactly the schedule %q", st.names(), id)
	}
	if got.DefName != "nightly" || got.CronExpression != "0 0 * * *" || got.Timezone != "America/New_York" || string(got.Input) != `{"k":1}` {
		t.Errorf("the stored schedule is %+v, want the four arguments unchanged", got)
	}
}

// Each refusal is the HOST's text raised as an error (cron / schedule_cron), and nothing is stored: the control is the previous test.
func TestCSharpGroupFCronCreateRefusalsAreErrorsAndStoreNothing(t *testing.T) {
	for _, tc := range []struct {
		name, workflow, expr, tz, input, want string
	}{
		// The dispatcher refuses an invalid NAME before the engine runs, as a permanent call failure rather than a store-level text.
		{"empty name", "", "0 0 * * *", "UTC", "{}", "not a valid name"},
		{"bad expression", "nightly", "not a cron", "UTC", "{}", "schedule_cron"},
		{"bad zone", "nightly", "0 0 * * *", "Mars/Olympus", "{}", "schedule_cron"},
		{"input not JSON", "nightly", "0 0 * * *", "UTC", "{oops", "input is not valid JSON"},
	} {
		st := newFakeScheduleStore()
		m := mustFinish(t, runConformanceEntry(t, "f_cron_create", entryRun{Input: cronInput(tc.workflow, tc.expr, tc.tz, tc.input), Options: withScheduleStore(st)}))
		if m["failed"] != true || !strings.HasPrefix(fmt.Sprint(m["type"]), "CleatCall") || m["service"] != "cron" || m["operation"] != "schedule_cron" {
			t.Errorf("%s: got %v, want a CleatCall exception for cron/schedule_cron", tc.name, m)
			continue
		}
		if !strings.Contains(fmt.Sprint(m["message"]), tc.want) {
			t.Errorf("%s: message = %q, want it to contain %q", tc.name, m["message"], tc.want)
		}
		if len(st.byName) != 0 {
			t.Errorf("%s: the store holds %v after a refusal", tc.name, st.names())
		}
	}
}

func TestCSharpGroupFCronCreateAStoreFailureIsAnError(t *testing.T) {
	st := newFakeScheduleStore()
	st.createFn = func(engine.Schedule) error { return errors.New("the schedule table is locked") }
	m := mustFinish(t, runConformanceEntry(t, "f_cron_create", entryRun{Input: cronInput("nightly", "0 0 * * *", "UTC", "{}"), Options: withScheduleStore(st)}))
	if m["failed"] != true || !strings.Contains(fmt.Sprint(m["message"]), "the schedule table is locked") {
		t.Errorf("got %v, want the store's failure raised", m)
	}
}

// create, list, delete, list, delete again, list: the second delete of a schedule that is gone succeeds (a retry must not fail the workflow).
func TestCSharpGroupFCronCycleListsCreatesDeletesAndToleratesASecondDelete(t *testing.T) {
	st := newFakeScheduleStore()
	res := runConformanceEntry(t, "f_cron_cycle", entryRun{WorkflowID: "wf-f-4", Input: cronInput("nightly", "30 2 * * 1", "Europe/Paris", `{"a":true}`), Options: withScheduleStore(st)})
	m := mustFinish(t, res)
	id := m["id"].(string)
	want := []any{map[string]any{"id": id, "workflow": "nightly", "expr": "30 2 * * 1", "tz": "Europe/Paris", "input": `{"a":true}`, "enabled": true}}
	for key, w := range map[string]any{"before": []any{}, "afterCreate": want, "afterDelete": []any{}, "afterSecondDelete": []any{}} {
		if fmt.Sprint(m[key]) != fmt.Sprint(w) {
			t.Errorf("%s = %v, want %v", key, m[key], w)
		}
	}
	if len(st.byName) != 0 {
		t.Errorf("the store still holds %v", st.names())
	}
}

// A schedule with no zone is reported in UTC, and one created with no input has {} (what the host stores and lists).
func TestCSharpGroupFCronDefaultsAreReportedAsTheHostSpellsThem(t *testing.T) {
	st := newFakeScheduleStore()
	mustFinish(t, runConformanceEntry(t, "f_cron_create", entryRun{Input: cronInput("nightly", "0 0 * * *", "", ""), Options: withScheduleStore(st)}))
	res := runConformanceEntry(t, "f_list_crons", entryRun{Options: withScheduleStore(st)})
	list := mustFinish(t, res)["schedules"].([]any)
	if len(list) != 1 {
		t.Fatalf("list = %v", list)
	}
	s := list[0].(map[string]any)
	if s["tz"] != "UTC" || s["input"] != "{}" {
		t.Errorf("schedule = %v, want timezone UTC and input {}", s)
	}
}

// The host answers a failed list with its error text in the place of the JSON. An SDK that treated unparseable text as "no schedules"
// would tell a workflow that deletes-then-recreates its cron that it has none.
func TestCSharpGroupFListCronsAStoreFailureIsAnErrorNotAnEmptyList(t *testing.T) {
	st := newFakeScheduleStore()
	st.listErr = errors.New("the schedule table is down")
	m := mustFinish(t, runConformanceEntry(t, "f_list_crons", entryRun{Options: withScheduleStore(st)}))
	if m["failed"] != true || !strings.Contains(fmt.Sprint(m["message"]), "the schedule table is down") {
		t.Errorf("got %v, want a CleatCallException carrying the host's text", m)
	}
	if m["schedules"] != nil {
		t.Errorf("got schedules %v for a failed list", m["schedules"])
	}
}

// The list is recorded: a replay returns what the first run saw, not what the store holds now.
func TestCSharpGroupFListCronsReplayReturnsTheRecordedList(t *testing.T) {
	st := newFakeScheduleStore()
	mustFinish(t, runConformanceEntry(t, "f_cron_create", entryRun{WorkflowID: "wf-f-5", Input: cronInput("nightly", "0 0 * * *", "UTC", "{}"), Options: withScheduleStore(st)}))
	fresh := runConformanceEntry(t, "f_list_crons", entryRun{WorkflowID: "wf-f-6", Options: withScheduleStore(st)})
	if n := len(mustFinish(t, fresh)["schedules"].([]any)); n != 1 {
		t.Fatalf("the fresh list has %d schedules, want 1", n)
	}
	empty := newFakeScheduleStore()
	replay := runConformanceEntry(t, "f_list_crons", entryRun{WorkflowID: "wf-f-6", Replay: fresh.History, Options: withScheduleStore(empty)})
	if n := len(mustFinish(t, replay)["schedules"].([]any)); n != 1 {
		t.Errorf("the replay listed %d schedules, want the recorded 1 even though the store is now empty", n)
	}
}

// A retired schedule is listed with enabled=false. Without one in the store, a list that hard-wired enabled=true would pass every other test.
func TestCSharpGroupFListCronsReportsARetiredScheduleAsDisabled(t *testing.T) {
	st := newFakeScheduleStore()
	retired := time.Now()
	st.byName["a-live"] = engine.Schedule{Name: "a-live", DefName: "w1", CronExpression: "0 0 * * *", Input: json.RawMessage(`{}`)}
	st.byName["b-retired"] = engine.Schedule{Name: "b-retired", DefName: "w2", CronExpression: "0 1 * * *", Input: json.RawMessage(`{}`), DisabledAt: &retired}
	list := mustFinish(t, runConformanceEntry(t, "f_list_crons", entryRun{Options: withScheduleStore(st)}))["schedules"].([]any)
	if len(list) != 2 {
		t.Fatalf("list = %v, want both schedules, ordered by id", list)
	}
	live, gone := list[0].(map[string]any), list[1].(map[string]any)
	if live["id"] != "a-live" || live["enabled"] != true || gone["id"] != "b-retired" || gone["enabled"] != false {
		t.Errorf("live = %v, retired = %v, want enabled true then false", live, gone)
	}
}
