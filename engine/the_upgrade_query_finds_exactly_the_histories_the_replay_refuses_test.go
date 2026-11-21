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
