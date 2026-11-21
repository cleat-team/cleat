package engine

// cleat#4285 step 3. scripts/find-histories-with-a-call-after-a-pending-await.sql is what an operator runs BEFORE upgrading to find the in-flight workflows whose replay
// the new divergence refuses. A query nobody ran against a row is a claim, so this runs the file itself (not a copy of it) on a real PostgreSQL, over histories written
// THROUGH THE STORE, not inserted by hand: the shape of a stored row is the store's to say.
//
// Two directions, because a query that finds nothing reads exactly like "nothing is affected": the KNOWN POSITIVE must be found, and the histories the replay accepts
// must NOT be. The healthy ones are chosen to be the ones a looser query would catch: a defer drain's own call after a pending await (both stamps), and an await that was
// answered. A workflow in a terminal state is not listed either: it will never replay.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

func TestTheUpgradeQueryFindsExactlyTheHistoriesTheReplayRefuses(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, db)
	defer testutil.CleanupPostgresTestData(t, db)
	sqlText, err := os.ReadFile("../scripts/find-histories-with-a-call-after-a-pending-await.sql")
	if err != nil {
		t.Fatalf("reading the query: %v", err)
	}
	store := NewPostgresStore(db)
	setupTestData(t, store)
	ctx := context.Background()
	call := func(step int) EventRecord {
		return EventRecord{Step: step, EventType: EventTypeCall, Service: "work", Op: "after_suspend", Request: `{}`, Response: `{"ok":true}`}
	}
	pendingAll := EventRecord{Step: 0, EventType: EventTypeAwaitAllChildren, Request: `["c1","c2"]`}
	pendingAny := EventRecord{Step: 0, EventType: EventTypeAwaitAnyChild, Request: `["c1","c2"]`}
	pendingOne := EventRecord{Step: 0, EventType: EventTypeAwaitChild, RunID: "c1"}
	drain, segment := call(1), call(1)
	drain.InDeferPhase, segment.InDeferSegment = true, true
	id := func(name string) string { return fmt.Sprintf("q4285-%s-%d", name, time.Now().UnixNano()) }

	type seeded struct {
		name     string
		events   []EventRecord
		terminal bool
		want     bool // the query must list it
	}
	cases := []seeded{
		{name: "pending-await-child-then-a-body-call", events: []EventRecord{pendingOne, call(1)}, want: true},
		{name: "pending-await-all-then-a-body-call", events: []EventRecord{pendingAll, call(1)}, want: true},
		{name: "pending-await-any-then-a-body-call", events: []EventRecord{pendingAny, call(1)}, want: true},
		{name: "a-drains-own-call-after-its-pending-await", events: []EventRecord{pendingOne, drain}},
		{name: "a-segments-call-after-its-pending-await", events: []EventRecord{pendingOne, segment}},
		{name: "an-answered-await-then-a-call", events: []EventRecord{{Step: 0, EventType: EventTypeAwaitChild, RunID: "c1", Response: "R"}, call(1)}},
		{name: "a-pending-await-that-is-last", events: []EventRecord{pendingOne}},
		{name: "a-body-call-BEFORE-a-pending-await", events: []EventRecord{call(0), {Step: 1, EventType: EventTypeAwaitChild, RunID: "c1"}}},
		{name: "a-pending-await-then-a-record-that-is-no-effect", events: []EventRecord{pendingOne, {Step: 1, EventType: EventTypeSignalReceived, Service: SignalEventService, Op: "deliver"}}},
		{name: "a-known-positive-in-a-finished-workflow", events: []EventRecord{pendingOne, call(1)}, terminal: true},
	}
	// The pending await is read as each await's own replay reads it (cleat-review's finding on the first version): an await_child that carries an ERROR is an
	// answer (a failed child), not a pending marker, so a parent that handled it and went on is healthy; await_all_children and await_any_child are pending on an
	// empty response alone.
	failedChild := EventRecord{Step: 0, EventType: EventTypeAwaitChild, RunID: "c1", Err: "child failed"}
	cases = append(cases,
		seeded{name: "a-failed-child-await-then-a-body-call", events: []EventRecord{failedChild, call(1)}},
		seeded{name: "an-await-all-with-an-error-and-no-response-then-a-body-call", events: []EventRecord{{Step: 0, EventType: EventTypeAwaitAllChildren, Request: `["c1","c2"]`, Err: "x"}, call(1)}, want: true},
	)
	ids := map[string]string{}
	for _, c := range cases {
		wid := id(c.name)
		ids[wid] = c.name
		if _, _, err := store.StartNewRun(ctx, wid, "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
			t.Fatalf("%s: StartNewRun: %v", c.name, err)
		}
		for _, ev := range c.events {
			if err := store.AppendEventHistory(ctx, wid, ev); err != nil {
				t.Fatalf("%s: AppendEventHistory step %d: %v", c.name, ev.Step, err)
			}
		}
		if c.terminal {
			if _, err := db.ExecContext(ctx, `UPDATE workflow_leases SET status = 'done' WHERE id = $1`, wid); err != nil {
				t.Fatalf("%s: marking it finished: %v", c.name, err)
			}
		}
	}
	// PRECONDITION: the seeded rows are there, so an empty answer cannot be "nothing was written".
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM event_history WHERE workflow_id LIKE 'q4285-%'`).Scan(&rows); err != nil || rows == 0 {
		t.Fatalf("PRECONDITION FAILED: no seeded events are visible to this handle (rows=%d, err=%v): the query would read as clean on a role filtered by row level security", rows, err)
	}
	res, err := db.QueryContext(ctx, string(sqlText))
	if err != nil {
		t.Fatalf("running the query: %v", err)
	}
	defer res.Close()
	found := map[string]bool{}
	for res.Next() {
		var wid, pendingEv, laterEv string
		var ps, ls int
		if err := res.Scan(&wid, &ps, &pendingEv, &ls, &laterEv); err != nil {
			t.Fatalf("scan: %v", err)
		}
		found[ids[wid]] = true
	}
	if err := res.Err(); err != nil {
		t.Fatal(err)
	}
	var got, want []string
	for name := range found {
		got = append(got, name)
	}
	for _, c := range cases {
		if c.want {
			want = append(want, c.name)
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the query listed %v, want exactly %v", got, want)
	}
}

// TestTheUpgradeQueryAndTheReplayRefuseTheSameHistories is the structural half, and it exists because the first version of the query was wrong in BOTH directions and a
// hand-picked list of cases could not have said so (cleat-review's measurement on #4309): it listed an await_child that carried an error (a handled failed child, which the
// engine does not refuse: it would have told an operator to terminate a healthy workflow), and it missed a promise resolve, a promise reject and a signal send (which the
// engine does refuse). Both came from the SQL being written from one reading of the engine's rule and the test from the same reading.
//
// So the second derivation here is THE ENGINE ITSELF. For every shape, the history is written through the store; the engine's decision is made by running the real await
// call (AwaitChild, AwaitAllChildren, AwaitAnyChild) on the history the store hands back, exactly as a replay would, and the SQL is run over the same rows. The two sets
// of refused histories must be EQUAL. The shapes are generated from what the engine's decision reads, not listed: every await kind in each state the replay tells apart
// (pending, answered, errored), then every event type that exists, with every service and op its by-op rule reads and one it does not, each unstamped, drain-stamped and
// segment-stamped. A new stoppable event, a new await state or a changed pending rule therefore makes the two disagree and this go red, instead of a query that
// is quietly wrong. The control is that the sets are not empty and hold the known positives: equal empty sets would agree on nothing.
func TestTheUpgradeQueryAndTheReplayRefuseTheSameHistories(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, db)
	defer testutil.CleanupPostgresTestData(t, db)
	sqlText, err := os.ReadFile("../scripts/find-histories-with-a-call-after-a-pending-await.sql")
	if err != nil {
		t.Fatalf("reading the query: %v", err)
	}
	store := NewPostgresStore(db)
	setupTestData(t, store)
	ctx := context.Background()

	type awaitKind struct {
		name string
		call childAwaitCall
		ids  []string
		rec  func(state string) EventRecord
	}
	kinds := []awaitKind{
		{"await_child", callOfAwaitChild, []string{"c1"}, func(state string) EventRecord {
			r := EventRecord{Step: 0, EventType: EventTypeAwaitChild, RunID: "c1"}
			switch state {
			case "answered":
				r.Response = "R"
			case "errored":
				r.Err = "child failed"
			}
			return r
		}},
		{"await_all", callOfAwaitAllChildren, []string{"c1", "c2"}, func(state string) EventRecord {
			r := EventRecord{Step: 0, EventType: EventTypeAwaitAllChildren, Request: `["c1","c2"]`}
			switch state {
			case "answered":
				r.Response = `[{"run_id":"c1","result":"R1"},{"run_id":"c2","result":"R2"}]`
			case "errored":
				r.Err = "x"
			}
			return r
		}},
		{"await_any", callOfAwaitAnyChild, []string{"c1", "c2"}, func(state string) EventRecord {
			r := EventRecord{Step: 0, EventType: EventTypeAwaitAnyChild, Request: `["c1","c2"]`}
			switch state {
			case "answered":
				r.Response = `{"run_id":"c1","result":"R1"}`
			case "errored":
				r.Err = "x"
			}
			return r
		}},
	}
	// every (service, op) a later record can carry that the engine's decision reads: a by-op type's own service and each of its ops, plus a service and an op it does not.
	type later struct {
		name    string
		et      EventType
		service string
		op      string
	}
	var laters []later
	for _, et := range allEventTypes(t) {
		if so, byOp := stoppableByOp[et]; byOp {
			for op := range so.ops {
				laters = append(laters, later{string(et) + "/" + op, et, so.service, op})
			}
			laters = append(laters, later{string(et) + "/other-op", et, so.service, "not-the-effect"}, later{string(et) + "/other-service", et, "not-the-service", ""})
			continue
		}
		laters = append(laters, later{string(et), et, "work", "op"})
	}
	// The by-op pairs the QUERY names, taken from the constants and NOT from the table above: shapes derived only from stoppableByOp vanish with an entry removed from it,
	// and the query's hand-written clause would then be exercised by nothing (a mutant that deleted the promise-resolve entry stayed green until this).
	for _, p := range []struct {
		et          EventType
		service, op string
	}{{EventTypePromiseResolved, PromiseEventService, PromiseOpResolve}, {EventTypePromiseRejected, PromiseEventService, PromiseOpReject}, {EventTypeSignalReceived, SignalEventService, SignalOpSend}} {
		laters = append(laters, later{string(p.et) + "/" + p.op + " (the query's own pair)", p.et, p.service, p.op})
	}
	stamps := []struct {
		label          string
		phase, segment bool
	}{{"unstamped", false, false}, {"drain", true, false}, {"segment", false, true}}

	engineRefuses := func(k awaitKind, hist []EventRecord) bool {
		eng := NewEngine(nil, &mockCaller{}, WithWorkflowID("wf-struct"))
		eng.childWfStore = &mockChildStore{getChildResultFn: func(context.Context, string) (ChildOutcome, error) { return ChildOutcome{}, nil }}
		s := &execSession{engine: eng, workflowID: "wf-struct", nowMs: 1_800_000_000_000, deferrals: map[string]string{}, queryState: map[string]string{},
			isReplay: len(hist) > 0, history: append([]EventRecord(nil), hist...)}
		buf := make([]byte, 600)
		c := contextWithRawMemBuf(context.Background(), buf)
		ids, _ := json.Marshal(k.ids)
		switch k.call {
		case callOfAwaitChild:
			s.AwaitChild(c, nil, k.ids[0], 0, 400)
		case callOfAwaitAllChildren:
			s.AwaitAllChildren(c, nil, string(ids), 0, 400)
		case callOfAwaitAnyChild:
			s.AwaitAnyChild(c, nil, string(ids), 0, 400)
		}
		return strings.Contains(string(buf), "ignored a child-await suspension")
	}

	type shape struct {
		name  string
		k     awaitKind
		wid   string
		fresh bool
	}
	var shapes []shape
	n := 0
	for _, k := range kinds {
		for _, state := range []string{"pending", "answered", "errored"} {
			for _, l := range laters {
				for _, st := range stamps {
					n++
					wid := fmt.Sprintf("qs-%d-%d", n, time.Now().UnixNano()%1_000_000)
					name := k.name + "/" + state + " then " + l.name + " (" + st.label + ")"
					later := EventRecord{Step: 1, EventType: l.et, Service: l.service, Op: l.op, Request: `{}`, Response: `{}`, InDeferPhase: st.phase, InDeferSegment: st.segment}
					if _, _, err := store.StartNewRun(ctx, wid, "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
						t.Fatalf("%s: StartNewRun: %v", name, err)
					}
					for _, ev := range []EventRecord{k.rec(state), later} {
						if err := store.AppendEventHistory(ctx, wid, ev); err != nil {
							t.Fatalf("%s: AppendEventHistory step %d (%s): %v", name, ev.Step, ev.EventType, err)
						}
					}
					shapes = append(shapes, shape{name: name, k: k, wid: wid})
				}
			}
		}
	}

	res, err := db.QueryContext(ctx, string(sqlText))
	if err != nil {
		t.Fatalf("running the query: %v", err)
	}
	defer res.Close()
	listed := map[string]bool{}
	for res.Next() {
		var wid, pe, le string
		var ps, ls int
		if err := res.Scan(&wid, &ps, &pe, &ls, &le); err != nil {
			t.Fatalf("scan: %v", err)
		}
		listed[wid] = true
	}
	if err := res.Err(); err != nil {
		t.Fatal(err)
	}

	var refusedCount, listedCount int
	var onlyEngine, onlySQL []string
	for _, sh := range shapes {
		hist, err := store.LoadEventHistory(ctx, sh.wid)
		if err != nil || len(hist) != 2 {
			t.Fatalf("%s: reading back what the store wrote: %d records, err=%v", sh.name, len(hist), err)
		}
		eng, sql := engineRefuses(sh.k, hist), listed[sh.wid]
		if eng {
			refusedCount++
		}
		if sql {
			listedCount++
		}
		switch {
		case eng && !sql:
			onlyEngine = append(onlyEngine, sh.name)
		case sql && !eng:
			onlySQL = append(onlySQL, sh.name)
		}
	}
	// CONTROL: two empty sets are equal. The engine must refuse a real number of these, including each await kind's plain pending-then-effect shape.
	if refusedCount < 3 || listedCount < 3 {
		t.Fatalf("PRECONDITION FAILED: the engine refused %d of %d shapes and the query listed %d; equal near-empty sets would agree on nothing", refusedCount, len(shapes), listedCount)
	}
	for _, k := range kinds {
		name := k.name + "/pending then side_effect (unstamped)"
		found := false
		for _, sh := range shapes {
			if sh.name == name && listed[sh.wid] {
				found = true
			}
		}
		if !found {
			t.Errorf("PRECONDITION FAILED: the known positive %q was not listed by the query", name)
		}
	}
	sort.Strings(onlyEngine)
	sort.Strings(onlySQL)
	if len(onlyEngine) > 0 {
		t.Errorf("the replay REFUSES these histories and the query does NOT list them (%d): %v", len(onlyEngine), firstNames(onlyEngine, 12))
	}
	if len(onlySQL) > 0 {
		t.Errorf("the query LISTS these histories and the replay does NOT refuse them (%d; an operator would be told to terminate a healthy workflow): %v", len(onlySQL), firstNames(onlySQL, 12))
	}
	t.Logf("%d shapes; the engine refuses %d and the query lists %d", len(shapes), refusedCount, listedCount)
}

func firstNames(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
