package engine

// The round trip for schedule_cron, delete_cron and list_crons (cleat#4152, G1c), the same idea as the promise, G1a and G1b ones: run the
// call FRESH into a real event_history and a real schedules table, load that history back from the store, replay it, and check what replay
// did. The history is the one the engine WROTE, never one built by hand (cleat#4177).
//
// These calls reach a table, so "the effect happened once" is stated as what the table holds, and every property is arranged so that a replay
// that touched it would be SEEN:
//
//	schedule_cron  (b) the created row is deleted between the fresh run and the replay, so a replay that re-created it would bring it back;
//	               (c) the replay answers the id the original run was told; (d) a second replay does the same; (e) a call at the frontier
//	               creates a NEW schedule (the id is derived from the step, so it differs).
//	delete_cron    (b) the row is put BACK before the replay, so a replay that deleted again would remove it; (d) twice; (e) a call at the
//	               frontier really deletes.
//	list_crons     (c) a second schedule is added before the replay, so a replay that re-read the table would list it: the replay must answer
//	               what was RECORDED, which does not include it; (d) twice; (e) a call at the frontier lists the table as it is NOW.
//
// Run against each dialect that has a DSN, like the other store-backed tests; an unset DSN skips that dialect (read the skip names).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

func TestEveryG1cSpecRoundTripsThroughARealEventHistory(t *testing.T) {
	for _, d := range []struct {
		name    string
		dialect testutil.Dialect
		setup   func(*testing.T, *sql.DB)
	}{
		{"postgres", testutil.DialectPostgres, func(t *testing.T, db *sql.DB) { testutil.SetupFullSchema(t, db, testutil.DialectPostgres) }},
		{"mssql", testutil.DialectMSSQL, func(t *testing.T, db *sql.DB) { testutil.SetupMSSQLFullSchema(t, db) }},
		{"mysql", testutil.DialectMySQL, func(t *testing.T, db *sql.DB) { testutil.SetupMySQLFullSchema(t, db) }},
	} {
		d := d
		t.Run(d.name, func(t *testing.T) {
			db := testutil.TestDB(t, d.dialect)
			d.setup(t, db)
			defer db.Close()
			for i, name := range []string{"schedule_cron", "delete_cron", "list_crons"} {
				name, i := name, i
				t.Run(name, func(t *testing.T) { roundTripG1cCall(t, d.dialect, db, i, name) })
			}
		})
	}
}

func roundTripG1cCall(t *testing.T, dialect testutil.Dialect, db *sql.DB, n int, name string) {
	t.Helper()
	ctx := context.Background()
	tag := fmt.Sprintf("g1c-%s-%s%d", dialect, time.Now().Format("150405.000000"), n)
	wfID := "round-trip-" + tag
	seedWorkflowInstance(t, db, dialect, wfID)
	store := storeFor(t, dialect, db)
	eng := NewEngine(nil, nil, WithWorkflowID(wfID), WithTenantID(DefaultTenantUUID), WithWorkflowStore(store), WithDB(db))
	session := func(history []EventRecord) *execSession {
		return &execSession{
			engine: eng, workflowID: wfID, tenantID: DefaultTenantUUID, nowMs: time.Now().UnixMilli(),
			isReplay: len(history) > 0, history: history,
			deferrals: map[string]string{}, queryState: map[string]string{},
		}
	}
	load := func() []EventRecord {
		h, err := store.LoadEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("load history: %v", err)
		}
		return h
	}
	call := func(s *execSession, f func(context.Context, *execSession) int64) (res int64, wrote string) {
		buf := make([]byte, 4096)
		res = f(context.WithValue(ctx, wasmMemBufKey{}, buf), s)
		k := 0
		for k < len(buf) && buf[k] != 0 {
			k++
		}
		return res, string(buf[:k])
	}
	noViolations := func(label string, s *execSession) {
		t.Helper()
		if len(s.stepViolations) != 0 {
			t.Errorf("%s: the dispatcher noted %v", label, s.stepViolations)
		}
	}
	// What the table holds for this test's own schedules. Everything is named after tag, so other tests' rows are never read.
	schedules := func() map[string]Schedule {
		all, err := store.ListSchedules(ctx)
		if err != nil {
			t.Fatalf("list schedules: %v", err)
		}
		mine := map[string]Schedule{}
		for _, sc := range all {
			if strings.HasSuffix(sc.DefName, tag) {
				mine[sc.Name] = sc
			}
		}
		return mine
	}
	var created []string
	t.Cleanup(func() {
		for _, id := range created {
			_ = store.DeleteSchedule(ctx, id)
		}
		for id := range schedules() {
			_ = store.DeleteSchedule(ctx, id)
		}
	})
	seed := func(id string) {
		t.Helper()
		created = append(created, id)
		err := store.CreateSchedule(ctx, Schedule{
			Name: id, DefName: "def-" + tag, CronExpression: "0 3 * * *", Input: []byte(`{}`),
			NextRunAt: time.Now().Add(time.Hour), Timezone: "UTC",
		})
		if err != nil {
			t.Fatalf("seed schedule %s: %v", id, err)
		}
	}
	wfName := "def-" + tag
	scheduleCron := func(ctx context.Context, s *execSession) int64 {
		return s.ScheduleCron(ctx, nil, wfName, "0 3 * * *", "UTC", `{}`, 0, 4096)
	}
	listCrons := func(ctx context.Context, s *execSession) int64 { return s.ListCrons(ctx, nil, 0, 4096) }
	names := func(m map[string]Schedule) string {
		var ks []string
		for k := range m {
			ks = append(ks, k)
		}
		return fmt.Sprint(ks)
	}

	switch name {
	case "schedule_cron":
		fresh := session(nil)
		freshRes, freshID := call(fresh, scheduleCron)
		history := load()
		if len(history) != 1 || history[0].EventType != EventTypeScheduleCron || history[0].CronScheduleID == "" || freshID != history[0].CronScheduleID {
			t.Fatalf("PRECONDITION FAILED: the fresh run left %d events (first %v) and answered %q; want one schedule_cron carrying the id the guest was told", len(history), history, freshID)
		}
		id := freshID
		created = append(created, id)
		if got := schedules(); len(got) != 1 || got[id].DefName != wfName {
			t.Fatalf("PRECONDITION FAILED: the fresh run left the table holding %s, want exactly %s", names(got), id)
		}
		// (b) take the row away, so a replay that created it again would bring it back.
		if err := store.DeleteSchedule(ctx, id); err != nil {
			t.Fatalf("remove the created row: %v", err)
		}
		replayOnce := func(label string) (int64, string) {
			s := session(load())
			res, wrote := call(s, scheduleCron)
			if s.stepCount != len(s.history) {
				t.Errorf("%s: (a) replay ended at step %d with %d records in history", label, s.stepCount, len(s.history))
			}
			noViolations(label, s)
			if got := schedules(); len(got) != 0 {
				t.Errorf("%s: (b) replaying a schedule_cron changed the table: it now holds %s, want nothing", label, names(got))
			}
			return res, wrote
		}
		first, firstID := replayOnce("first replay")
		if first != freshRes || firstID != id {
			t.Errorf("(c) the replay answered %#x %q where the original run was told %#x %q", first, firstID, freshRes, id)
		}
		if second, secondID := replayOnce("second replay"); second != first || secondID != firstID {
			t.Errorf("(d) a second replay answered %#x %q, the first answered %#x %q", second, secondID, first, firstID)
		}
		s := session(load())
		call(s, scheduleCron) // replays the recorded one
		_, secondID := call(s, scheduleCron)
		noViolations("(e) frontier", s)
		if secondID == "" || secondID == id {
			t.Errorf("(e) the call at the frontier answered %q, want a NEW schedule id (the original was %q)", secondID, id)
		}
		created = append(created, secondID)
		if got := schedules(); len(got) != 1 || got[secondID].Name == "" {
			t.Errorf("(e) the table holds %s after the frontier call, want exactly the new schedule %s", names(got), secondID)
		}
		if h := load(); len(h) != 2 {
			t.Errorf("(e) the frontier call left %d events in the store, want 2", len(h))
		}

	case "delete_cron":
		id := "cron-" + tag
		seed(id)
		deleteIt := func(ctx context.Context, s *execSession) int64 { return s.DeleteCron(ctx, nil, id) }
		fresh := session(nil)
		freshRes, _ := call(fresh, deleteIt)
		history := load()
		if len(history) != 1 || history[0].EventType != EventTypeDeleteCron || history[0].CronScheduleID != id {
			t.Fatalf("PRECONDITION FAILED: the fresh run left %d events, first %v; want one delete_cron of %q", len(history), history, id)
		}
		if got := schedules(); len(got) != 0 {
			t.Fatalf("PRECONDITION FAILED: the fresh delete left the table holding %s, want nothing", names(got))
		}
		replayOnce := func(label string) int64 {
			seed(id) // (b) put the row BACK, so a replay that deleted again would remove it
			s := session(load())
			res, _ := call(s, deleteIt)
			if s.stepCount != len(s.history) {
				t.Errorf("%s: (a) replay ended at step %d with %d records in history", label, s.stepCount, len(s.history))
			}
			noViolations(label, s)
			if got := schedules(); len(got) != 1 {
				t.Errorf("%s: (b) replaying a delete_cron deleted again: the table holds %s, want the row that was put back", label, names(got))
			}
			_ = store.DeleteSchedule(ctx, id)
			return res
		}
		first := replayOnce("first replay")
		if first != freshRes {
			t.Errorf("(c) the replay answered %#x where the original run was told %#x", first, freshRes)
		}
		if second := replayOnce("second replay"); second != first {
			t.Errorf("(d) a second replay answered %#x, the first answered %#x", second, first)
		}
		seed(id)
		s := session(load())
		call(s, deleteIt) // replays the recorded one
		call(s, deleteIt) // a new one at the frontier, which must really delete
		noViolations("(e) frontier", s)
		if got := schedules(); len(got) != 0 {
			t.Errorf("(e) the delete at the frontier left the table holding %s, want nothing", names(got))
		}
		if h := load(); len(h) != 2 {
			t.Errorf("(e) the frontier call left %d events in the store, want 2", len(h))
		}

	case "list_crons":
		a := "cron-a-" + tag
		seed(a)
		fresh := session(nil)
		freshRes, freshList := call(fresh, listCrons)
		history := load()
		if len(history) != 1 || history[0].EventType != EventTypeListCrons || !strings.Contains(history[0].CronResult, a) || freshList != history[0].CronResult {
			t.Fatalf("PRECONDITION FAILED: the fresh run left %d events (first %v) and answered %q; want one list_crons whose result names %s", len(history), history, freshList, a)
		}
		b := "cron-b-" + tag
		seed(b) // (c) the world changes after the original run
		replayOnce := func(label string) (int64, string) {
			s := session(load())
			res, wrote := call(s, listCrons)
			if s.stepCount != len(s.history) {
				t.Errorf("%s: (a) replay ended at step %d with %d records in history", label, s.stepCount, len(s.history))
			}
			noViolations(label, s)
			return res, wrote
		}
		first, firstList := replayOnce("first replay")
		if first != freshRes || firstList != freshList {
			t.Errorf("(c) the replay answered %#x %q where the original run was told %#x %q", first, firstList, freshRes, freshList)
		}
		if strings.Contains(firstList, b) {
			t.Errorf("(c) the replay listed %s, which did not exist when the original run listed: replay re-read the table instead of answering what was recorded", b)
		}
		if second, secondList := replayOnce("second replay"); second != first || secondList != firstList {
			t.Errorf("(d) a second replay answered %#x %q, the first answered %#x %q", second, secondList, first, firstList)
		}
		s := session(load())
		call(s, listCrons) // replays the recorded one
		_, live := call(s, listCrons)
		noViolations("(e) frontier", s)
		if !strings.Contains(live, a) || !strings.Contains(live, b) {
			t.Errorf("(e) the list at the frontier answered %q, want the table as it is NOW, naming both %s and %s", live, a, b)
		}
		if h := load(); len(h) != 2 {
			t.Errorf("(e) the frontier call left %d events in the store, want 2", len(h))
		}
	}
}
