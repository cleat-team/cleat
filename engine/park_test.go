package engine

// W2b of cleat#4152: the park-or-suspend helper. No wake source exists in production, so every property here is over a scripted fake; the
// behaviour that matters in production (every wait still suspends exactly as before) is pinned by a_sleep_behaves_as_recorded_before_the_park_helper_test.go.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// scriptedWake is a wakeSource whose answers a test scripts, recording every request it was handed.
type scriptedWake struct {
	reqs   []parkRequest
	answer func(i int, req parkRequest) parkOutcome
}

func (w *scriptedWake) Wait(_ context.Context, req parkRequest) parkOutcome {
	i := len(w.reqs)
	w.reqs = append(w.reqs, req)
	return w.answer(i, req)
}

func always(o parkOutcome) func(int, parkRequest) parkOutcome {
	return func(int, parkRequest) parkOutcome { return o }
}

func goodReq() parkRequest {
	return parkRequest{Kind: wakeSignal, Key: "sig-a", Baseline: 41, DeadlineMs: 1_800_000_005_000, Reason: "await_signals(sig-a)", Until: time.UnixMilli(1_800_000_005_000)}
}

// parkSession is a session that CAN park: a fresh one with a wake source. Every ineligibility row below changes exactly one thing about it.
func parkSession(ws wakeSource, opts ...EngineOption) *execSession {
	s := stepSession(opts, nil)
	s.engine.wakeSource = ws
	return s
}

func TestAWaitSuspendsExactlyAsItDidWhenThereIsNoWakeSource(t *testing.T) {
	s := parkSession(nil) // the production state: no wake source
	req := goodReq()
	if got := s.parkOrSuspend(context.Background(), req); got != parkRefused {
		t.Fatalf("with no wake source parkOrSuspend returned %v, want parkRefused", got)
	}
	if s.suspendErr == nil || s.suspendErr.Reason != req.Reason || !s.suspendErr.Until.Equal(req.Until) {
		t.Errorf("suspension = %+v, want Reason %q and Until %v from the request", s.suspendErr, req.Reason, req.Until)
	}
	if len(s.parkViolations) != 0 {
		t.Errorf("a valid request noted violations: %v", s.parkViolations)
	}
}

func TestAParkedWaitReturnsWhatTheWakeSourceSaidAndSuspendsNothing(t *testing.T) {
	for name, want := range map[string]parkOutcome{"woke": parkWoke, "deadline passed": parkDeadline} {
		ws := &scriptedWake{answer: always(want)}
		s := parkSession(ws)
		if got := s.parkOrSuspend(context.Background(), goodReq()); got != want {
			t.Errorf("%s: returned %v, want %v", name, got, want)
		}
		if s.suspendErr != nil {
			t.Errorf("%s: a parked wait that resumed also set a suspension: %+v", name, s.suspendErr)
		}
		if len(ws.reqs) != 1 {
			t.Errorf("%s: the wake source was asked %d times, want 1", name, len(ws.reqs))
		}
	}
}

// The request reaches the wake source unmodified. The lost-wakeup protection lives in the source (it registers, THEN compares Baseline), so the
// helper's part is to hand over the caller's Baseline and deadline exactly as they were.
func TestTheWakeSourceIsHandedTheRequestUnmodified(t *testing.T) {
	ws := &scriptedWake{answer: always(parkWoke)}
	s := parkSession(ws)
	want := goodReq()
	s.parkOrSuspend(context.Background(), want)
	if len(ws.reqs) != 1 || ws.reqs[0] != want {
		t.Errorf("the wake source saw %+v, want %+v", ws.reqs, want)
	}
}

// A source that declines, or answers with something that is not an outcome, suspends: a refusal is always the safe reading.
func TestAWakeSourceThatDeclinesOrIsConfusedSuspends(t *testing.T) {
	for name, answer := range map[string]parkOutcome{"declines": parkRefused, "answers with a value that is no outcome": parkOutcome(99)} {
		ws := &scriptedWake{answer: always(answer)}
		s := parkSession(ws)
		if got := s.parkOrSuspend(context.Background(), goodReq()); got != parkRefused {
			t.Errorf("%s: returned %v, want parkRefused", name, got)
		}
		if s.suspendErr == nil {
			t.Errorf("%s: no suspension was set", name)
		}
		if len(ws.reqs) != 1 {
			t.Errorf("%s: the source was asked %d times, want 1 (it declined; the helper does not retry)", name, len(ws.reqs))
		}
	}
}

// Each row changes ONE thing about an eligible session, so each condition is shown to decide the outcome alone. The control is the unchanged
// session, which must reach the wake source: without it a source that is never consulted would make every row pass.
func TestASessionThatIsNotFreeToWaitSuspendsWithoutAskingTheWakeSource(t *testing.T) {
	closedShutdown := make(chan struct{})
	close(closedShutdown)
	rows := []struct {
		name string
		opts []EngineOption
		mod  func(*execSession)
	}{
		{"a defer segment's body", []EngineOption{WithDeferPhase()}, nil},
		{"a recorded event that could not be written", nil, func(s *execSession) { s.eventWriteFailure = context.DeadlineExceeded }},
		{"a worker that is shutting down", []EngineOption{WithShutdownSignal(closedShutdown)}, nil},
		{"a segment draining its defers", []EngineOption{WithDeferPhase()}, func(s *execSession) { s.inDeferDrain = true }},
		{"the middle of history, not its end", nil, func(s *execSession) {
			s.history = []EventRecord{{Step: 0, EventType: fakeEvent}, {Step: 1, EventType: fakeEvent}}
			s.isReplay = true
			s.stepCount = 1
		}},
	}
	for _, r := range rows {
		ws := &scriptedWake{answer: always(parkWoke)}
		s := parkSession(ws, r.opts...)
		if r.mod != nil {
			r.mod(s)
		}
		if got := s.parkOrSuspend(context.Background(), goodReq()); got != parkRefused {
			t.Errorf("%s: returned %v, want parkRefused", r.name, got)
		}
		if len(ws.reqs) != 0 {
			t.Errorf("%s: the wake source was consulted %d times; a session that is not free to wait must not park", r.name, len(ws.reqs))
		}
		if s.suspendErr == nil {
			t.Errorf("%s: no suspension was set", r.name)
		}
	}
	// CONTROL: the base session parks.
	ws := &scriptedWake{answer: always(parkWoke)}
	if got := parkSession(ws).parkOrSuspend(context.Background(), goodReq()); got != parkWoke || len(ws.reqs) != 1 {
		t.Errorf("CONTROL: the unchanged session returned %v after %d wake-source calls, want parkWoke after 1", got, len(ws.reqs))
	}
}

// A wait that reaches the END of history is at the frontier and may park, and that includes a sleep, which records nothing and so never leaves
// replay: after a wake it is the call that reaches the frontier while isReplay is still true. Refusing every session in replay would make the
// most common wake-then-wait case unparkable.
func TestAWaitAtTheFrontierOfAReplayMayPark(t *testing.T) {
	ws := &scriptedWake{answer: always(parkWoke)}
	s := parkSession(ws)
	s.history = []EventRecord{{Step: 0, EventType: fakeEvent}}
	s.isReplay = true
	s.stepCount = 1 // everything consumed: this call reaches the end of history
	if got := s.parkOrSuspend(context.Background(), goodReq()); got != parkWoke || len(ws.reqs) != 1 {
		t.Errorf("a wait at the frontier of a replay returned %v after %d wake-source calls, want parkWoke after 1", got, len(ws.reqs))
	}
}

// An invalid request suspends anyway (the safe failure) and says why; the wake source is not asked. Each row breaks one rule of validate().
func TestAnInvalidParkRequestSuspendsAndIsNoted(t *testing.T) {
	rows := []struct {
		name string
		mut  func(*parkRequest)
		want string
	}{
		{"no kind", func(r *parkRequest) { r.Kind = 0 }, "no valid kind"},
		{"a kind past the last", func(r *parkRequest) { r.Kind = wakeUpdate + 1 }, "no valid kind"},
		{"a negative deadline", func(r *parkRequest) { r.DeadlineMs = -1 }, "negative deadline"},
		{"a timer with no deadline", func(r *parkRequest) { r.Kind = wakeTimer; r.DeadlineMs = 0 }, "must carry its deadline"},
	}
	for _, r := range rows {
		ws := &scriptedWake{answer: always(parkWoke)}
		s := parkSession(ws)
		req := goodReq()
		r.mut(&req)
		if got := s.parkOrSuspend(context.Background(), req); got != parkRefused {
			t.Errorf("%s: returned %v, want parkRefused", r.name, got)
		}
		if len(ws.reqs) != 0 {
			t.Errorf("%s: the wake source was asked about an invalid request", r.name)
		}
		if s.suspendErr == nil {
			t.Errorf("%s: no suspension was set", r.name)
		}
		if len(s.parkViolations) != 1 || !strings.Contains(s.parkViolations[0], r.want) {
			t.Errorf("%s: violations = %v, want one naming %q", r.name, s.parkViolations, r.want)
		}
	}
}

// The five names are the label set the parking metrics read; they are one name each, not free text beside a kind.
func TestAWakeKindsNamesAreTheMetricsLabels(t *testing.T) {
	want := map[wakeKind]string{wakeTimer: "sleep", wakeSignal: "signals", wakePromise: "promise", wakeChild: "child", wakeUpdate: "update"}
	for k, name := range want {
		if k.String() != name {
			t.Errorf("%d.String() = %q, want %q", int(k), k.String(), name)
		}
	}
	if got := wakeKind(0).String(); !strings.HasPrefix(got, "wakeKind(") {
		t.Errorf("the zero kind prints %q, want it marked as no kind", got)
	}
}

// DurableSleep on the helper, with a wake source. A timer's outcome is ADVISORY and the CLOCK decides: a sleep records nothing and is served by
// the clock on replay, so completing it before its deadline would let the run record events after a sleep that a replay still finds waiting
// (cleat-review3's condition on the first version of this test, which pinned exactly that early completion: clock at T, deadline at T+5000).
func TestADurableSleepIsCompletedByTheClockAndNotByTheWakeAlone(t *testing.T) {
	const T = int64(1_800_000_000_000)
	for name, answer := range map[string]parkOutcome{"woke": parkWoke, "deadline passed": parkDeadline} {
		// The source advances the clock to the deadline BEFORE it answers, as a real timer wake does, so the sleep is served.
		now := T
		ws := &scriptedWake{answer: func(int, parkRequest) parkOutcome { now = T + 5000; return answer }}
		s := parkSession(ws, WithClock(func() int64 { return now }))
		s.nowMs = T
		got := s.DurableSleep(context.Background(), nil, 5000)
		if got != packSleepResult(sleepStatusCompleted, 0) {
			t.Errorf("%s, clock at the deadline: a parked sleep returned %#x, want completed", name, got)
		}
		if s.suspendErr != nil {
			t.Errorf("%s, clock at the deadline: a completed parked sleep set a suspension: %+v", name, s.suspendErr)
		}
		if len(ws.reqs) != 1 {
			t.Fatalf("%s: the wake source was asked %d times, want 1", name, len(ws.reqs))
		}
		if r := ws.reqs[0]; r.Kind != wakeTimer || r.DeadlineMs != T+5000 || r.Reason != "cleat_sleep(5000ms)" {
			t.Errorf("%s: the wake source saw %+v, want a timer for %d with the sleep's reason", name, r, T+5000)
		}
		if s.eventCount != 0 {
			t.Errorf("%s: a sleep recorded %d events; it records none", name, s.eventCount)
		}
	}

	// A wake or a passed deadline that arrives while the CLOCK still says it is early does not complete the sleep: it suspends with the same request,
	// exactly as a refusal would have. Both outcomes, because each is a way a source could be wrong.
	for name, answer := range map[string]parkOutcome{"an early wake": parkWoke, "an early deadline": parkDeadline} {
		ws := &scriptedWake{answer: always(answer)} // the clock never moves
		s := parkSession(ws, WithClock(func() int64 { return T }))
		s.nowMs = T
		got := s.DurableSleep(context.Background(), nil, 5000)
		if got != packSleepResult(sleepStatusSuspend, 5000) {
			t.Errorf("%s: returned %#x, want suspend: the clock is at T and the deadline at T+5000", name, got)
		}
		if s.suspendErr == nil || s.suspendErr.Reason != "cleat_sleep(5000ms)" || !s.suspendErr.Until.Equal(time.UnixMilli(T+5000)) {
			t.Errorf("%s: suspension = %+v, want the sleep's own (cleat_sleep(5000ms), until T+5000)", name, s.suspendErr)
		}
		if len(ws.reqs) != 1 {
			t.Errorf("%s: the wake source was asked %d times, want 1 (no loop: the clock decides, and a replay serves the sleep)", name, len(ws.reqs))
		}
	}

	// CONTROL: a source that advances the clock to the deadline and then DECLINES still suspends. The clock decides only a non-refused outcome; a
	// refusal is the helper's answer, so the completions above came from a wake the clock agreed with and not from the clock alone.
	declNow := T
	declining := &scriptedWake{answer: func(int, parkRequest) parkOutcome { declNow = T + 5000; return parkRefused }}
	ds := parkSession(declining, WithClock(func() int64 { return declNow }))
	ds.nowMs = T
	if got := ds.DurableSleep(context.Background(), nil, 5000); got != packSleepResult(sleepStatusSuspend, 5000) || ds.suspendErr == nil {
		t.Errorf("CONTROL: a declined park with the clock at the deadline returned %#x (suspension %v), want suspend", got, ds.suspendErr)
	}
}

// The stop-site guards exempt canPark from being a host stop site, on the ground that it answers the helper with a bool and refuses no guest
// (stop_correspondence_guard_test.go). That is a claim about its CALLERS, and it holds only while parkOrSuspend is the one: a function that used
// canPark to refuse a guest would consult canPark and never stopBeforeNewWork, so the scan would not see it. leaveReplay carries the same
// exemption for the same reason, with the same kind of test (step_dispatcher_test.go).
func TestCanParkIsCalledOnlyByParkOrSuspend(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	callers := map[string]bool{}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", f, err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "canPark" {
						callers[fn.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	got := make([]string, 0, len(callers))
	for c := range callers {
		got = append(got, c)
	}
	sort.Strings(got)
	// CONTROL: the scan finds the real caller. If it found nothing, "only parkOrSuspend calls it" would be true of a scan that sees nothing.
	if len(got) == 0 {
		t.Fatal("the scan found no caller of canPark at all, so it cannot be trusted to find a second one")
	}
	if len(got) != 1 || got[0] != "parkOrSuspend" {
		t.Errorf("canPark is called from %v, want exactly [parkOrSuspend]: the stop-site guards exempt canPark on the ground that it refuses no guest, which a second caller would make unverifiable", got)
	}
}
