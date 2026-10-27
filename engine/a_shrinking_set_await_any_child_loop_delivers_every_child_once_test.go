package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestAShrinkingSetAwaitAnyChildLoopDeliversEveryChildOnce is cleat#3483. It
// pins the loop clew's fan-ins want to use:
//
//	pending := {a, b, c}
//	for len(pending) > 0 {
//	    id, res := AwaitAnyChild(pending...)
//	    handle(id, res); delete(pending, id)
//	}
//
// #3423 fixed cleat#3391 (an AwaitAnyChild that waits across several wakes
// dropped its result and restarted what followed it) and its test drives ONE
// await on ONE child. This is the shape that test does not reach: the SAME
// loop with a request set that shrinks, where two or more children finish
// between two parent wakes -- or all of them before the first -- so the second
// AwaitAnyChild has to hand back an already-finished child on replay without
// suspending, dropping it or starting anything again.
//
// The parent here is a real run of the engine's own host calls, in the order a
// guest makes them, against a REAL event_history on each dialect:
//
//   - the children are started through ChildWorkflowWithOptions and the
//     dialect's StartChildWorkflowAtomic, so they are real rows and the
//     starts are real child_workflow events;
//   - every wake loads the history from the database and replays it, as a
//     worker does;
//   - "a child finishes" is the only simulated thing: GetChildResult answers
//     from a set the test grows between wakes.
//
// WHAT IS ASSERTED, and where each number comes from:
//
//   - exactly-once START: child_workflow events in the history READ BACK FROM
//     THE DATABASE, plus the child rows themselves. Counted from event_history
//     as #3423's reproduction did, not from a counter in the test.
//   - exactly-once DELIVERY: the await_any_child records with a result are
//     one per child, and the guest's own received sequence equals them.
//   - exactly-once POST-AWAIT OPERATION: the operation after each await is a
//     durable `call` (or, in the other variant, a StartChild -- the report in
//     #3391) and is counted both as "really ran" by the harness and as rows.
//   - WHICH child wins each await is asserted against an oracle written from
//     the rule alone (the lexically first finished child of the CURRENT set),
//     because that set changes as it shrinks.
//   - a wake that finds nothing new adds no row (the #3391 signature).
func TestAShrinkingSetAwaitAnyChildLoopDeliversEveryChildOnce(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectPostgres)
		testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
		defer db.Close()
		runShrinkingSetScenarios(t, testutil.DialectPostgres, db)
	})
	t.Run("mssql", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMSSQL)
		testutil.SetupMSSQLFullSchema(t, db)
		defer db.Close()
		runShrinkingSetScenarios(t, testutil.DialectMSSQL, db)
	})
	t.Run("mysql", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMySQL)
		testutil.SetupMySQLFullSchema(t, db)
		defer db.Close()
		runShrinkingSetScenarios(t, testutil.DialectMySQL, db)
	})
}

// shrinkingScenario is a list of wakes. waves[i] names the children (by their
// position in LEXICAL run-ID order, so "0" is the one a sorted poll tries
// first) that finish immediately before wake i+1. An empty wave is a wake that
// finds nothing new.
//
// legacyEmpties, when set, writes that many extra EMPTY await_any_child records
// after the first wake. One extra is the history an engine from before #3423
// left for a child that took a second wake: it replayed the first empty
// suspend, advanced past it, found the child still running and recorded
// another empty one at the next step. A third wake could not add a third --
// it re-recorded at that same next step, where the conflict clause drops the
// write -- so the old engine stopped at TWO adjacent empties. Two extras is
// NOT a history it could write; it is kept as a robustness case for the
// skip-every-empty loop (the second half of #3423) reading more of them than
// the old engine ever left.
type shrinkingScenario struct {
	name          string
	n             int
	waves         [][]int
	legacyEmpties int
}

var shrinkingScenarios = []shrinkingScenario{
	// All N finish before the first wake that gets to AwaitAnyChild at all: the
	// loop never suspends, so no empty record is ever written.
	{"all finish before the first await", 3, [][]int{{0, 1, 2}}, 0},
	// All N finish after the first wake (which started them and suspended) and
	// before the second.
	{"all finish before the first wake after the suspend", 3, [][]int{{}, {0, 1, 2}}, 0},
	// Two finish between wakes, then the other two.
	{"two then two", 4, [][]int{{}, {1, 3}, {0, 2}}, 0},
	// The two that finish are the lexically LAST two, so the first await's
	// winner is not the child that finished first in any sense but sorting.
	{"the lexically last two finish first", 4, [][]int{{}, {3, 2}, {1, 0}}, 0},
	// Two, then one, then one: the set shrinks by two and then by one.
	{"two then one then one", 4, [][]int{{}, {2, 3}, {1}, {0}}, 0},
	// Three of five, then the other two.
	{"three of five then two", 5, [][]int{{}, {0, 2, 4}, {1, 3}}, 0},
	// #3391's own shape first -- several wakes with nothing finished -- and
	// only then a pair, so the empty record has been through many wakes.
	{"still running across wakes then a pair", 3, [][]int{{}, {}, {}, {0, 2}, {1}}, 0},
	// All N finish only after several wakes in which none had.
	{"all finish after several still-running wakes", 3, [][]int{{}, {}, {}, {0, 1, 2}}, 0},
	// The SECOND await is the one that waits across wakes: a pair finishes,
	// the loop is handed one and suspends on the rest, and that suspend is
	// polled several times before the last two finish.
	{"the second await waits across wakes", 4, [][]int{{}, {1, 3}, {}, {}, {0, 2}}, 0},
	// Both awaits wait across wakes, with a pair finishing in between.
	{"every await waits across wakes", 4, [][]int{{}, {}, {1, 3}, {}, {}, {0}, {2}}, 0},
	// One child is already done at the first await; the other two finish
	// together later. The first await returns at once and the second suspends.
	{"one done at the first await then a pair", 3, [][]int{{1}, {0, 2}}, 0},
	// One at a time, as a baseline: no wake has two to hand back.
	{"one at a time", 3, [][]int{{}, {2}, {0}, {1}}, 0},
	// A history written by the engine BEFORE #3423: the first await left two
	// adjacent empty suspends (the most that engine could write).
	{"a history from before the fix, two empty suspends", 3, [][]int{{}, {0, 1, 2}}, 1},
	{"a history from before the fix, two empty suspends then a pair", 4, [][]int{{}, {1, 3}, {0, 2}}, 1},
	// More adjacent empties than the old engine could write, as a robustness
	// case only (see legacyEmpties).
	{"three adjacent empty suspends then a pair", 4, [][]int{{}, {1, 3}, {0, 2}}, 2},
}

// postAwaitOp is what the guest does after each await. A durable call is the
// simplest; a StartChild is #3391's actual symptom (a child started per wake).
type postAwaitOp string

const (
	postAwaitCall       postAwaitOp = "call"
	postAwaitStartChild postAwaitOp = "start-child"
)

func runShrinkingSetScenarios(t *testing.T, dialect testutil.Dialect, db *sql.DB) {
	t.Helper()
	runStamp := time.Now().UnixNano() % 1_000_000_000
	for si, sc := range shrinkingScenarios {
		for _, op := range []postAwaitOp{postAwaitCall, postAwaitStartChild} {
			sc, op := sc, op
			// Unique per run: the child rows carry fixed IDs, and a database a
			// previous run (or an interrupted one) used still holds them.
			wfID := fmt.Sprintf("shrink-%s-%d-%s-%d", dialect, si, op, runStamp)
			t.Run(fmt.Sprintf("%s/%s", sc.name, op), func(t *testing.T) {
				runShrinkingSetScenario(t, dialect, db, wfID, sc, op)
			})
		}
	}
}

// shrinkingWake is what one wake of the parent reports.
type shrinkingWake struct {
	suspended bool
	received  []string // child run IDs, in the order the guest was handed them
	results   map[string]string
}

func runShrinkingSetScenario(t *testing.T, dialect testutil.Dialect, db *sql.DB, wfID string, sc shrinkingScenario, op postAwaitOp) {
	t.Helper()
	seedWorkflowInstance(t, db, dialect, wfID)
	store := storeFor(t, dialect, db)
	eng := NewEngine(nil, nil,
		WithWorkflowID(wfID),
		WithTenantID(DefaultTenantUUID),
		WithWorkflowStore(store),
		WithDB(db))

	realChildren, ok := store.(ChildWorkflowStore)
	if !ok {
		t.Fatalf("the %s store does not implement ChildWorkflowStore", dialect)
	}
	finished := map[string]bool{}
	if sc.n > 10 {
		t.Fatalf("scenario %q has %d children; the run IDs are only in lexical order up to 10", sc.name, sc.n)
	}
	// Children get explicit run IDs, "<parent>-c0" ... "<parent>-cN", so that
	// their lexical order -- the order a sorted poll tries them in -- is known
	// to the test before they exist. Follow-up starts get "<parent>-after-K".
	order := make([]string, sc.n)
	for i := range order {
		order[i] = fmt.Sprintf("%s-c%d", wfID, i)
	}
	followUps := 0
	resultOf := func(runID string) string { return `{"ok":"` + runID + `"}` }
	eng.childWfStore = &mockChildStore{
		// Real child rows and real child_workflow events: the start is the
		// dialect's own atomic insert.
		startChildAtomicFn: func(ctx context.Context, childID, parentID, defName, inputJSON string, defVersion int, policy string, event EventRecord, priority int) (string, error) {
			if childID == "" {
				var in struct {
					For string `json:"for"`
				}
				_ = json.Unmarshal([]byte(inputJSON), &in)
				if i, err := strconv.Atoi(strings.TrimPrefix(in.For, "child-")); err == nil && strings.HasPrefix(in.For, "child-") {
					childID = order[i]
				} else {
					followUps++
					childID = fmt.Sprintf("%s-after-%d", wfID, followUps)
				}
			}
			return realChildren.StartChildWorkflowAtomic(ctx, childID, parentID, defName, inputJSON, defVersion, policy, event, priority)
		},
		getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
			if finished[runID] {
				return ChildOutcome{Completed: true, Result: resultOf(runID)}, nil
			}
			return ChildOutcome{}, nil
		},
	}

	ctx := context.Background()
	postOps := map[string]int{} // how many times each post-await op REALLY ran

	rows := func() int {
		t.Helper()
		var n int
		if err := testutil.AdminDB(t, db, dialect).QueryRow(countEventsSQL(dialect), wfID).Scan(&n); err != nil {
			t.Fatalf("count events: %v", err)
		}
		return n
	}

	newSession := func(history []EventRecord) *execSession {
		return &execSession{
			engine:     eng,
			workflowID: wfID,
			tenantID:   DefaultTenantUUID,
			nowMs:      time.Now().UnixMilli(),
			isReplay:   len(history) > 0,
			history:    history,
			deferrals:  map[string]string{},
			queryState: map[string]string{},
		}
	}

	// writeLegacyEmpty appends one empty await_any_child record that repeats
	// the last record's request: what a pre-#3423 wake left behind.
	writeLegacyEmpty := func() {
		t.Helper()
		history, err := store.LoadEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("load history: %v", err)
		}
		last := history[len(history)-1]
		if last.EventType != EventTypeAwaitAnyChild || last.Response != "" {
			t.Fatalf("a legacy empty suspend can only repeat an empty await_any_child; the last record is %q/%q", last.EventType, last.Response)
		}
		s := newSession(history)
		for i := range history {
			r := history[i]
			if !s.advanceReplayStep(ctx, &r) {
				t.Fatalf("could not replay step %d to extend the history", i)
			}
		}
		s.exitReplay()
		s.recordEvent(EventRecord{Step: s.stepCount, EventType: EventTypeAwaitAnyChild, Request: last.Request})
	}

	// wake runs the parent once from its recorded history: replay what is
	// recorded, go fresh past the frontier, exactly as a worker does per wake.
	wake := func() shrinkingWake {
		t.Helper()
		history, err := store.LoadEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("load history: %v", err)
		}
		s := newSession(history)
		out := shrinkingWake{results: map[string]string{}}

		startChild := func(label string) string {
			t.Helper()
			before := s.stepCount
			s.ChildWorkflowWithOptions(ctx, nil, "flush-dialect", `{"for":"`+label+`"}`, 1, 0, "ABANDON", 0, 0)
			if s.stepCount != before+1 || s.history[s.stepCount-1].EventType != EventTypeChildWorkflow {
				t.Fatalf("starting child %q did not consume or record one child_workflow event (step %d -> %d)", label, before, s.stepCount)
			}
			return s.history[s.stepCount-1].RunID
		}

		// 1. Start the children.
		started := make([]string, sc.n)
		for i := range started {
			started[i] = startChild(fmt.Sprintf("child-%d", i))
		}
		pending := append([]string(nil), started...)

		// 2. The shrinking-set loop. The set is handed over in REVERSE sorted
		// order on purpose: the engine must treat it as a set.
		for len(pending) > 0 {
			req := append([]string(nil), pending...)
			sort.Sort(sort.Reverse(sort.StringSlice(req)))
			reqJSON, _ := json.Marshal(req)

			res := s.AwaitAnyChild(ctx, nil, string(reqJSON), 0, 0)
			if s.suspendErr != nil || res == packAwaitChildResultSuspend() {
				out.suspended = true
				return out
			}
			// Where AwaitAnyChild left the session is the record it handed back.
			rec := s.history[s.stepCount-1]
			if rec.EventType != EventTypeAwaitAnyChild || rec.Response == "" {
				t.Fatalf("after AwaitAnyChild the last consumed record is %q with response %q, want a delivered result",
					rec.EventType, rec.Response)
			}
			var got struct {
				RunID  string `json:"run_id"`
				Result string `json:"result"`
				Error  string `json:"error"`
			}
			if err := json.Unmarshal([]byte(rec.Response), &got); err != nil {
				t.Fatalf("unreadable AwaitAnyChild result %q: %v", rec.Response, err)
			}
			if got.Error != "" {
				t.Fatalf("AwaitAnyChild delivered an error for %s: %q", got.RunID, got.Error)
			}
			idx := -1
			for i, p := range pending {
				if p == got.RunID {
					idx = i
				}
			}
			if idx < 0 {
				t.Fatalf("AwaitAnyChild delivered %s, which is not in the request %v", got.RunID, req)
			}
			if got.Result != resultOf(got.RunID) {
				t.Fatalf("AwaitAnyChild delivered %s with result %q, want %q", got.RunID, got.Result, resultOf(got.RunID))
			}
			out.received = append(out.received, got.RunID)
			out.results[got.RunID] = got.Result
			pending = append(pending[:idx], pending[idx+1:]...)

			// 3. The operation after the await.
			label := "handled:" + got.RunID
			switch op {
			case postAwaitCall:
				if s.isReplay && s.stepCount < len(s.history) {
					r := s.history[s.stepCount]
					if r.EventType != EventTypeCall || r.Op != label {
						t.Fatalf("replay divergence: step %d is %q/%q, want the post-await call %q", r.Step, r.EventType, r.Op, label)
					}
					s.advanceReplayStep(ctx, &r)
					break
				}
				s.exitReplay()
				postOps[label]++
				s.recordEvent(EventRecord{
					Step:      s.stepCount,
					EventType: EventTypeCall,
					Service:   "svc",
					Op:        label,
					Response:  `{}`,
				})
			case postAwaitStartChild:
				replaying := s.isReplay && s.stepCount < len(s.history)
				startChild(label)
				if !replaying {
					postOps[label]++
				}
			}
		}
		return out
	}

	// ---- the oracle: the rule, written without reading the engine ----------
	//
	// Whenever the guest asks, it is handed the lexically first child that has
	// finished and has not been handed over yet. "Lexically first" is index
	// order here BY CONSTRUCTION (the IDs are <parent>-c0 ... -c9), not a
	// second call to sort.Strings: the engine sorts, this states the rule.
	var delivered []string
	oracleStep := func(done map[string]bool) {
		for {
			var pick string
			for _, id := range order {
				if done[id] && !shrinkContains(delivered, id) {
					pick = id
					break
				}
			}
			if pick == "" {
				return
			}
			delivered = append(delivered, pick)
		}
	}

	waves := sc.waves
	var last shrinkingWake
	var rowsAfterQuiet int
	for i, wave := range waves {
		for _, idx := range wave {
			finished[order[idx]] = true
		}
		before := rows()
		last = wake()
		oracleStep(finished)

		wantSuspended := len(delivered) < sc.n
		if last.suspended != wantSuspended {
			t.Fatalf("wake %d (finished so far %d of %d): suspended=%v, want %v",
				i+1, shrinkCountTrue(finished), sc.n, last.suspended, wantSuspended)
		}
		if !reflect.DeepEqual(last.received, delivered) && !(len(last.received) == 0 && len(delivered) == 0) {
			t.Fatalf("wake %d: the guest was handed %v, want %v (the lexically first finished child of the CURRENT set, each time)",
				i+1, shrinkShortIDs(last.received), shrinkShortIDs(delivered))
		}
		// A wake that finds nothing new must add no row (the #3391 signature).
		if len(wave) == 0 && i > 0 {
			if got := rows(); got != before {
				t.Errorf("wake %d finished nothing new and event_history went from %d rows to %d: a suspended await must not add a row per wake",
					i+1, before, got)
			}
		}
		rowsAfterQuiet = rows()
		if i == 0 {
			for k := 0; k < sc.legacyEmpties; k++ {
				writeLegacyEmpty()
			}
		}
	}
	if last.suspended {
		t.Fatalf("after every wave the parent is still suspended")
	}

	// The parent has completed. Wake it several more times, as a worker does
	// when a duplicate delivery or a late timer wakes a finished parent: each
	// must replay the same answer and add nothing.
	for i := 1; i <= 3; i++ {
		w := wake()
		if w.suspended || !reflect.DeepEqual(w.received, delivered) {
			t.Fatalf("extra replay %d: suspended=%v received %v, want %v", i, w.suspended, shrinkShortIDs(w.received), shrinkShortIDs(delivered))
		}
		if got := rows(); got != rowsAfterQuiet {
			t.Errorf("extra replay %d of a completed parent changed event_history from %d rows to %d", i, rowsAfterQuiet, got)
		}
	}

	// ---- read it all back from the database --------------------------------
	hist, err := store.LoadEventHistory(ctx, wfID)
	if err != nil {
		t.Fatalf("load final history: %v", err)
	}
	var starts, deliveredRows []string
	postRows := map[string]int{}
	var adjacentEmpties int
	for i, r := range hist {
		switch r.EventType {
		case EventTypeChildWorkflow:
			starts = append(starts, r.RunID)
			if strings.HasPrefix(r.ChildInput, `{"for":"handled:`) {
				postRows[strings.TrimSuffix(strings.TrimPrefix(r.ChildInput, `{"for":"`), `"}`)]++
			}
		case EventTypeAwaitAnyChild:
			if r.Response != "" {
				var o struct {
					RunID string `json:"run_id"`
				}
				_ = json.Unmarshal([]byte(r.Response), &o)
				deliveredRows = append(deliveredRows, o.RunID)
			} else if i > 0 && hist[i-1].EventType == EventTypeAwaitAnyChild && hist[i-1].Response == "" {
				adjacentEmpties++
			}
		case EventTypeCall:
			postRows[r.Op]++
		}
	}

	wantStarts := sc.n
	if op == postAwaitStartChild {
		wantStarts = 2 * sc.n
	}
	if len(starts) != wantStarts {
		t.Errorf("event_history holds %d child_workflow events, want exactly %d (%d children%s)",
			len(starts), wantStarts, sc.n, map[bool]string{true: " and one follow-up start per await", false: ""}[op == postAwaitStartChild])
	}
	if dup := shrinkFirstDuplicate(starts); dup != "" {
		t.Errorf("child %s was started more than once", dup)
	}
	if n := shrinkChildRowCount(t, store, wfID); n != wantStarts {
		t.Errorf("workflow_instances holds %d children of %s, want %d: starts must be exactly once in the child table too", n, wfID, wantStarts)
	}
	if !reflect.DeepEqual(deliveredRows, delivered) {
		t.Errorf("event_history delivers %v, want each child exactly once in the order %v", shrinkShortIDs(deliveredRows), shrinkShortIDs(delivered))
	}
	if len(deliveredRows) != sc.n {
		t.Errorf("event_history holds %d delivered results, want %d", len(deliveredRows), sc.n)
	}
	for _, id := range order {
		label := "handled:" + id
		if postOps[label] != 1 {
			t.Errorf("the operation after the await for %s really ran %d times, want exactly 1 (cleat#3391: each extra is a StartChild that starts another child)", shrinkShortID(id), postOps[label])
		}
		if postRows[label] != 1 {
			t.Errorf("event_history holds %d post-await records for %s, want exactly 1", postRows[label], shrinkShortID(id))
		}
	}
	// The only adjacent empties allowed are the ones the test itself wrote.
	if want := sc.legacyEmpties; adjacentEmpties != want {
		t.Errorf("event_history holds %d adjacent empty await_any_child records, want %d: a resumed call that finds nothing records nothing (cleat#3391)", adjacentEmpties, want)
	}
}

// shrinkChildRowCount counts the parent's child ROWS through the store's own
// reader, which is scoped by tenant on every dialect. The children here never
// reach a terminal status in the database (the test only says they finished),
// so none is excluded as a continued run.
func shrinkChildRowCount(t *testing.T, store WorkflowStore, parentID string) int {
	t.Helper()
	ids, err := store.OriginalChildRunIDs(context.Background(), parentID)
	if err != nil {
		t.Fatalf("list children of %s: %v", parentID, err)
	}
	return len(ids)
}

func shrinkContains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func shrinkCountTrue(m map[string]bool) int {
	n := 0
	for _, v := range m {
		if v {
			n++
		}
	}
	return n
}

func shrinkFirstDuplicate(xs []string) string {
	seen := map[string]bool{}
	for _, x := range xs {
		if seen[x] {
			return x
		}
		seen[x] = true
	}
	return ""
}

func shrinkShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func shrinkShortIDs(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = shrinkShortID(id)
	}
	return out
}
