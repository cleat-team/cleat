package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// A `call` is identified by its service and operation, and a `plugin_call` by its plugin and function; until cleat#4424 neither looked at the REQUEST, so a replay that asked the same
// service the same operation with a DIFFERENT body was handed the recorded answer for the old one, with no divergence. `fetch` already compares its body (method, url, body), which is
// the known positive that this shape can be caught: cleat-77 measured the three side by side on #4206 (issuecomment-7094424347).
//
// The identity is the request or input STRING the record carries, compared exactly, as fetch compares its body. Every case has its control: the same body must replay, because a
// comparison that diverged on everything would pass the "a different body diverges" rows and break every run.

const idBufLen = 16384

// idReplay runs one host call against a replaying session over the given history, and returns what the call wrote to the guest, the session (for stepCount), and the packed result.
func idReplay(opts []EngineOption, history []EventRecord, call func(ctx context.Context, s *execSession) int64) (string, *execSession) {
	s := stepSession(opts, history)
	buf := make([]byte, idBufLen)
	call(context.WithValue(context.Background(), wasmMemBufKey{}, buf), s)
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return string(buf[:n]), s
}

func isDivergence(msg string) bool { return strings.HasPrefix(msg, "replay divergence at step ") }

// trimMsg keeps a failure message readable when the body in it is 100 KB.
func trimMsg(s string) string {
	if len(s) > 160 {
		return s[:160] + "..."
	}
	return s
}

// idPluginRegistry registers one plain function and one that declares a secret-only field.
func idPluginRegistry(t *testing.T) []EngineOption {
	t.Helper()
	pr := NewPluginRegistry()
	fn := plugin.PluginFunc(func(context.Context, string) (string, error) { return `"live"`, nil })
	if err := pr.Register("p", "plain", fn); err != nil {
		t.Fatalf("register plain: %v", err)
	}
	if err := pr.RegisterWithPolicy("p", "secret", fn, ReplayPolicy{}, []string{"api_key"}); err != nil {
		t.Fatalf("register secret-only: %v", err)
	}
	return []EngineOption{WithPluginRegistry(pr)}
}

const (
	idBodyRecorded = `{"order":1001,"note":"RECORDED-SENTINEL"}`
	idBodyOther    = `{"order":1002,"note":"OTHER-SENTINEL"}`
	// the recorded request as another SDK's serialiser writes it: the same JSON value, the keys in another order
	idBodyRecordedReordered = `{"note":"RECORDED-SENTINEL","order":1001}`
)

func idCallHistory() []EventRecord {
	return []EventRecord{{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Request: idBodyRecorded, Response: `"recorded answer"`}}
}

func idPluginHistory(fn, input string) []EventRecord {
	return []EventRecord{{Step: 0, EventType: EventTypePluginCall, PluginName: "p", PluginFunc: fn, PluginInput: input, PluginOutput: `"recorded answer"`}}
}

func TestACallAndAPluginCallReplayOnlyTheRequestTheyRecorded(t *testing.T) {
	opts := idPluginRegistry(t)
	type row struct {
		name string
		call func(ctx context.Context, s *execSession) int64
		hist []EventRecord
		diff bool // the live request differs from the recorded one
	}
	call := func(body string) func(ctx context.Context, s *execSession) int64 {
		return func(ctx context.Context, s *execSession) int64 {
			return s.DurableCall(ctx, nil, "svc", "op", body, 0, idBufLen)
		}
	}
	pcall := func(fn, input string) func(ctx context.Context, s *execSession) int64 {
		return func(ctx context.Context, s *execSession) int64 {
			return s.PluginCall(ctx, nil, "p", fn, input, 0, idBufLen)
		}
	}
	fetch := func(body string) func(ctx context.Context, s *execSession) int64 {
		return func(ctx context.Context, s *execSession) int64 {
			return s.Fetch(ctx, nil, "POST", "http://example.invalid/x", "{}", body, 0, idBufLen)
		}
	}
	fetchHistory := []EventRecord{{Step: 0, EventType: EventTypeFetch, FetchMethod: "POST", FetchURL: "http://example.invalid/x", FetchBody: idBodyRecorded, FetchResponse: `"recorded answer"`}}
	rows := []row{
		// fetch compared its body byte for byte until cleat#4424; it now compares it as a call's request is (the owner's decision), so the same JSON in another key order replays.
		{"fetch, the same body (control)", fetch(idBodyRecorded), fetchHistory, false},
		{"fetch, the same JSON with its keys in another order (loosened by cleat#4424)", fetch(idBodyRecordedReordered), fetchHistory, false},
		{"fetch, a different value", fetch(idBodyOther), fetchHistory, true},
		{"call, the same body (control)", call(idBodyRecorded), idCallHistory(), false},
		{"call, the same request written another way (control: cross-language, ABI.md 6.7)", call(idBodyRecordedReordered), idCallHistory(), false},
		{"call, a different body", call(idBodyOther), idCallHistory(), true},
		// WHICH RECORDS CARRY A REQUEST is decided by the record's TYPE, not by whether the field is empty: a call recorded with no request, replayed with one, is a different call. A
		// comparison keyed on `rec.Request != ""` would skip exactly this case (found in review by a mutant that stayed green).
		{"call recorded with an EMPTY request, replayed with a body", call("x"), []EventRecord{{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Response: `"recorded answer"`}}, true},
		{"plugin_call, the same input (control)", pcall("plain", idBodyRecorded), idPluginHistory("plain", idBodyRecorded), false},
		{"plugin_call, the same input written another way (control: cross-language)", pcall("plain", idBodyRecordedReordered), idPluginHistory("plain", idBodyRecorded), false},
		{"plugin_call, a different input", pcall("plain", idBodyOther), idPluginHistory("plain", idBodyRecorded), true},
	}
	for _, r := range rows {
		r := r
		t.Run(r.name, func(t *testing.T) {
			msg, s := idReplay(opts, r.hist, r.call)
			if !r.diff {
				if isDivergence(msg) || msg != `"recorded answer"` || s.stepCount != 1 {
					t.Fatalf("the same request must replay the recorded answer and consume exactly one step: got %q, stepCount %d", msg, s.stepCount)
				}
				return
			}
			if !isDivergence(msg) {
				t.Fatalf("a different request was handed %q with stepCount %d: the recorded answer for another request, silently", msg, s.stepCount)
			}
			if s.stepCount != 0 {
				t.Errorf("a divergence consumed %d step(s); it must leave the record unread", s.stepCount)
			}
			// The message names each body by its size and a short hash and NEVER prints it (cleat#4321): a store hands back the DECRYPTED payload because replay acts on it, and the message
			// goes on to the guest, the stored run error and the logs.
			for _, leak := range []string{"RECORDED-SENTINEL", "OTHER-SENTINEL", "1001", "1002"} {
				if strings.Contains(msg, leak) {
					t.Errorf("the divergence message prints part of a request (%q):\n%s", leak, msg)
				}
			}
			if !strings.Contains(msg, "[redacted: ") {
				t.Errorf("the divergence message does not name the bodies by size and hash:\n%s", msg)
			}
		})
	}
}

// A plugin function that declares secret-only fields records a FAILED call's input with those fields masked (plugins.go invokePlugin), and a SUCCESSFUL call's input as the guest passed
// it. The live input at replay is always the guest's, so a comparison against the record alone would diverge on every failed call to such a function. Both shapes must replay, and a
// different input must still diverge under both.
func TestAPluginCallToAFunctionWithSecretOnlyFieldsReplaysWhetherItsInputWasRecordedMaskedOrNot(t *testing.T) {
	opts := idPluginRegistry(t)
	live := `{"api_key":"LITERAL-NOT-A-REFERENCE","x":1}`
	masked := redactSecretOnlyFields([]string{"api_key"}, live)
	if masked == live || !strings.Contains(masked, secretOnlyFieldRedactionMarker) {
		t.Fatalf("PRECONDITION: the masked form %q is not different from the live one, so this test would measure nothing", masked)
	}
	ref := `{"api_key":"${secret:k}","x":1}`
	pcall := func(input string) func(ctx context.Context, s *execSession) int64 {
		return func(ctx context.Context, s *execSession) int64 {
			return s.PluginCall(ctx, nil, "p", "secret", input, 0, idBufLen)
		}
	}
	failedHistory := []EventRecord{{Step: 0, EventType: EventTypePluginCall, PluginName: "p", PluginFunc: "secret", PluginInput: masked, PluginError: "refused: literal in a secret-only field"}}
	for _, tc := range []struct {
		name  string
		input string
		hist  []EventRecord
		want  string // "" = replays; else a divergence
		reads string // what a replay is served, when it replays
	}{
		{"a refused call recorded masked, replayed with the same live input", live, failedHistory, "", "refused: literal in a secret-only field"},
		{"a refused call recorded masked, replayed with a DIFFERENT live input", `{"api_key":"LITERAL-NOT-A-REFERENCE","x":2}`, failedHistory, "diverge", ""},
		{"a successful call recorded as passed, replayed with the same input", ref, idPluginHistory("secret", ref), "", `"recorded answer"`},
		{"a successful call recorded as passed, replayed with a different input", `{"api_key":"${secret:k}","x":2}`, idPluginHistory("secret", ref), "diverge", ""},
		// Only a FAILED call is recorded masked, so only a failure record is offered the live input in its masked form. A success record that merely EQUALS the masked form of the live
		// input is another input's record, and accepting it would let two different inputs look alike through a mask that is lossy by design.
		{"a SUCCESS record equal to the masked form of the live input is not that input's", live, idPluginHistory("secret", masked), "diverge", ""},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			msg, s := idReplay(opts, tc.hist, pcall(tc.input))
			if tc.want == "" {
				if isDivergence(msg) || s.stepCount != 1 || !strings.Contains(msg, tc.reads) {
					t.Fatalf("expected the recorded answer (%q) and one step consumed; got %q with stepCount %d", tc.reads, msg, s.stepCount)
				}
				return
			}
			if !isDivergence(msg) {
				t.Fatalf("a different input was served the recorded answer: %q (stepCount %d)", msg, s.stepCount)
			}
			if strings.Contains(msg, "LITERAL-NOT-A-REFERENCE") {
				t.Errorf("the divergence message prints a literal that was in a secret-only field:\n%s", msg)
			}
		})
	}
}

// The three siblings of the plain call carry a request too, and until cleat#4424 each served the recorded answer to a different body exactly as the plain call did (measured with a
// throwaway probe on develop): call_with_retry, call_with_heartbeat and durable_send. They share the plain call's comparison (callRequestIdentity), and each has the same two rows.
//
// A run is several records, so "consumes nothing" is measured on the whole run: a different body at the call that ends it must leave the attempts or heartbeats before it unread too.
func TestTheRetriedHeartbeatAndSendCallsReplayOnlyTheRequestTheyRecorded(t *testing.T) {
	type kind struct {
		name    string
		records func(body string) []EventRecord
		call    func(ctx context.Context, s *execSession, body string) int64
		message bool // the call has an output buffer, so a divergence is readable
	}
	kinds := []kind{
		{"call_with_retry", func(body string) []EventRecord {
			return []EventRecord{
				{Step: 0, EventType: EventTypeCallAttemptFailed, Service: "svc", Op: "op", Attempt: 1, Err: "TRY_AGAIN: 503"},
				{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "op", Request: body, Response: `"recorded answer"`},
			}
		}, func(ctx context.Context, s *execSession, body string) int64 {
			return s.DurableCallWithRetry(ctx, nil, "svc", "op", body, 3, 1, 100, 1, "", 0, idBufLen)
		}, true},
		{"call_with_heartbeat", func(body string) []EventRecord {
			return []EventRecord{
				{Step: 0, EventType: EventTypeHeartbeat, Service: "svc", Op: "op"},
				{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "op", Request: body, Response: `"recorded answer"`},
			}
		}, func(ctx context.Context, s *execSession, body string) int64 {
			return s.DurableCallWithHeartbeat(ctx, nil, "svc", "op", body, 1000, 0, idBufLen)
		}, true},
		{"durable_send", func(body string) []EventRecord {
			return []EventRecord{{Step: 0, EventType: EventTypeDurableSend, Service: "svc", Op: "op", Request: body}}
		}, func(ctx context.Context, s *execSession, body string) int64 {
			return s.DurableSend(ctx, nil, "svc", "op", body)
		}, false},
	}
	for _, k := range kinds {
		k := k
		t.Run(k.name+", the same body (control)", func(t *testing.T) {
			hist := k.records(idBodyRecorded)
			msg, s := idReplay(nil, hist, func(ctx context.Context, s *execSession) int64 { return k.call(ctx, s, idBodyRecorded) })
			if isDivergence(msg) || s.stepCount != len(hist) {
				t.Fatalf("the same request must replay and consume the whole run (%d records): got %q, stepCount %d", len(hist), trimMsg(msg), s.stepCount)
			}
			if k.message && msg != `"recorded answer"` {
				t.Fatalf("the same request must be answered the recorded answer: got %q", msg)
			}
		})
		t.Run(k.name+", a different body", func(t *testing.T) {
			hist := k.records(idBodyRecorded)
			msg, s := idReplay(nil, hist, func(ctx context.Context, s *execSession) int64 { return k.call(ctx, s, idBodyOther) })
			if s.stepCount != 0 {
				t.Fatalf("a different request consumed %d of %d records (%q): it was served the recorded answer for another request, or part of its run", s.stepCount, len(hist), trimMsg(msg))
			}
			if k.message {
				if !isDivergence(msg) {
					t.Fatalf("a different request left the step unread but reported no divergence: %q", trimMsg(msg))
				}
				for _, leak := range []string{"RECORDED-SENTINEL", "OTHER-SENTINEL", "1001", "1002"} {
					if strings.Contains(msg, leak) {
						t.Errorf("the divergence message prints part of a request (%q):\n%s", leak, msg)
					}
				}
				if !strings.Contains(msg, "[redacted: ") || !strings.Contains(msg, "with a different request") {
					t.Errorf("the divergence message does not name the bodies by size and hash:\n%s", msg)
				}
			}
		})
	}
}

// THE LIMIT, pinned so nobody reads the comparison as wider than it is. The request is compared on a record that carries one. call_attempt_failed and heartbeat records carry none,
// and a call_retry_refused record deliberately carries none (freshCallWithRetry: a request body is the kind of value the sealed-payload machinery exists for), so a history that
// stops inside a run, or a policy refused for its length, is accepted on its service and operation alone. Probed on the spec's own Identity: running such a history would go live
// and need a caller, and the claim here is about what the comparison can see.
func TestTheRequestIsComparedOnlyOnARecordThatCarriesOne(t *testing.T) {
	a := stepArgs{s1: "svc", s2: "op", s3: idBodyOther}
	carries := EventRecord{EventType: EventTypeCall, Service: "svc", Op: "op", Request: idBodyRecorded}
	for _, spec := range []*stepSpec{&callStep, &retryStep, &heartbeatStep} {
		if why := spec.Identity(a, &carries); why == "" {
			t.Errorf("%s: a call record with a different request was accepted", spec.Name)
		}
	}
	if why := durableSendStep.Identity(a, &EventRecord{EventType: EventTypeDurableSend, Service: "svc", Op: "op", Request: idBodyRecorded}); why == "" {
		t.Errorf("durable_send: a send record with a different request was accepted")
	}
	// The converse, pinned by RECORD TYPE: a call or send record with an EMPTY request still carries one, so a live body that is not empty differs from it.
	for _, c := range []struct {
		spec *stepSpec
		rec  EventRecord
	}{
		{&callStep, EventRecord{EventType: EventTypeCall, Service: "svc", Op: "op"}},
		{&retryStep, EventRecord{EventType: EventTypeCall, Service: "svc", Op: "op"}},
		{&heartbeatStep, EventRecord{EventType: EventTypeCall, Service: "svc", Op: "op"}},
		{&durableSendStep, EventRecord{EventType: EventTypeDurableSend, Service: "svc", Op: "op"}},
	} {
		if why := c.spec.Identity(a, &c.rec); why == "" {
			t.Errorf("%s: a %s record with an empty request was accepted for a live body %q: the record TYPE decides whether a request is compared, not whether the field is empty", c.spec.Name, c.rec.EventType, a.s3)
		}
	}
	for _, c := range []struct {
		spec *stepSpec
		rec  EventRecord
	}{
		{&retryStep, EventRecord{EventType: EventTypeCallAttemptFailed, Service: "svc", Op: "op"}},
		{&retryStep, EventRecord{EventType: EventTypeCallRetryRefused, Service: "svc", Op: "op"}},
		{&heartbeatStep, EventRecord{EventType: EventTypeHeartbeat, Service: "svc", Op: "op"}},
	} {
		if why := c.spec.Identity(a, &c.rec); why != "" {
			t.Errorf("%s: a %s record carries no request, so its body cannot be compared, yet it was refused: %s", c.spec.Name, c.rec.EventType, why)
		}
		// and its service and operation are still compared
		wrong := c.rec
		wrong.Op = "other"
		if why := c.spec.Identity(a, &wrong); why == "" {
			t.Errorf("%s: a %s record of another operation was accepted", c.spec.Name, c.rec.EventType)
		}
	}
}
