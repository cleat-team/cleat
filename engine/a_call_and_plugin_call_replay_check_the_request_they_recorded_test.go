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
	rows := []row{
		{"call, the same body (control)", call(idBodyRecorded), idCallHistory(), false},
		{"call, a different body", call(idBodyOther), idCallHistory(), true},
		{"plugin_call, the same input (control)", pcall("plain", idBodyRecorded), idPluginHistory("plain", idBodyRecorded), false},
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
