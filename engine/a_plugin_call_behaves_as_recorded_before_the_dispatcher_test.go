package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/monitoring/prometheus"
)

// cleat#4152, G3: what PluginCall (plugin_call) does in each situation, pinned BEFORE the call moves onto the dispatcher, so the move reads as a diff.
//
// plugin_call is not a durable call: it records a plugin_call event with the function's output and error, and it has TWO replay behaviours, the usual one (serve
// the recorded answer) and a RE-INVOCATION one (replay discards the recorded answer and runs the function live, recording nothing, when the function's replay
// policy or the record's own flags allow it, cleat#1318). The second does not fit a record-or-decode model, which is why this table is written first.
//
// Each row is a situation and the text is what the call did: the result word, the bytes it wrote, whether the session is still replaying, the step, the events
// it recorded (type, function, input, output, error and the replay flags the record carries), how many times each registered function actually RAN and with what
// input, the suspension it set, and the replay-step and fresh-step counters and series.
//
// WHAT THIS TABLE IS, AND IS NOT. It is text recorded from the code, so it cannot say a result word is RIGHT, only that it did not move
// (TestG3iGoldenRowsDecodeInTheDurableCallLayout reads each word the way the guest does). Every row's expectation was derived by reading PluginCall,
// replayPluginCall and freshPluginCallInternal BEFORE the output was looked at.
//
// NOT COVERED, and not claimed: the flush of a recorded event to a real database (the engine here has none, so the `engine.db != nil` branch never runs), a real
// plugin's health tracker beyond the one panic row, the CallContext a function receives (which both_plugin_call_paths_carry_the_tenant_test.go owns), the secret
// reference resolution that happens before the call reaches the engine, and a real worker.

const g3iBufLen = 4096

var g3iSeries = regexp.MustCompile(`^(cleat_[a-z_]*(?:replay|fresh|plugin|calls)[a-z_]*(?:_total|_count))(\{[^}]*\})? ([0-9.e+-]+)$`)

type g3iCase struct {
	history   []EventRecord
	replaying bool
	// suspended makes the session already suspended, the way a guest that did not stop at a suspension leaves it.
	suspended  bool
	deferPhase bool
	shutdown   bool
	quit       bool
	// calls is how many PluginCalls the guest makes, in order, each of plugin p and function fn with input; 0 means one.
	calls int
	fn    string // "" means "ok"
	input string // "" means `{"key":"val"}`
	// noRegistry leaves the engine without a plugin registry.
	noRegistry bool
	// guardFrom makes the session a call FROM that plugin, under a guard that allows nothing.
	guardFrom string
	// bufLen is the guest's output buffer; 0 means g3iBufLen.
	bufLen int
}

type g3iRun struct {
	mu  map[string]int
	in  map[string][]string
	log []string
}

func g3iRegistry(run *g3iRun) *PluginRegistry {
	reg := NewPluginRegistry()
	add := func(name string, policy ReplayPolicy, secret []string, fn func(string) (string, error)) {
		if err := reg.RegisterWithPolicy("p", name, func(_ context.Context, input string) (string, error) {
			run.mu[name]++
			run.in[name] = append(run.in[name], input)
			return fn(input)
		}, policy, secret); err != nil {
			panic(err)
		}
	}
	live := func(out string) func(string) (string, error) {
		return func(string) (string, error) { return out, nil }
	}
	add("ok", ReplayPolicy{}, nil, live(`{"out":"live"}`))
	add("fails", ReplayPolicy{}, nil, func(string) (string, error) { return "", errors.New("boom") })
	add("panics", ReplayPolicy{}, nil, func(string) (string, error) { panic("kaboom") })
	add("idem", ReplayPolicy{Idempotent: true, SameValueOnReplay: true}, nil, live(`{"out":"live-idem"}`))
	add("idem_only", ReplayPolicy{Idempotent: true}, nil, live(`{"out":"live-idem-only"}`))
	add("idem_fails", ReplayPolicy{Idempotent: true, SameValueOnReplay: true}, nil, func(string) (string, error) { return "", errors.New("live boom") })
	add("secret", ReplayPolicy{}, []string{"token"}, live(`{"out":"live-secret"}`))
	add("secret_fails", ReplayPolicy{}, []string{"token"}, func(string) (string, error) { return "", errors.New("secret boom") })
	add("big", ReplayPolicy{}, nil, live(`{"out":"`+strings.Repeat("x", 200)+`"}`))
	return reg
}

func g3iEvents(history []EventRecord, from int) string {
	var parts []string
	for _, r := range history[from:] {
		parts = append(parts, fmt.Sprintf("%s/step=%d/%s.%s/in=%q/out=%q/err=%q/idem=%v/same=%v", r.EventType, r.Step, r.PluginName, r.PluginFunc, r.PluginInput, r.PluginOutput, r.PluginError, r.Idempotent, r.SameValueOnReplay))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func (c g3iCase) observe() string {
	m, err := prometheus.New(prometheus.Config{})
	if err != nil {
		return "UNMEASURED: no metrics: " + err.Error()
	}
	run := &g3iRun{mu: map[string]int{}, in: map[string][]string{}}
	opts := []EngineOption{WithWorkflowID("wf-g3i")}
	if !c.noRegistry {
		opts = append(opts, WithPluginRegistry(g3iRegistry(run)))
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
		// A caller the guard does not know is ALWAYS allowed (workflow code, Go plugins), so a refusal needs the caller listed with a target that is not "p".
		guard := NewPluginCallGuard()
		guard.Allow(c.guardFrom, []string{"some-other-plugin"})
		opts = append(opts, WithPluginCallGuard(guard))
	}
	eng := NewEngine(nil, nil, opts...)
	eng.Metrics = m
	s := &execSession{
		engine: eng, workflowID: "wf-g3i", defName: "def-g3i", nowMs: time.Now().UnixMilli(), originalInput: `{}`,
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
	if n == 0 {
		n = 1
	}
	fn, input, bufLen := c.fn, c.input, c.bufLen
	if fn == "" {
		fn = "ok"
	}
	if input == "" {
		input = `{"key":"val"}`
	}
	if bufLen == 0 {
		bufLen = g3iBufLen
	}
	before := len(s.history)
	replayBefore := ReplayStepCount()
	var words, wrote []string
	var panicked string
	func() {
		// A row that panics is a row: the table records the defect and is not aborted by it.
		defer func() {
			if r := recover(); r != nil {
				panicked = fmt.Sprint(r)
			}
		}()
		for i := 0; i < n; i++ {
			buf := make([]byte, bufLen)
			ctx := context.WithValue(context.Background(), wasmMemBufKey{}, buf)
			res := s.PluginCall(ctx, nil, "p", fn, input, 0, uint32(bufLen))
			k := 0
			for k < len(buf) && buf[k] != 0 {
				k++
			}
			words = append(words, fmt.Sprintf("%#x", res))
			wrote = append(wrote, string(buf[:k]))
		}
	}()
	rec := httptest.NewRecorder()
	m.ServeHTTP().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	var lines []string
	for _, l := range strings.Split(rec.Body.String(), "\n") {
		if g3iSeries.MatchString(l) {
			lines = append(lines, l)
		}
	}
	sort.Strings(lines)
	var ran []string
	for name, cnt := range run.mu {
		ran = append(ran, fmt.Sprintf("%s:%d(in=%q)", name, cnt, run.in[name]))
	}
	sort.Strings(ran)
	susp := "none"
	if s.suspendErr != nil {
		susp = s.suspendErr.Reason
	}
	out := fmt.Sprintf("res=%v wrote=%q isReplay=%v step=%d recorded=%s ran=%v suspension=%s replayStepCount+=%d series=%q",
		words, wrote, s.isReplay, s.stepCount, g3iEvents(s.history, before), ran, susp, ReplayStepCount()-replayBefore, lines)
	if panicked != "" {
		out = "PANIC=" + fmt.Sprintf("%q", panicked) + " " + out
	}
	if len(s.stepViolations) > 0 {
		out += fmt.Sprintf(" SPEC-VIOLATIONS=%q", s.stepViolations)
	}
	return out
}

func g3iCases() map[string]g3iCase {
	rec := func(step int, fn, input, output, errStr string, idem, same bool) EventRecord {
		return EventRecord{Step: step, EventType: EventTypePluginCall, PluginName: "p", PluginFunc: fn, PluginInput: input, PluginOutput: output, PluginError: errStr, Idempotent: idem, SameValueOnReplay: same}
	}
	in := `{"key":"val"}`
	recorded := func(fn string) EventRecord { return rec(0, fn, in, `{"out":"recorded"}`, "", false, false) }
	return map[string]g3iCase{
		// Fresh.
		"fresh_ok":                        {},
		"fresh_function_error":            {fn: "fails"},
		"fresh_function_panics":           {fn: "panics"},
		"fresh_panic_then_the_same_again": {fn: "panics", calls: 2},
		"fresh_unregistered":              {fn: "nope"},
		"fresh_no_registry":               {noRegistry: true},
		"fresh_call_guard_refuses":        {guardFrom: "other"},
		"fresh_secret_only_reference":     {fn: "secret", input: `{"token":"${secret:abc}"}`},
		"fresh_secret_only_literal":       {fn: "secret", input: `{"token":"hunter2"}`},
		"fresh_secret_only_malformed":     {fn: "secret", input: `not json`},
		"fresh_secret_only_fails_live":    {fn: "secret_fails", input: `{"token":"${secret:abc}","other":"x"}`},
		"fresh_output_too_large":          {fn: "big", bufLen: 32},
		"fresh_two_calls":                 {calls: 2},
		// Stops and suspensions.
		"stopping_fresh":            {shutdown: true},
		"stopping_at_the_frontier":  {shutdown: true, replaying: true},
		"defer_phase_fresh":         {deferPhase: true},
		"suspended_fresh":           {suspended: true},
		"suspended_replay_recorded": {suspended: true, replaying: true, history: []EventRecord{recorded("ok")}},
		"suspended_at_the_frontier": {suspended: true, replaying: true},
		// Replay of the call's own record.
		"replay_recorded_output":           {replaying: true, history: []EventRecord{recorded("ok")}},
		"replay_recorded_error":            {replaying: true, history: []EventRecord{rec(0, "ok", in, "", "recorded boom", false, false)}},
		"replay_recorded_output_too_large": {replaying: true, bufLen: 32, history: []EventRecord{rec(0, "ok", in, `{"out":"`+strings.Repeat("y", 200)+`"}`, "", false, false)}},
		"replay_quit":                      {replaying: true, quit: true, history: []EventRecord{recorded("ok")}},
		// Divergence.
		"replay_other_event_type":                 {replaying: true, history: []EventRecord{{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Request: `{}`}}},
		"replay_other_plugin":                     {replaying: true, history: []EventRecord{{Step: 0, EventType: EventTypePluginCall, PluginName: "q", PluginFunc: "ok", PluginInput: in, PluginOutput: `{}`}}},
		"replay_other_function":                   {replaying: true, history: []EventRecord{recorded("fails")}},
		"replay_other_function_live_secret_input": {replaying: true, fn: "secret", input: `{"token":"hunter2"}`, history: []EventRecord{recorded("ok")}},
		"replay_divergence_then_a_call":           {replaying: true, calls: 2, history: []EventRecord{recorded("fails")}},
		// The frontier.
		"replay_frontier_empty_history": {replaying: true},
		"replay_one_then_the_frontier":  {replaying: true, calls: 2, history: []EventRecord{recorded("ok")}},
		// Re-invocation on replay: the record's flags, the registry's policy, and the refusals of both.
		"reinvoke_by_the_record_flags":       {replaying: true, history: []EventRecord{rec(0, "ok", in, `{"out":"recorded"}`, "", true, true)}},
		"reinvoke_by_the_registry":           {replaying: true, fn: "idem", history: []EventRecord{recorded("idem")}},
		"reinvoke_fails_live":                {replaying: true, fn: "idem_fails", history: []EventRecord{recorded("idem_fails")}},
		"idempotent_alone_does_not_reinvoke": {replaying: true, fn: "idem_only", history: []EventRecord{rec(0, "idem_only", in, `{"out":"recorded"}`, "", true, false)}},
		"a_secret_function_never_reinvokes":  {replaying: true, fn: "secret", input: `{"token":"${secret:abc}"}`, history: []EventRecord{rec(0, "secret", `{"token":"${secret:abc}"}`, `{"out":"recorded"}`, "", true, true)}},
		// KNOWN DEFECT, pinned as it is (cleat#4152, G3; found by cleat-review3 on this table's first version): the RE-INVOCATION path has neither the stop refusal nor the
		// suspension refusal. Every other way a plugin_call arrives refuses first (stopping_*, suspended_*, defer_phase_fresh), but a replayed record whose function may
		// be re-invoked runs the function LIVE while the session is stopping, after it has suspended and in a defer phase, and answers the live result. The move
		// closes it, and these rows are the ones that flip. Reachable today for blobstore.blobGet and llm.embed (both registered with both flags) and for any
		// plugin whose manifest or options declare both.
		"KNOWN_DEFECT_reinvoke_while_stopping_by_the_registry":       {shutdown: true, replaying: true, fn: "idem", history: []EventRecord{recorded("idem")}},
		"KNOWN_DEFECT_reinvoke_while_stopping_by_the_record_flags":   {shutdown: true, replaying: true, history: []EventRecord{rec(0, "ok", in, `{"out":"recorded"}`, "", true, true)}},
		"KNOWN_DEFECT_reinvoke_after_suspension_by_the_registry":     {suspended: true, replaying: true, fn: "idem", history: []EventRecord{recorded("idem")}},
		"KNOWN_DEFECT_reinvoke_after_suspension_by_the_record_flags": {suspended: true, replaying: true, history: []EventRecord{rec(0, "ok", in, `{"out":"recorded"}`, "", true, true)}},
		"KNOWN_DEFECT_reinvoke_in_a_defer_phase_by_the_registry":     {deferPhase: true, replaying: true, fn: "idem", history: []EventRecord{recorded("idem")}},
		// Not a defect, the control for the five above: a recorded answer for a function that may NOT be re-invoked is served while stopping.
		"stopping_replay_recorded":     {shutdown: true, replaying: true, history: []EventRecord{recorded("ok")}},
		"reinvoke_then_the_next_call":  {replaying: true, calls: 2, fn: "idem", history: []EventRecord{recorded("idem")}},
		"reinvoke_of_a_recorded_error": {replaying: true, fn: "idem", history: []EventRecord{rec(0, "idem", in, "", "recorded boom", false, false)}},
	}
}

func TestG3iPluginCallBehavesAsRecordedBeforeTheDispatcher(t *testing.T) {
	cases := g3iCases()
	if len(cases) != len(g3iGolden) {
		t.Fatalf("PRECONDITION: %d situations but %d recorded rows", len(cases), len(g3iGolden))
	}
	for name, c := range cases {
		want, ok := g3iGolden[name]
		if !ok {
			t.Errorf("%s: no recorded row", name)
			continue
		}
		if got := c.observe(); got != want {
			t.Errorf("%s changed:\n  was: %s\n  now: %s", name, want, got)
		}
	}
}

// g3iGolden is the table recorded against the code as it stood (develop fa959e81), one string per situation.
var g3iGolden = map[string]string{
	"KNOWN_DEFECT_reinvoke_after_suspension_by_the_record_flags": "res=[0xe0000000000] wrote=[\"{\\\"out\\\":\\\"live\\\"}\"] isReplay=true step=1 recorded=[] ran=[ok:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] suspension=earlier replayStepCount+=0 series=[\"cleat_plugin_call_duration_seconds_count{func=\\\"ok\\\",plugin=\\\"p\\\",worker_id=\\\"\\\"} 1\"]",
	"KNOWN_DEFECT_reinvoke_after_suspension_by_the_registry":     "res=[0x130000000000] wrote=[\"{\\\"out\\\":\\\"live-idem\\\"}\"] isReplay=true step=1 recorded=[] ran=[idem:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] suspension=earlier replayStepCount+=0 series=[\"cleat_plugin_call_duration_seconds_count{func=\\\"idem\\\",plugin=\\\"p\\\",worker_id=\\\"\\\"} 1\"]",
	"KNOWN_DEFECT_reinvoke_in_a_defer_phase_by_the_registry":     "res=[0x130000000000] wrote=[\"{\\\"out\\\":\\\"live-idem\\\"}\"] isReplay=true step=1 recorded=[] ran=[idem:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] suspension=none replayStepCount+=0 series=[\"cleat_plugin_call_duration_seconds_count{func=\\\"idem\\\",plugin=\\\"p\\\",worker_id=\\\"\\\"} 1\"]",
	"KNOWN_DEFECT_reinvoke_while_stopping_by_the_record_flags":   "res=[0xe0000000000] wrote=[\"{\\\"out\\\":\\\"live\\\"}\"] isReplay=true step=1 recorded=[] ran=[ok:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] suspension=none replayStepCount+=0 series=[\"cleat_plugin_call_duration_seconds_count{func=\\\"ok\\\",plugin=\\\"p\\\",worker_id=\\\"\\\"} 1\"]",
	"KNOWN_DEFECT_reinvoke_while_stopping_by_the_registry":       "res=[0x130000000000] wrote=[\"{\\\"out\\\":\\\"live-idem\\\"}\"] isReplay=true step=1 recorded=[] ran=[idem:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] suspension=none replayStepCount+=0 series=[\"cleat_plugin_call_duration_seconds_count{func=\\\"idem\\\",plugin=\\\"p\\\",worker_id=\\\"\\\"} 1\"]",
	"stopping_replay_recorded":                                   "res=[0x120000000000] wrote=[\"{\\\"out\\\":\\\"recorded\\\"}\"] isReplay=true step=1 recorded=[] ran=[] suspension=none replayStepCount+=0 series=[]",
	"a_secret_function_never_reinvokes":                          "res=[0x120000000000] wrote=[\"{\\\"out\\\":\\\"recorded\\\"}\"] isReplay=true step=1 recorded=[] ran=[] suspension=none replayStepCount+=0 series=[]",
	"defer_phase_fresh":                                          "res=[0x80000000] wrote=[\"\"] isReplay=false step=0 recorded=[] ran=[] suspension=none replayStepCount+=0 series=[]",
	"fresh_call_guard_refuses":                                   "res=[0x710000000201] wrote=[\"plugin \\\"other\\\" is not allowed to call plugin \\\"p\\\". Add \\\"p\\\" to the caller's call_plugin capability in its manifest.\"] isReplay=false step=1 recorded=[plugin_call/step=0/p.ok/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"\"/err=\"plugin \\\"other\\\" is not allowed to call plugin \\\"p\\\". Add \\\"p\\\" to the caller's call_plugin capability in its manifest.\"/idem=false/same=false] ran=[] suspension=none replayStepCount+=0 series=[]",
	"fresh_function_error":                                       "res=[0x40000000201] wrote=[\"boom\"] isReplay=false step=1 recorded=[plugin_call/step=0/p.fails/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"\"/err=\"boom\"/idem=false/same=false] ran=[fails:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] suspension=none replayStepCount+=0 series=[\"cleat_plugin_call_duration_seconds_count{func=\\\"fails\\\",plugin=\\\"p\\\",worker_id=\\\"\\\"} 1\"]",
	"fresh_function_panics":                                      "res=[0x1b0000000201] wrote=[\"plugin \\\"p\\\" panicked: kaboom\"] isReplay=false step=1 recorded=[plugin_call/step=0/p.panics/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"\"/err=\"plugin \\\"p\\\" panicked: kaboom\"/idem=false/same=false] ran=[panics:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] suspension=none replayStepCount+=0 series=[\"cleat_plugin_call_duration_seconds_count{func=\\\"panics\\\",plugin=\\\"p\\\",worker_id=\\\"\\\"} 1\"]",
	"fresh_no_registry":                                          "res=[0x9a0000000001] wrote=[\"plugin function p/ok not available: no plugin registry configured. Check that the plugin is deployed and its version satisfies the workflow's plugin_deps.\"] isReplay=false step=0 recorded=[] ran=[] suspension=none replayStepCount+=0 series=[]",
	"fresh_ok":                                                   "res=[0xe0000000000] wrote=[\"{\\\"out\\\":\\\"live\\\"}\"] isReplay=false step=1 recorded=[plugin_call/step=0/p.ok/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"{\\\"out\\\":\\\"live\\\"}\"/err=\"\"/idem=false/same=false] ran=[ok:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] suspension=none replayStepCount+=0 series=[\"cleat_plugin_call_duration_seconds_count{func=\\\"ok\\\",plugin=\\\"p\\\",worker_id=\\\"\\\"} 1\"]",
	"fresh_output_too_large":                                     "res=[0x200000000707] wrote=[\"{\\\"out\\\":\\\"xxxxxxxxxxxxxxxxxxxxxxxx\"] isReplay=false step=1 recorded=[plugin_call/step=0/p.big/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"{\\\"out\\\":\\\"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\\\"}\"/err=\"\"/idem=false/same=false] ran=[big:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] suspension=none replayStepCount+=0 series=[\"cleat_plugin_call_duration_seconds_count{func=\\\"big\\\",plugin=\\\"p\\\",worker_id=\\\"\\\"} 1\"]",
	"fresh_panic_then_the_same_again":                            "res=[0x1b0000000201 0x1b0000000201] wrote=[\"plugin \\\"p\\\" panicked: kaboom\" \"plugin \\\"p\\\" panicked: kaboom\"] isReplay=false step=2 recorded=[plugin_call/step=0/p.panics/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"\"/err=\"plugin \\\"p\\\" panicked: kaboom\"/idem=false/same=false plugin_call/step=1/p.panics/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"\"/err=\"plugin \\\"p\\\" panicked: kaboom\"/idem=false/same=false] ran=[panics:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] suspension=none replayStepCount+=0 series=[\"cleat_plugin_call_duration_seconds_count{func=\\\"panics\\\",plugin=\\\"p\\\",worker_id=\\\"\\\"} 2\"]",
	"fresh_secret_only_fails_live":                               "res=[0xb0000000201] wrote=[\"secret boom\"] isReplay=false step=1 recorded=[plugin_call/step=0/p.secret_fails/in=\"{\\\"token\\\":\\\"[secret-only field, literal value refused]\\\",\\\"other\\\":\\\"x\\\"}\"/out=\"\"/err=\"secret boom\"/idem=false/same=false] ran=[secret_fails:1(in=[\"{\\\"token\\\":\\\"${secret:abc}\\\",\\\"other\\\":\\\"x\\\"}\"])] suspension=none replayStepCount+=0 series=[\"cleat_plugin_call_duration_seconds_count{func=\\\"secret_fails\\\",plugin=\\\"p\\\",worker_id=\\\"\\\"} 1\"]",
	"fresh_secret_only_literal":                                  "res=[0x10b0000000201] wrote=[\"plugin function p/secret: field \\\"token\\\" is declared secret-only and must be exactly a ${secret:NAME} reference, written with no JSON escaping inside it -- a literal value, a non-string value, or an escaped reference ResolveSecretRefs could never have found is refused\"] isReplay=false step=1 recorded=[plugin_call/step=0/p.secret/in=\"{\\\"token\\\":\\\"[secret-only field, literal value refused]\\\"}\"/out=\"\"/err=\"plugin function p/secret: field \\\"token\\\" is declared secret-only and must be exactly a ${secret:NAME} reference, written with no JSON escaping inside it -- a literal value, a non-string value, or an escaped reference ResolveSecretRefs could never have found is refused\"/idem=false/same=false] ran=[] suspension=none replayStepCount+=0 series=[]",
	"fresh_secret_only_malformed":                                "res=[0x630000000201] wrote=[\"plugin function p/secret: input is not a JSON object, and this function declares secret-only fields\"] isReplay=false step=1 recorded=[plugin_call/step=0/p.secret/in=\"[secret-only field, literal value refused]\"/out=\"\"/err=\"plugin function p/secret: input is not a JSON object, and this function declares secret-only fields\"/idem=false/same=false] ran=[] suspension=none replayStepCount+=0 series=[]",
	"fresh_secret_only_reference":                                "res=[0x150000000000] wrote=[\"{\\\"out\\\":\\\"live-secret\\\"}\"] isReplay=false step=1 recorded=[plugin_call/step=0/p.secret/in=\"{\\\"token\\\":\\\"${secret:abc}\\\"}\"/out=\"{\\\"out\\\":\\\"live-secret\\\"}\"/err=\"\"/idem=false/same=false] ran=[secret:1(in=[\"{\\\"token\\\":\\\"${secret:abc}\\\"}\"])] suspension=none replayStepCount+=0 series=[\"cleat_plugin_call_duration_seconds_count{func=\\\"secret\\\",plugin=\\\"p\\\",worker_id=\\\"\\\"} 1\"]",
	"fresh_two_calls":                                            "res=[0xe0000000000 0xe0000000000] wrote=[\"{\\\"out\\\":\\\"live\\\"}\" \"{\\\"out\\\":\\\"live\\\"}\"] isReplay=false step=2 recorded=[plugin_call/step=0/p.ok/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"{\\\"out\\\":\\\"live\\\"}\"/err=\"\"/idem=false/same=false plugin_call/step=1/p.ok/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"{\\\"out\\\":\\\"live\\\"}\"/err=\"\"/idem=false/same=false] ran=[ok:2(in=[\"{\\\"key\\\":\\\"val\\\"}\" \"{\\\"key\\\":\\\"val\\\"}\"])] suspension=none replayStepCount+=0 series=[\"cleat_plugin_call_duration_seconds_count{func=\\\"ok\\\",plugin=\\\"p\\\",worker_id=\\\"\\\"} 2\"]",
	"fresh_unregistered":                                         "res=[0x7e0000000201] wrote=[\"plugin function p/nope not registered. Check that the plugin is deployed and its version satisfies the workflow's plugin_deps.\"] isReplay=false step=1 recorded=[plugin_call/step=0/p.nope/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"\"/err=\"plugin function p/nope not registered. Check that the plugin is deployed and its version satisfies the workflow's plugin_deps.\"/idem=false/same=false] ran=[] suspension=none replayStepCount+=0 series=[]",
	"idempotent_alone_does_not_reinvoke":                         "res=[0x120000000000] wrote=[\"{\\\"out\\\":\\\"recorded\\\"}\"] isReplay=true step=1 recorded=[] ran=[] suspension=none replayStepCount+=0 series=[]",
	"reinvoke_by_the_record_flags":                               "res=[0xe0000000000] wrote=[\"{\\\"out\\\":\\\"live\\\"}\"] isReplay=true step=1 recorded=[] ran=[ok:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] suspension=none replayStepCount+=0 series=[\"cleat_plugin_call_duration_seconds_count{func=\\\"ok\\\",plugin=\\\"p\\\",worker_id=\\\"\\\"} 1\"]",
	"reinvoke_by_the_registry":                                   "res=[0x130000000000] wrote=[\"{\\\"out\\\":\\\"live-idem\\\"}\"] isReplay=true step=1 recorded=[] ran=[idem:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] suspension=none replayStepCount+=0 series=[\"cleat_plugin_call_duration_seconds_count{func=\\\"idem\\\",plugin=\\\"p\\\",worker_id=\\\"\\\"} 1\"]",
	"reinvoke_fails_live":                                        "res=[0x90000000201] wrote=[\"live boom\"] isReplay=true step=1 recorded=[] ran=[idem_fails:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] suspension=none replayStepCount+=0 series=[\"cleat_plugin_call_duration_seconds_count{func=\\\"idem_fails\\\",plugin=\\\"p\\\",worker_id=\\\"\\\"} 1\"]",
	"reinvoke_of_a_recorded_error":                               "res=[0x130000000000] wrote=[\"{\\\"out\\\":\\\"live-idem\\\"}\"] isReplay=true step=1 recorded=[] ran=[idem:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] suspension=none replayStepCount+=0 series=[\"cleat_plugin_call_duration_seconds_count{func=\\\"idem\\\",plugin=\\\"p\\\",worker_id=\\\"\\\"} 1\"]",
	"reinvoke_then_the_next_call":                                "res=[0x130000000000 0x130000000000] wrote=[\"{\\\"out\\\":\\\"live-idem\\\"}\" \"{\\\"out\\\":\\\"live-idem\\\"}\"] isReplay=false step=2 recorded=[plugin_call/step=1/p.idem/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"{\\\"out\\\":\\\"live-idem\\\"}\"/err=\"\"/idem=true/same=true] ran=[idem:2(in=[\"{\\\"key\\\":\\\"val\\\"}\" \"{\\\"key\\\":\\\"val\\\"}\"])] suspension=none replayStepCount+=0 series=[\"cleat_plugin_call_duration_seconds_count{func=\\\"idem\\\",plugin=\\\"p\\\",worker_id=\\\"\\\"} 2\"]",
	"replay_divergence_then_a_call":                              "res=[0x1490000000001 0xe0000000000] wrote=[\"replay divergence at step 0: workflow called p/ok but history has p/fails.\\n  actual input: {\\\"key\\\":\\\"val\\\"}\\n  expected (cached) input: {\\\"key\\\":\\\"val\\\"}\\n  expected (cached) output: {\\\"out\\\":\\\"recorded\\\"}\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" \"{\\\"out\\\":\\\"live\\\"}\"] isReplay=false step=2 recorded=[plugin_call/step=1/p.ok/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"{\\\"out\\\":\\\"live\\\"}\"/err=\"\"/idem=false/same=false] ran=[ok:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] suspension=none replayStepCount+=0 series=[\"cleat_plugin_call_duration_seconds_count{func=\\\"ok\\\",plugin=\\\"p\\\",worker_id=\\\"\\\"} 1\" \"cleat_replay_failures_total{worker_id=\\\"\\\"} 1\"]",
	"replay_frontier_empty_history":                              "res=[0xe0000000000] wrote=[\"{\\\"out\\\":\\\"live\\\"}\"] isReplay=false step=1 recorded=[plugin_call/step=0/p.ok/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"{\\\"out\\\":\\\"live\\\"}\"/err=\"\"/idem=false/same=false] ran=[ok:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] suspension=none replayStepCount+=0 series=[\"cleat_plugin_call_duration_seconds_count{func=\\\"ok\\\",plugin=\\\"p\\\",worker_id=\\\"\\\"} 1\"]",
	"replay_one_then_the_frontier":                               "res=[0x120000000000 0xe0000000000] wrote=[\"{\\\"out\\\":\\\"recorded\\\"}\" \"{\\\"out\\\":\\\"live\\\"}\"] isReplay=false step=2 recorded=[plugin_call/step=1/p.ok/in=\"{\\\"key\\\":\\\"val\\\"}\"/out=\"{\\\"out\\\":\\\"live\\\"}\"/err=\"\"/idem=false/same=false] ran=[ok:1(in=[\"{\\\"key\\\":\\\"val\\\"}\"])] suspension=none replayStepCount+=0 series=[\"cleat_plugin_call_duration_seconds_count{func=\\\"ok\\\",plugin=\\\"p\\\",worker_id=\\\"\\\"} 1\"]",
	"replay_other_event_type":                                    "res=[0x1220000000001] wrote=[\"replay divergence at step 0: expected plugin_call event, got call.\\n  actual input: {\\\"key\\\":\\\"val\\\"}\\n  expected (cached) input: \\n  expected (cached) output: \\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\"] isReplay=true step=1 recorded=[] ran=[] suspension=none replayStepCount+=0 series=[\"cleat_replay_failures_total{worker_id=\\\"\\\"} 1\"]",
	"replay_other_function":                                      "res=[0x1490000000001] wrote=[\"replay divergence at step 0: workflow called p/ok but history has p/fails.\\n  actual input: {\\\"key\\\":\\\"val\\\"}\\n  expected (cached) input: {\\\"key\\\":\\\"val\\\"}\\n  expected (cached) output: {\\\"out\\\":\\\"recorded\\\"}\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\"] isReplay=true step=1 recorded=[] ran=[] suspension=none replayStepCount+=0 series=[\"cleat_replay_failures_total{worker_id=\\\"\\\"} 1\"]",
	"replay_other_function_live_secret_input":                    "res=[0x1730000000001] wrote=[\"replay divergence at step 0: workflow called p/secret but history has p/ok.\\n  actual input: {\\\"token\\\":\\\"[secret-only field, literal value refused]\\\"}\\n  expected (cached) input: {\\\"key\\\":\\\"val\\\"}\\n  expected (cached) output: {\\\"out\\\":\\\"recorded\\\"}\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\"] isReplay=true step=1 recorded=[] ran=[] suspension=none replayStepCount+=0 series=[\"cleat_replay_failures_total{worker_id=\\\"\\\"} 1\"]",
	"replay_other_plugin":                                        "res=[0x1360000000001] wrote=[\"replay divergence at step 0: workflow called p/ok but history has q/ok.\\n  actual input: {\\\"key\\\":\\\"val\\\"}\\n  expected (cached) input: {\\\"key\\\":\\\"val\\\"}\\n  expected (cached) output: {}\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\"] isReplay=true step=1 recorded=[] ran=[] suspension=none replayStepCount+=0 series=[\"cleat_replay_failures_total{worker_id=\\\"\\\"} 1\"]",
	"replay_quit":                                                "res=[0x0] wrote=[\"\"] isReplay=true step=1 recorded=[] ran=[] suspension=none replayStepCount+=0 series=[]",
	"replay_recorded_error":                                      "res=[0xd0000000201] wrote=[\"recorded boom\"] isReplay=true step=1 recorded=[] ran=[] suspension=none replayStepCount+=0 series=[]",
	"replay_recorded_output":                                     "res=[0x120000000000] wrote=[\"{\\\"out\\\":\\\"recorded\\\"}\"] isReplay=true step=1 recorded=[] ran=[] suspension=none replayStepCount+=0 series=[]",
	"replay_recorded_output_too_large":                           "res=[0x200000000707] wrote=[\"{\\\"out\\\":\\\"yyyyyyyyyyyyyyyyyyyyyyyy\"] isReplay=true step=1 recorded=[] ran=[] suspension=none replayStepCount+=0 series=[]",
	"stopping_at_the_frontier":                                   "res=[0x80000000] wrote=[\"\"] isReplay=false step=0 recorded=[] ran=[] suspension=none replayStepCount+=0 series=[]",
	"stopping_fresh":                                             "res=[0x80000000] wrote=[\"\"] isReplay=false step=0 recorded=[] ran=[] suspension=none replayStepCount+=0 series=[]",
	"suspended_at_the_frontier":                                  "res=[0x80000000] wrote=[\"\"] isReplay=false step=0 recorded=[] ran=[] suspension=earlier replayStepCount+=0 series=[]",
	"suspended_fresh":                                            "res=[0x80000000] wrote=[\"\"] isReplay=false step=0 recorded=[] ran=[] suspension=earlier replayStepCount+=0 series=[]",
	"suspended_replay_recorded":                                  "res=[0x120000000000] wrote=[\"{\\\"out\\\":\\\"recorded\\\"}\"] isReplay=true step=1 recorded=[] ran=[] suspension=earlier replayStepCount+=0 series=[]",
}

// g3iRowProblem reads a row's result words the way the GUEST does (decodeDurableCallResult) and requires each decoded length to be the number of bytes the row
// says that call wrote: true of every word only if it is in the durable-call layout. It returns "" for a row that agrees, "UNMEASURED: ..." for one it cannot
// read (never a pass), or what disagrees. A stop word (0x80000000) writes nothing and decodes to 0.
var g3iRowShape = regexp.MustCompile(`res=\[([^\]]*)\] wrote=\[((?:"(?:[^"\\]|\\.)*"|\s)*)\] isReplay=`)
var g3iQuoted = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

func g3iRowProblem(row string) string {
	m := g3iRowShape.FindStringSubmatch(row)
	if m == nil {
		return "UNMEASURED: the row is not readable as res=[...] wrote=[...]: " + row[:min(len(row), 80)]
	}
	words := strings.Fields(m[1])
	wrote := g3iQuoted.FindAllString(m[2], -1)
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

func TestG3iGoldenRowsDecodeInTheDurableCallLayout(t *testing.T) {
	withMessage := 0
	for name, golden := range g3iGolden {
		if p := g3iRowProblem(golden); p != "" {
			t.Errorf("%s: %s", name, p)
		}
		if !strings.Contains(golden, `wrote=[""]`) {
			withMessage++
		}
	}
	if withMessage < 20 {
		t.Fatalf("PRECONDITION FAILED: only %d of %d rows carry a message; the check reads too little to mean anything", withMessage, len(g3iGolden))
	}
}

// CONTROL: the row check refuses a row whose word is in the SIMPLE layout, refuses an unreadable row as UNMEASURED, and accepts the same message in the
// durable-call layout. Without it the test above could pass for any layout, or for rows it cannot read.
func TestG3iRowCheckRefusesARowInTheWrongLayoutAndAcceptsTheRightOne(t *testing.T) {
	msg := "plugin function p/ok not registered"
	durable := fmt.Sprintf("res=[%#x] wrote=[%q] isReplay=false", packDurableCallResult(len(msg), callErrorUnknown, 1), msg)
	simple := fmt.Sprintf("res=[%#x] wrote=[%q] isReplay=false", packSimpleResult(1, uint32(len(msg))), msg)
	if p := g3iRowProblem(durable); p != "" {
		t.Errorf("a row in the durable-call layout was refused: %s", p)
	}
	if p := g3iRowProblem(simple); p == "" || strings.HasPrefix(p, "UNMEASURED") {
		t.Errorf("a row in the simple layout was %q, want a disagreement", p)
	}
	if p := g3iRowProblem("not a row"); !strings.HasPrefix(p, "UNMEASURED") {
		t.Errorf("an unreadable row was %q, want UNMEASURED", p)
	}
}
