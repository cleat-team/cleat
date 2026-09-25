package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// This file is cleat#2043's behavioural evidence: SecretOnlyFields, declared
// on a plugin function's registration, must refuse a literal value before the
// function is invoked and before anything resembling the literal reaches
// event_history -- across a fresh call, a replay of a previously-refused
// call, and a replay of history recorded under an OLDER, more permissive
// registration. Design reviewed twice by cleat-review; see the issue for the
// two rounds this settled.
//
// checkSecretOnlyFields and redactSecretOnlyFields are exercised here only
// through the dispatch chokepoint (freshPluginCallInternal,
// freshPluginCallStreaming, replayPluginCall), deliberately -- proving they
// are reached on every path a real workflow can take is the property the
// design's two review rounds were about, and it is a different property from
// "the two functions classify correctly in isolation."

// secretOnlyProbeFn is a plugin.PluginFunc that records whether it was
// called and what it saw, for asserting a NEGATIVE ("the plugin was never
// reached") without that negative being satisfied by a harness that never
// dispatches at all. Every test below pairs it with at least one call this
// registry is known to reach live (either the same function on a differently
// shaped input, or the CONTROL below), so a harness bug that skips dispatch
// entirely cannot pass silently.
type secretOnlyProbeFn struct {
	calls int
	saw   []string
}

func (p *secretOnlyProbeFn) fn(_ context.Context, in string) (string, error) {
	p.calls++
	p.saw = append(p.saw, in)
	return `{"ok":true}`, nil
}

// unpackCallResult mirrors the decode TestAnIdempotentRegistrationAloneDoesNotReInvokeOnReplay
// uses: bits 0-7 are errCode, the next byte is callErrorCode. Good enough to
// distinguish success (0, 0) from a refusal (0, callFailureCode).
func unpackCallResult(r int64) (errCode, callErrorCode byte) {
	return byte(r & 0xFF), byte((r >> 8) & 0xFF)
}

// buf256 is a fixed guest output buffer plus the context it must be wired
// through, bundled so call sites do not repeat the plumbing.
type buf256 struct{ mem [256]byte }

func (b *buf256) ctx(ctx context.Context) context.Context {
	return contextWithRawMemBuf(ctx, b.mem[:])
}

// ---------------------------------------------------------------------------
// 1. A literal is refused, and no row reaching event_history holds it.
//
// This is the property #2023 explicitly could not deliver (it only saw the
// plugin's SIDE of a call, after ${secret:NAME} substitution had already
// happened or failed) and the reason #2043 exists at all. Real database,
// every registered dialect: what LoadEventHistory returns is what a second
// worker, or an operator reading the table by hand, would see.
// ---------------------------------------------------------------------------

func TestASecretOnlyFieldRefusesALiteralAndEventHistoryNeverHoldsIt(t *testing.T) {
	const literal = "sk-do-not-persist-this-literal"

	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			wfID := newIntentWorkflow(t, ctx, store, "secret-only-literal")
			wf, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil || wf == nil {
				t.Fatalf("ClaimWorkflow: wf=%v err=%v", wf, err)
			}

			probe := &secretOnlyProbeFn{}
			reg := NewPluginRegistry()
			if err := reg.RegisterWithPolicy("llmtest", "chat", probe.fn,
				ReplayPolicy{}, []string{"api_key"}); err != nil {
				t.Fatalf("RegisterWithPolicy: %v", err)
			}

			eng := NewEngine(nil, nil,
				WithDB(rawDBOf(t, store)),
				WithWorkflowStore(store),
				WithWorkflowID(wfID),
				WithWorkerID("worker-1"),
				WithGeneration(wf.Generation),
				WithTenantID(DefaultTenantUUID),
				WithPluginRegistry(reg))

			s := &execSession{
				engine:     eng,
				workflowID: wfID,
				nowMs:      1000000,
				deferrals:  make(map[string]string),
				queryState: make(map[string]string),
			}

			b := &buf256{}
			input := `{"api_key":"` + literal + `","model":"m"}`
			res := s.PluginCall(b.ctx(ctx), nil, "llmtest", "chat", input, 0, 200)

			errCode, callErrorCode := unpackCallResult(res)
			if callErrorCode != callFailureCode {
				t.Fatalf("callErrorCode = %d, want callFailureCode (%d); errCode=%d",
					callErrorCode, callFailureCode, errCode)
			}
			if probe.calls != 0 {
				t.Fatalf("the plugin function ran with a literal api_key: saw %v", probe.saw)
			}

			hist, err := store.LoadEventHistory(ctx, wfID)
			if err != nil {
				t.Fatalf("LoadEventHistory: %v", err)
			}
			if len(hist) != 1 {
				t.Fatalf("event_history has %d rows, want 1: %v", len(hist), hist)
			}
			rec := hist[0]
			if rec.PluginError == "" {
				t.Errorf("recorded row has no PluginError; the refusal must be recorded like any "+
					"other failed call, not silently dropped: %+v", rec)
			}
			if strings.Contains(rec.PluginInput, literal) {
				t.Fatalf("event_history.plugin_input contains the literal secret:\n  %s", rec.PluginInput)
			}
			if !strings.Contains(rec.PluginInput, secretOnlyFieldRedactionMarker) {
				t.Errorf("recorded plugin_input does not carry the redaction marker: %s", rec.PluginInput)
			}

			// CONTROL, in the same run: prove this harness dispatches to the
			// plugin at all, on the SAME registration, so "probe.calls == 0"
			// above is a finding about the literal and not about a harness
			// that never reaches the plugin. A fresh execSession -- the first
			// call already advanced s.stepCount and s.history -- but that is
			// exactly what step 1 of a real run looks like.
			validInput := `{"api_key":"${secret:openai}","model":"m"}`
			res2 := s.PluginCall(b.ctx(ctx), nil, "llmtest", "chat", validInput, 0, 200)
			_, callErrorCode2 := unpackCallResult(res2)
			if callErrorCode2 != 0 {
				t.Fatalf("CONTROL: a reference-only api_key was refused too (callErrorCode=%d); "+
					"the harness may not be reaching the plugin, which would make the finding above worthless",
					callErrorCode2)
			}
			if probe.calls != 1 {
				t.Fatalf("CONTROL: the plugin never ran for a reference-only input; probe.calls=%d", probe.calls)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 2. A case-variant duplicate is refused as an ambiguity, not silently
// resolved one way or the other. cleat-review's must-fix-2: encoding/json's
// own struct decode falls back to case-insensitive field matching, so an
// exact-key check here would pass an input the plugin's own decode could
// still bind the literal half of.
// ---------------------------------------------------------------------------

func TestASecretOnlyFieldRefusesACaseVariantDuplicate(t *testing.T) {
	s := newTestExecSession()
	probe := &secretOnlyProbeFn{}
	reg := NewPluginRegistry()
	if err := reg.RegisterWithPolicy("llmtest", "chat", probe.fn,
		ReplayPolicy{}, []string{"api_key"}); err != nil {
		t.Fatalf("RegisterWithPolicy: %v", err)
	}
	s.engine.pluginRegistry = reg

	b := &buf256{}
	input := `{"api_key":"${secret:openai}","API_KEY":"sk-literal-in-the-shadow-field"}`
	res := s.PluginCall(b.ctx(context.Background()), nil, "llmtest", "chat", input, 0, 200)

	errCode, callErrorCode := unpackCallResult(res)
	if callErrorCode != callFailureCode {
		t.Fatalf("callErrorCode = %d, want callFailureCode (%d); errCode=%d", callErrorCode, callFailureCode, errCode)
	}
	if probe.calls != 0 {
		t.Fatalf("the plugin function ran despite an ambiguous case-variant duplicate: saw %v", probe.saw)
	}

	// CONTROL: the same registry, a single well-formed key, is reachable.
	res2 := s.PluginCall(b.ctx(context.Background()), nil, "llmtest", "chat",
		`{"api_key":"${secret:openai}"}`, 0, 200)
	if _, callErrorCode2 := unpackCallResult(res2); callErrorCode2 != 0 {
		t.Fatalf("CONTROL: a single well-formed api_key was refused too (callErrorCode=%d)", callErrorCode2)
	}
	if probe.calls != 1 {
		t.Fatalf("CONTROL: the plugin never ran for a single well-formed key; probe.calls=%d", probe.calls)
	}
}

// ---------------------------------------------------------------------------
// 3. Malformed (non-JSON) input is refused before it reaches the plugin's
// own decode, for a function that declared secret-only fields. checkSecretOnlyFields's
// doc comment states this is deliberate: opting into the check buys no free
// pass for input that never even parses.
// ---------------------------------------------------------------------------

func TestASecretOnlyFieldRefusesMalformedInput(t *testing.T) {
	s := newTestExecSession()
	probe := &secretOnlyProbeFn{}
	reg := NewPluginRegistry()
	if err := reg.RegisterWithPolicy("llmtest", "chat", probe.fn,
		ReplayPolicy{}, []string{"api_key"}); err != nil {
		t.Fatalf("RegisterWithPolicy: %v", err)
	}
	s.engine.pluginRegistry = reg

	b := &buf256{}
	res := s.PluginCall(b.ctx(context.Background()), nil, "llmtest", "chat", `not json at all`, 0, 200)

	if _, callErrorCode := unpackCallResult(res); callErrorCode != callFailureCode {
		t.Fatalf("callErrorCode = %d, want callFailureCode (%d)", callErrorCode, callFailureCode)
	}
	if probe.calls != 0 {
		t.Fatalf("the plugin function ran on malformed input: saw %v", probe.saw)
	}

	res2 := s.PluginCall(b.ctx(context.Background()), nil, "llmtest", "chat",
		`{"api_key":"${secret:openai}"}`, 0, 200)
	if _, callErrorCode2 := unpackCallResult(res2); callErrorCode2 != 0 {
		t.Fatalf("CONTROL: well-formed input was refused too (callErrorCode=%d)", callErrorCode2)
	}
	if probe.calls != 1 {
		t.Fatalf("CONTROL: the plugin never ran for well-formed input; probe.calls=%d", probe.calls)
	}
}

// ---------------------------------------------------------------------------
// 4. A refused call followed by a real, valid call in the same run: steps
// stay positionally correct. cleat-review's must-fix-1 -- skipping the
// EventRecord for a refused call (an earlier draft's design) would misalign
// replay's positional index the moment a workflow catches the refusal and
// makes one more recorded call. This proves the fix (record a normal, redacted
// EventRecord) keeps step numbering intact.
// ---------------------------------------------------------------------------

func TestARefusedSecretOnlyCallDoesNotMisalignSubsequentSteps(t *testing.T) {
	s := newTestExecSession()
	probe := &secretOnlyProbeFn{}
	reg := NewPluginRegistry()
	if err := reg.RegisterWithPolicy("llmtest", "chat", probe.fn,
		ReplayPolicy{}, []string{"api_key"}); err != nil {
		t.Fatalf("RegisterWithPolicy: %v", err)
	}
	s.engine.pluginRegistry = reg

	b := &buf256{}
	ctx := context.Background()

	res1 := s.PluginCall(b.ctx(ctx), nil, "llmtest", "chat", `{"api_key":"sk-literal"}`, 0, 200)
	if _, ec := unpackCallResult(res1); ec != callFailureCode {
		t.Fatalf("first call: callErrorCode = %d, want callFailureCode", ec)
	}

	res2 := s.PluginCall(b.ctx(ctx), nil, "llmtest", "chat", `{"api_key":"${secret:openai}"}`, 0, 200)
	if _, ec := unpackCallResult(res2); ec != 0 {
		t.Fatalf("second call: callErrorCode = %d, want 0 (success)", ec)
	}

	if probe.calls != 1 {
		t.Fatalf("the plugin ran %d times, want exactly 1 (only the second, valid call)", probe.calls)
	}
	// recordEvent appends to s.history directly -- there is no separate
	// "new events" slice on a fresh (non-replay) session; s.history IS the
	// growing record of this run.
	if len(s.history) != 2 {
		t.Fatalf("s.history has %d records, want 2: %+v", len(s.history), s.history)
	}
	if s.history[0].Step != 0 || s.history[1].Step != 1 {
		t.Fatalf("steps are not positionally correct: got %d, %d, want 0, 1",
			s.history[0].Step, s.history[1].Step)
	}
	if s.history[0].PluginError == "" {
		t.Errorf("step 0 (the refused call) has no PluginError recorded")
	}
	if s.history[1].PluginError != "" {
		t.Errorf("step 1 (the valid call) recorded an error: %q", s.history[1].PluginError)
	}
	if s.stepCount != 2 {
		t.Fatalf("s.stepCount = %d, want 2", s.stepCount)
	}
}

// ---------------------------------------------------------------------------
// 5. Worker restart: replay serves the recorded refusal from history without
// re-invoking the plugin live. Real database, every registered dialect --
// this is the scenario the redact-not-skip decision exists for: a worker
// dies after recording the refusal, a second worker (a fresh *Engine, a
// fresh registry, its own probe) resumes from what LoadEventHistory returns.
// ---------------------------------------------------------------------------

func TestWorkerRestartServesARecordedSecretOnlyRefusalWithoutReinvokingLive(t *testing.T) {
	const literal = "sk-must-not-be-sent-twice"

	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			wfID := newIntentWorkflow(t, ctx, store, "secret-only-restart")
			wf, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil || wf == nil {
				t.Fatalf("ClaimWorkflow: wf=%v err=%v", wf, err)
			}

			// Worker 1: makes the call, records the refusal, then "dies" --
			// nothing more happens on this engine/session.
			probe1 := &secretOnlyProbeFn{}
			reg1 := NewPluginRegistry()
			if err := reg1.RegisterWithPolicy("llmtest", "chat", probe1.fn,
				ReplayPolicy{}, []string{"api_key"}); err != nil {
				t.Fatalf("RegisterWithPolicy (worker 1): %v", err)
			}
			eng1 := NewEngine(nil, nil,
				WithDB(rawDBOf(t, store)),
				WithWorkflowStore(store),
				WithWorkflowID(wfID),
				WithWorkerID("worker-1"),
				WithGeneration(wf.Generation),
				WithTenantID(DefaultTenantUUID),
				WithPluginRegistry(reg1))
			s1 := &execSession{
				engine: eng1, workflowID: wfID, nowMs: 1000000,
				deferrals: make(map[string]string), queryState: make(map[string]string),
			}
			b1 := &buf256{}
			input := `{"api_key":"` + literal + `"}`
			res1 := s1.PluginCall(b1.ctx(ctx), nil, "llmtest", "chat", input, 0, 200)
			if _, ec := unpackCallResult(res1); ec != callFailureCode {
				t.Fatalf("worker 1: callErrorCode = %d, want callFailureCode", ec)
			}
			if probe1.calls != 0 {
				t.Fatalf("worker 1: the plugin ran with a literal api_key")
			}

			// The restart: a genuinely separate load from the database, the
			// way a resumed worker gets its history.
			hist, err := store.LoadEventHistory(ctx, wfID)
			if err != nil {
				t.Fatalf("LoadEventHistory: %v", err)
			}
			if len(hist) != 1 || hist[0].PluginError == "" {
				t.Fatalf("expected one refused row in history, got: %+v", hist)
			}

			// Worker 2: a fresh Engine, a fresh registry, a fresh probe. The
			// workflow's own code is deterministic, so it reconstructs the
			// identical literal call on replay -- that is what a real guest
			// does; nothing here special-cases the redacted record.
			probe2 := &secretOnlyProbeFn{}
			reg2 := NewPluginRegistry()
			if err := reg2.RegisterWithPolicy("llmtest", "chat", probe2.fn,
				ReplayPolicy{}, []string{"api_key"}); err != nil {
				t.Fatalf("RegisterWithPolicy (worker 2): %v", err)
			}
			eng2 := NewEngine(nil, nil,
				WithDB(rawDBOf(t, store)),
				WithWorkflowStore(store),
				WithWorkflowID(wfID),
				WithWorkerID("worker-1"), // same claim; a real restart re-claims first
				WithGeneration(wf.Generation),
				WithTenantID(DefaultTenantUUID),
				WithPluginRegistry(reg2))
			s2 := &execSession{
				engine: eng2, workflowID: wfID, nowMs: 1000000,
				deferrals: make(map[string]string), queryState: make(map[string]string),
				isReplay: true, history: hist,
			}
			b2 := &buf256{}
			res2 := s2.PluginCall(b2.ctx(ctx), nil, "llmtest", "chat", input, 0, 200)

			if probe2.calls != 0 {
				t.Fatalf("REPLAY RE-INVOKED THE PLUGIN LIVE with the literal secret; "+
					"a worker restart must serve the recorded refusal, not dispatch again: saw %v", probe2.saw)
			}
			_, ec2 := unpackCallResult(res2)
			if ec2 != callFailureCode {
				t.Fatalf("worker 2 (replay): callErrorCode = %d, want callFailureCode (the recorded refusal)", ec2)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 6. The WithHistory re-invoke path (replayPluginCall's `if rec.Idempotent`
// branch) re-runs the secret-only check rather than trusting the old
// record's success. cleat-review's second-round finding: this branch is
// driven by the STORED record's Idempotent/SameValueOnReplay flags, not by
// the CURRENT registration -- so history written before a function declared
// SecretOnlyFields (when it may have been registered Idempotent +
// SameValueOnReplay, before RegisterWithPolicy's mutual-exclusion check
// existed to forbid that combination) can still reach this branch today.
// RegisterWithPolicy's exclusion stops any NEW registration from creating
// this combination; it cannot rewrite old rows. The runtime check is the
// backstop cleat-review asked to keep regardless, and this is what it is
// for: proving a re-invocation this branch would otherwise make is blocked
// before the plugin function runs.
//
// A synthetic registration, not RegisterWithPolicy, builds the old-shaped
// history: RegisterWithPolicy would itself refuse to create a function
// simultaneously Idempotent+SameValueOnReplay AND SecretOnlyFields, which is
// exactly the point -- this history could only exist from before that
// refusal shipped.
// ---------------------------------------------------------------------------

func TestWithHistoryReplayRerunsTheSecretOnlyCheckOnOldPermissiveHistory(t *testing.T) {
	s := newTestExecSession()
	probe := &secretOnlyProbeFn{}

	reg := NewPluginRegistry()
	// The CURRENT registration: secret-only fields declared, and (as
	// RegisterWithPolicy requires) NOT eligible for re-invocation on replay.
	if err := reg.RegisterWithPolicy("llmtest", "chat", probe.fn,
		ReplayPolicy{}, []string{"api_key"}); err != nil {
		t.Fatalf("RegisterWithPolicy: %v", err)
	}
	s.engine.pluginRegistry = reg

	// The OLD record: as if written back when this function was registered
	// Idempotent+SameValueOnReplay (legal before secret-only fields existed),
	// and the call genuinely succeeded with a literal -- exactly the
	// suspicious-in-hindsight row #2043 is about.
	const literal = "sk-old-data-from-before-the-fix"
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypePluginCall,
		PluginName: "llmtest", PluginFunc: "chat",
		PluginInput:       `{"api_key":"` + literal + `"}`,
		PluginOutput:      `{"ok":true}`,
		PluginError:       "",
		Idempotent:        true,
		SameValueOnReplay: true,
	}}

	b := &buf256{}
	res := s.PluginCall(b.ctx(context.Background()), nil, "llmtest", "chat",
		`{"api_key":"`+literal+`"}`, 0, 200)

	if probe.calls != 0 {
		t.Fatalf("the WithHistory re-invoke path called the live plugin with the old literal; "+
			"the secret-only check must block re-invocation here too: saw %v", probe.saw)
	}
	_, callErrorCode := unpackCallResult(res)
	if callErrorCode != callFailureCode {
		t.Fatalf("callErrorCode = %d, want callFailureCode -- re-invocation must be refused, "+
			"not silently fall through to the old recorded success", callErrorCode)
	}

	// CONTROL: without a secret-only violation, the very same old-shaped
	// record (Idempotent+SameValueOnReplay) DOES take the WithHistory branch
	// and calls the live function -- proving the refusal above comes from
	// checkSecretOnlyFields, not from some other reason this branch never
	// runs at all (e.g. a nil registry, a lookup miss).
	probeControl := &secretOnlyProbeFn{}
	regControl := NewPluginRegistry()
	if err := regControl.RegisterWithPolicy("llmtest", "chat", probeControl.fn,
		ReplayPolicy{}, nil); err != nil {
		t.Fatalf("RegisterWithPolicy (control): %v", err)
	}
	sControl := newTestExecSession()
	sControl.engine.pluginRegistry = regControl
	sControl.isReplay = true
	sControl.history = []EventRecord{{
		Step: 0, EventType: EventTypePluginCall,
		PluginName: "llmtest", PluginFunc: "chat",
		PluginInput: `{"api_key":"` + literal + `"}`, PluginOutput: `{"ok":true}`,
		Idempotent: true, SameValueOnReplay: true,
	}}
	bControl := &buf256{}
	sControl.PluginCall(bControl.ctx(context.Background()), nil, "llmtest", "chat",
		`{"api_key":"`+literal+`"}`, 0, 200)
	if probeControl.calls != 1 {
		t.Fatalf("CONTROL: an old Idempotent+SameValueOnReplay record with NO secret-only fields "+
			"declared did not re-invoke live (calls=%d); the WithHistory branch may not be reachable "+
			"in this harness, which would make the refusal above unproven", probeControl.calls)
	}
}

// ---------------------------------------------------------------------------
// 7. The streaming path (freshPluginCallStreaming) refuses a literal the same
// way the non-streaming path does. Not one of the design's original six --
// added because chat_stream declares SecretOnlyFields exactly as chat does,
// and streamFailure's record-then-return shape is different code from
// freshPluginCallInternal's; nothing above exercises it.
// ---------------------------------------------------------------------------

func TestASecretOnlyFieldRefusesALiteralOnTheStreamingPath(t *testing.T) {
	s := newTestExecSession()

	var streamCalls int
	psr := NewPluginStreamRegistry()
	if err := psr.RegisterStream("llmtest", plugin.FuncOptions{
		Name:             "chat_stream",
		SecretOnlyFields: []string{"api_key"},
	}, func(_ context.Context, _ string) (<-chan plugin.StreamEvent, error) {
		streamCalls++
		ch := make(chan plugin.StreamEvent, 1)
		close(ch)
		return ch, nil
	}); err != nil {
		t.Fatalf("RegisterStream: %v", err)
	}
	s.engine.pluginStreamRegistry = psr

	buf := make([]byte, 256)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	res := s.PluginCallStreaming(ctx, nil, "llmtest", "chat_stream", `{"api_key":"sk-literal"}`, 0, 200)

	if _, callErrorCode := unpackCallResult(res); callErrorCode != callFailureCode {
		t.Fatalf("callErrorCode = %d, want callFailureCode (%d)", callErrorCode, callFailureCode)
	}
	if streamCalls != 0 {
		t.Fatalf("the streaming plugin function ran with a literal api_key")
	}
	if len(s.history) != 1 || s.history[0].PluginOutput == "" {
		t.Fatalf("expected one recorded stream-error row, got: %+v", s.history)
	}
	if strings.Contains(s.history[0].PluginInput, "sk-literal") {
		t.Fatalf("recorded plugin_input contains the literal secret: %s", s.history[0].PluginInput)
	}

	// CONTROL, same registry: a reference-only api_key reaches the plugin.
	res2 := s.PluginCallStreaming(ctx, nil, "llmtest", "chat_stream", `{"api_key":"${secret:openai}"}`, 0, 200)
	if _, callErrorCode2 := unpackCallResult(res2); callErrorCode2 != 0 {
		t.Fatalf("CONTROL: a reference-only api_key was refused too (callErrorCode=%d)", callErrorCode2)
	}
	if streamCalls != 1 {
		t.Fatalf("CONTROL: the streaming plugin never ran for a reference-only input; calls=%d", streamCalls)
	}
}
