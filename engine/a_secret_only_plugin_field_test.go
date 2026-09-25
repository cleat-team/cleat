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
// branch) never re-invokes -- and never refuses live either -- for a
// function whose CURRENT registration declares secret-only fields. It falls
// through and serves the OLD record's own recorded output, exactly as if
// this branch did not exist at all.
//
// This test's own first version asserted the OPPOSITE (a live refusal), and
// that was wrong: cleat-review's second-round finding on #2329 is that
// refusing live has the same determinism problem re-invoking live does, just
// in the other direction. This branch used to decide purely from the STORED
// record's Idempotent/SameValueOnReplay flags, without consulting the
// registry first, so history written under a permissive pre-#2043
// registration -- one that genuinely succeeded with a literal, because the
// check did not exist yet -- would still take the branch today and call
// freshPluginCallWithHistory, which runs checkSecretOnlyFields against the
// CURRENT declaration and refuses it live: a run that finished successfully
// turns into a replay failure. That is exactly the "flip a run that finished
// done into one that fails on replay" determinism break
// RegisterWithPolicy's own doc comment warns SecretOnlyFields +
// MayReInvokeOnReplay's registration-time exclusion exists to prevent -- and
// a live refusal here recreates it just as surely as a live re-invocation
// would, from history the exclusion never got a chance to reject. The fix is
// to gate the branch itself on the CURRENT registration's secret-only
// declaration, ahead of consulting rec.Idempotent at all, and let a declared
// function fall through to the ordinary "serve rec.PluginOutput /
// rec.PluginError" logic below like any other non-reinvoke-eligible replay.
//
// A synthetic registration, not RegisterWithPolicy, builds the old-shaped
// history: RegisterWithPolicy would itself refuse to create a function
// simultaneously Idempotent+SameValueOnReplay AND SecretOnlyFields, which is
// exactly the point -- this history could only exist from before that
// refusal shipped.
// ---------------------------------------------------------------------------

func TestWithHistoryReplaySkipsReinvocationAndServesRecordedOutputForADeclaredFunction(t *testing.T) {
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
	const recordedOutput = `{"ok":true,"from":"history"}`
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypePluginCall,
		PluginName: "llmtest", PluginFunc: "chat",
		PluginInput:       `{"api_key":"` + literal + `"}`,
		PluginOutput:      recordedOutput,
		PluginError:       "",
		Idempotent:        true,
		SameValueOnReplay: true,
	}}

	b := &buf256{}
	res := s.PluginCall(b.ctx(context.Background()), nil, "llmtest", "chat",
		`{"api_key":"`+literal+`"}`, 0, 200)

	if probe.calls != 0 {
		t.Fatalf("the WithHistory re-invoke path called the live plugin with the old literal; "+
			"a declared function must never re-invoke here, live or otherwise: saw %v", probe.saw)
	}
	errCode, callErrorCode := unpackCallResult(res)
	if callErrorCode != 0 {
		t.Fatalf("callErrorCode = %d, want 0 (success) -- a declared function must serve the OLD "+
			"recorded output rather than refuse live and flip a done run into a replay failure; errCode=%d",
			callErrorCode, errCode)
	}
	written := int64(uint64(res) >> 40)
	if got := string(b.mem[:written]); got != recordedOutput {
		t.Fatalf("served output = %q, want the OLD recorded output %q -- success alone is not enough "+
			"evidence without probe.calls==0 ruling out a fresh call, and without this ruling out a "+
			"coincidentally-successful fresh call with a different body", got, recordedOutput)
	}

	// CONTROL: without secret-only fields declared, the very same old-shaped
	// record (Idempotent+SameValueOnReplay) DOES take the WithHistory branch
	// and calls the live function -- proving the skip above comes from the
	// current registration's SecretOnlyFields, not from some other reason
	// this branch never runs at all (e.g. a nil registry, a lookup miss).
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
		PluginInput: `{"api_key":"` + literal + `"}`, PluginOutput: recordedOutput,
		Idempotent: true, SameValueOnReplay: true,
	}}
	bControl := &buf256{}
	sControl.PluginCall(bControl.ctx(context.Background()), nil, "llmtest", "chat",
		`{"api_key":"`+literal+`"}`, 0, 200)
	if probeControl.calls != 1 {
		t.Fatalf("CONTROL: an old Idempotent+SameValueOnReplay record with NO secret-only fields "+
			"declared did not re-invoke live (calls=%d); the WithHistory branch may not be reachable "+
			"in this harness, which would make the skip above unproven", probeControl.calls)
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

// ---------------------------------------------------------------------------
// 8. An EXACT duplicate key -- not a case-variant, the identical spelling
// twice -- is refused too, and the literal never reaches recordedInput.
// cleat-review's MUST-FIX on #2329 (commit 7a94d970): checkSecretOnlyFields
// used to unmarshal inputJSON into a map[string]json.RawMessage, and
// encoding/json's map decode silently keeps only the LAST value for an
// EXACT-duplicate key -- so {"api_key":"LIT5","api_key":"${secret:X}"}
// decoded to a ONE-entry map holding just the well-formed reference, and the
// literal was still in the raw inputJSON that went on to be recorded to
// event_history on all three dialects. Test 2 above does not cover this: it
// is a DIFFERENT-spelling duplicate (api_key/API_KEY), and both spellings
// survive map unmarshaling as two distinct map keys -- only an EXACT
// spelling match collapses. The fix walks inputJSON's top-level keys with a
// json.Decoder token stream instead of unmarshaling into a map, so every
// occurrence is seen before anything can collapse them.
// ---------------------------------------------------------------------------

func TestASecretOnlyFieldRefusesAnExactDuplicateKey(t *testing.T) {
	const literal = "LIT5"

	t.Run("literal first, reference second", func(t *testing.T) {
		s := newTestExecSession()
		probe := &secretOnlyProbeFn{}
		reg := NewPluginRegistry()
		if err := reg.RegisterWithPolicy("llmtest", "chat", probe.fn,
			ReplayPolicy{}, []string{"api_key"}); err != nil {
			t.Fatalf("RegisterWithPolicy: %v", err)
		}
		s.engine.pluginRegistry = reg

		b := &buf256{}
		// Exactly cleat-review's reported input: a last-value-wins map decode
		// would have kept only the (harmless-looking) reference here.
		input := `{"api_key":"` + literal + `","api_key":"${secret:X}"}`
		res := s.PluginCall(b.ctx(context.Background()), nil, "llmtest", "chat", input, 0, 200)

		errCode, callErrorCode := unpackCallResult(res)
		if callErrorCode != callFailureCode {
			t.Fatalf("callErrorCode = %d, want callFailureCode (%d); errCode=%d -- an exact-duplicate "+
				"key was accepted, and the literal may have reached storage", callErrorCode, callFailureCode, errCode)
		}
		if probe.calls != 0 {
			t.Fatalf("the plugin function ran despite an exact-duplicate api_key: saw %v", probe.saw)
		}
		if len(s.history) != 1 {
			t.Fatalf("s.history has %d records, want 1: %+v", len(s.history), s.history)
		}
		if strings.Contains(s.history[0].PluginInput, literal) {
			t.Fatalf("event_history.plugin_input contains the literal from the duplicate key: %s",
				s.history[0].PluginInput)
		}
		if !strings.Contains(s.history[0].PluginInput, secretOnlyFieldRedactionMarker) {
			t.Errorf("recorded plugin_input does not carry the redaction marker: %s", s.history[0].PluginInput)
		}
	})

	t.Run("reference first, literal second", func(t *testing.T) {
		// The reverse ordering: a last-value-wins map decode would have kept
		// the LITERAL here, not the reference -- included so the fix is
		// proven order-independent, not merely lucky on the order above.
		s := newTestExecSession()
		probe := &secretOnlyProbeFn{}
		reg := NewPluginRegistry()
		if err := reg.RegisterWithPolicy("llmtest", "chat", probe.fn,
			ReplayPolicy{}, []string{"api_key"}); err != nil {
			t.Fatalf("RegisterWithPolicy: %v", err)
		}
		s.engine.pluginRegistry = reg

		b := &buf256{}
		input := `{"api_key":"${secret:X}","api_key":"` + literal + `"}`
		res := s.PluginCall(b.ctx(context.Background()), nil, "llmtest", "chat", input, 0, 200)

		if _, callErrorCode := unpackCallResult(res); callErrorCode != callFailureCode {
			t.Fatalf("callErrorCode = %d, want callFailureCode", callErrorCode)
		}
		if probe.calls != 0 {
			t.Fatalf("the plugin function ran despite an exact-duplicate api_key")
		}
		if strings.Contains(s.history[0].PluginInput, literal) {
			t.Fatalf("event_history.plugin_input contains the literal from the duplicate key: %s",
				s.history[0].PluginInput)
		}
	})

	// CONTROL: the same registration, a single well-formed key (no
	// duplicate), is reachable -- proves the refusals above are about the
	// duplicate, not about this harness failing to dispatch at all.
	t.Run("CONTROL: a single well-formed key is not affected", func(t *testing.T) {
		s := newTestExecSession()
		probe := &secretOnlyProbeFn{}
		reg := NewPluginRegistry()
		if err := reg.RegisterWithPolicy("llmtest", "chat", probe.fn,
			ReplayPolicy{}, []string{"api_key"}); err != nil {
			t.Fatalf("RegisterWithPolicy: %v", err)
		}
		s.engine.pluginRegistry = reg

		b := &buf256{}
		res := s.PluginCall(b.ctx(context.Background()), nil, "llmtest", "chat",
			`{"api_key":"${secret:X}"}`, 0, 200)
		if _, callErrorCode := unpackCallResult(res); callErrorCode != 0 {
			t.Fatalf("CONTROL: a single well-formed api_key was refused too (callErrorCode=%d)", callErrorCode)
		}
		if probe.calls != 1 {
			t.Fatalf("CONTROL: the plugin never ran for a single well-formed key; probe.calls=%d", probe.calls)
		}
	})
}

// ---------------------------------------------------------------------------
// 9. A call-guard rejection (no call_plugin capability) redacts a declared
// function's secret-only fields before recording, on both the non-streaming
// and the streaming paths. cleat-review's should-fix-1 on #2329: the call
// guard is checked BEFORE checkSecretOnlyFields on both paths, so a rejected
// call short-circuits with inputJSON never having been validated -- and
// until this fix, the recorded PluginInput stayed the raw, unvalidated
// inputJSON on that branch, which could still hold a literal in a declared
// field.
// ---------------------------------------------------------------------------

func TestACallGuardRefusalRedactsSecretOnlyFieldsOnBothPaths(t *testing.T) {
	const literal = "sk-should-never-be-recorded-via-the-guard-path"

	t.Run("non-streaming", func(t *testing.T) {
		s := newTestExecSession()
		probe := &secretOnlyProbeFn{}
		reg := NewPluginRegistry()
		if err := reg.RegisterWithPolicy("llmtest", "chat", probe.fn,
			ReplayPolicy{}, []string{"api_key"}); err != nil {
			t.Fatalf("RegisterWithPolicy: %v", err)
		}
		s.engine.pluginRegistry = reg

		guard := NewPluginCallGuard()
		guard.Allow("caller-plugin", []string{"someone-else"}) // NOT llmtest
		s.engine.pluginCallGuard = guard
		s.callerPluginName = "caller-plugin"

		b := &buf256{}
		input := `{"api_key":"` + literal + `"}`
		res := s.PluginCall(b.ctx(context.Background()), nil, "llmtest", "chat", input, 0, 200)

		if _, callErrorCode := unpackCallResult(res); callErrorCode != callFailureCode {
			t.Fatalf("callErrorCode = %d, want callFailureCode (the guard rejection)", callErrorCode)
		}
		if probe.calls != 0 {
			t.Fatalf("the plugin function ran despite the call-guard rejection: saw %v", probe.saw)
		}
		if len(s.history) != 1 {
			t.Fatalf("s.history has %d records, want 1: %+v", len(s.history), s.history)
		}
		if strings.Contains(s.history[0].PluginInput, literal) {
			t.Fatalf("a call-guard-refused call recorded the literal: %s", s.history[0].PluginInput)
		}
		if !strings.Contains(s.history[0].PluginInput, secretOnlyFieldRedactionMarker) {
			t.Errorf("recorded plugin_input does not carry the redaction marker: %s", s.history[0].PluginInput)
		}

		// CONTROL: a caller the guard DOES allow reaches the plugin, on the
		// same registration -- proves the rejection above is about the
		// guard, not about this harness never dispatching.
		s.callerPluginName = "" // callerPluginName == "" bypasses the guard check entirely
		res2 := s.PluginCall(b.ctx(context.Background()), nil, "llmtest", "chat",
			`{"api_key":"${secret:openai}"}`, 0, 200)
		if _, ec2 := unpackCallResult(res2); ec2 != 0 {
			t.Fatalf("CONTROL: an unrestricted caller was refused too (callErrorCode=%d)", ec2)
		}
		if probe.calls != 1 {
			t.Fatalf("CONTROL: the plugin never ran for an unrestricted caller; probe.calls=%d", probe.calls)
		}
	})

	t.Run("streaming", func(t *testing.T) {
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

		guard := NewPluginCallGuard()
		guard.Allow("caller-plugin", []string{"someone-else"})
		s.engine.pluginCallGuard = guard
		s.callerPluginName = "caller-plugin"

		buf := make([]byte, 256)
		ctx := contextWithRawMemBuf(context.Background(), buf)
		input := `{"api_key":"` + literal + `"}`
		res := s.PluginCallStreaming(ctx, nil, "llmtest", "chat_stream", input, 0, 200)

		if _, callErrorCode := unpackCallResult(res); callErrorCode != callFailureCode {
			t.Fatalf("callErrorCode = %d, want callFailureCode (the guard rejection)", callErrorCode)
		}
		if streamCalls != 0 {
			t.Fatalf("the streaming plugin function ran despite the call-guard rejection")
		}
		if len(s.history) != 1 {
			t.Fatalf("s.history has %d records, want 1: %+v", len(s.history), s.history)
		}
		if strings.Contains(s.history[0].PluginInput, literal) {
			t.Fatalf("a call-guard-refused streaming call recorded the literal: %s", s.history[0].PluginInput)
		}

		// CONTROL: an unrestricted caller reaches the streaming plugin.
		s.callerPluginName = ""
		res2 := s.PluginCallStreaming(ctx, nil, "llmtest", "chat_stream", `{"api_key":"${secret:openai}"}`, 0, 200)
		if _, ec2 := unpackCallResult(res2); ec2 != 0 {
			t.Fatalf("CONTROL: an unrestricted caller was refused too (callErrorCode=%d)", ec2)
		}
		if streamCalls != 1 {
			t.Fatalf("CONTROL: the streaming plugin never ran for an unrestricted caller; calls=%d", streamCalls)
		}
	})
}

// ---------------------------------------------------------------------------
// 10. RegisterWithPolicy's mutual-exclusion assert, pinned directly.
// cleat-review's should-fix-2 on #2329: nothing failed if this check were
// removed -- every test above registers a NON-reinvoke-eligible policy
// alongside SecretOnlyFields, so none of them would notice its absence.
// Without it, a NEW registration could combine SecretOnlyFields with
// MayReInvokeOnReplay()==true, which is exactly the combination test 6
// above depends on never being possible to create going forward (only to
// have existed in OLD history, from before this check shipped).
// ---------------------------------------------------------------------------

func TestRegisterWithPolicyRefusesSecretOnlyFieldsWithReinvokeEligiblePolicy(t *testing.T) {
	reg := NewPluginRegistry()
	err := reg.RegisterWithPolicy("llmtest", "chat",
		func(context.Context, string) (string, error) { return "", nil },
		ReplayPolicy{Idempotent: true, SameValueOnReplay: true}, // MayReInvokeOnReplay() == true
		[]string{"api_key"})
	if err == nil {
		t.Fatal("RegisterWithPolicy allowed SecretOnlyFields combined with a reinvoke-eligible " +
			"policy; a function like this could reach checkSecretOnlyFields's refusal live during " +
			"replay re-invocation, which is the exact determinism break the exclusion exists to " +
			"prevent (see test 6's WithHistory test, and RegisterWithPolicy's own doc comment)")
	}
	if reg.Has("llmtest", "chat") {
		t.Error("the refused registration was still recorded in the registry")
	}

	// CONTROL: the same declaration, non-reinvoke-eligible, is accepted --
	// proves the error above is about the combination, not about
	// SecretOnlyFields or this policy shape individually.
	if err := reg.RegisterWithPolicy("llmtest", "chat",
		func(context.Context, string) (string, error) { return "", nil },
		ReplayPolicy{}, []string{"api_key"}); err != nil {
		t.Fatalf("CONTROL: RegisterWithPolicy refused SecretOnlyFields with a non-reinvoke-eligible "+
			"policy, which must be allowed: %v", err)
	}
}
