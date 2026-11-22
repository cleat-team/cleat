package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/monitoring/prometheus"
	"github.com/cleat-team/cleat/plugin"
)

// cleat#4152, G3: what PluginCallStreaming (plugin_call_streaming) does in each situation, pinned BEFORE the call moves onto the dispatcher, so the move reads as
// a diff.
//
// A streaming call is a MULTI-RECORD step: it records one plugin_call_stream_chunk event PER CHUNK, each at its own step, and replay reads the run of consecutive
// chunk events back. A stream-level failure (no registry, an unregistered function, a call-guard refusal, a secret-only violation, a function that fails to start)
// is recorded as ONE synthetic finished chunk that carries the error code and is marked as not plugin output (StreamData false).
//
// Each row is a situation and the text is what the call did: the result word, the bytes it wrote, whether the session is still replaying, the step, the events it
// recorded (type, function, input, output, chunk index, finish, data, error code), how many times each registered function actually RAN and with what input, the
// chunks PUBLISHED to a live reader (step, index, finish, durable), the suspension, and the replay-step counter and series.
//
// Every row's expectation was written down from reading PluginCallStreaming, freshPluginCallStreaming and replayPluginCallStreaming BEFORE the output was looked at.
//
// TWO ROWS PIN A DEFECT AS IT IS and are named so (KNOWN_DEFECT_*): replay does not check the plugin or the function the record names, and it reads EVERY consecutive
// chunk record without a boundary, so the first of two adjacent streaming calls replays the chunks of both. See the comments on those rows.
//
// THE CONTEXT-CANCELLED EXIT OF THE CHUNK LOOP is pinned by fresh_cancelled_mid_stream without a timer: the function holds its channel open and the test releases it only
// once the call is seen blocked in its deferred drain, which with the channel open and empty only the cancel branch can reach (a first version closed the channel straight after
// the cancel, so both exits were ready and Go picked at random; a mutant that discarded the partial result survived).
//
// THE MOVE ONTO THE DISPATCHER EDITED NINE OF THESE ROWS (cleat#4152, G3), and only these, by key; the others read byte-for-byte as they did. Three that were
// KNOWN_DEFECT_ rows are divergences now (a record for another function, another plugin, and one that is not a chunk), two more are two answers and not one (adjacent
// streams), a third that swallowed the next call's chunks is a divergence on identity, and two answer the same but leave replay (a stream that never finished, answered from
// what was recorded), and one more: a STOPPING session whose history ends inside such a stream is answered the stop word, because the dispatcher refuses before the recorded
// chunks are served (the old replay had no refusal on its replay path). The one defect left is the same shape with the SAME function both times (KNOWN_DEFECT_..._same_function_...): cleat#4342.
//
// NOT COVERED, and not claimed: a chunk the database refuses (the engine here has no database, so recordEvent cannot fail a flush), a real plugin's health tracker
// beyond the one panic row, the CallContext a function receives, secret-reference resolution before the call reaches the engine, and a real worker.
// callerPluginName is assigned nowhere in production, so the call-guard row is reachable by a test only.

const g3sBufLen = 4096

var g3sSeries = regexp.MustCompile(`^(cleat_[a-z_]*(?:replay|fresh|plugin|calls)[a-z_]*(?:_total|_count))(\{[^}]*\})? ([0-9.e+-]+)$`)

type g3sCase struct {
	history    []EventRecord
	replaying  bool
	suspended  bool
	deferPhase bool
	shutdown   bool
	quit       bool
	// calls is how many PluginCallStreamings the guest makes, in order, each of plugin p and function fn with input; 0 means one.
	calls int
	fn    string // "" means "three"
	// seq, when set, is the function each call uses, in order (and calls is its length).
	seq   []string
	input string // "" means `{"key":"val"}`
	// noRegistry leaves the engine without a stream registry.
	noRegistry bool
	guardFrom  string
	bufLen     int
}

type g3sRun struct {
	n  map[string]int
	in map[string][]string
	// release lets cancel_mid close its channel; see g3sReleaseWhenTheCallIsInItsDrain.
	release chan struct{}
}

func g3sRegistry(run *g3sRun, cancel context.CancelFunc) *PluginStreamRegistry {
	reg := NewPluginStreamRegistry()
	add := func(name string, secret []string, fn func(ctx context.Context, input string) (<-chan plugin.StreamEvent, error)) {
		if err := reg.registerWithSecretOnlyFields("p", name, func(ctx context.Context, input string) (<-chan plugin.StreamEvent, error) {
			run.n[name]++
			run.in[name] = append(run.in[name], input)
			return fn(ctx, input)
		}, secret); err != nil {
			panic(err)
		}
	}
	emit := func(events ...plugin.StreamEvent) func(context.Context, string) (<-chan plugin.StreamEvent, error) {
		return func(_ context.Context, _ string) (<-chan plugin.StreamEvent, error) {
			ch := make(chan plugin.StreamEvent, len(events))
			for _, e := range events {
				ch <- e
			}
			close(ch)
			return ch, nil
		}
	}
	add("three", nil, emit(plugin.StreamEvent{Index: 0, Content: "a"}, plugin.StreamEvent{Index: 1, Content: "b"}, plugin.StreamEvent{Index: 2, Content: "c", Finish: true}))
	add("empty", nil, emit())
	add("again", nil, emit(plugin.StreamEvent{Index: 0, Content: "a"}, plugin.StreamEvent{Index: 1, Content: "b"}, plugin.StreamEvent{Index: 2, Content: "c", Finish: true}))
	add("one_final", nil, emit(plugin.StreamEvent{Index: 0, Content: "only", Finish: true}))
	add("no_finish", nil, emit(plugin.StreamEvent{Index: 0, Content: "a"}, plugin.StreamEvent{Index: 1, Content: "b"}))
	add("start_fails", nil, func(context.Context, string) (<-chan plugin.StreamEvent, error) {
		return nil, errors.New("cannot start")
	})
	add("panics", nil, func(context.Context, string) (<-chan plugin.StreamEvent, error) { panic("kaboom") })
	add("secret", []string{"token"}, emit(plugin.StreamEvent{Index: 0, Content: "s", Finish: true}))
	// cancel_mid sends one chunk, cancels the call's context, and then holds the channel OPEN until the test releases it. The test releases it only once the call
	// is blocked in its deferred drain (g3sReleaseWhenTheCallIsInItsDrain), and the call can reach that drain with the channel still open only by leaving its loop
	// through the context-cancelled branch: with the channel open and empty, that is the only branch that can have been taken. So the cancel exit is exercised
	// deterministically, with no timer.
	add("cancel_mid", nil, func(ctx context.Context, _ string) (<-chan plugin.StreamEvent, error) {
		ch := make(chan plugin.StreamEvent)
		go func() {
			ch <- plugin.StreamEvent{Index: 0, Content: "a"}
			cancel()
			<-run.release
			close(ch)
		}()
		return ch, nil
	})
	return reg
}

// g3sReleaseWhenTheCallIsInItsDrain closes release once some goroutine is blocked on a channel receive inside freshPluginCallStreaming's deferred drain, and reports
// whether it ever saw that. It spins on the runtime's own goroutine dump rather than waiting on a clock. A bounded spin: when it gives up it releases anyway and reports
// false, so the row says so (UNMEASURED) rather than hanging or passing.
func g3sReleaseWhenTheCallIsInItsDrain(release chan struct{}) (saw bool) {
	defer close(release)
	buf := make([]byte, 1<<20)
	for i := 0; i < 2_000_000; i++ {
		n := runtime.Stack(buf, true)
		for _, g := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.Contains(g, "[chan receive") && strings.Contains(g, "freshPluginCallStreaming.func") {
				return true
			}
		}
		runtime.Gosched()
	}
	return false
}

func g3sEvents(history []EventRecord, from int) string {
	var parts []string
	for _, r := range history[from:] {
		parts = append(parts, fmt.Sprintf("%s/step=%d/%s.%s/in=%q/out=%q/i=%d/fin=%v/data=%v/code=%d", r.EventType, r.Step, r.PluginName, r.PluginFunc, r.PluginInput, r.PluginOutput, r.StreamChunkIndex, r.StreamFinish, r.StreamData, r.StreamErrCode))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func (c g3sCase) observe() string {
	m, err := prometheus.New(prometheus.Config{})
	if err != nil {
		return "UNMEASURED: no metrics: " + err.Error()
	}
	baseCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &g3sRun{n: map[string]int{}, in: map[string][]string{}, release: make(chan struct{})}
	hub := NewStreamHub(0)
	opts := []EngineOption{WithWorkflowID("wf-g3s"), WithStreamHub(hub)}
	if !c.noRegistry {
		opts = append(opts, WithPluginStreamRegistry(g3sRegistry(run, cancel)))
	}
	if c.deferPhase {
		opts = append(opts, WithDeferPhase())
	}
	if c.shutdown {
		down := make(chan struct{})
		close(down)
		opts = append(opts, WithShutdownSignal(down))
	}
	if c.guardFrom != "" {
		guard := NewPluginCallGuard()
		guard.Allow(c.guardFrom, []string{"some-other-plugin"})
		opts = append(opts, WithPluginCallGuard(guard))
	}
	eng := NewEngine(nil, nil, opts...)
	eng.Metrics = m
	sub, _ := hub.Subscribe("wf-g3s")
	s := &execSession{
		engine: eng, workflowID: "wf-g3s", defName: "def-g3s", nowMs: time.Now().UnixMilli(), originalInput: `{}`,
		deferrals: map[string]string{}, queryState: map[string]string{},
		isReplay: c.replaying, history: append([]EventRecord(nil), c.history...),
		callerPluginName: c.guardFrom,
	}
	if c.suspended {
		s.suspendErr = &SuspendError{Reason: "earlier"}
	}
	if c.quit {
		s.stepCallback = func(int, *EventRecord, map[string]string) ReplayStepAction { return ReplayQuit }
	}
	n := c.calls
	if len(c.seq) > 0 {
		n = len(c.seq)
	}
	if n == 0 {
		n = 1
	}
	fn, input, bufLen := c.fn, c.input, c.bufLen
	if fn == "" {
		fn = "three"
	}
	if input == "" {
		input = `{"key":"val"}`
	}
	if bufLen == 0 {
		bufLen = g3sBufLen
	}
	sawDrain := make(chan bool, 1)
	if fn == "cancel_mid" {
		go func() { sawDrain <- g3sReleaseWhenTheCallIsInItsDrain(run.release) }()
	} else {
		close(run.release)
	}
	before := len(s.history)
	replayBefore := ReplayStepCount()
	var words, wrote []string
	var panicked string
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = fmt.Sprint(r)
			}
		}()
		for i := 0; i < n; i++ {
			buf := make([]byte, bufLen)
			ctx := context.WithValue(baseCtx, wasmMemBufKey{}, buf)
			callFn := fn
			if len(c.seq) > 0 {
				callFn = c.seq[i]
			}
			res := s.PluginCallStreaming(ctx, nil, "p", callFn, input, 0, uint32(bufLen))
			k := 0
			for k < len(buf) && buf[k] != 0 {
				k++
			}
			words = append(words, fmt.Sprintf("%#x", res))
			wrote = append(wrote, string(buf[:k]))
		}
	}()
	if fn == "cancel_mid" {
		if !<-sawDrain {
			return "UNMEASURED: the call was never seen blocked in its drain, so the cancel exit was not shown to be the one taken"
		}
	}
	var published []string
	if sub != nil {
	drain:
		for {
			select {
			case ch := <-sub.ch:
				published = append(published, "s"+strconv.Itoa(ch.Step)+"/i"+strconv.Itoa(ch.Index)+"/fin="+strconv.FormatBool(ch.Finish)+"/dur="+strconv.FormatBool(ch.Durable))
			default:
				break drain
			}
		}
	}
	rec := httptest.NewRecorder()
	m.ServeHTTP().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	var lines []string
	for _, l := range strings.Split(rec.Body.String(), "\n") {
		if g3sSeries.MatchString(l) {
			lines = append(lines, l)
		}
	}
	sort.Strings(lines)
	var ran []string
	for name, cnt := range run.n {
		ran = append(ran, fmt.Sprintf("%s:%d(in=%q)", name, cnt, run.in[name]))
	}
	sort.Strings(ran)
	susp := "none"
	if s.suspendErr != nil {
		susp = s.suspendErr.Reason
	}
	out := fmt.Sprintf("res=%v wrote=%q isReplay=%v step=%d recorded=%s ran=%v published=%v suspension=%s replayStepCount+=%d series=%q",
		words, wrote, s.isReplay, s.stepCount, g3sEvents(s.history, before), ran, published, susp, ReplayStepCount()-replayBefore, lines)
	if panicked != "" {
		out = "PANIC=" + fmt.Sprintf("%q", panicked) + " " + out
	}
	if len(s.stepViolations) > 0 {
		out += fmt.Sprintf(" SPEC-VIOLATIONS=%q", s.stepViolations)
	}
	return out
}

func g3sCases() map[string]g3sCase {
	chunk := func(step int, fn string, idx int, content string, finish, data bool, code int) EventRecord {
		return EventRecord{Step: step, EventType: EventTypePluginCallStreamChunk, PluginName: "p", PluginFunc: fn, PluginInput: `{"key":"val"}`, PluginOutput: content, StreamChunkIndex: idx, StreamFinish: finish, StreamData: data, StreamErrCode: code}
	}
	three := func(fn string) []EventRecord {
		return []EventRecord{chunk(0, fn, 0, "a", false, true, 0), chunk(1, fn, 1, "b", false, true, 0), chunk(2, fn, 2, "c", true, true, 0)}
	}
	return map[string]g3sCase{
		// Fresh.
		"fresh_three_chunks":          {},
		"fresh_empty_stream":          {fn: "empty"},
		"fresh_one_final_chunk":       {fn: "one_final"},
		"fresh_no_finish_chunk":       {fn: "no_finish"},
		"fresh_cancelled_mid_stream":  {fn: "cancel_mid"},
		"fresh_start_fails":           {fn: "start_fails"},
		"fresh_function_panics":       {fn: "panics"},
		"fresh_unregistered":          {fn: "nope"},
		"fresh_no_registry":           {noRegistry: true},
		"fresh_call_guard_refuses":    {guardFrom: "other"},
		"fresh_secret_only_reference": {fn: "secret", input: `{"token":"${secret:abc}"}`},
		"fresh_secret_only_literal":   {fn: "secret", input: `{"token":"hunter2"}`},
		"fresh_secret_only_malformed": {fn: "secret", input: `not json`},
		"fresh_output_too_large":      {bufLen: 8},
		// An EMPTY stream records NOTHING (the step stays 0): there is no chunk to record. A later replay therefore cannot tell it from a call that is not there.
		"fresh_empty_then_a_stream": {seq: []string{"empty", "three"}},
		"fresh_two_calls":           {calls: 2},
		// Stops and suspensions.
		"stopping_fresh":            {shutdown: true},
		"stopping_at_the_frontier":  {shutdown: true, replaying: true},
		"defer_phase_fresh":         {deferPhase: true},
		"suspended_fresh":           {suspended: true},
		"suspended_replay_recorded": {suspended: true, replaying: true, history: three("three")},
		"suspended_at_the_frontier": {suspended: true, replaying: true},
		// Replay of the call's own records.
		"replay_three_chunks":                {replaying: true, history: three("three")},
		"replay_one_final_data_chunk":        {replaying: true, fn: "one_final", history: []EventRecord{chunk(0, "one_final", 0, "only", true, true, 0)}},
		"replay_single_failure_unknown":      {replaying: true, history: []EventRecord{chunk(0, "three", 0, "recorded failure", true, false, 0)}},
		"replay_single_failure_failure_code": {replaying: true, history: []EventRecord{chunk(0, "three", 0, "recorded failure", true, false, int(callFailureCode))}},
		// A single NON-FINAL chunk without the data flag: not a failure, because a failure is one FINISHED chunk. Fresh always writes the data flag, so this shape exists only in
		// history recorded before cleat#4117, which must replay as it always did (it is served as a one-element stream).
		"replay_single_non_final_chunk_without_the_data_flag": {replaying: true, history: []EventRecord{chunk(0, "three", 0, "half", false, false, 0)}},
		// A stream that never finished, FOLLOWED BY another call's record (and not by the end of history): a complete stream with no final, answered from what was recorded.
		"replay_unfinished_chunks_then_another_call": {replaying: true, history: []EventRecord{chunk(0, "three", 0, "a", false, true, 0), chunk(1, "three", 1, "b", false, true, 0), {Step: 2, EventType: EventTypePluginCall, PluginName: "p", PluginFunc: "ok", PluginInput: `{}`, PluginOutput: `{}`}}},
		// A finished chunk that restarts at index 0 after a chunk that did not finish: one run, and the finished chunk keeps its recorded index.
		"replay_a_final_chunk_that_restarts_at_index_zero": {replaying: true, history: []EventRecord{chunk(0, "three", 0, "a", false, true, 0), chunk(1, "three", 0, "b", true, true, 0)}},
		// THE IDENTITY DOES NOT READ THE INPUT, and these two rows are what says so (cleat-review3, on the first head: an input comparison in the Identity left every row green, and
		// it would be WRONG, not only stricter, because a failure for a secret-only function records the REDACTED input, which a live call never equals). A secret function's recorded
		// failure replays as that failure whatever literal the live call carries, and a recorded stream replays as that stream under a different live input.
		"replay_a_secret_functions_recorded_failure_with_the_live_input": {replaying: true, fn: "secret", input: `{"token":"hunter2"}`, history: []EventRecord{{Step: 0, EventType: EventTypePluginCallStreamChunk, PluginName: "p", PluginFunc: "secret", PluginInput: "[secret-only field, literal value refused]", PluginOutput: "plugin stream function p/secret: field \"token\" is declared secret-only and must be exactly a ${secret:NAME} reference, written with no JSON escaping inside it -- a literal value, a non-string value, or an escaped reference ResolveSecretRefs could never have found is refused", StreamFinish: true, StreamErrCode: int(callFailureCode)}}},
		"replay_three_chunks_under_a_different_live_input":               {replaying: true, input: `{"another":"input"}`, history: three("three")},
		// A history that ENDS INSIDE a stream, met by a stopping session: the dispatcher's stop refusal comes before the recorded chunks are served, so the guest gets the stop word. The old
		// replay served the chunks (it had no refusal on its replay path).
		"stopping_with_history_ending_inside_a_stream": {shutdown: true, replaying: true, history: []EventRecord{chunk(0, "three", 0, "a", false, true, 0), chunk(1, "three", 1, "b", false, true, 0)}},
		"replay_no_finish":        {replaying: true, history: []EventRecord{chunk(0, "three", 0, "a", false, true, 0), chunk(1, "three", 1, "b", false, true, 0)}},
		"replay_output_too_large": {replaying: true, bufLen: 8, history: three("three")},
		"replay_quit":             {replaying: true, quit: true, history: three("three")},
		// The record is followed by another call's record: the chunks are served and the run ends there.
		"replay_chunks_then_another_call": {replaying: true, history: append(three("three"), EventRecord{Step: 3, EventType: EventTypePluginCall, PluginName: "p", PluginFunc: "ok", PluginInput: `{}`, PluginOutput: `{}`})},
		// The frontier.
		"replay_frontier_empty_history": {replaying: true},
		"replay_one_then_the_frontier":  {replaying: true, calls: 2, history: three("three")},
		// KNOWN DEFECTS, pinned as they are (cleat#4152, G3). Replay reads EVERY consecutive chunk record and checks neither the plugin nor the function the record names.
		// (1) A record for ANOTHER function is served as this call's answer, with no divergence.
		"replay_other_function_is_a_divergence": {replaying: true, fn: "three", history: three("empty")},
		"replay_other_plugin_is_a_divergence":   {replaying: true, fn: "three", history: []EventRecord{{Step: 0, EventType: EventTypePluginCallStreamChunk, PluginName: "q", PluginFunc: "three", PluginInput: `{}`, PluginOutput: "x", StreamChunkIndex: 0, StreamFinish: true, StreamData: true}}},
		// (2) A record that is NOT a chunk at the call's step is not a divergence either: the loop reads nothing and the call answers SUCCESS with a null stream, without
		// advancing, so the record is left for whatever is called next.
		"replay_foreign_record_is_a_divergence": {replaying: true, history: []EventRecord{{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Request: `{}`}}},
		// (2b) The same reading makes an empty stream swallow the NEXT call's chunks: it recorded nothing, so replay reads the following stream's records as its own,
		// and the second call reaches the frontier and runs the function live a second time.
		"replay_an_empty_stream_then_a_stream_of_another_function_is_a_divergence": {replaying: true, seq: []string{"empty", "three"}, history: three("three")},
		// (2d) The SAME shape with the SAME function both times: call 1 was an empty stream (recorded nothing) and call 2 the three chunks. The dispatcher's Identity cannot tell
		// them apart, so the first replay is still served the second call's chunks and the second reaches the frontier and runs the function live again (cleat#4342).
		"KNOWN_DEFECT_replay_an_empty_stream_swallows_the_next_same_function_streams_chunks": {replaying: true, seq: []string{"again", "again"}, history: three("again")},
		// (2c) The same merge, where the second call is ONE FINAL chunk at index 0: its recorded index survives the renumbering (a chunk at index 0 that finishes is
		// kept as recorded), so the answer carries i:0 for the second call's chunk after the first call's i:2.
		"replay_two_adjacent_streams_the_second_one_final_chunk": {replaying: true, calls: 2, history: append(three("three"), chunk(3, "three", 0, "x", true, true, 0))},
		// (3) There is no boundary between two adjacent streaming calls' chunks, so the first call's replay returns BOTH calls' chunks and the second reaches the frontier.
		"replay_two_adjacent_streams_are_two_answers": {replaying: true, calls: 2, history: append(three("three"), chunk(3, "three", 0, "x", false, true, 0), chunk(4, "three", 1, "y", true, true, 0))},
	}
}

func TestG3sPluginStreamCallBehavesAsRecordedBeforeTheDispatcher(t *testing.T) {
	cases := g3sCases()
	if len(cases) != len(g3sGolden) {
		t.Fatalf("PRECONDITION: %d situations but %d recorded rows", len(cases), len(g3sGolden))
	}
	for name, c := range cases {
		want, ok := g3sGolden[name]
		if !ok {
			t.Errorf("%s: no recorded row", name)
			continue
		}
		if got := c.observe(); got != want {
			t.Errorf("%s changed:\n  was: %s\n  now: %s", name, want, got)
		}
	}
}

// g3sGolden is the table recorded against the code as it stood (develop 3cd7a57c), one string per situation.
var g3sGolden = map[string]string{
	"replay_two_adjacent_streams_the_second_one_final_chunk":                   "res=[0x4e0000000000 0x1a0000000000] wrote=[\"[{\\\"i\\\":0,\\\"c\\\":\\\"a\\\",\\\"f\\\":false},{\\\"i\\\":1,\\\"c\\\":\\\"b\\\",\\\"f\\\":false},{\\\"i\\\":2,\\\"c\\\":\\\"c\\\",\\\"f\\\":true}]\" \"[{\\\"i\\\":0,\\\"c\\\":\\\"x\\\",\\\"f\\\":true}]\"] isReplay=true step=4 recorded=[] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
	"replay_single_non_final_chunk_without_the_data_flag":                      "res=[0x1e0000000000] wrote=[\"[{\\\"i\\\":0,\\\"c\\\":\\\"half\\\",\\\"f\\\":false}]\"] isReplay=false step=1 recorded=[] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
	"replay_an_empty_stream_then_a_stream_of_another_function_is_a_divergence": "res=[0x1390000000001 0x4e0000000000] wrote=[\"replay divergence at step 0: plugin_call_streaming asked for something other than the plugin_call_stream_chunk recorded there: plugin stream function p/empty, recorded p/three.\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" \"[{\\\"i\\\":0,\\\"c\\\":\\\"a\\\",\\\"f\\\":false},{\\\"i\\\":1,\\\"c\\\":\\\"b\\\",\\\"f\\\":false},{\\\"i\\\":2,\\\"c\\\":\\\"c\\\",\\\"f\\\":true}]\"] isReplay=true step=3 recorded=[] ran=[] published=[] suspension=none replayStepCount+=0 series=[\"cleat_replay_failures_total{worker_id=\\\"\\\"} 1\"]",
	"replay_foreign_record_is_a_divergence":                                    "res=[0xd20000000001] wrote=[\"replay divergence at step 0: expected plugin_call_stream_chunk, got call.\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\"] isReplay=true step=0 recorded=[] ran=[] published=[] suspension=none replayStepCount+=0 series=[\"cleat_replay_failures_total{worker_id=\\\"\\\"} 1\"]",
	"replay_other_function_is_a_divergence":                                    "res=[0x1390000000001] wrote=[\"replay divergence at step 0: plugin_call_streaming asked for something other than the plugin_call_stream_chunk recorded there: plugin stream function p/three, recorded p/empty.\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\"] isReplay=true step=0 recorded=[] ran=[] published=[] suspension=none replayStepCount+=0 series=[\"cleat_replay_failures_total{worker_id=\\\"\\\"} 1\"]",
	"replay_other_plugin_is_a_divergence":                                      "res=[0x1390000000001] wrote=[\"replay divergence at step 0: plugin_call_streaming asked for something other than the plugin_call_stream_chunk recorded there: plugin stream function p/three, recorded q/three.\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\"] isReplay=true step=0 recorded=[] ran=[] published=[] suspension=none replayStepCount+=0 series=[\"cleat_replay_failures_total{worker_id=\\\"\\\"} 1\"]",
	"replay_two_adjacent_streams_are_two_answers":                              "res=[0x4e0000000000 0x340000000000] wrote=[\"[{\\\"i\\\":0,\\\"c\\\":\\\"a\\\",\\\"f\\\":false},{\\\"i\\\":1,\\\"c\\\":\\\"b\\\",\\\"f\\\":false},{\\\"i\\\":2,\\\"c\\\":\\\"c\\\",\\\"f\\\":true}]\" \"[{\\\"i\\\":0,\\\"c\\\":\\\"x\\\",\\\"f\\\":false},{\\\"i\\\":1,\\\"c\\\":\\\"y\\\",\\\"f\\\":true}]\"] isReplay=true step=5 recorded=[] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
	"defer_phase_fresh":                  "res=[0x80000000] wrote=[\"\"] isReplay=false step=0 recorded=[] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
	"fresh_call_guard_refuses":           "res=[0x710000000201] wrote=[\"plugin \\\"other\\\" is not allowed to call plugin \\\"p\\\". Add \\\"p\\\" to the caller's call_plugin capability in its manifest.\"] isReplay=false step=1 recorded=[plugin_call_stream_chunk/step=0/p.three/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"plugin \\\"other\\\" is not allowed to call plugin \\\"p\\\". Add \\\"p\\\" to the caller's call_plugin capability in its manifest.\"/i=0/fin=true/data=false/code=2] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
	"fresh_cancelled_mid_stream":         "res=[0x1b0000000000] wrote=[\"[{\\\"i\\\":0,\\\"c\\\":\\\"a\\\",\\\"f\\\":false}]\"] isReplay=false step=1 recorded=[plugin_call_stream_chunk/step=0/p.cancel_mid/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"a\"/i=0/fin=false/data=true/code=0] ran=[cancel_mid:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] published=[s0/i0/fin=false/dur=false] suspension=none replayStepCount+=0 series=[]",
	"fresh_empty_stream":                 "res=[0x40000000000] wrote=[\"null\"] isReplay=false step=0 recorded=[] ran=[empty:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] published=[] suspension=none replayStepCount+=0 series=[]",
	"fresh_empty_then_a_stream":          "res=[0x40000000000 0x4e0000000000] wrote=[\"null\" \"[{\\\"i\\\":0,\\\"c\\\":\\\"a\\\",\\\"f\\\":false},{\\\"i\\\":1,\\\"c\\\":\\\"b\\\",\\\"f\\\":false},{\\\"i\\\":2,\\\"c\\\":\\\"c\\\",\\\"f\\\":true}]\"] isReplay=false step=3 recorded=[plugin_call_stream_chunk/step=0/p.three/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"a\"/i=0/fin=false/data=true/code=0 plugin_call_stream_chunk/step=1/p.three/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"b\"/i=1/fin=false/data=true/code=0 plugin_call_stream_chunk/step=2/p.three/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"c\"/i=2/fin=true/data=true/code=0] ran=[empty:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"]) three:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] published=[s0/i0/fin=false/dur=false s1/i1/fin=false/dur=false s2/i2/fin=true/dur=false] suspension=none replayStepCount+=0 series=[]",
	"fresh_function_panics":              "res=[0x3b0000000201] wrote=[\"plugin_call_streaming p/panics: plugin \\\"p\\\" panicked: kaboom\"] isReplay=false step=1 recorded=[plugin_call_stream_chunk/step=0/p.panics/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"plugin_call_streaming p/panics: plugin \\\"p\\\" panicked: kaboom\"/i=0/fin=true/data=false/code=2] ran=[panics:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] published=[] suspension=none replayStepCount+=0 series=[]",
	"fresh_no_finish_chunk":              "res=[0x350000000000] wrote=[\"[{\\\"i\\\":0,\\\"c\\\":\\\"a\\\",\\\"f\\\":false},{\\\"i\\\":1,\\\"c\\\":\\\"b\\\",\\\"f\\\":false}]\"] isReplay=false step=2 recorded=[plugin_call_stream_chunk/step=0/p.no_finish/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"a\"/i=0/fin=false/data=true/code=0 plugin_call_stream_chunk/step=1/p.no_finish/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"b\"/i=1/fin=false/data=true/code=0] ran=[no_finish:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] published=[s0/i0/fin=false/dur=false s1/i1/fin=false/dur=false] suspension=none replayStepCount+=0 series=[]",
	"fresh_no_registry":                  "res=[0x3b0000000001] wrote=[\"plugin_call_streaming: no plugin stream registry configured\"] isReplay=false step=1 recorded=[plugin_call_stream_chunk/step=0/p.three/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"plugin_call_streaming: no plugin stream registry configured\"/i=0/fin=true/data=false/code=0] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
	"fresh_one_final_chunk":              "res=[0x1d0000000000] wrote=[\"[{\\\"i\\\":0,\\\"c\\\":\\\"only\\\",\\\"f\\\":true}]\"] isReplay=false step=1 recorded=[plugin_call_stream_chunk/step=0/p.one_final/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"only\"/i=0/fin=true/data=true/code=0] ran=[one_final:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] published=[s0/i0/fin=true/dur=false] suspension=none replayStepCount+=0 series=[]",
	"fresh_output_too_large":             "res=[0x80000000707] wrote=[\"[{\\\"i\\\":0,\"] isReplay=false step=3 recorded=[plugin_call_stream_chunk/step=0/p.three/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"a\"/i=0/fin=false/data=true/code=0 plugin_call_stream_chunk/step=1/p.three/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"b\"/i=1/fin=false/data=true/code=0 plugin_call_stream_chunk/step=2/p.three/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"c\"/i=2/fin=true/data=true/code=0] ran=[three:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] published=[s0/i0/fin=false/dur=false s1/i1/fin=false/dur=false s2/i2/fin=true/dur=false] suspension=none replayStepCount+=0 series=[]",
	"fresh_secret_only_literal":          "res=[0x1120000000201] wrote=[\"plugin stream function p/secret: field \\\"token\\\" is declared secret-only and must be exactly a ${secret:NAME} reference, written with no JSON escaping inside it -- a literal value, a non-string value, or an escaped reference ResolveSecretRefs could never have found is refused\"] isReplay=false step=1 recorded=[plugin_call_stream_chunk/step=0/p.secret/in=\"{\\\"token\\\":\\\"[secret-only field, literal value refused]\\\"}\"/out=\"plugin stream function p/secret: field \\\"token\\\" is declared secret-only and must be exactly a ${secret:NAME} reference, written with no JSON escaping inside it -- a literal value, a non-string value, or an escaped reference ResolveSecretRefs could never have found is refused\"/i=0/fin=true/data=false/code=2] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
	"fresh_secret_only_malformed":        "res=[0x6a0000000201] wrote=[\"plugin stream function p/secret: input is not a JSON object, and this function declares secret-only fields\"] isReplay=false step=1 recorded=[plugin_call_stream_chunk/step=0/p.secret/in=\"[secret-only field, literal value refused]\"/out=\"plugin stream function p/secret: input is not a JSON object, and this function declares secret-only fields\"/i=0/fin=true/data=false/code=2] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
	"fresh_secret_only_reference":        "res=[0x1a0000000000] wrote=[\"[{\\\"i\\\":0,\\\"c\\\":\\\"s\\\",\\\"f\\\":true}]\"] isReplay=false step=1 recorded=[plugin_call_stream_chunk/step=0/p.secret/in=\"{\\\"token\\\":\\\"${secret:abc}\\\"}\"/out=\"s\"/i=0/fin=true/data=true/code=0] ran=[secret:1(in=[\"{\\\"token\\\":\\\"${secret:abc}\\\"}\"])] published=[s0/i0/fin=true/dur=false] suspension=none replayStepCount+=0 series=[]",
	"fresh_start_fails":                  "res=[0x310000000201] wrote=[\"plugin_call_streaming p/start_fails: cannot start\"] isReplay=false step=1 recorded=[plugin_call_stream_chunk/step=0/p.start_fails/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"plugin_call_streaming p/start_fails: cannot start\"/i=0/fin=true/data=false/code=2] ran=[start_fails:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] published=[] suspension=none replayStepCount+=0 series=[]",
	"fresh_three_chunks":                 "res=[0x4e0000000000] wrote=[\"[{\\\"i\\\":0,\\\"c\\\":\\\"a\\\",\\\"f\\\":false},{\\\"i\\\":1,\\\"c\\\":\\\"b\\\",\\\"f\\\":false},{\\\"i\\\":2,\\\"c\\\":\\\"c\\\",\\\"f\\\":true}]\"] isReplay=false step=3 recorded=[plugin_call_stream_chunk/step=0/p.three/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"a\"/i=0/fin=false/data=true/code=0 plugin_call_stream_chunk/step=1/p.three/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"b\"/i=1/fin=false/data=true/code=0 plugin_call_stream_chunk/step=2/p.three/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"c\"/i=2/fin=true/data=true/code=0] ran=[three:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] published=[s0/i0/fin=false/dur=false s1/i1/fin=false/dur=false s2/i2/fin=true/dur=false] suspension=none replayStepCount+=0 series=[]",
	"fresh_two_calls":                    "res=[0x4e0000000000 0x4e0000000000] wrote=[\"[{\\\"i\\\":0,\\\"c\\\":\\\"a\\\",\\\"f\\\":false},{\\\"i\\\":1,\\\"c\\\":\\\"b\\\",\\\"f\\\":false},{\\\"i\\\":2,\\\"c\\\":\\\"c\\\",\\\"f\\\":true}]\" \"[{\\\"i\\\":0,\\\"c\\\":\\\"a\\\",\\\"f\\\":false},{\\\"i\\\":1,\\\"c\\\":\\\"b\\\",\\\"f\\\":false},{\\\"i\\\":2,\\\"c\\\":\\\"c\\\",\\\"f\\\":true}]\"] isReplay=false step=6 recorded=[plugin_call_stream_chunk/step=0/p.three/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"a\"/i=0/fin=false/data=true/code=0 plugin_call_stream_chunk/step=1/p.three/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"b\"/i=1/fin=false/data=true/code=0 plugin_call_stream_chunk/step=2/p.three/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"c\"/i=2/fin=true/data=true/code=0 plugin_call_stream_chunk/step=3/p.three/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"a\"/i=0/fin=false/data=true/code=0 plugin_call_stream_chunk/step=4/p.three/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"b\"/i=1/fin=false/data=true/code=0 plugin_call_stream_chunk/step=5/p.three/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"c\"/i=2/fin=true/data=true/code=0] ran=[three:2(in=[\"{\\\"key\\\":\\\"val\\\"}\" \"{\\\"key\\\":\\\"val\\\"}\"])] published=[s0/i0/fin=false/dur=false s1/i1/fin=false/dur=false s2/i2/fin=true/dur=false s3/i0/fin=false/dur=false s4/i1/fin=false/dur=false s5/i2/fin=true/dur=false] suspension=none replayStepCount+=0 series=[]",
	"fresh_unregistered":                 "res=[0x850000000201] wrote=[\"plugin stream function p/nope not registered. Check that the plugin is deployed and its version satisfies the workflow's plugin_deps.\"] isReplay=false step=1 recorded=[plugin_call_stream_chunk/step=0/p.nope/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"plugin stream function p/nope not registered. Check that the plugin is deployed and its version satisfies the workflow's plugin_deps.\"/i=0/fin=true/data=false/code=2] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
	"replay_chunks_then_another_call":    "res=[0x4e0000000000] wrote=[\"[{\\\"i\\\":0,\\\"c\\\":\\\"a\\\",\\\"f\\\":false},{\\\"i\\\":1,\\\"c\\\":\\\"b\\\",\\\"f\\\":false},{\\\"i\\\":2,\\\"c\\\":\\\"c\\\",\\\"f\\\":true}]\"] isReplay=true step=3 recorded=[] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
	"replay_frontier_empty_history":      "res=[0x4e0000000000] wrote=[\"[{\\\"i\\\":0,\\\"c\\\":\\\"a\\\",\\\"f\\\":false},{\\\"i\\\":1,\\\"c\\\":\\\"b\\\",\\\"f\\\":false},{\\\"i\\\":2,\\\"c\\\":\\\"c\\\",\\\"f\\\":true}]\"] isReplay=false step=3 recorded=[plugin_call_stream_chunk/step=0/p.three/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"a\"/i=0/fin=false/data=true/code=0 plugin_call_stream_chunk/step=1/p.three/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"b\"/i=1/fin=false/data=true/code=0 plugin_call_stream_chunk/step=2/p.three/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"c\"/i=2/fin=true/data=true/code=0] ran=[three:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] published=[s0/i0/fin=false/dur=false s1/i1/fin=false/dur=false s2/i2/fin=true/dur=false] suspension=none replayStepCount+=0 series=[]",
	"replay_no_finish":                   "res=[0x350000000000] wrote=[\"[{\\\"i\\\":0,\\\"c\\\":\\\"a\\\",\\\"f\\\":false},{\\\"i\\\":1,\\\"c\\\":\\\"b\\\",\\\"f\\\":false}]\"] isReplay=false step=2 recorded=[] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
	"replay_one_final_data_chunk":        "res=[0x1d0000000000] wrote=[\"[{\\\"i\\\":0,\\\"c\\\":\\\"only\\\",\\\"f\\\":true}]\"] isReplay=true step=1 recorded=[] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
	"replay_one_then_the_frontier":       "res=[0x4e0000000000 0x4e0000000000] wrote=[\"[{\\\"i\\\":0,\\\"c\\\":\\\"a\\\",\\\"f\\\":false},{\\\"i\\\":1,\\\"c\\\":\\\"b\\\",\\\"f\\\":false},{\\\"i\\\":2,\\\"c\\\":\\\"c\\\",\\\"f\\\":true}]\" \"[{\\\"i\\\":0,\\\"c\\\":\\\"a\\\",\\\"f\\\":false},{\\\"i\\\":1,\\\"c\\\":\\\"b\\\",\\\"f\\\":false},{\\\"i\\\":2,\\\"c\\\":\\\"c\\\",\\\"f\\\":true}]\"] isReplay=false step=6 recorded=[plugin_call_stream_chunk/step=3/p.three/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"a\"/i=0/fin=false/data=true/code=0 plugin_call_stream_chunk/step=4/p.three/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"b\"/i=1/fin=false/data=true/code=0 plugin_call_stream_chunk/step=5/p.three/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"c\"/i=2/fin=true/data=true/code=0] ran=[three:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] published=[s3/i0/fin=false/dur=false s4/i1/fin=false/dur=false s5/i2/fin=true/dur=false] suspension=none replayStepCount+=0 series=[]",
	"replay_output_too_large":            "res=[0x80000000707] wrote=[\"[{\\\"i\\\":0,\"] isReplay=true step=3 recorded=[] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
	"replay_quit":                        "res=[0x0] wrote=[\"\"] isReplay=true step=1 recorded=[] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
	"replay_single_failure_failure_code": "res=[0x100000000201] wrote=[\"recorded failure\"] isReplay=true step=1 recorded=[] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
	"replay_single_failure_unknown":      "res=[0x100000000001] wrote=[\"recorded failure\"] isReplay=true step=1 recorded=[] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
	"replay_three_chunks":                "res=[0x4e0000000000] wrote=[\"[{\\\"i\\\":0,\\\"c\\\":\\\"a\\\",\\\"f\\\":false},{\\\"i\\\":1,\\\"c\\\":\\\"b\\\",\\\"f\\\":false},{\\\"i\\\":2,\\\"c\\\":\\\"c\\\",\\\"f\\\":true}]\"] isReplay=true step=3 recorded=[] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
	"stopping_at_the_frontier":           "res=[0x80000000] wrote=[\"\"] isReplay=false step=0 recorded=[] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
	"stopping_fresh":                     "res=[0x80000000] wrote=[\"\"] isReplay=false step=0 recorded=[] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
	"suspended_at_the_frontier":          "res=[0x80000000] wrote=[\"\"] isReplay=false step=0 recorded=[] ran=[] published=[] suspension=earlier replayStepCount+=0 series=[]",
	"suspended_fresh":                    "res=[0x80000000] wrote=[\"\"] isReplay=false step=0 recorded=[] ran=[] published=[] suspension=earlier replayStepCount+=0 series=[]",
	"suspended_replay_recorded":          "res=[0x4e0000000000] wrote=[\"[{\\\"i\\\":0,\\\"c\\\":\\\"a\\\",\\\"f\\\":false},{\\\"i\\\":1,\\\"c\\\":\\\"b\\\",\\\"f\\\":false},{\\\"i\\\":2,\\\"c\\\":\\\"c\\\",\\\"f\\\":true}]\"] isReplay=true step=3 recorded=[] ran=[] published=[] suspension=earlier replayStepCount+=0 series=[]",
	"KNOWN_DEFECT_replay_an_empty_stream_swallows_the_next_same_function_streams_chunks": "res=[0x4e0000000000 0x4e0000000000] wrote=[\"[{\\\"i\\\":0,\\\"c\\\":\\\"a\\\",\\\"f\\\":false},{\\\"i\\\":1,\\\"c\\\":\\\"b\\\",\\\"f\\\":false},{\\\"i\\\":2,\\\"c\\\":\\\"c\\\",\\\"f\\\":true}]\" \"[{\\\"i\\\":0,\\\"c\\\":\\\"a\\\",\\\"f\\\":false},{\\\"i\\\":1,\\\"c\\\":\\\"b\\\",\\\"f\\\":false},{\\\"i\\\":2,\\\"c\\\":\\\"c\\\",\\\"f\\\":true}]\"] isReplay=false step=6 recorded=[plugin_call_stream_chunk/step=3/p.again/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"a\"/i=0/fin=false/data=true/code=0 plugin_call_stream_chunk/step=4/p.again/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"b\"/i=1/fin=false/data=true/code=0 plugin_call_stream_chunk/step=5/p.again/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"c\"/i=2/fin=true/data=true/code=0] ran=[again:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] published=[s3/i0/fin=false/dur=false s4/i1/fin=false/dur=false s5/i2/fin=true/dur=false] suspension=none replayStepCount+=0 series=[]",
	"replay_unfinished_chunks_then_another_call":                                         "res=[0x350000000000] wrote=[\"[{\\\"i\\\":0,\\\"c\\\":\\\"a\\\",\\\"f\\\":false},{\\\"i\\\":1,\\\"c\\\":\\\"b\\\",\\\"f\\\":false}]\"] isReplay=true step=2 recorded=[] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
	"replay_a_final_chunk_that_restarts_at_index_zero":                                   "res=[0x340000000000] wrote=[\"[{\\\"i\\\":0,\\\"c\\\":\\\"a\\\",\\\"f\\\":false},{\\\"i\\\":0,\\\"c\\\":\\\"b\\\",\\\"f\\\":true}]\"] isReplay=true step=2 recorded=[] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
	"replay_a_secret_functions_recorded_failure_with_the_live_input":                     "res=[0x1120000000201] wrote=[\"plugin stream function p/secret: field \\\"token\\\" is declared secret-only and must be exactly a ${secret:NAME} reference, written with no JSON escaping inside it -- a literal value, a non-string value, or an escaped reference ResolveSecretRefs could never have found is refused\"] isReplay=true step=1 recorded=[] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
	"replay_three_chunks_under_a_different_live_input":                                   "res=[0x4e0000000000] wrote=[\"[{\\\"i\\\":0,\\\"c\\\":\\\"a\\\",\\\"f\\\":false},{\\\"i\\\":1,\\\"c\\\":\\\"b\\\",\\\"f\\\":false},{\\\"i\\\":2,\\\"c\\\":\\\"c\\\",\\\"f\\\":true}]\"] isReplay=true step=3 recorded=[] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
	"stopping_with_history_ending_inside_a_stream":                                       "res=[0x80000000] wrote=[\"\"] isReplay=false step=2 recorded=[] ran=[] published=[] suspension=none replayStepCount+=0 series=[]",
}

var g3sRowShape = regexp.MustCompile(`res=\[([^\]]*)\] wrote=\[((?:"(?:[^"\\]|\\.)*"|\s)*)\] isReplay=`)
var g3sQuoted = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// g3sRowProblem reads a row's result words the way the GUEST does (decodeDurableCallResult) and requires each decoded length to equal the bytes the row says that call
// wrote. It returns "" for a row that agrees, "UNMEASURED: ..." for one it cannot read (never a pass), or what disagrees. A stop word writes nothing and decodes to 0.
func g3sRowProblem(row string) string {
	m := g3sRowShape.FindStringSubmatch(row)
	if m == nil {
		return "UNMEASURED: the row is not readable as res=[...] wrote=[...]: " + row[:min(len(row), 80)]
	}
	words := strings.Fields(m[1])
	wrote := g3sQuoted.FindAllString(m[2], -1)
	if len(words) != len(wrote) {
		return fmt.Sprintf("UNMEASURED: %d result words but %d written texts", len(words), len(wrote))
	}
	for i, w := range words {
		word, err := strconv.ParseUint(strings.TrimPrefix(w, "0x"), 16, 64)
		if err != nil {
			return "UNMEASURED: the result word " + w + " does not parse"
		}
		text, err := strconv.Unquote(wrote[i])
		if err != nil {
			return "UNMEASURED: the written text does not unquote: " + wrote[i]
		}
		if length, _, _ := decodeDurableCallResult(int64(word)); int(length) != len(text) {
			return fmt.Sprintf("call %d: the guest's durable-call decoder reads a length of %d from %s, but the row says %d bytes were written", i+1, length, w, len(text))
		}
	}
	return ""
}

func TestG3sGoldenRowsDecodeInTheDurableCallLayout(t *testing.T) {
	withMessage := 0
	for name, golden := range g3sGolden {
		if p := g3sRowProblem(golden); p != "" {
			t.Errorf("%s: %s", name, p)
		}
		if !strings.Contains(golden, `wrote=[""]`) {
			withMessage++
		}
	}
	if withMessage < 15 {
		t.Fatalf("PRECONDITION FAILED: only %d of %d rows carry a message; the check reads too little to mean anything", withMessage, len(g3sGolden))
	}
}

// CONTROL: the row check refuses a row in the SIMPLE layout, refuses an unreadable row as UNMEASURED and accepts the same message in the durable-call layout.
func TestG3sRowCheckRefusesARowInTheWrongLayoutAndAcceptsTheRightOne(t *testing.T) {
	msg := "plugin_call_streaming: no plugin stream registry configured"
	durable := fmt.Sprintf("res=[%#x] wrote=[%q] isReplay=false", packDurableCallResult(len(msg), callErrorUnknown, 1), msg)
	simple := fmt.Sprintf("res=[%#x] wrote=[%q] isReplay=false", packSimpleResult(1, uint32(len(msg))), msg)
	if p := g3sRowProblem(durable); p != "" {
		t.Errorf("a row in the durable-call layout was refused: %s", p)
	}
	if p := g3sRowProblem(simple); p == "" || strings.HasPrefix(p, "UNMEASURED") {
		t.Errorf("a row in the simple layout was %q, want a disagreement", p)
	}
	if p := g3sRowProblem("not a row"); !strings.HasPrefix(p, "UNMEASURED") {
		t.Errorf("an unreadable row was %q, want UNMEASURED", p)
	}
}
