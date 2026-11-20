package engine

// cleat#4152, group G1b of docs/design/replay-step-dispatcher.md: a characterization of SignalWorkflow, DurableSend and Fetch, RECORDED FROM THE
// CODE AS IT WAS BEFORE THEY WENT THROUGH step() (develop 6fc6340d, no production change), and kept so that moving them is checkable as "changes
// where the code lives and nothing else" and every row it does change is visible in the migration's diff.
//
// Each call is driven through a set of situations and the result is one string per (call, situation): the packed result, the bytes the call wrote
// to the guest's buffer, whether the session is still replaying, the step count, the events the call RECORDED, and what the call did to the OUTSIDE
// WORLD (the signals delivered, the service calls dispatched, the fetches made). A row that changes after the migration is a behaviour change.
//
// What a row cannot say: the timestamp of a recorded event (masked), and anything the wasm backends add around the call.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

const g1bBufLen = 512

// g1bWorld records what the three calls did to the outside world.
type g1bWorld struct {
	mu         sync.Mutex
	deliveries []string
	fetches    []string
	sends      []string
	sendDone   chan struct{}

	deliverErr error // what the signal store answers
	fetchResp  string
	fetchErr   error
}

func (w *g1bWorld) DeliverSignal(_ context.Context, workflowID, signalName, payload string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deliveries = append(w.deliveries, workflowID+"/"+signalName+"/"+payload)
	return w.deliverErr
}
func (w *g1bWorld) PollSignal(context.Context, string, string) (SignalDelivery, bool, error) {
	return SignalDelivery{}, false, nil
}
func (w *g1bWorld) ConsumeSignal(context.Context, string, int64) error { return nil }
func (w *g1bWorld) PollCancellation(context.Context, string) (bool, string, error) {
	return false, "", nil
}
func (w *g1bWorld) Fetch(_ context.Context, method, url, headers, body string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.fetches = append(w.fetches, method+" "+url+" "+body)
	return w.fetchResp, w.fetchErr
}

// Call is the fire-and-forget service the send dispatches to, on a goroutine.
func (w *g1bWorld) Call(_ context.Context, service, operation, request string) (string, error) {
	w.mu.Lock()
	w.sends = append(w.sends, service+"."+operation+"("+request+")")
	w.mu.Unlock()
	select {
	case w.sendDone <- struct{}{}:
	default:
	}
	return `{}`, nil
}

type g1bCall struct {
	name string
	call func(ctx context.Context, s *execSession) int64
}

var g1bCalls = []g1bCall{
	{"signal_workflow", func(ctx context.Context, s *execSession) int64 {
		return s.SignalWorkflow(ctx, nil, "target-run", "go", `{"n":1}`)
	}},
	{"durable_send", func(ctx context.Context, s *execSession) int64 {
		return s.DurableSend(ctx, nil, "svc", "notify", `{"to":"x"}`)
	}},
	{"fetch", func(ctx context.Context, s *execSession) int64 {
		return s.Fetch(ctx, nil, "GET", "https://example.test/a", `{}`, "", 0, g1bBufLen)
	}},
}

func g1bSession(w *g1bWorld, history []EventRecord, replaying bool, opts ...EngineOption) *execSession {
	base := []EngineOption{WithSignalStore(w), WithFetcher(w)}
	return &execSession{
		engine:     NewEngine(nil, w, append(base, opts...)...),
		workflowID: "wf-g1b", defName: "caller-def", nowMs: time.Now().UnixMilli(),
		deferrals: map[string]string{}, queryState: map[string]string{},
		isReplay: replaying, history: history,
	}
}

func g1bDescribe(history []EventRecord, from int) string {
	var parts []string
	for _, r := range history[from:] {
		parts = append(parts, fmt.Sprintf("%s/step=%d/svc=%q/op=%q/resp=%q/sig=%q/payload=%q/run=%q/req=%q/err=%q/fm=%q/fu=%q/fb=%q/fr=%q",
			r.EventType, r.Step, r.Service, r.Op, r.Response, r.SignalName, r.SignalPayload, r.RunID, r.Request, r.Err,
			r.FetchMethod, r.FetchURL, r.FetchBody, r.FetchResponse))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// g1bObserve runs the call and renders everything observable about it, including what reached the outside world.
func g1bObserve(gc g1bCall, w *g1bWorld, s *execSession) string {
	buf := make([]byte, g1bBufLen)
	ctx := context.WithValue(context.Background(), wasmMemBufKey{}, buf)
	before := len(s.history)
	res := gc.call(ctx, s)
	// the send dispatches on a goroutine: wait for it, bounded, so a refusal (which sends nothing) costs the bound and a send is seen
	if gc.name == "durable_send" {
		select {
		case <-w.sendDone:
		case <-time.After(150 * time.Millisecond):
		}
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	w.mu.Lock()
	world := fmt.Sprintf("deliveries=%v sends=%v fetches=%v", w.deliveries, w.sends, w.fetches)
	w.mu.Unlock()
	return fmt.Sprintf("res=%#x wrote=%q isReplay=%v step=%d recorded=%s %s", res, string(buf[:n]), s.isReplay, s.stepCount, g1bDescribe(s.history, before), world)
}

func newG1bWorld() *g1bWorld {
	return &g1bWorld{sendDone: make(chan struct{}, 4), fetchResp: `{"ok":true}`}
}

// g1bOwn is the record a fresh, healthy run of this call leaves.
func g1bOwn(gc g1bCall) []EventRecord {
	switch gc.name {
	case "durable_send":
		return []EventRecord{{Step: 0, EventType: EventTypeDurableSend, Service: "svc", Op: "notify", Request: `{"to":"x"}`}}
	case "fetch":
		return []EventRecord{{Step: 0, EventType: EventTypeFetch, FetchMethod: "GET", FetchURL: "https://example.test/a", FetchHeaders: `{}`, FetchResponse: `{"recorded":true}`}}
	default:
		return []EventRecord{{Step: 0, EventType: EventTypeSignalReceived, Service: SignalEventService, Op: SignalOpSend, SignalName: "go", SignalPayload: `{"n":1}`, RunID: "target-run"}}
	}
}

func g1bCharacterize(t *testing.T, gc g1bCall) map[string]string {
	t.Helper()
	got := map[string]string{}
	own := func() []EventRecord { return append([]EventRecord(nil), g1bOwn(gc)...) }
	other := func() []EventRecord {
		return []EventRecord{{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Response: "{}"}}
	}
	run := func(history []EventRecord, replaying bool, mut func(*g1bWorld, *execSession), opts ...EngineOption) (*g1bWorld, *execSession) {
		w := newG1bWorld()
		s := g1bSession(w, history, replaying, opts...)
		if mut != nil {
			mut(w, s)
		}
		return w, s
	}

	w, s := run(nil, false, nil)
	got["fresh"] = g1bObserve(gc, w, s)

	w, s = run(own(), true, nil)
	got["replay"] = g1bObserve(gc, w, s)

	w, s = run(own(), true, nil)
	g1bObserve(gc, w, s)
	got["frontier"] = g1bObserve(gc, w, s)

	w, s = run(other(), true, nil)
	got["divergence"] = g1bObserve(gc, w, s)

	w, s = run(nil, true, nil, WithDeferPhase())
	got["segment_fresh"] = g1bObserve(gc, w, s)

	w, s = run(own(), true, nil, WithDeferPhase())
	g1bObserve(gc, w, s)
	got["segment_frontier"] = g1bObserve(gc, w, s)

	w, s = run(nil, false, func(_ *g1bWorld, s *execSession) { s.eventWriteFailure = errors.New("event write refused") })
	got["write_failed_fresh"] = g1bObserve(gc, w, s)

	w, s = run(own(), true, func(_ *g1bWorld, s *execSession) { s.eventWriteFailure = errors.New("event write refused") })
	g1bObserve(gc, w, s)
	got["write_failed_frontier"] = g1bObserve(gc, w, s)

	down := make(chan struct{})
	close(down)
	w, s = run(nil, false, nil, WithShutdownSignal(down))
	got["shutdown_fresh"] = g1bObserve(gc, w, s)

	w, s = run(own(), true, func(_ *g1bWorld, s *execSession) {
		s.stepCallback = func(int, *EventRecord, map[string]string) ReplayStepAction { return ReplayQuit }
	})
	got["replay_quit"] = g1bObserve(gc, w, s)

	switch gc.name {
	case "signal_workflow":
		// the three outcomes of a delivery, fresh
		w, s = run(nil, false, func(w *g1bWorld, _ *execSession) { w.deliverErr = ErrWorkflowNotFound })
		got["fresh_not_found"] = g1bObserve(gc, w, s)
		w, s = run(nil, false, func(w *g1bWorld, _ *execSession) { w.deliverErr = errors.New("store down") })
		got["fresh_delivery_failed"] = g1bObserve(gc, w, s)
		w, s = run(nil, false, nil, WithRequireSignalAuth(true), WithSignalAuthCheck(func(context.Context, string, string) error { return errors.New("denied") }))
		got["fresh_auth_required"] = g1bObserve(gc, w, s)
		// a recorded outcome replays as that outcome; an unknown one as a failure; an old send (no Op) as success
		for _, o := range []struct{ name, resp string }{{"replay_outcome_not_found", signalOutcomeNotFound}, {"replay_outcome_unknown", "from_a_newer_engine"}} {
			rec := own()
			rec[0].Response = o.resp
			w, s = run(rec, true, nil)
			got[o.name] = g1bObserve(gc, w, s)
		}
		legacy := own()
		legacy[0].Service, legacy[0].Op = "", ""
		w, s = run(legacy, true, nil)
		got["replay_delivery_or_legacy_record"] = g1bObserve(gc, w, s)
		// a record of another signal and another target: the old replay accepted it
		rec := own()
		rec[0].SignalName, rec[0].RunID = "stop", "another-run"
		w, s = run(rec, true, nil)
		got["replay_other_target_and_name"] = g1bObserve(gc, w, s)
	case "durable_send":
		// a record of another service and operation: the old replay accepted it
		rec := own()
		rec[0].Service, rec[0].Op = "billing", "charge"
		w, s = run(rec, true, nil)
		got["replay_other_service_and_op"] = g1bObserve(gc, w, s)
	case "fetch":
		w, s = run(nil, false, func(w *g1bWorld, _ *execSession) { w.fetchErr = errors.New("egress denied") })
		got["fresh_fetch_error"] = g1bObserve(gc, w, s)
		w = newG1bWorld()
		s = g1bSession(w, nil, false)
		s.engine.fetcher = nil
		got["fresh_no_fetcher"] = g1bObserve(gc, w, s)
		rec := own()
		rec[0].Err = "egress denied"
		w, s = run(rec, true, nil)
		got["replay_error_record"] = g1bObserve(gc, w, s)
		// the old replay consumed the record BEFORE comparing it, and compared method, url and body
		for _, m := range []struct {
			name string
			mut  func(*EventRecord)
		}{
			{"replay_other_method", func(r *EventRecord) { r.FetchMethod = "POST" }},
			{"replay_other_url", func(r *EventRecord) { r.FetchURL = "https://example.test/b" }},
			{"replay_other_body", func(r *EventRecord) { r.FetchBody = "changed" }},
			{"replay_other_headers", func(r *EventRecord) { r.FetchHeaders = `{"x":"y"}` }},
		} {
			rec := own()
			m.mut(&rec[0])
			w, s = run(rec, true, nil)
			got[m.name] = g1bObserve(gc, w, s)
		}
	}
	return got
}

// g1bGolden is the table recorded against the code as it was (develop 6fc6340d), one string per (call, situation): the reference the migration is
// checked against. A row that changes is a behaviour change, and the PR that changes it says so.
var g1bGolden = map[string]map[string]string{
	"signal_workflow": {
		"divergence":                       "res=0x1 wrote=\"\" isReplay=true step=0 recorded=[] deliveries=[] sends=[] fetches=[]",
		"fresh":                            "res=0x0 wrote=\"\" isReplay=false step=1 recorded=[signal_received/step=0/svc=\"signal\"/op=\"send\"/resp=\"\"/sig=\"go\"/payload=\"{\\\"n\\\":1}\"/run=\"target-run\"/req=\"\"/err=\"\"/fm=\"\"/fu=\"\"/fb=\"\"/fr=\"\"] deliveries=[target-run/go/{\"n\":1}] sends=[] fetches=[]",
		"fresh_auth_required":              "res=-0xfffffffe wrote=\"\" isReplay=false step=1 recorded=[signal_received/step=0/svc=\"signal\"/op=\"send\"/resp=\"auth_required\"/sig=\"go\"/payload=\"{\\\"n\\\":1}\"/run=\"target-run\"/req=\"\"/err=\"\"/fm=\"\"/fu=\"\"/fb=\"\"/fr=\"\"] deliveries=[] sends=[] fetches=[]",
		"fresh_delivery_failed":            "res=-0xfffffffd wrote=\"\" isReplay=false step=1 recorded=[signal_received/step=0/svc=\"signal\"/op=\"send\"/resp=\"delivery_failed\"/sig=\"go\"/payload=\"{\\\"n\\\":1}\"/run=\"target-run\"/req=\"\"/err=\"\"/fm=\"\"/fu=\"\"/fb=\"\"/fr=\"\"] deliveries=[target-run/go/{\"n\":1}] sends=[] fetches=[]",
		"fresh_not_found":                  "res=-0xfffffffc wrote=\"\" isReplay=false step=1 recorded=[signal_received/step=0/svc=\"signal\"/op=\"send\"/resp=\"not_found\"/sig=\"go\"/payload=\"{\\\"n\\\":1}\"/run=\"target-run\"/req=\"\"/err=\"\"/fm=\"\"/fu=\"\"/fb=\"\"/fr=\"\"] deliveries=[target-run/go/{\"n\":1}] sends=[] fetches=[]",
		"frontier":                         "res=0x0 wrote=\"\" isReplay=false step=2 recorded=[signal_received/step=1/svc=\"signal\"/op=\"send\"/resp=\"\"/sig=\"go\"/payload=\"{\\\"n\\\":1}\"/run=\"target-run\"/req=\"\"/err=\"\"/fm=\"\"/fu=\"\"/fb=\"\"/fr=\"\"] deliveries=[target-run/go/{\"n\":1}] sends=[] fetches=[]",
		"replay":                           "res=0x0 wrote=\"\" isReplay=true step=1 recorded=[] deliveries=[] sends=[] fetches=[]",
		"replay_delivery_or_legacy_record": "res=0x0 wrote=\"\" isReplay=true step=1 recorded=[] deliveries=[] sends=[] fetches=[]",
		"replay_other_target_and_name":     "res=0x0 wrote=\"\" isReplay=true step=1 recorded=[] deliveries=[] sends=[] fetches=[]",
		"replay_outcome_not_found":         "res=-0xfffffffc wrote=\"\" isReplay=true step=1 recorded=[] deliveries=[] sends=[] fetches=[]",
		"replay_outcome_unknown":           "res=-0xfffffffd wrote=\"\" isReplay=true step=1 recorded=[] deliveries=[] sends=[] fetches=[]",
		"replay_quit":                      "res=0x0 wrote=\"\" isReplay=true step=1 recorded=[] deliveries=[] sends=[] fetches=[]",
		"segment_fresh":                    "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] deliveries=[] sends=[] fetches=[]",
		"segment_frontier":                 "res=0x80000000 wrote=\"\" isReplay=false step=1 recorded=[] deliveries=[] sends=[] fetches=[]",
		"shutdown_fresh":                   "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] deliveries=[] sends=[] fetches=[]",
		"write_failed_fresh":               "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] deliveries=[] sends=[] fetches=[]",
		"write_failed_frontier":            "res=0x80000000 wrote=\"\" isReplay=false step=1 recorded=[] deliveries=[] sends=[] fetches=[]",
	},
	"durable_send": {
		"divergence":                  "res=0x1 wrote=\"\" isReplay=true step=0 recorded=[] deliveries=[] sends=[] fetches=[]",
		"fresh":                       "res=0x0 wrote=\"\" isReplay=false step=1 recorded=[durable_send/step=0/svc=\"svc\"/op=\"notify\"/resp=\"\"/sig=\"\"/payload=\"\"/run=\"\"/req=\"{\\\"to\\\":\\\"x\\\"}\"/err=\"\"/fm=\"\"/fu=\"\"/fb=\"\"/fr=\"\"] deliveries=[] sends=[svc.notify({\"to\":\"x\"})] fetches=[]",
		"frontier":                    "res=0x0 wrote=\"\" isReplay=false step=2 recorded=[durable_send/step=1/svc=\"svc\"/op=\"notify\"/resp=\"\"/sig=\"\"/payload=\"\"/run=\"\"/req=\"{\\\"to\\\":\\\"x\\\"}\"/err=\"\"/fm=\"\"/fu=\"\"/fb=\"\"/fr=\"\"] deliveries=[] sends=[svc.notify({\"to\":\"x\"})] fetches=[]",
		"replay":                      "res=0x0 wrote=\"\" isReplay=true step=1 recorded=[] deliveries=[] sends=[] fetches=[]",
		"replay_other_service_and_op": "res=0x0 wrote=\"\" isReplay=true step=1 recorded=[] deliveries=[] sends=[] fetches=[]",
		"replay_quit":                 "res=0x0 wrote=\"\" isReplay=true step=1 recorded=[] deliveries=[] sends=[] fetches=[]",
		"segment_fresh":               "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] deliveries=[] sends=[] fetches=[]",
		"segment_frontier":            "res=0x80000000 wrote=\"\" isReplay=false step=1 recorded=[] deliveries=[] sends=[] fetches=[]",
		"shutdown_fresh":              "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] deliveries=[] sends=[] fetches=[]",
		"write_failed_fresh":          "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] deliveries=[] sends=[] fetches=[]",
		"write_failed_frontier":       "res=0x80000000 wrote=\"\" isReplay=false step=1 recorded=[] deliveries=[] sends=[] fetches=[]",
	},
	"fetch": {
		"divergence":            "res=0x12100000001 wrote=\"replay divergence at step 0: Fetch mismatch.\\n  workflow: GET https://example.test/a\\n  history:  \\n  actual body: \\n  expected body: \\n  expected response: \\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=1 recorded=[] deliveries=[] sends=[] fetches=[]",
		"fresh":                 "res=0xb00000000 wrote=\"{\\\"ok\\\":true}\" isReplay=false step=1 recorded=[fetch/step=0/svc=\"\"/op=\"\"/resp=\"\"/sig=\"\"/payload=\"\"/run=\"\"/req=\"\"/err=\"\"/fm=\"GET\"/fu=\"https://example.test/a\"/fb=\"\"/fr=\"{\\\"ok\\\":true}\"] deliveries=[] sends=[] fetches=[GET https://example.test/a ]",
		"fresh_fetch_error":     "res=0xd00000001 wrote=\"egress denied\" isReplay=false step=1 recorded=[fetch/step=0/svc=\"\"/op=\"\"/resp=\"\"/sig=\"\"/payload=\"\"/run=\"\"/req=\"\"/err=\"egress denied\"/fm=\"GET\"/fu=\"https://example.test/a\"/fb=\"\"/fr=\"{\\\"ok\\\":true}\"] deliveries=[] sends=[] fetches=[GET https://example.test/a ]",
		"fresh_no_fetcher":      "res=0x11c00000001 wrote=\"cleat_fetch is disabled: this engine has no Fetcher. A cleat-worker serves cleat_fetch only when started with --enable-workflow-fetch (off by default; docs/operations/egress-policy.md); an embedding host supplies engine.WithFetcher(...). Workflow  attempted GET https://example.test/a\" isReplay=false step=1 recorded=[fetch/step=0/svc=\"\"/op=\"\"/resp=\"\"/sig=\"\"/payload=\"\"/run=\"\"/req=\"\"/err=\"cleat_fetch is disabled: this engine has no Fetcher. A cleat-worker serves cleat_fetch only when started with --enable-workflow-fetch (off by default; docs/operations/egress-policy.md); an embedding host supplies engine.WithFetcher(...). Workflow  attempted GET https://example.test/a\"/fm=\"GET\"/fu=\"https://example.test/a\"/fb=\"\"/fr=\"\"] deliveries=[] sends=[] fetches=[]",
		"frontier":              "res=0xb00000000 wrote=\"{\\\"ok\\\":true}\" isReplay=false step=2 recorded=[fetch/step=1/svc=\"\"/op=\"\"/resp=\"\"/sig=\"\"/payload=\"\"/run=\"\"/req=\"\"/err=\"\"/fm=\"GET\"/fu=\"https://example.test/a\"/fb=\"\"/fr=\"{\\\"ok\\\":true}\"] deliveries=[] sends=[] fetches=[GET https://example.test/a ]",
		"replay":                "res=0x1100000000 wrote=\"{\\\"recorded\\\":true}\" isReplay=true step=1 recorded=[] deliveries=[] sends=[] fetches=[]",
		"replay_error_record":   "res=0xd00000001 wrote=\"egress denied\" isReplay=true step=1 recorded=[] deliveries=[] sends=[] fetches=[]",
		"replay_other_body":     "res=0x15200000001 wrote=\"replay divergence at step 0: Fetch mismatch.\\n  workflow: GET https://example.test/a\\n  history: GET https://example.test/a\\n  actual body: \\n  expected body: changed\\n  expected response: {\\\"recorded\\\":true}\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=1 recorded=[] deliveries=[] sends=[] fetches=[]",
		"replay_other_headers":  "res=0x1100000000 wrote=\"{\\\"recorded\\\":true}\" isReplay=true step=1 recorded=[] deliveries=[] sends=[] fetches=[]",
		"replay_other_method":   "res=0x14c00000001 wrote=\"replay divergence at step 0: Fetch mismatch.\\n  workflow: GET https://example.test/a\\n  history: POST https://example.test/a\\n  actual body: \\n  expected body: \\n  expected response: {\\\"recorded\\\":true}\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=1 recorded=[] deliveries=[] sends=[] fetches=[]",
		"replay_other_url":      "res=0x14b00000001 wrote=\"replay divergence at step 0: Fetch mismatch.\\n  workflow: GET https://example.test/a\\n  history: GET https://example.test/b\\n  actual body: \\n  expected body: \\n  expected response: {\\\"recorded\\\":true}\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=1 recorded=[] deliveries=[] sends=[] fetches=[]",
		"replay_quit":           "res=0x0 wrote=\"\" isReplay=true step=1 recorded=[] deliveries=[] sends=[] fetches=[]",
		"segment_fresh":         "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] deliveries=[] sends=[] fetches=[]",
		"segment_frontier":      "res=0x80000000 wrote=\"\" isReplay=false step=1 recorded=[] deliveries=[] sends=[] fetches=[]",
		"shutdown_fresh":        "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] deliveries=[] sends=[] fetches=[]",
		"write_failed_fresh":    "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] deliveries=[] sends=[] fetches=[]",
		"write_failed_frontier": "res=0x80000000 wrote=\"\" isReplay=false step=1 recorded=[] deliveries=[] sends=[] fetches=[]",
	},
}

func TestG1bCallsBehaveAsRecordedBeforeTheDispatcher(t *testing.T) {
	for _, gc := range g1bCalls {
		gc := gc
		t.Run(gc.name, func(t *testing.T) {
			want, ok := g1bGolden[gc.name]
			if !ok {
				t.Fatalf("no golden rows for %s", gc.name)
			}
			got := g1bCharacterize(t, gc)
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
