package engine

// cleat#4152, PR 0 of docs/design/replay-step-dispatcher.md: a characterization of the three promise single-step host calls
// (CreatePromise, ResolvePromise, RejectPromise), RECORDED FROM THE CODE AS IT WAS BEFORE THEY WENT THROUGH step(), and kept so that
// moving them is checkable as "changes where the code lives and nothing else".
//
// Each call is driven through the same eight situations and the result is one string per (call, situation): the packed result, whether
// the session is still replaying, the step count, and the events the call RECORDED (not the ones it consumed). The golden strings below
// were produced by running this file against develop 7d1d0e20 with no production change; a row that changes after the migration is a
// behaviour change, and the PR that changes it has to say so.
//
// What a row cannot say: the guest-visible bytes of a divergence message (the harness writes to a nil module here, as the other session
// tests do, so only the packed result and the step count are observed), and the timestamp of a recorded event, which is masked.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

type promiseCall struct {
	name string
	// call performs the host call once on s.
	call func(s *execSession) int64
}

var promiseCalls = []promiseCall{
	{"create_promise", func(s *execSession) int64 { return s.CreatePromise(context.Background(), nil, "n", 0, 0) }},
	{"resolve_promise", func(s *execSession) int64 { return s.ResolvePromise(context.Background(), nil, "p-1", "v") }},
	{"reject_promise", func(s *execSession) int64 { return s.RejectPromise(context.Background(), nil, "p-1", "e") }},
}

// describeRecorded renders the events at and after index from, without anything that varies between runs.
func describeRecorded(history []EventRecord, from int) string {
	var parts []string
	for _, r := range history[from:] {
		id := r.PromiseID
		if r.EventType == EventTypeCreatePromise && id != "" {
			id = "<generated>" // CreatePromise draws a UUID
		}
		parts = append(parts, fmt.Sprintf("%s/step=%d/svc=%q/op=%q/id=%q/name=%q/result=%q/err=%q", r.EventType, r.Step, r.Service, r.Op, id, r.PromiseName, r.PromiseResult, r.Err))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func observe(s *execSession, res int64, historyBefore int) string {
	return fmt.Sprintf("res=%#x isReplay=%v step=%d recorded=%s", res, s.isReplay, s.stepCount, describeRecorded(s.history, historyBefore))
}

// characterizePromiseCall returns every situation's observation, keyed by situation.
func characterizePromiseCall(t *testing.T, pc promiseCall) map[string]string {
	t.Helper()
	got := map[string]string{}
	newSession := func(opts []EngineOption, history []EventRecord) *execSession {
		s := promiseSession(&mockPromiseStore{}, history)
		if opts != nil {
			s.engine = NewEngine(nil, nil, append(opts, WithPromiseStore(&mockPromiseStore{}))...)
		}
		return s
	}

	// The record a fresh run of this call leaves, which is the history every replay situation starts from.
	s0 := newSession(nil, nil)
	res0 := pc.call(s0)
	got["fresh"] = observe(s0, res0, 0)
	recorded := append([]EventRecord(nil), s0.history...)
	if len(recorded) != 1 {
		t.Fatalf("PRECONDITION FAILED: %s recorded %d events fresh, the table below assumes exactly one", pc.name, len(recorded))
	}

	// replay: the call meets its own record.
	s1 := newSession(nil, append([]EventRecord(nil), recorded...))
	got["replay"] = observe(s1, pc.call(s1), len(s1.history))

	// frontier: the call meets its own record, then the same call is made again past the end of history.
	s2 := newSession(nil, append([]EventRecord(nil), recorded...))
	pc.call(s2)
	n := len(s2.history)
	got["frontier"] = observe(s2, pc.call(s2), n)

	// divergence: the record at this step is another call's.
	other := []EventRecord{{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Response: "{}"}}
	s3 := newSession(nil, other)
	got["divergence"] = observe(s3, pc.call(s3), len(s3.history))

	// segment, fresh: a defer segment's body past the end of its (empty) history.
	s4 := newSession([]EngineOption{WithDeferPhase()}, nil)
	s4.isReplay = true // a segment starts in replay with nothing left to read
	got["segment_fresh"] = observe(s4, pc.call(s4), len(s4.history))

	// segment, frontier: the segment replays this call's record and the body then makes it again.
	s5 := newSession([]EngineOption{WithDeferPhase()}, append([]EventRecord(nil), recorded...))
	pc.call(s5)
	n5 := len(s5.history)
	got["segment_frontier"] = observe(s5, pc.call(s5), n5)

	// no store: the engine has no promise store, so the event is recorded and the guest is told it failed.
	s6 := promiseSession(nil, nil)
	s6.engine = NewEngine(nil, nil)
	s6.nowMs = time.Now().UnixMilli()
	got["no_store"] = observe(s6, pc.call(s6), 0)

	// store error: the store refuses; the event is already recorded and stays, and the guest is told the call failed.
	s7 := promiseSession(&mockPromiseStore{}, nil)
	s7.engine = NewEngine(nil, nil, WithPromiseStore(&refusingPromiseStore{}))
	got["store_error"] = observe(s7, pc.call(s7), 0)

	// store error, then replay: nothing in the recorded event says the store write failed, so a replay of that history hands the guest the
	// SUCCESS the event implies, where the original run was handed an error (WS-cleat-2's reading of the round-trip property (c), measured
	// here). Kept as a row so that the migration neither hides nor fixes it silently; filed as cleat#4203.
	recordedAfterFailure := append([]EventRecord(nil), s7.history...)
	s8 := newSession(nil, recordedAfterFailure)
	got["store_error_then_replay"] = observe(s8, pc.call(s8), len(s8.history))

	// by type alone: replay accepts a record of this call's EVENT TYPE whether or not it carries the Service/Op that marks it as this
	// call's own effect (resolve and reject share their event types with the await's own outcome). A record of this call's type with
	// neither Service nor Op is what await_promise writes for a promise that was already settled.
	if pc.name != "create_promise" {
		bare := append([]EventRecord(nil), recorded...)
		bare[0].Service, bare[0].Op = "", ""
		s9 := newSession(nil, bare)
		got["replay_by_type_alone"] = observe(s9, pc.call(s9), len(s9.history))
	}
	return got
}

// refusingPromiseStore refuses every write; the package's failingPromiseStore fails CreatePromise only and mockPromiseStore never fails.
type refusingPromiseStore struct{ mockPromiseStore }

func (refusingPromiseStore) CreatePromise(context.Context, string, string, string) error {
	return fmt.Errorf("store down")
}
func (refusingPromiseStore) ResolvePromise(context.Context, string, string) error {
	return fmt.Errorf("store down")
}
func (refusingPromiseStore) RejectPromise(context.Context, string, string) error {
	return fmt.Errorf("store down")
}

// promiseGolden is what characterizePromiseCall returned on develop 7d1d0e20, before any of these calls went through step().
var promiseGolden = map[string]map[string]string{
	"create_promise": {
		"divergence":              "res=0x1 isReplay=true step=0 recorded=[]",
		"fresh":                   "res=0x0 isReplay=false step=1 recorded=[create_promise/step=0/svc=\"\"/op=\"\"/id=\"<generated>\"/name=\"n\"/result=\"\"/err=\"\"]",
		"frontier":                "res=0x0 isReplay=false step=2 recorded=[create_promise/step=1/svc=\"\"/op=\"\"/id=\"<generated>\"/name=\"n\"/result=\"\"/err=\"\"]",
		"no_store":                "res=0x1 isReplay=false step=1 recorded=[create_promise/step=0/svc=\"\"/op=\"\"/id=\"<generated>\"/name=\"n\"/result=\"\"/err=\"\"]",
		"replay":                  "res=0x0 isReplay=true step=1 recorded=[]",
		"segment_fresh":           "res=0x80000000 isReplay=false step=0 recorded=[]",
		"segment_frontier":        "res=0x80000000 isReplay=false step=1 recorded=[]",
		"store_error":             "res=0x1 isReplay=false step=1 recorded=[create_promise/step=0/svc=\"\"/op=\"\"/id=\"<generated>\"/name=\"n\"/result=\"\"/err=\"\"]",
		"store_error_then_replay": "res=0x0 isReplay=true step=1 recorded=[]",
	},
	"resolve_promise": {
		"divergence":              "res=0x1 isReplay=true step=0 recorded=[]",
		"fresh":                   "res=0x0 isReplay=false step=1 recorded=[promise_resolved/step=0/svc=\"promise\"/op=\"resolve\"/id=\"p-1\"/name=\"\"/result=\"v\"/err=\"\"]",
		"frontier":                "res=0x0 isReplay=false step=2 recorded=[promise_resolved/step=1/svc=\"promise\"/op=\"resolve\"/id=\"p-1\"/name=\"\"/result=\"v\"/err=\"\"]",
		"no_store":                "res=0x1 isReplay=false step=1 recorded=[promise_resolved/step=0/svc=\"promise\"/op=\"resolve\"/id=\"p-1\"/name=\"\"/result=\"v\"/err=\"\"]",
		"replay":                  "res=0x0 isReplay=true step=1 recorded=[]",
		"replay_by_type_alone":    "res=0x0 isReplay=true step=1 recorded=[]",
		"segment_fresh":           "res=0x80000000 isReplay=false step=0 recorded=[]",
		"segment_frontier":        "res=0x80000000 isReplay=false step=1 recorded=[]",
		"store_error":             "res=0x1 isReplay=false step=1 recorded=[promise_resolved/step=0/svc=\"promise\"/op=\"resolve\"/id=\"p-1\"/name=\"\"/result=\"v\"/err=\"\"]",
		"store_error_then_replay": "res=0x0 isReplay=true step=1 recorded=[]",
	},
	"reject_promise": {
		"divergence":              "res=0x1 isReplay=true step=0 recorded=[]",
		"fresh":                   "res=0x0 isReplay=false step=1 recorded=[promise_rejected/step=0/svc=\"promise\"/op=\"reject\"/id=\"p-1\"/name=\"\"/result=\"\"/err=\"\"]",
		"frontier":                "res=0x0 isReplay=false step=2 recorded=[promise_rejected/step=1/svc=\"promise\"/op=\"reject\"/id=\"p-1\"/name=\"\"/result=\"\"/err=\"\"]",
		"no_store":                "res=0x1 isReplay=false step=1 recorded=[promise_rejected/step=0/svc=\"promise\"/op=\"reject\"/id=\"p-1\"/name=\"\"/result=\"\"/err=\"\"]",
		"replay":                  "res=0x0 isReplay=true step=1 recorded=[]",
		"replay_by_type_alone":    "res=0x0 isReplay=true step=1 recorded=[]",
		"segment_fresh":           "res=0x80000000 isReplay=false step=0 recorded=[]",
		"segment_frontier":        "res=0x80000000 isReplay=false step=1 recorded=[]",
		"store_error":             "res=0x1 isReplay=false step=1 recorded=[promise_rejected/step=0/svc=\"promise\"/op=\"reject\"/id=\"p-1\"/name=\"\"/result=\"\"/err=\"\"]",
		"store_error_then_replay": "res=0x0 isReplay=true step=1 recorded=[]",
	},
}

func TestAPromiseSingleStepBehavesAsItDidBeforeTheDispatcher(t *testing.T) {
	for _, pc := range promiseCalls {
		pc := pc
		t.Run(pc.name, func(t *testing.T) {
			got := characterizePromiseCall(t, pc)
			want := promiseGolden[pc.name]
			if want == nil {
				for k, v := range got {
					t.Errorf("GOLDEN MISSING %s/%s: %s", pc.name, k, v)
				}
				return
			}
			for k, v := range got {
				if want[k] != v {
					t.Errorf("%s/%s changed:\n   got  %s\n   want %s", pc.name, k, v, want[k])
				}
			}
			for k := range want {
				if _, ok := got[k]; !ok {
					t.Errorf("%s/%s: a golden situation is no longer observed", pc.name, k)
				}
			}
		})
	}
}
