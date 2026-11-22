package engine

import (
	"context"
	"strings"
	"testing"
)

// cleat#4321: a divergence message names a body by its size and a short hash and never prints it.
//
// A divergence message printed the call's request and the recorded one, a fetch's two bodies and the recorded response, a plugin call's input and cached input and
// output, and a schedule's input (cut at 4096 bytes and printed whole under that). The message goes to the guest and from there to a stored run error, a log line and
// an API response, and by replay time the engine holds the DECRYPTED payload, so a run that encrypted its sensitive payloads still wrote them out in a sentence that
// egress redaction, which reads a JSON document and leaves other text alone, cannot reach. The message now redacts where it is built; the labels are unchanged and
// docs/troubleshooting.md shows them.
//
// EVERY EXPECTED TEXT BELOW IS WRITTEN OUT LITERALLY, and its size and hash were computed outside Go (sha256 of the bytes, first 8 bytes in hex), not by redactedBody:
// a row that built its expectation with the function under test would agree with it whatever it did. Each row also asserts that no piece of any body reaches the
// message, so a call that kept printing a body fails even if it ALSO printed the redacted form. One row per call that prints detail (cleat#4303 and #4308: a message
// shared by several calls lost fields, so each call pins its own).

const (
	r4321Act   = "{\"memo\":\"MARKER-ACT-4321\"}"
	r4321Rec   = "{\"memo\":\"MARKER-REC-4321\"}"
	r4321Resp  = "{\"out\":\"MARKER-RESP-4321\"}"
	r4321FB    = "MARKER-FETCH-BODY-ACT-4321"
	r4321FRB   = "MARKER-FETCH-BODY-REC-4321"
	r4321FResp = "MARKER-FETCH-RESP-4321"
)

// a distinctive piece of every body above; none may appear in a message
var r4321Pieces = []string{"MARKER-ACT-4321", "MARKER-REC-4321", "MARKER-RESP-4321", "MARKER-FETCH-BODY-ACT-4321", "MARKER-FETCH-BODY-REC-4321", "MARKER-FETCH-RESP-4321"}

func TestRedactedBodyIsItsSizeAndAShortHashAndNothingElse(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", "[redacted: 0 bytes, sha256=e3b0c44298fc1c14]"},
		{"abc", "abc", "[redacted: 3 bytes, sha256=ba7816bf8f01cfea]"},
		{"a body past the old 4096-byte cut is named by its WHOLE size and hash", strings.Repeat("x", 5000), "[redacted: 5000 bytes, sha256=c59d3c0480cc2d71]"},
		{"a request", r4321Act, "[redacted: 26 bytes, sha256=0dc4fab9a4425bbe]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactedBody(tc.in); got != tc.want {
				t.Errorf("redactedBody(%.40q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	// equal bodies read the same and different ones differently: the comparison a divergence about a request is read for
	if redactedBody(r4321Act) != redactedBody(r4321Act) || redactedBody(r4321Act) == redactedBody(r4321Rec) {
		t.Error("two bodies must read the same when equal and differently when not")
	}
}

func TestEveryCallThatPrintsDetailNamesItsBodiesAndNeverPrintsThem(t *testing.T) {
	wrongType := func(rec EventRecord) []EventRecord {
		rec.Step, rec.EventType = 0, EventTypeSideEffect
		return []EventRecord{rec}
	}
	reg := NewPluginRegistry()
	if err := reg.RegisterWithPolicy("p", "ok", func(_ context.Context, in string) (string, error) { return in, nil }, ReplayPolicy{}, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		opts []EngineOption
		call func(ctx context.Context, s *execSession) int64
		hist []EventRecord
		want string // the whole detail and what follows it, exactly
		has  string // for an identity divergence: the sentence that names the bodies
	}{
		{"the plain call", nil, func(ctx context.Context, s *execSession) int64 {
			return s.DurableCall(ctx, nil, "svc", "op", r4321Act, 0, 16384)
		}, wrongType(EventRecord{Request: r4321Rec}),
			"\n  actual request: [redacted: 26 bytes, sha256=0dc4fab9a4425bbe]\n  expected request: [redacted: 26 bytes, sha256=55abb75b5f4b6772]" + g3fAdvice, ""},
		{"the retried call", nil, func(ctx context.Context, s *execSession) int64 {
			return s.DurableCallWithRetry(ctx, nil, "svc", "op", r4321Act, 3, 1, 100, 1, "", 0, 16384)
		}, wrongType(EventRecord{Request: r4321Rec}),
			"\n  actual request: [redacted: 26 bytes, sha256=0dc4fab9a4425bbe]\n  expected request: [redacted: 26 bytes, sha256=55abb75b5f4b6772]" + g3fAdvice, ""},
		{"the heartbeat call", nil, func(ctx context.Context, s *execSession) int64 {
			return s.DurableCallWithHeartbeat(ctx, nil, "svc", "op", r4321Act, 1000, 0, 16384)
		}, wrongType(EventRecord{Request: r4321Rec}),
			"\n  actual request: [redacted: 26 bytes, sha256=0dc4fab9a4425bbe]\n  expected request: [redacted: 26 bytes, sha256=55abb75b5f4b6772]" + g3fAdvice, ""},
		{"fetch, a record of another type", nil, func(ctx context.Context, s *execSession) int64 {
			return s.Fetch(ctx, nil, "GET", "http://x", "{}", r4321FB, 0, 16384)
		}, wrongType(EventRecord{FetchMethod: "POST", FetchURL: "http://y", FetchBody: r4321FRB, FetchResponse: r4321FResp}),
			"\n  workflow: GET http://x\n  history: POST http://y\n  actual body: [redacted: 26 bytes, sha256=dc80791a94a70a1a]\n  expected body: [redacted: 26 bytes, sha256=b6d0c3559ea76f81]\n  expected response: [redacted: 22 bytes, sha256=f89cf09c60ff9354]" + g3fAdvice, ""},
		{"fetch, the same fetch with a different body", nil, func(ctx context.Context, s *execSession) int64 {
			return s.Fetch(ctx, nil, "GET", "http://x", "{}", r4321FB, 0, 16384)
		}, []EventRecord{{Step: 0, EventType: EventTypeFetch, FetchMethod: "GET", FetchURL: "http://x", FetchBody: r4321FRB, FetchResponse: r4321FResp}},
			"\n  workflow: GET http://x\n  history: GET http://x\n  actual body: [redacted: 26 bytes, sha256=dc80791a94a70a1a]\n  expected body: [redacted: 26 bytes, sha256=b6d0c3559ea76f81]\n  expected response: [redacted: 22 bytes, sha256=f89cf09c60ff9354]" + g3fAdvice,
			"GET http://x with a different body: [redacted: 26 bytes, sha256=dc80791a94a70a1a], recorded [redacted: 26 bytes, sha256=b6d0c3559ea76f81]."},
		{"the plugin call", []EngineOption{WithPluginRegistry(reg)}, func(ctx context.Context, s *execSession) int64 {
			return s.PluginCall(ctx, nil, "p", "ok", r4321Act, 0, 16384)
		}, wrongType(EventRecord{PluginName: "p", PluginFunc: "ok", PluginInput: r4321Rec, PluginOutput: r4321Resp}),
			"\n  actual input: [redacted: 26 bytes, sha256=0dc4fab9a4425bbe]\n  expected (cached) input: [redacted: 26 bytes, sha256=55abb75b5f4b6772]\n  expected (cached) output: [redacted: 26 bytes, sha256=1514ac7dddbddd19]" + g3fAdvice, ""},
		{"schedule_cron, a record of another type", nil, func(ctx context.Context, s *execSession) int64 {
			return s.ScheduleCron(ctx, nil, "wf", "* * * * *", "UTC", r4321Act, 0, 16384)
		}, wrongType(EventRecord{CronWorkflowName: "other", CronExpr: "0 * * * *", CronTimezone: "EST", CronInput: r4321Rec}),
			"\n  workflow: wf \"* * * * *\" tz=\"UTC\" input=[redacted: 26 bytes, sha256=0dc4fab9a4425bbe]\n  history:  other \"0 * * * *\" tz=\"EST\" input=[redacted: 26 bytes, sha256=55abb75b5f4b6772]" + g3fAdvice, ""},
		{"schedule_cron, the same schedule with a different input", nil, func(ctx context.Context, s *execSession) int64 {
			return s.ScheduleCron(ctx, nil, "wf", "* * * * *", "UTC", r4321Act, 0, 16384)
		}, []EventRecord{{Step: 0, EventType: EventTypeScheduleCron, CronWorkflowName: "wf", CronExpr: "* * * * *", CronTimezone: "UTC", CronInput: r4321Rec}},
			"\n  workflow: wf \"* * * * *\" tz=\"UTC\" input=[redacted: 26 bytes, sha256=0dc4fab9a4425bbe]\n  history:  wf \"* * * * *\" tz=\"UTC\" input=[redacted: 26 bytes, sha256=55abb75b5f4b6772]" + g3fAdvice,
			"schedule \"wf\" \"* * * * *\" tz=\"UTC\" input=[redacted: 26 bytes, sha256=0dc4fab9a4425bbe], recorded \"wf\" \"* * * * *\" tz=\"UTC\" input=[redacted: 26 bytes, sha256=55abb75b5f4b6772]."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := g3fMessage2(tc.opts, tc.call, tc.hist)
			if !strings.HasPrefix(got, "replay divergence at step 0: ") {
				t.Fatalf("PRECONDITION FAILED: not a divergence message, so this row measures nothing: %q", got)
			}
			for _, piece := range r4321Pieces {
				if strings.Contains(got, piece) {
					t.Errorf("the divergence message prints a body (%q):\n%s", piece, got)
				}
			}
			if !strings.HasSuffix(got, tc.want) {
				t.Errorf("the detail is not the redacted detail this call pins.\n  got:  %q\n  want it to end with: %q", got, tc.want)
			}
			if tc.has != "" && !strings.Contains(got, tc.has) {
				t.Errorf("the sentence that names the differing bodies is not the redacted one.\n  got:  %q\n  want it to contain: %q", got, tc.has)
			}
		})
	}
}

// g3fMessage2 is g3fMessage with engine options, which the plugin call needs for its registry.
func g3fMessage2(opts []EngineOption, call func(ctx context.Context, s *execSession) int64, history []EventRecord) string {
	s := stepSession(opts, history)
	buf := make([]byte, 16384)
	call(context.WithValue(context.Background(), wasmMemBufKey{}, buf), s)
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return string(buf[:n])
}

// wait_for_event is not a divergence but prints the same kind of text: when the awaited function's output is not claim-shaped it names the output. The output is
// whatever the plugin returned (a webhook's payload, say), so it is named by size and hash and not printed (cleat#4321).
func TestAMalformedWaitForEventOutputIsNamedAndNotPrinted(t *testing.T) {
	const out = `{"payload":"MARKER-WAIT-OUT-4321"}`
	counter := &eventClaimCounter{outputs: []string{out}}
	s := waitForEventSession(t, counter, nil, false)
	buf := make([]byte, 1024)
	packed := s.WaitForEvent(contextWithRawMemBuf(context.Background(), buf), nil, "webhook-ingest", "await_webhook", "{}", `["__evt:payment"]`, 60_000, 0, 1000)
	n := uint32(uint64(packed) >> 40)
	if n == 0 {
		t.Fatalf("PRECONDITION FAILED: no failure message was written, so this row measures nothing (packed %#x)", packed)
	}
	got := string(buf[:n])
	if !strings.Contains(got, "has no boolean \"found\" field") {
		t.Fatalf("PRECONDITION FAILED: not the malformed-output message: %q", got)
	}
	if strings.Contains(got, "MARKER-WAIT-OUT-4321") {
		t.Errorf("the message prints the plugin's output: %q", got)
	}
	if want := "Output was: [redacted: 34 bytes, sha256=74c13c3fc099fcee]"; !strings.HasSuffix(got, want) {
		t.Errorf("the message does not end by naming the output.\n  got:  %q\n  want it to end with: %q", got, want)
	}
}

// A divergence inside wait_for_event reaches the guest inside the malformed-output sentence, because the call returns it with a zero call-error byte. The message
// is the engine's own and is shown, so the reader learns what diverged; the input it names is a body and is not printed (cleat#4321).
func TestADivergenceInsideWaitForEventIsShownAndNamesItsInputWithoutPrintingIt(t *testing.T) {
	const in = `{"memo":"MARKER-WAIT-IN-4321"}`
	counter := &eventClaimCounter{}
	other := EventRecord{Step: 0, EventType: EventTypePluginCall, PluginName: "other-plugin", PluginFunc: "other_fn", PluginInput: r4321Rec, PluginOutput: r4321Resp}
	s := waitForEventSession(t, counter, []EventRecord{other}, true)
	buf := make([]byte, 4096)
	packed := s.WaitForEvent(contextWithRawMemBuf(context.Background(), buf), nil, "webhook-ingest", "await_webhook", in, `["__evt:payment"]`, 60_000, 0, 4096)
	n := uint32(uint64(packed) >> 40)
	if n == 0 {
		t.Fatalf("PRECONDITION FAILED: no message was written, so this row measures nothing (packed %#x)", packed)
	}
	got := string(buf[:n])
	if !strings.Contains(got, "Output was: replay divergence at step 0: ") {
		t.Fatalf("the divergence is not shown inside the wait's message, so a reader cannot tell what went wrong: %q", got)
	}
	for _, piece := range append([]string{"MARKER-WAIT-IN-4321"}, r4321Pieces...) {
		if strings.Contains(got, piece) {
			t.Errorf("the message prints a body (%q): %q", piece, got)
		}
	}
	if want := "\n  actual input: [redacted: 30 bytes, sha256=eb9bfd7c78fa3d66]\n  expected (cached) input: [redacted: 26 bytes, sha256=55abb75b5f4b6772]\n  expected (cached) output: [redacted: 26 bytes, sha256=1514ac7dddbddd19]" + g3fAdvice; !strings.HasSuffix(got, want) {
		t.Errorf("the divergence detail inside the wait is not the redacted one.\n  got:  %q\n  want it to end with: %q", got, want)
	}
}
