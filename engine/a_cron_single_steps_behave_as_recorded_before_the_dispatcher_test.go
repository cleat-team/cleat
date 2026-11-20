package engine

// cleat#4152, group G1c of docs/design/replay-step-dispatcher.md: a characterization of ScheduleCron, DeleteCron and ListCrons, RECORDED FROM THE
// CODE AS IT WAS BEFORE THEY WENT THROUGH step() (develop 46b7308a, no production change), and kept so that moving them is checkable as "changes
// where the code lives and nothing else" and every row it does change is visible in the migration's diff.
//
// Each call is driven through a set of situations and the result is one string per (call, situation): the packed result, the bytes the call wrote
// to the guest's buffer, whether the session is still replaying, the step count, the events the call RECORDED, and what the call did to the OUTSIDE
// WORLD (the schedules in the store afterwards, and how many create, delete and list calls reached it). A row that changes after the migration is a
// behaviour change.
//
// What a row cannot say: a schedule's next_run_at (time-dependent, not observed), the timestamp of a recorded event, and anything the wasm backends add
// around the call.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const g1cBufLen = 512

// g1cStore is a WorkflowStore that keeps schedules in memory and counts the calls that reach it. Everything else is the embedded stub's.
type g1cStore struct {
	stubWorkflowStore
	mu        sync.Mutex
	schedules []Schedule
	creates   int
	deletes   int
	lists     int
	createErr error
	deleteErr error
	listErr   error
}

func (c *g1cStore) CreateSchedule(_ context.Context, s Schedule) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.creates++
	if c.createErr != nil {
		return c.createErr
	}
	c.schedules = append(c.schedules, s)
	return nil
}

func (c *g1cStore) ListSchedules(context.Context) ([]Schedule, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lists++
	if c.listErr != nil {
		return nil, c.listErr
	}
	out := append([]Schedule(nil), c.schedules...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *g1cStore) DeleteSchedule(_ context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletes++
	if c.deleteErr != nil {
		return c.deleteErr
	}
	for i := range c.schedules {
		if c.schedules[i].Name == name {
			c.schedules = append(c.schedules[:i], c.schedules[i+1:]...)
			return nil
		}
	}
	return ErrScheduleNotFound
}

func (c *g1cStore) describe() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	parts := make([]string, 0, len(c.schedules))
	sorted := append([]Schedule(nil), c.schedules...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, s := range sorted {
		parts = append(parts, fmt.Sprintf("%s:%s|%s|%s|%s", s.Name, s.DefName, s.CronExpression, s.Timezone, string(s.Input)))
	}
	return fmt.Sprintf("store=[%s] creates=%d deletes=%d lists=%d", strings.Join(parts, " "), c.creates, c.deletes, c.lists)
}

type g1cCall struct {
	name string
	call func(ctx context.Context, s *execSession) int64
}

var g1cCalls = []g1cCall{
	{"schedule_cron", func(ctx context.Context, s *execSession) int64 {
		return s.ScheduleCron(ctx, nil, "nightly", "0 3 * * *", "America/New_York", `{"depth":2}`, 0, g1cBufLen)
	}},
	{"delete_cron", func(ctx context.Context, s *execSession) int64 {
		return s.DeleteCron(ctx, nil, "cron-existing")
	}},
	{"list_crons", func(ctx context.Context, s *execSession) int64 {
		return s.ListCrons(ctx, nil, 0, g1cBufLen)
	}},
}

func g1cSession(st *g1cStore, history []EventRecord, replaying bool, opts ...EngineOption) *execSession {
	base := []EngineOption{WithWorkflowStore(st)}
	return &execSession{
		engine:     NewEngine(nil, nil, append(base, opts...)...),
		workflowID: "wf-g1c", tenantID: "default", defName: "caller-def", nowMs: time.Now().UnixMilli(),
		deferrals: map[string]string{}, queryState: map[string]string{},
		isReplay: replaying, history: history,
	}
}

func g1cDescribe(history []EventRecord, from int) string {
	var parts []string
	for _, r := range history[from:] {
		parts = append(parts, fmt.Sprintf("%s/step=%d/wf=%q/expr=%q/tz=%q/input=%q/id=%q/result=%q/err=%q",
			r.EventType, r.Step, r.CronWorkflowName, r.CronExpr, r.CronTimezone, r.CronInput, r.CronScheduleID, r.CronResult, r.Err))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// g1cObserve runs the call and renders everything observable about it, including what reached the store.
func g1cObserve(gc g1cCall, st *g1cStore, s *execSession) string {
	buf := make([]byte, g1cBufLen)
	ctx := context.WithValue(context.Background(), wasmMemBufKey{}, buf)
	before := len(s.history)
	res := gc.call(ctx, s)
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return fmt.Sprintf("res=%#x wrote=%q isReplay=%v step=%d recorded=%s %s", res, string(buf[:n]), s.isReplay, s.stepCount, g1cDescribe(s.history, before), st.describe())
}

// g1cSeeded is a store that already holds the schedule delete_cron deletes and list_crons lists.
func g1cSeeded() *g1cStore {
	return &g1cStore{schedules: []Schedule{{Name: "cron-existing", DefName: "existing-wf", CronExpression: "*/5 * * * *", Timezone: "UTC", Input: []byte(`{"k":1}`)}}}
}

// g1cOwn is the record a fresh, healthy run of this call leaves.
func g1cOwn(gc g1cCall) []EventRecord {
	switch gc.name {
	case "delete_cron":
		return []EventRecord{{Step: 0, EventType: EventTypeDeleteCron, CronScheduleID: "cron-existing"}}
	case "list_crons":
		return []EventRecord{{Step: 0, EventType: EventTypeListCrons, CronResult: `[{"schedule_id":"cron-recorded"}]`}}
	default:
		return []EventRecord{{Step: 0, EventType: EventTypeScheduleCron, CronWorkflowName: "nightly", CronExpr: "0 3 * * *", CronTimezone: "America/New_York", CronInput: `{"depth":2}`, CronScheduleID: "cron-recorded"}}
	}
}

func g1cCharacterize(t *testing.T, gc g1cCall) map[string]string {
	t.Helper()
	got := map[string]string{}
	own := func() []EventRecord { return append([]EventRecord(nil), g1cOwn(gc)...) }
	other := func() []EventRecord {
		return []EventRecord{{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Response: "{}"}}
	}
	run := func(history []EventRecord, replaying bool, mut func(*g1cStore, *execSession), opts ...EngineOption) (*g1cStore, *execSession) {
		st := g1cSeeded()
		s := g1cSession(st, history, replaying, opts...)
		if mut != nil {
			mut(st, s)
		}
		return st, s
	}

	st, s := run(nil, false, nil)
	got["fresh"] = g1cObserve(gc, st, s)

	st, s = run(own(), true, nil)
	got["replay"] = g1cObserve(gc, st, s)

	st, s = run(own(), true, nil)
	g1cObserve(gc, st, s)
	got["frontier"] = g1cObserve(gc, st, s)

	st, s = run(other(), true, nil)
	got["divergence"] = g1cObserve(gc, st, s)

	st, s = run(nil, true, nil, WithDeferPhase())
	got["segment_fresh"] = g1cObserve(gc, st, s)

	st, s = run(own(), true, nil, WithDeferPhase())
	g1cObserve(gc, st, s)
	got["segment_frontier"] = g1cObserve(gc, st, s)

	// A defer BODY (inDeferDrain) is let through where the segment's own work is refused.
	st, s = run(nil, false, func(_ *g1cStore, s *execSession) { s.inDeferDrain = true }, WithDeferPhase())
	got["segment_defer_body_fresh"] = g1cObserve(gc, st, s)

	st, s = run(nil, false, func(_ *g1cStore, s *execSession) { s.eventWriteFailure = errors.New("event write refused") })
	got["write_failed_fresh"] = g1cObserve(gc, st, s)

	st, s = run(own(), true, func(_ *g1cStore, s *execSession) { s.eventWriteFailure = errors.New("event write refused") })
	g1cObserve(gc, st, s)
	got["write_failed_frontier"] = g1cObserve(gc, st, s)

	down := make(chan struct{})
	close(down)
	st, s = run(nil, false, nil, WithShutdownSignal(down))
	got["shutdown_fresh"] = g1cObserve(gc, st, s)

	st, s = run(own(), true, func(_ *g1cStore, s *execSession) {
		s.stepCallback = func(int, *EventRecord, map[string]string) ReplayStepAction { return ReplayQuit }
	})
	got["replay_quit"] = g1cObserve(gc, st, s)

	// a recorded failure replays as that failure
	rec := own()
	rec[0].Err = "the store refused"
	st, s = run(rec, true, nil)
	got["replay_error_record"] = g1cObserve(gc, st, s)

	switch gc.name {
	case "schedule_cron":
		st, s = run(nil, false, func(st *g1cStore, _ *execSession) { st.createErr = errors.New("store down") })
		got["fresh_store_error"] = g1cObserve(gc, st, s)
		st, s = run(nil, false, func(st *g1cStore, _ *execSession) { st.listErr = errors.New("list down") })
		got["fresh_list_error"] = g1cObserve(gc, st, s)
		st, s = run(nil, false, nil, WithMaxQuotaSchedules(1))
		got["fresh_quota_exceeded"] = g1cObserve(gc, st, s)
		// a retry at the same step finds its own row already created: success, not a duplicate
		st, s = run(nil, false, func(st *g1cStore, s *execSession) {
			st.schedules = append(st.schedules, Schedule{Name: scheduleIDFor(s.tenantID, s.workflowID, 0), DefName: "nightly"})
		})
		got["fresh_derived_id_already_exists"] = g1cObserve(gc, st, s)
		// all four arguments are compared on replay, and the old replay consumed the record BEFORE comparing
		for _, m := range []struct {
			name string
			mut  func(*EventRecord)
		}{
			{"replay_other_workflow", func(r *EventRecord) { r.CronWorkflowName = "weekly" }},
			{"replay_other_expression", func(r *EventRecord) { r.CronExpr = "0 4 * * *" }},
			{"replay_other_timezone", func(r *EventRecord) { r.CronTimezone = "UTC" }},
			{"replay_other_input", func(r *EventRecord) { r.CronInput = `{"depth":3}` }},
		} {
			rec := own()
			m.mut(&rec[0])
			st, s = run(rec, true, nil)
			got[m.name] = g1cObserve(gc, st, s)
		}
		// validation errors are recorded like any other failure
		bad := g1cCall{"schedule_cron_invalid", func(ctx context.Context, s *execSession) int64 {
			return s.ScheduleCron(ctx, nil, "nightly", "not a cron", "UTC", `{}`, 0, g1cBufLen)
		}}
		st, s = run(nil, false, nil)
		got["fresh_invalid_expression"] = g1cObserve(bad, st, s)
		empty := g1cCall{"schedule_cron_empty", func(ctx context.Context, s *execSession) int64 {
			return s.ScheduleCron(ctx, nil, "", "0 3 * * *", "UTC", `{}`, 0, g1cBufLen)
		}}
		st, s = run(nil, false, nil)
		got["fresh_empty_workflow_name"] = g1cObserve(empty, st, s)
	case "delete_cron":
		st, s = run(nil, false, func(st *g1cStore, _ *execSession) { st.schedules = nil })
		got["fresh_not_found"] = g1cObserve(gc, st, s)
		st, s = run(nil, false, func(st *g1cStore, _ *execSession) { st.deleteErr = errors.New("store down") })
		got["fresh_store_error"] = g1cObserve(gc, st, s)
		rec := own()
		rec[0].CronScheduleID = "cron-another"
		st, s = run(rec, true, nil)
		got["replay_other_schedule"] = g1cObserve(gc, st, s)
	case "list_crons":
		st, s = run(nil, false, func(st *g1cStore, _ *execSession) { st.schedules = nil })
		got["fresh_empty"] = g1cObserve(gc, st, s)
		st, s = run(nil, false, func(st *g1cStore, _ *execSession) { st.listErr = errors.New("store down") })
		got["fresh_store_error"] = g1cObserve(gc, st, s)
	}
	return got
}

// g1cGolden is the table recorded against the code as it was (develop 46b7308a), one string per (call, situation): the reference the migration is
// checked against. A row that changes is a behaviour change, and the PR that changes it says so.
var g1cGolden = map[string]map[string]string{
	"schedule_cron": {
		"divergence":                      "res=0x12100000001 wrote=\"replay divergence at step 0: ScheduleCron mismatch.\\n  workflow: nightly \\\"0 3 * * *\\\" tz=\\\"America/New_York\\\" input={\\\"depth\\\":2}\\n  history:   \\\"\\\" tz=\\\"\\\" input=\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=1 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"fresh":                           "res=0x1900000000 wrote=\"cron-c4d6fd9b7cb689995fd3\" isReplay=false step=1 recorded=[schedule_cron/step=0/wf=\"nightly\"/expr=\"0 3 * * *\"/tz=\"America/New_York\"/input=\"{\\\"depth\\\":2}\"/id=\"cron-c4d6fd9b7cb689995fd3\"/result=\"\"/err=\"\"] store=[cron-c4d6fd9b7cb689995fd3:nightly|0 3 * * *|America/New_York|{\"depth\":2} cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=1 deletes=0 lists=1",
		"fresh_derived_id_already_exists": "res=0x1900000000 wrote=\"cron-c4d6fd9b7cb689995fd3\" isReplay=false step=1 recorded=[schedule_cron/step=0/wf=\"nightly\"/expr=\"0 3 * * *\"/tz=\"America/New_York\"/input=\"{\\\"depth\\\":2}\"/id=\"cron-c4d6fd9b7cb689995fd3\"/result=\"\"/err=\"\"] store=[cron-c4d6fd9b7cb689995fd3:nightly||| cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=1",
		"fresh_empty_workflow_name":       "res=0x2500000001 wrote=\"schedule_cron: workflow name is empty\" isReplay=false step=1 recorded=[schedule_cron/step=0/wf=\"\"/expr=\"0 3 * * *\"/tz=\"UTC\"/input=\"{}\"/id=\"\"/result=\"\"/err=\"schedule_cron: workflow name is empty\"] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"fresh_invalid_expression":        "res=0x6e00000001 wrote=\"schedule_cron \\\"nightly\\\": cron: \\\"not a cron\\\" has 3 field(s), want 5: minute hour day-of-month month day-of-week\" isReplay=false step=1 recorded=[schedule_cron/step=0/wf=\"nightly\"/expr=\"not a cron\"/tz=\"UTC\"/input=\"{}\"/id=\"\"/result=\"\"/err=\"schedule_cron \\\"nightly\\\": cron: \\\"not a cron\\\" has 3 field(s), want 5: minute hour day-of-month month day-of-week\"] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"fresh_list_error":                "res=0x3200000001 wrote=\"schedule_cron \\\"nightly\\\": list schedules: list down\" isReplay=false step=1 recorded=[schedule_cron/step=0/wf=\"nightly\"/expr=\"0 3 * * *\"/tz=\"America/New_York\"/input=\"{\\\"depth\\\":2}\"/id=\"\"/result=\"\"/err=\"schedule_cron \\\"nightly\\\": list schedules: list down\"] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=1",
		"fresh_quota_exceeded":            "res=0x4300000001 wrote=\"schedule_cron \\\"nightly\\\": schedule quota exceeded (current 1, max 1)\" isReplay=false step=1 recorded=[schedule_cron/step=0/wf=\"nightly\"/expr=\"0 3 * * *\"/tz=\"America/New_York\"/input=\"{\\\"depth\\\":2}\"/id=\"\"/result=\"\"/err=\"schedule_cron \\\"nightly\\\": schedule quota exceeded (current 1, max 1)\"] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=1",
		"fresh_store_error":               "res=0x2300000001 wrote=\"schedule_cron \\\"nightly\\\": store down\" isReplay=false step=1 recorded=[schedule_cron/step=0/wf=\"nightly\"/expr=\"0 3 * * *\"/tz=\"America/New_York\"/input=\"{\\\"depth\\\":2}\"/id=\"\"/result=\"\"/err=\"schedule_cron \\\"nightly\\\": store down\"] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=1 deletes=0 lists=1",
		"frontier":                        "res=0x1900000000 wrote=\"cron-0fc15c5fa6764fc51285\" isReplay=false step=2 recorded=[schedule_cron/step=1/wf=\"nightly\"/expr=\"0 3 * * *\"/tz=\"America/New_York\"/input=\"{\\\"depth\\\":2}\"/id=\"cron-0fc15c5fa6764fc51285\"/result=\"\"/err=\"\"] store=[cron-0fc15c5fa6764fc51285:nightly|0 3 * * *|America/New_York|{\"depth\":2} cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=1 deletes=0 lists=1",
		"replay":                          "res=0xd00000000 wrote=\"cron-recorded\" isReplay=true step=1 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"replay_error_record":             "res=0x1100000001 wrote=\"the store refused\" isReplay=true step=1 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"replay_other_expression":         "res=0x14c00000001 wrote=\"replay divergence at step 0: ScheduleCron mismatch.\\n  workflow: nightly \\\"0 3 * * *\\\" tz=\\\"America/New_York\\\" input={\\\"depth\\\":2}\\n  history:  nightly \\\"0 4 * * *\\\" tz=\\\"America/New_York\\\" input={\\\"depth\\\":2}\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=1 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"replay_other_input":              "res=0x14c00000001 wrote=\"replay divergence at step 0: ScheduleCron mismatch.\\n  workflow: nightly \\\"0 3 * * *\\\" tz=\\\"America/New_York\\\" input={\\\"depth\\\":2}\\n  history:  nightly \\\"0 3 * * *\\\" tz=\\\"America/New_York\\\" input={\\\"depth\\\":3}\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=1 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"replay_other_timezone":           "res=0x13f00000001 wrote=\"replay divergence at step 0: ScheduleCron mismatch.\\n  workflow: nightly \\\"0 3 * * *\\\" tz=\\\"America/New_York\\\" input={\\\"depth\\\":2}\\n  history:  nightly \\\"0 3 * * *\\\" tz=\\\"UTC\\\" input={\\\"depth\\\":2}\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=1 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"replay_other_workflow":           "res=0x14b00000001 wrote=\"replay divergence at step 0: ScheduleCron mismatch.\\n  workflow: nightly \\\"0 3 * * *\\\" tz=\\\"America/New_York\\\" input={\\\"depth\\\":2}\\n  history:  weekly \\\"0 3 * * *\\\" tz=\\\"America/New_York\\\" input={\\\"depth\\\":2}\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=1 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"replay_quit":                     "res=0x0 wrote=\"\" isReplay=true step=1 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"segment_defer_body_fresh":        "res=0x1900000000 wrote=\"cron-c4d6fd9b7cb689995fd3\" isReplay=false step=1 recorded=[schedule_cron/step=0/wf=\"nightly\"/expr=\"0 3 * * *\"/tz=\"America/New_York\"/input=\"{\\\"depth\\\":2}\"/id=\"cron-c4d6fd9b7cb689995fd3\"/result=\"\"/err=\"\"] store=[cron-c4d6fd9b7cb689995fd3:nightly|0 3 * * *|America/New_York|{\"depth\":2} cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=1 deletes=0 lists=1",
		"segment_fresh":                   "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"segment_frontier":                "res=0x80000000 wrote=\"\" isReplay=false step=1 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"shutdown_fresh":                  "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"write_failed_fresh":              "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"write_failed_frontier":           "res=0x80000000 wrote=\"\" isReplay=false step=1 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
	},
	"delete_cron": {
		"divergence":               "res=0x1 wrote=\"\" isReplay=true step=1 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"fresh":                    "res=0x0 wrote=\"\" isReplay=false step=1 recorded=[delete_cron/step=0/wf=\"\"/expr=\"\"/tz=\"\"/input=\"\"/id=\"cron-existing\"/result=\"\"/err=\"\"] store=[] creates=0 deletes=1 lists=0",
		"fresh_not_found":          "res=0x0 wrote=\"\" isReplay=false step=1 recorded=[delete_cron/step=0/wf=\"\"/expr=\"\"/tz=\"\"/input=\"\"/id=\"cron-existing\"/result=\"\"/err=\"\"] store=[] creates=0 deletes=1 lists=0",
		"fresh_store_error":        "res=0x1 wrote=\"\" isReplay=false step=1 recorded=[delete_cron/step=0/wf=\"\"/expr=\"\"/tz=\"\"/input=\"\"/id=\"cron-existing\"/result=\"\"/err=\"store down\"] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=1 lists=0",
		"frontier":                 "res=0x0 wrote=\"\" isReplay=false step=2 recorded=[delete_cron/step=1/wf=\"\"/expr=\"\"/tz=\"\"/input=\"\"/id=\"cron-existing\"/result=\"\"/err=\"\"] store=[] creates=0 deletes=1 lists=0",
		"replay":                   "res=0x0 wrote=\"\" isReplay=true step=1 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"replay_error_record":      "res=0x1 wrote=\"\" isReplay=true step=1 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"replay_other_schedule":    "res=0x1 wrote=\"\" isReplay=true step=1 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"replay_quit":              "res=0x0 wrote=\"\" isReplay=true step=1 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"segment_defer_body_fresh": "res=0x0 wrote=\"\" isReplay=false step=1 recorded=[delete_cron/step=0/wf=\"\"/expr=\"\"/tz=\"\"/input=\"\"/id=\"cron-existing\"/result=\"\"/err=\"\"] store=[] creates=0 deletes=1 lists=0",
		"segment_fresh":            "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"segment_frontier":         "res=0x80000000 wrote=\"\" isReplay=false step=1 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"shutdown_fresh":           "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"write_failed_fresh":       "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"write_failed_frontier":    "res=0x80000000 wrote=\"\" isReplay=false step=1 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
	},
	"list_crons": {
		"divergence":               "res=0xcd00000001 wrote=\"replay divergence at step 0: ListCrons mismatch.\\n  history has: call\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=1 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"fresh":                    "res=0x8d00000000 wrote=\"[{\\\"schedule_id\\\":\\\"cron-existing\\\",\\\"workflow_name\\\":\\\"existing-wf\\\",\\\"cron_expr\\\":\\\"*/5 * * * *\\\",\\\"timezone\\\":\\\"UTC\\\",\\\"input\\\":\\\"{\\\\\\\"k\\\\\\\":1}\\\",\\\"enabled\\\":true}]\" isReplay=false step=1 recorded=[list_crons/step=0/wf=\"\"/expr=\"\"/tz=\"\"/input=\"\"/id=\"\"/result=\"[{\\\"schedule_id\\\":\\\"cron-existing\\\",\\\"workflow_name\\\":\\\"existing-wf\\\",\\\"cron_expr\\\":\\\"*/5 * * * *\\\",\\\"timezone\\\":\\\"UTC\\\",\\\"input\\\":\\\"{\\\\\\\"k\\\\\\\":1}\\\",\\\"enabled\\\":true}]\"/err=\"\"] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=1",
		"fresh_empty":              "res=0x200000000 wrote=\"[]\" isReplay=false step=1 recorded=[list_crons/step=0/wf=\"\"/expr=\"\"/tz=\"\"/input=\"\"/id=\"\"/result=\"[]\"/err=\"\"] store=[] creates=0 deletes=0 lists=1",
		"fresh_store_error":        "res=0x1600000001 wrote=\"list_crons: store down\" isReplay=false step=1 recorded=[list_crons/step=0/wf=\"\"/expr=\"\"/tz=\"\"/input=\"\"/id=\"\"/result=\"\"/err=\"list_crons: store down\"] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=1",
		"frontier":                 "res=0x8d00000000 wrote=\"[{\\\"schedule_id\\\":\\\"cron-existing\\\",\\\"workflow_name\\\":\\\"existing-wf\\\",\\\"cron_expr\\\":\\\"*/5 * * * *\\\",\\\"timezone\\\":\\\"UTC\\\",\\\"input\\\":\\\"{\\\\\\\"k\\\\\\\":1}\\\",\\\"enabled\\\":true}]\" isReplay=false step=2 recorded=[list_crons/step=1/wf=\"\"/expr=\"\"/tz=\"\"/input=\"\"/id=\"\"/result=\"[{\\\"schedule_id\\\":\\\"cron-existing\\\",\\\"workflow_name\\\":\\\"existing-wf\\\",\\\"cron_expr\\\":\\\"*/5 * * * *\\\",\\\"timezone\\\":\\\"UTC\\\",\\\"input\\\":\\\"{\\\\\\\"k\\\\\\\":1}\\\",\\\"enabled\\\":true}]\"/err=\"\"] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=1",
		"replay":                   "res=0x2100000000 wrote=\"[{\\\"schedule_id\\\":\\\"cron-recorded\\\"}]\" isReplay=true step=1 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"replay_error_record":      "res=0x1100000001 wrote=\"the store refused\" isReplay=true step=1 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"replay_quit":              "res=0x0 wrote=\"\" isReplay=true step=1 recorded=[] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=0",
		"segment_defer_body_fresh": "res=0x8d00000000 wrote=\"[{\\\"schedule_id\\\":\\\"cron-existing\\\",\\\"workflow_name\\\":\\\"existing-wf\\\",\\\"cron_expr\\\":\\\"*/5 * * * *\\\",\\\"timezone\\\":\\\"UTC\\\",\\\"input\\\":\\\"{\\\\\\\"k\\\\\\\":1}\\\",\\\"enabled\\\":true}]\" isReplay=false step=1 recorded=[list_crons/step=0/wf=\"\"/expr=\"\"/tz=\"\"/input=\"\"/id=\"\"/result=\"[{\\\"schedule_id\\\":\\\"cron-existing\\\",\\\"workflow_name\\\":\\\"existing-wf\\\",\\\"cron_expr\\\":\\\"*/5 * * * *\\\",\\\"timezone\\\":\\\"UTC\\\",\\\"input\\\":\\\"{\\\\\\\"k\\\\\\\":1}\\\",\\\"enabled\\\":true}]\"/err=\"\"] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=1",
		"segment_fresh":            "res=0x8d00000000 wrote=\"[{\\\"schedule_id\\\":\\\"cron-existing\\\",\\\"workflow_name\\\":\\\"existing-wf\\\",\\\"cron_expr\\\":\\\"*/5 * * * *\\\",\\\"timezone\\\":\\\"UTC\\\",\\\"input\\\":\\\"{\\\\\\\"k\\\\\\\":1}\\\",\\\"enabled\\\":true}]\" isReplay=false step=1 recorded=[list_crons/step=0/wf=\"\"/expr=\"\"/tz=\"\"/input=\"\"/id=\"\"/result=\"[{\\\"schedule_id\\\":\\\"cron-existing\\\",\\\"workflow_name\\\":\\\"existing-wf\\\",\\\"cron_expr\\\":\\\"*/5 * * * *\\\",\\\"timezone\\\":\\\"UTC\\\",\\\"input\\\":\\\"{\\\\\\\"k\\\\\\\":1}\\\",\\\"enabled\\\":true}]\"/err=\"\"] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=1",
		"segment_frontier":         "res=0x8d00000000 wrote=\"[{\\\"schedule_id\\\":\\\"cron-existing\\\",\\\"workflow_name\\\":\\\"existing-wf\\\",\\\"cron_expr\\\":\\\"*/5 * * * *\\\",\\\"timezone\\\":\\\"UTC\\\",\\\"input\\\":\\\"{\\\\\\\"k\\\\\\\":1}\\\",\\\"enabled\\\":true}]\" isReplay=false step=2 recorded=[list_crons/step=1/wf=\"\"/expr=\"\"/tz=\"\"/input=\"\"/id=\"\"/result=\"[{\\\"schedule_id\\\":\\\"cron-existing\\\",\\\"workflow_name\\\":\\\"existing-wf\\\",\\\"cron_expr\\\":\\\"*/5 * * * *\\\",\\\"timezone\\\":\\\"UTC\\\",\\\"input\\\":\\\"{\\\\\\\"k\\\\\\\":1}\\\",\\\"enabled\\\":true}]\"/err=\"\"] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=1",
		"shutdown_fresh":           "res=0x8d00000000 wrote=\"[{\\\"schedule_id\\\":\\\"cron-existing\\\",\\\"workflow_name\\\":\\\"existing-wf\\\",\\\"cron_expr\\\":\\\"*/5 * * * *\\\",\\\"timezone\\\":\\\"UTC\\\",\\\"input\\\":\\\"{\\\\\\\"k\\\\\\\":1}\\\",\\\"enabled\\\":true}]\" isReplay=false step=1 recorded=[list_crons/step=0/wf=\"\"/expr=\"\"/tz=\"\"/input=\"\"/id=\"\"/result=\"[{\\\"schedule_id\\\":\\\"cron-existing\\\",\\\"workflow_name\\\":\\\"existing-wf\\\",\\\"cron_expr\\\":\\\"*/5 * * * *\\\",\\\"timezone\\\":\\\"UTC\\\",\\\"input\\\":\\\"{\\\\\\\"k\\\\\\\":1}\\\",\\\"enabled\\\":true}]\"/err=\"\"] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=1",
		"write_failed_fresh":       "res=0x8d00000000 wrote=\"[{\\\"schedule_id\\\":\\\"cron-existing\\\",\\\"workflow_name\\\":\\\"existing-wf\\\",\\\"cron_expr\\\":\\\"*/5 * * * *\\\",\\\"timezone\\\":\\\"UTC\\\",\\\"input\\\":\\\"{\\\\\\\"k\\\\\\\":1}\\\",\\\"enabled\\\":true}]\" isReplay=false step=1 recorded=[list_crons/step=0/wf=\"\"/expr=\"\"/tz=\"\"/input=\"\"/id=\"\"/result=\"[{\\\"schedule_id\\\":\\\"cron-existing\\\",\\\"workflow_name\\\":\\\"existing-wf\\\",\\\"cron_expr\\\":\\\"*/5 * * * *\\\",\\\"timezone\\\":\\\"UTC\\\",\\\"input\\\":\\\"{\\\\\\\"k\\\\\\\":1}\\\",\\\"enabled\\\":true}]\"/err=\"\"] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=1",
		"write_failed_frontier":    "res=0x8d00000000 wrote=\"[{\\\"schedule_id\\\":\\\"cron-existing\\\",\\\"workflow_name\\\":\\\"existing-wf\\\",\\\"cron_expr\\\":\\\"*/5 * * * *\\\",\\\"timezone\\\":\\\"UTC\\\",\\\"input\\\":\\\"{\\\\\\\"k\\\\\\\":1}\\\",\\\"enabled\\\":true}]\" isReplay=false step=2 recorded=[list_crons/step=1/wf=\"\"/expr=\"\"/tz=\"\"/input=\"\"/id=\"\"/result=\"[{\\\"schedule_id\\\":\\\"cron-existing\\\",\\\"workflow_name\\\":\\\"existing-wf\\\",\\\"cron_expr\\\":\\\"*/5 * * * *\\\",\\\"timezone\\\":\\\"UTC\\\",\\\"input\\\":\\\"{\\\\\\\"k\\\\\\\":1}\\\",\\\"enabled\\\":true}]\"/err=\"\"] store=[cron-existing:existing-wf|*/5 * * * *|UTC|{\"k\":1}] creates=0 deletes=0 lists=1",
	},
}

func TestG1cCallsBehaveAsRecordedBeforeTheDispatcher(t *testing.T) {
	for _, gc := range g1cCalls {
		gc := gc
		t.Run(gc.name, func(t *testing.T) {
			want, ok := g1cGolden[gc.name]
			if !ok {
				t.Fatalf("no golden rows for %s", gc.name)
			}
			got := g1cCharacterize(t, gc)
			for sit, w := range want {
				if g, ok := got[sit]; !ok {
					t.Errorf("%s/%s: situation no longer exercised", gc.name, sit)
				} else if g != w {
					t.Errorf("%s/%s changed:\n  was: %s\n  now: %s", gc.name, sit, w, g)
				}
			}
			for sit := range got {
				if _, ok := want[sit]; !ok {
					t.Errorf("%s/%s: a situation with no golden row", gc.name, sit)
				}
			}
		})
	}
}
