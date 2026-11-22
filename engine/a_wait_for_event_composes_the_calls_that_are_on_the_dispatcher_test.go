package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// cleat#4152, G3: cleat_wait_for_event is COMPOSITE. It has no record type of its own and no replay half: it loops over PluginCall and DurableAwaitSignals, both already on the step dispatcher,
// so what it does on a divergent, compacted, suspended or stopped history is what those two do, seen through its loop. That is why it needs no stepSpec and why there is nothing to move.
// This table pins what that composition ACTUALLY does today, one row per situation, with the expectations WRITTEN BEFORE the first run (see wfeWant: literals, not recorded output). A row that
// came out differently from what was written is a finding and is named in its comment.

type wfeCase struct {
	name    string
	history []EventRecord
	// replaying is whether the session starts in replay (a history is being served), as opposed to a fresh session.
	replaying bool
	// outputs are the claim function's answers for fresh invocations, in order; the last repeats. Empty means found=false.
	outputs   []string
	timeoutMs int64 // 0 means 60000; use noTimeout for a literal 0
	// startMs is the session's durable clock when the call is made, when it is not "now": a run resumed hours after its last record has a clock in the PAST, which is what
	// a deadline taken from the wall clock cannot be told apart from without it. Zero means now.
	startMs   int64
	noTimeout bool
	shutdown  bool
	deferred  bool
	suspended bool
	calls     int // WaitForEvent calls on one session (0 means 1)
}

type wfeWant struct {
	// results is one entry per call: "stop", "ok:<output>", or "fail(callErr=0|nz,errCode=N):<first 60 characters of the message>".
	results []string
	// contains are substrings the LAST failure's full message must contain.
	contains  []string
	invoked   int      // fresh invocations of the claim function
	recorded  []string // event types the calls wrote, in order
	suspended bool     // whether the session ended suspended
}

type wfeObserved struct {
	results     []string
	lastMessage string
	invoked     int
	recorded    []string
	suspended   bool
}

func (c wfeCase) observe(t *testing.T) wfeObserved {
	t.Helper()
	counter := &eventClaimCounter{outputs: c.outputs}
	pr := NewPluginRegistry()
	if err := pr.Register("webhook-ingest", "await_webhook", counter.call); err != nil {
		t.Fatalf("registering the claim function: %v", err)
	}
	opts := []EngineOption{WithPluginRegistry(pr), WithWorkflowID("wf-1")}
	if c.shutdown {
		down := make(chan struct{})
		close(down)
		opts = append(opts, WithShutdownSignal(down))
	}
	if c.deferred {
		opts = append(opts, WithDeferPhase())
	}
	startMs := c.startMs
	if startMs == 0 {
		startMs = time.Now().UnixMilli()
	}
	s := &execSession{
		engine: NewEngine(nil, &mockCaller{}, opts...), workflowID: "wf-1", nowMs: startMs,
		deferrals: map[string]string{}, queryState: map[string]string{},
		isReplay: c.replaying, history: append([]EventRecord(nil), c.history...),
	}
	if c.suspended {
		s.suspendErr = &SuspendError{Reason: "earlier"}
	}
	before := len(s.history)
	timeout := c.timeoutMs
	if timeout == 0 && !c.noTimeout {
		timeout = 60_000
	}
	n := c.calls
	if n == 0 {
		n = 1
	}
	var o wfeObserved
	for i := 0; i < n; i++ {
		buf := make([]byte, 4096)
		ctx := contextWithRawMemBuf(context.Background(), buf)
		packed := s.WaitForEvent(ctx, nil, "webhook-ingest", "await_webhook", "{}", `["__evt:payment"]`, timeout, 0, 4096)
		switch {
		case packed == callSuspendSentinel:
			o.results = append(o.results, "stop")
		default:
			callErr, errCode := byte((packed>>8)&0xFF), byte(packed)
			msg := string(buf[:uint32(uint64(packed)>>40)])
			if callErr == 0 && errCode == 0 {
				o.results = append(o.results, "ok:"+msg)
				break
			}
			o.lastMessage = msg
			kind := "0"
			if callErr != 0 {
				kind = "nz"
			}
			short := strings.ReplaceAll(msg, "\n", "⏎")
			if len(short) > 60 {
				short = short[:60]
			}
			o.results = append(o.results, fmt.Sprintf("fail(callErr=%s,errCode=%d):%s", kind, errCode, short))
		}
	}
	o.invoked = counter.calls
	for _, r := range s.history[before:] {
		// An await's recorded wait and the signals it registered are what the loop CHOOSES (the backoff schedule and the signal list); everything else a record carries is the plugin's or the dispatcher's.
		if r.EventType == EventTypeAwaitSignals {
			o.recorded = append(o.recorded, fmt.Sprintf("await_signals(timeout=%d,names=%s)", r.TimeoutMs, r.SignalNames))
			continue
		}
		o.recorded = append(o.recorded, string(r.EventType))
	}
	o.suspended = s.suspendErr != nil
	return o
}

func wfeSeq(n int) []EventRecord { return recordedClaimSequence()[:n] }

func wfeWrapped(callErr string) string {
	return fmt.Sprintf(`fail(callErr=%s,errCode=1):wait_for_event: webhook-ingest/await_webhook output has no b`, callErr)
}

const (
	wfeFoundPaid   = `{"found":true,"payload":"PAID"}`
	wfeTimeoutHead = `fail(callErr=nz,errCode=1):wait_for_event: no event for webhook-ingest/await_webhook wi`
)

func wfeCases() []wfeCase {
	future := time.Now().Add(10 * time.Minute).UnixMilli()
	recordedMiss := pluginClaimRecord(0, `{"found":false}`)
	recordedMiss.TimestampMs = future
	otherFn := EventRecord{Step: 0, EventType: EventTypePluginCall, PluginName: "other-plugin", PluginFunc: "other_fn", PluginOutput: `{"found":true}`}
	// A run resumed an hour after its clock stopped: its durable clock reads an hour ago, and its records are stamped in that time.
	oldStart := time.Now().Add(-time.Hour).UnixMilli()
	missAt := func(offsetMs int64) EventRecord {
		r := pluginClaimRecord(0, `{"found":false}`)
		r.TimestampMs = oldStart + offsetMs
		return r
	}
	signalFirst := EventRecord{Step: 0, EventType: EventTypeSignalReceived, SignalName: "__evt:payment"}
	return []wfeCase{
		{name: "replay_found_first", history: []EventRecord{pluginClaimRecord(0, `{"found":true,"payload":"X"}`)}, replaying: true},
		{name: "replay_claim_sequence", history: recordedClaimSequence(), replaying: true},
		{name: "replay_malformed_recorded_output", history: []EventRecord{pluginClaimRecord(0, `{}`)}, replaying: true},
		{name: "replay_recorded_plugin_error", history: []EventRecord{{Step: 0, EventType: EventTypePluginCall, PluginName: "webhook-ingest", PluginFunc: "await_webhook", PluginError: "boom"}}, replaying: true},
		{name: "replay_miss_at_the_frontier", history: wfeSeq(1), replaying: true},
		{name: "replay_wake_then_a_fresh_claim_finds_it", history: wfeSeq(3), replaying: true, outputs: []string{`{"found":true,"payload":"Y"}`}},
		{name: "replay_wake_then_a_fresh_claim_misses", history: wfeSeq(3), replaying: true},
		{name: "replay_the_timeout_passed_while_the_run_was_down", history: []EventRecord{recordedMiss}, replaying: true, timeoutMs: 1000},
		{name: "replay_a_timeout_that_passed_in_the_runs_own_clock", history: []EventRecord{missAt(5000)}, replaying: true, startMs: oldStart, timeoutMs: 1000},
		{name: "replay_a_miss_with_less_budget_than_the_backoff", history: []EventRecord{missAt(400)}, replaying: true, startMs: oldStart, timeoutMs: 1000},
		{name: "diverge_other_function_at_the_call", history: []EventRecord{otherFn}, replaying: true},
		{name: "diverge_other_event_type_at_the_call", history: []EventRecord{signalFirst}, replaying: true},
		{name: "diverge_twice", history: []EventRecord{otherFn}, replaying: true, calls: 2},
		{name: "diverge_at_the_await_and_a_later_claim_is_served", history: []EventRecord{pluginClaimRecord(0, `{"found":false}`), pluginClaimRecord(1, `{"found":true,"payload":"Z"}`)}, replaying: true},
		{name: "a_lone_signal_received_is_taken_as_the_wake", history: []EventRecord{pluginClaimRecord(0, `{"found":false}`), {Step: 1, EventType: EventTypeSignalReceived, SignalName: "__evt:payment"}}, replaying: true},
		{name: "diverge_at_the_await_and_nothing_after", history: []EventRecord{pluginClaimRecord(0, `{"found":false}`), {Step: 1, EventType: EventTypeCall}}, replaying: true},
		{name: "fresh_found_first", outputs: []string{`{"found":true,"payload":"F"}`}},
		{name: "fresh_zero_timeout_and_nothing_found", noTimeout: true},
		{name: "fresh_while_stopping", shutdown: true},
		{name: "fresh_in_a_defer_segment", deferred: true},
		{name: "fresh_after_the_session_suspended", suspended: true},
		{name: "replay_complete_history_while_stopping", history: recordedClaimSequence(), replaying: true, shutdown: true},
		{name: "replay_frontier_while_stopping", history: wfeSeq(3), replaying: true, shutdown: true},
	}
}

// wfeGolden is WRITTEN BEFORE the table was first run. Where the first run disagreed, the row's entry below says so in a comment and the entry is the corrected one.
var wfeGolden = map[string]wfeWant{
	"replay_found_first":               {results: []string{`ok:{"found":true,"payload":"X"}`}},
	"replay_claim_sequence":            {results: []string{"ok:" + wfeFoundPaid}},
	"replay_malformed_recorded_output": {results: []string{wfeWrapped("nz")}, contains: []string{`no boolean "found" field`, "Output was: {}"}},
	// A recorded plugin error: the call answers with a non-zero callErr and the message is the recorded error, returned as it stands (the loop does not wrap it).
	"replay_recorded_plugin_error": {results: []string{"fail(callErr=nz,errCode=1):boom"}, contains: []string{"boom"}},
	// The recorded miss, then the frontier: the wait registers (records await_signals) and suspends; no claim is run.
	"replay_miss_at_the_frontier":             {results: []string{"stop"}, recorded: []string{`await_signals(timeout=6000,names=["__evt:payment"])`}, suspended: true},
	"replay_wake_then_a_fresh_claim_finds_it": {results: []string{`ok:{"found":true,"payload":"Y"}`}, invoked: 1, recorded: []string{"plugin_call"}},
	"replay_wake_then_a_fresh_claim_misses":   {results: []string{"stop"}, invoked: 1, recorded: []string{"plugin_call", `await_signals(timeout=12000,names=["__evt:payment"])`}, suspended: true},
	// The recorded miss carries a timestamp ten minutes ahead, so the durable clock is past the 1 s deadline before the wait is asked to wait: it times out without registering.
	"replay_the_timeout_passed_while_the_run_was_down": {results: []string{wfeTimeoutHead}, contains: []string{"within 1000ms"}},
	// WRITTEN BEFORE THE RUN, added after the mutants showed these two gaps. The run's own clock is an hour in the past: five seconds into a one-second budget it has timed out, whatever the
	// wall clock says (a deadline taken from the wall clock would put the end of the wait an hour in the future and wait again); and 400 ms into it there are 600 ms left, which caps the wait
	// below the 6 s the backoff would pick.
	"replay_a_timeout_that_passed_in_the_runs_own_clock": {results: []string{wfeTimeoutHead}, contains: []string{"within 1000ms"}},
	"replay_a_miss_with_less_budget_than_the_backoff":    {results: []string{"stop"}, recorded: []string{`await_signals(timeout=600,names=["__evt:payment"])`}, suspended: true},
	// A divergence inside the call: the dispatcher's answer has callErr 0 and errCode 1, which the loop's `callErr != 0` test does NOT take for a failure, so it reads the divergence
	// message as the claim's output and reports it as NOT claim-shaped. The guest still gets a failure (errCode 1) and the divergence text is inside it, but behind a misleading first line.
	"diverge_other_function_at_the_call":   {results: []string{wfeWrapped("nz")}, contains: []string{"replay divergence at step 0", "recorded other-plugin/other_fn"}},
	"diverge_other_event_type_at_the_call": {results: []string{wfeWrapped("nz")}, contains: []string{"replay divergence at step 0", "expected plugin_call, got signal_received"}},
	"diverge_twice":                        {results: []string{wfeWrapped("nz"), wfeWrapped("nz")}, contains: []string{"replay divergence at step 0"}},
	// A divergence at the AWAIT (the record there is a plugin_call): DurableAwaitSignals answers with an error that is not the stop word and sets no suspension, so the loop
	// takes it for an expired wait and goes round again; the claim at the next step is then served from that same record. The divergence is swallowed.
	"diverge_at_the_await_and_a_later_claim_is_served": {results: []string{`ok:{"found":true,"payload":"Z"}`}},
	// PREDICTED a divergence here and was WRONG: the first run showed the await taking a signal_received with NO await_signals before it as its own resolution (the wake), after which the
	// loop re-claims, runs the claim live at the end of the history (invoked 1), records it, and registers a fresh await, which suspends. The expectation below is what was observed, written
	// as a finding about the await's replay (a lone final record is accepted without its prefix), not as something wait_for_event does. The row after it is the divergence I meant.
	"a_lone_signal_received_is_taken_as_the_wake": {results: []string{"stop"}, invoked: 1, recorded: []string{"plugin_call", `await_signals(timeout=12000,names=["__evt:payment"])`}, suspended: true},
	"diverge_at_the_await_and_nothing_after":      {results: []string{wfeWrapped("nz")}, contains: []string{"expected plugin_call, got call"}},
	"fresh_found_first":                           {results: []string{`ok:{"found":true,"payload":"F"}`}, invoked: 1, recorded: []string{"plugin_call"}},
	"fresh_zero_timeout_and_nothing_found":        {results: []string{wfeTimeoutHead}, contains: []string{"within 0ms"}, invoked: 1, recorded: []string{"plugin_call"}},
	// Stopped, in a defer segment, or already suspended: the stop word, no claim, nothing recorded.
	"fresh_while_stopping":              {results: []string{"stop"}},
	"fresh_in_a_defer_segment":          {results: []string{"stop"}},
	"fresh_after_the_session_suspended": {results: []string{"stop"}, suspended: true},
	// A complete history is SERVED even while stopping (replay is not new work); at the frontier the call refuses.
	"replay_complete_history_while_stopping": {results: []string{"ok:" + wfeFoundPaid}},
	"replay_frontier_while_stopping":         {results: []string{"stop"}},
}

func (c wfeCase) check(t *testing.T, want wfeWant) []string {
	t.Helper()
	got := c.observe(t)
	var diffs []string
	if strings.Join(got.results, " | ") != strings.Join(want.results, " | ") {
		diffs = append(diffs, fmt.Sprintf("results:\n  got:  %q\n  want: %q", got.results, want.results))
	}
	for _, sub := range want.contains {
		if !strings.Contains(got.lastMessage, sub) {
			diffs = append(diffs, fmt.Sprintf("the last failure's message lacks %q; it reads %q", sub, got.lastMessage))
		}
	}
	if got.invoked != want.invoked {
		diffs = append(diffs, fmt.Sprintf("claim invocations: got %d, want %d", got.invoked, want.invoked))
	}
	if strings.Join(got.recorded, ",") != strings.Join(want.recorded, ",") {
		diffs = append(diffs, fmt.Sprintf("records written: got %v, want %v", got.recorded, want.recorded))
	}
	if got.suspended != want.suspended {
		diffs = append(diffs, fmt.Sprintf("suspended: got %v, want %v", got.suspended, want.suspended))
	}
	return diffs
}

func TestWaitForEventComposesTheCallsThatAreOnTheDispatcher(t *testing.T) {
	for _, c := range wfeCases() {
		c := c
		t.Run(c.name, func(t *testing.T) {
			want, ok := wfeGolden[c.name]
			if !ok {
				t.Fatalf("PRECONDITION FAILED: no expectation was written for row %q", c.name)
			}
			if diffs := c.check(t, want); len(diffs) > 0 {
				t.Errorf("row %q differs from what was written down:\n%s", c.name, strings.Join(diffs, "\n"))
			}
		})
	}
	if len(wfeGolden) != len(wfeCases()) {
		t.Errorf("PRECONDITION FAILED: %d expectations for %d rows", len(wfeGolden), len(wfeCases()))
	}
}

// A COMPACTED history, expanded again, must replay through the wait as the uncompacted one does (cleat#4325's lesson). Expectation written before the run: every history row survives, because
// the records the loop reads (plugin_call, await_signals, signal_received) are the ones compaction carries with their name, output, error and timeout. The rows that have no history are not here.
func TestACompactedHistoryReplaysThroughWaitForEvent(t *testing.T) {
	ran := 0
	for _, c := range wfeCases() {
		if len(c.history) == 0 {
			continue
		}
		c := c
		t.Run(c.name, func(t *testing.T) {
			want := wfeGolden[c.name]
			// The control: the uncompacted row reads as written, so a difference below is about compaction and not about the row.
			if diffs := c.check(t, want); len(diffs) > 0 {
				t.Fatalf("PRECONDITION FAILED: the uncompacted row does not read as written:\n%s", strings.Join(diffs, "\n"))
			}
			cs, err := extractCompactionStateFrom(nil, c.history)
			if err != nil {
				t.Fatalf("compacting the history: %v", err)
			}
			expanded := buildFullHistoryFromCompaction(nil, cs)
			if len(expanded) != len(c.history) {
				t.Fatalf("PRECONDITION FAILED: %d events went into compaction and %d came out", len(c.history), len(expanded))
			}
			c.history = expanded
			if diffs := c.check(t, want); len(diffs) > 0 {
				t.Errorf("a COMPACTED history did not replay through the wait as the uncompacted one does:\n%s", strings.Join(diffs, "\n"))
			}
			ran++
		})
	}
	if ran == 0 {
		t.Fatal("PRECONDITION FAILED: no row ran, so nothing was checked")
	}
}
