package engine

import (
	"context"
	"fmt"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/monitoring/prometheus"
)

// cleat#4152, G3: what DurableCall (cleat_call) COUNTS, pinned before the call moves onto the dispatcher.
//
// The call emits four Prometheus series and two package counters, and not all of them come from the same place. replayCall counts a replay step
// (cleat_replay_steps_total, and the package's ReplayStepCount) BEFORE it reads the record, so a replay that then diverges, and a replay at the
// frontier that hands off to a fresh call, have both been counted. freshCall counts a call (cleat_calls_total) and a fresh step
// (cleat_fresh_steps_total, and FreshCallCount) AFTER the two refusals (a stop, a suspension) and BEFORE the cancellation poll and every other
// pre-check, so a call that is then cancelled, refused or never made has still been counted. A divergence also counts cleat_replay_failures_total.
// The dispatcher does none of this for any call that goes through it today, so a move that does not carry it changes a dashboard and nothing fails.
//
// WHAT THIS TABLE IS, AND IS NOT. Each row is a situation and the text is what the series and counters read AFTER it, measured on the code as it
// stood, and then read against what replayCall and freshCall say they do (the header of each row says which statement it checks). It compares text
// and the move must leave the COUNTS byte-identical; it cannot say a count is RIGHT, only that it did not move.
//
// THE MOVE EDITED THIS TABLE, in two ways, and both are for the reviewer. (1) The observer no longer records the result word (`results=`): the words are the
// call table's (a_call_behaves_as_recorded_before_the_dispatcher_test.go), and the restored divergence detail changes their length, which is not a count.
// Every row's counts and series are otherwise as recorded. (2) ONE row's counts changed with its meaning: replay_then_a_divergence_then_a_call. Before, the
// call after a divergence went FRESH (1 fresh call, 1 service call) because the old code consumed the record it diverged on; the dispatcher consumes nothing on
// a divergence (cleat#4157), so the next call reads the same record and diverges again: 2 replay steps, 2 replay failures, no fresh call, no service call.
// Every other row is unchanged, including both frontier rows, which are where a move that did not carry the counts would have shown. Series are read back through the
// exposition text of a real prometheus.Metrics, labels included, so a renamed label or a dropped series is a difference too.
//
// NOT COVERED, and not claimed: the cancellation poll, the canStartNewWork refusal, the event cap and the shutdown path all sit AFTER the counts
// above and so cannot change them; the table proves nothing about those paths' own behaviour, which the move's own table must pin.

var g3cSeries = regexp.MustCompile(`^(cleat_(?:calls|fresh_steps|replay_steps|replay_failures)_total)(\{[^}]*\})? ([0-9.e+-]+)$`)

type g3cCase struct {
	history   []EventRecord
	replaying bool
	// suspended makes the session already suspended, the way a guest that did not stop at a suspension leaves it.
	suspended bool
	// quit makes the replay callback ask to stop (ReplayQuit) on every step it is shown.
	quit bool
	// calls is how many DurableCalls the guest makes, in order, each of svc.op; 0 means one.
	calls int
}

func g3cCases() map[string]g3cCase {
	call := func(step int, op string) EventRecord {
		return EventRecord{Step: step, EventType: EventTypeCall, Service: "svc", Op: op, Request: `{}`, Response: `{"ok":true}`}
	}
	return map[string]g3cCase{
		"fresh":                                {},
		"replay_a_recorded_call":               {replaying: true, history: []EventRecord{call(0, "op")}},
		"replay_other_event_type":              {replaying: true, history: []EventRecord{{Step: 0, EventType: EventTypeCallAttemptFailed, Service: "svc", Op: "op"}}},
		"replay_other_operation":               {replaying: true, history: []EventRecord{call(0, "another")}},
		"replay_frontier_empty_history":        {replaying: true},
		"replay_one_then_the_frontier":         {replaying: true, history: []EventRecord{call(0, "op")}, calls: 2},
		"replay_then_a_divergence_then_a_call": {replaying: true, history: []EventRecord{call(0, "another")}, calls: 2},
		"suspended_fresh":                      {suspended: true},
		"suspended_replay_a_recorded_call":     {replaying: true, suspended: true, history: []EventRecord{call(0, "op")}},
		"suspended_at_the_frontier":            {replaying: true, suspended: true},
		"replay_quit":                          {replaying: true, quit: true, history: []EventRecord{call(0, "op")}},
	}
}

func (c g3cCase) observe() string {
	m, err := prometheus.New(prometheus.Config{})
	if err != nil {
		return "UNMEASURED: no metrics: " + err.Error()
	}
	caller := newRecordingCaller()
	eng := NewEngine(nil, caller, WithWorkflowID("wf-g3c"))
	eng.Metrics = m
	s := &execSession{
		engine: eng, workflowID: "wf-g3c", defName: "def-g3c", nowMs: time.Now().UnixMilli(),
		deferrals: map[string]string{}, queryState: map[string]string{},
		isReplay: c.replaying, history: append([]EventRecord(nil), c.history...),
	}
	if c.suspended {
		s.suspendErr = &SuspendError{Reason: "earlier"}
	}
	if c.quit {
		s.stepCallback = func(int, *EventRecord, map[string]string) ReplayStepAction { return ReplayQuit }
	}
	n := c.calls
	if n == 0 {
		n = 1
	}
	replayBefore, freshBefore := ReplayStepCount(), FreshCallCount()
	for i := 0; i < n; i++ {
		buf := make([]byte, 4096)
		ctx := context.WithValue(context.Background(), wasmMemBufKey{}, buf)
		s.DurableCall(ctx, nil, "svc", "op", `{}`, 0, 4096)
	}
	rec := httptest.NewRecorder()
	m.ServeHTTP().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	var lines []string
	for _, l := range strings.Split(rec.Body.String(), "\n") {
		if g3cSeries.MatchString(l) {
			lines = append(lines, l)
		}
	}
	sort.Strings(lines)
	return fmt.Sprintf("replayStepCount+=%d freshCallCount+=%d calls_to_service=%d series=%q",
		ReplayStepCount()-replayBefore, FreshCallCount()-freshBefore, caller.attempts, lines)
}

func TestG3cDurableCallCountersBeforeTheDispatcher(t *testing.T) {
	cases := g3cCases()
	if len(cases) != len(g3cGolden) {
		t.Fatalf("PRECONDITION: %d situations but %d recorded rows", len(cases), len(g3cGolden))
	}
	for name, c := range cases {
		want, ok := g3cGolden[name]
		if !ok {
			t.Errorf("%s: no recorded row", name)
			continue
		}
		if got := c.observe(); got != want {
			t.Errorf("%s changed:\n  was: %s\n  now: %s", name, want, got)
		}
	}
}

var g3cGolden = map[string]string{
	"fresh":                                "replayStepCount+=0 freshCallCount+=1 calls_to_service=1 series=[\"cleat_calls_total{worker_id=\\\"\\\",workflow_name=\\\"def-g3c\\\"} 1\" \"cleat_fresh_steps_total{worker_id=\\\"\\\",workflow_name=\\\"def-g3c\\\"} 1\"]",
	"replay_a_recorded_call":               "replayStepCount+=1 freshCallCount+=0 calls_to_service=0 series=[\"cleat_replay_steps_total{def_name=\\\"def-g3c\\\",worker_id=\\\"\\\"} 1\"]",
	"replay_frontier_empty_history":        "replayStepCount+=1 freshCallCount+=1 calls_to_service=1 series=[\"cleat_calls_total{worker_id=\\\"\\\",workflow_name=\\\"def-g3c\\\"} 1\" \"cleat_fresh_steps_total{worker_id=\\\"\\\",workflow_name=\\\"def-g3c\\\"} 1\" \"cleat_replay_steps_total{def_name=\\\"def-g3c\\\",worker_id=\\\"\\\"} 1\"]",
	"replay_one_then_the_frontier":         "replayStepCount+=2 freshCallCount+=1 calls_to_service=1 series=[\"cleat_calls_total{worker_id=\\\"\\\",workflow_name=\\\"def-g3c\\\"} 1\" \"cleat_fresh_steps_total{worker_id=\\\"\\\",workflow_name=\\\"def-g3c\\\"} 1\" \"cleat_replay_steps_total{def_name=\\\"def-g3c\\\",worker_id=\\\"\\\"} 2\"]",
	"replay_other_event_type":              "replayStepCount+=1 freshCallCount+=0 calls_to_service=0 series=[\"cleat_replay_failures_total{worker_id=\\\"\\\"} 1\" \"cleat_replay_steps_total{def_name=\\\"def-g3c\\\",worker_id=\\\"\\\"} 1\"]",
	"replay_other_operation":               "replayStepCount+=1 freshCallCount+=0 calls_to_service=0 series=[\"cleat_replay_failures_total{worker_id=\\\"\\\"} 1\" \"cleat_replay_steps_total{def_name=\\\"def-g3c\\\",worker_id=\\\"\\\"} 1\"]",
	"replay_quit":                          "replayStepCount+=1 freshCallCount+=0 calls_to_service=0 series=[\"cleat_replay_steps_total{def_name=\\\"def-g3c\\\",worker_id=\\\"\\\"} 1\"]",
	"replay_then_a_divergence_then_a_call": "replayStepCount+=2 freshCallCount+=0 calls_to_service=0 series=[\"cleat_replay_failures_total{worker_id=\\\"\\\"} 2\" \"cleat_replay_steps_total{def_name=\\\"def-g3c\\\",worker_id=\\\"\\\"} 2\"]",
	"suspended_at_the_frontier":            "replayStepCount+=1 freshCallCount+=0 calls_to_service=0 series=[\"cleat_replay_steps_total{def_name=\\\"def-g3c\\\",worker_id=\\\"\\\"} 1\"]",
	"suspended_fresh":                      "replayStepCount+=0 freshCallCount+=0 calls_to_service=0 series=[]",
	"suspended_replay_a_recorded_call":     "replayStepCount+=1 freshCallCount+=0 calls_to_service=0 series=[\"cleat_replay_steps_total{def_name=\\\"def-g3c\\\",worker_id=\\\"\\\"} 1\"]",
}
