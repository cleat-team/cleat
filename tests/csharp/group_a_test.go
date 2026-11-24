//go:build cgo

package csharp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// Group A of cleat#4080's host-call coverage: durable-call-retry, durable-call-heartbeat, durable-min-version, durable-continue-as-new and
// continue-as-new-versioned. Each case runs a named entry of the conformance component (packages/cleat-csharp/conformance/GroupA.cs)
// through the real engine and wasmtime backend. Nothing here claims C# is supported.

// objectResult decodes an entry's result, which every group A entry that completes returns as one JSON object.
func objectResult(t *testing.T, res conformanceResult) map[string]any {
	t.Helper()
	if res.Err != nil {
		t.Fatalf("the entry failed: %v", res.Err)
	}
	if res.Suspended != nil {
		t.Fatalf("the entry suspended (%q), want it to complete", res.Suspended.Reason)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(res.Result), &m); err != nil {
		t.Fatalf("the result is not a JSON object: %v\n%s", err, res.Result)
	}
	return m
}

func retryInput(service, op string, max int, nonRetryable ...string) string {
	if nonRetryable == nil {
		nonRetryable = []string{}
	}
	b, _ := json.Marshal(map[string]any{
		"service": service, "operation": op, "request": map[string]any{"n": 1},
		"maxAttempts": max, "initialMs": 1, "backoff": 2.0, "maxMs": 5, "nonRetryable": nonRetryable,
	})
	return string(b)
}

func TestCSharpGroupACallRetryAnswersAfterTransientFailures(t *testing.T) {
	fail := func(_, _ string, nth int) error {
		if nth <= 2 {
			return errors.New("temporarily unavailable")
		}
		return nil
	}
	res := runConformanceEntry(t, "a_call_retry", entryRun{Input: retryInput("svc", "flaky", 5), Fail: fail, Responses: map[string]string{"svc/flaky": `{"ok":true}`}})
	m := objectResult(t, res)
	if m["response"] != `{"ok":true}` {
		t.Errorf("response = %v, want the third attempt's answer", m["response"])
	}
	if len(res.Calls) != 3 {
		t.Fatalf("the service was asked %d time(s), want 3 (two failures, then the success): %v", len(res.Calls), res.Calls)
	}
	for i, call := range res.Calls {
		if call.Service != "svc" || call.Operation != "flaky" || call.Request != `{"n":1}` {
			t.Errorf("attempt %d reached the service as %+v, want svc/flaky with the request unchanged", i+1, call)
		}
	}
}

// The attempt count is the policy's MaxAttempts, so it must be what reached the host: two attempts allowed, two made, then the LAST failure.
func TestCSharpGroupACallRetryExhaustsAndTranslatesTheLastFailure(t *testing.T) {
	fail := func(_, _ string, nth int) error { return errors.New("down on attempt " + string(rune('0'+nth))) }
	res := runConformanceEntry(t, "a_call_retry", entryRun{Input: retryInput("svc", "down", 2), Fail: fail})
	m := objectResult(t, res)
	if m["failed"] != true {
		t.Fatalf("the policy was exhausted but the entry saw %v, want a CleatCallException", m)
	}
	if !strings.Contains(m["message"].(string), "down on attempt 2") {
		t.Errorf("message = %q, want the LAST attempt's failure", m["message"])
	}
	if n := len(res.Calls); n != 2 {
		t.Errorf("the service was asked %d time(s), want MaxAttempts = 2", n)
	}
}

// One attempt means no retry at all: the smallest policy the SDK accepts, and the proof MaxAttempts is read as a count.
func TestCSharpGroupACallRetryWithOneAttemptDoesNotRetry(t *testing.T) {
	fail := func(_, _ string, _ int) error { return errors.New("down") }
	res := runConformanceEntry(t, "a_call_retry", entryRun{Input: retryInput("svc", "down", 1), Fail: fail})
	if objectResult(t, res)["failed"] != true {
		t.Fatal("a failing call with a single attempt must fail")
	}
	if n := len(res.Calls); n != 1 {
		t.Errorf("the service was asked %d time(s), want exactly 1", n)
	}
}

// The non-retryable list reaches the host as JSON and is matched against the failure's CODE. The control is the same failure with the code
// NOT listed, which must use every attempt: without it, "stopped after one" could be any reason.
func TestCSharpGroupACallRetryStopsAtOnceOnANonRetryableCode(t *testing.T) {
	fail := func(_, _ string, _ int) error {
		return &engine.ServiceError{Code: "INSUFFICIENT_FUNDS", Message: "not enough"}
	}
	listed := runConformanceEntry(t, "a_call_retry", entryRun{Input: retryInput("svc", "charge", 4, "CARD_DECLINED", "INSUFFICIENT_FUNDS"), Fail: fail})
	other := runConformanceEntry(t, "a_call_retry", entryRun{Input: retryInput("svc", "charge", 4, "CARD_DECLINED"), Fail: fail})
	if n := len(listed.Calls); n != 1 {
		t.Errorf("with the code listed the service was asked %d time(s), want 1 (not retried)", n)
	}
	if n := len(other.Calls); n != 4 {
		t.Errorf("CONTROL: with the code not listed the service was asked %d time(s), want all 4 attempts", n)
	}
}

// A policy whose worst-case backoff exceeds the host's budget is refused (the engine's default is 60 s): ten attempts from a ten second
// first interval, doubling to a thirty second cap, is far over it. The SDK surfaces the refusal as an error carrying the host's code, and
// NOTHING was called: no attempt was spent.
func TestCSharpGroupACallRetryPolicyOverTheHostBudgetIsRefusedWithCode6(t *testing.T) {
	in, _ := json.Marshal(map[string]any{
		"service": "svc", "operation": "slow", "request": map[string]any{},
		"maxAttempts": 10, "initialMs": 10000, "backoff": 2.0, "maxMs": 30000, "nonRetryable": []string{},
	})
	res := runConformanceEntry(t, "a_call_retry", entryRun{Input: string(in)})
	m := objectResult(t, res)
	if m["failed"] != true || m["code"] != float64(6) {
		t.Fatalf("an over-budget policy: got %v, want a failure with code 6 (RetryPolicyTooLong)", m)
	}
	if !strings.Contains(strings.ToLower(m["message"].(string)), "retry policy rejected") {
		t.Errorf("message = %q, want the host's reason", m["message"])
	}
	if n := len(res.Calls); n != 0 {
		t.Errorf("the service was asked %d time(s) for a refused policy, want 0", n)
	}
}

// A policy the SDK can see is meaningless never reaches the host.
func TestCSharpGroupACallRetryRefusesAMeaninglessPolicyBeforeTheHost(t *testing.T) {
	for _, tc := range []struct {
		name, param string
		in          map[string]any
	}{
		{"no attempts", "MaxAttempts", map[string]any{"maxAttempts": 0, "initialMs": 1, "backoff": 2.0, "maxMs": 5}},
		{"negative first interval", "InitialInterval", map[string]any{"maxAttempts": 3, "initialMs": -1, "backoff": 2.0, "maxMs": 5}},
		{"negative coefficient", "BackoffCoefficient", map[string]any{"maxAttempts": 3, "initialMs": 1, "backoff": -1.0, "maxMs": 5}},
	} {
		tc.in["service"], tc.in["operation"], tc.in["request"], tc.in["nonRetryable"] = "svc", "never", map[string]any{}, []string{}
		b, _ := json.Marshal(tc.in)
		res := runConformanceEntry(t, "a_call_retry", entryRun{Input: string(b)})
		m := objectResult(t, res)
		if m["invalid"] != tc.param {
			t.Errorf("%s: got %v, want the SDK to refuse parameter %s", tc.name, m, tc.param)
		}
		if n := len(res.Calls); n != 0 {
			t.Errorf("%s: a refused policy reached the service %d time(s), want 0", tc.name, n)
		}
	}
}

func TestCSharpGroupACallHeartbeatPassesTheCallThrough(t *testing.T) {
	in := `{"service":"svc","operation":"long","request":{"n":2},"intervalMs":50}`
	res := runConformanceEntry(t, "a_call_heartbeat", entryRun{Input: in, Responses: map[string]string{"svc/long": `{"done":1}`}})
	m := objectResult(t, res)
	if m["response"] != `{"done":1}` {
		t.Errorf("response = %v, want the service's answer", m["response"])
	}
	got := res.Calls
	if len(got) != 1 || got[0].Service != "svc" || got[0].Operation != "long" || got[0].Request != `{"n":2}` {
		t.Errorf("the service saw %v, want one svc/long call with the request unchanged", got)
	}
}

func TestCSharpGroupACallHeartbeatTranslatesAFailure(t *testing.T) {
	fail := func(_, _ string, _ int) error { return errors.New("the long call failed") }
	in := `{"service":"svc","operation":"long","request":{},"intervalMs":50}`
	m := objectResult(t, runConformanceEntry(t, "a_call_heartbeat", entryRun{Input: in, Fail: fail}))
	if m["failed"] != true || !strings.Contains(m["message"].(string), "the long call failed") {
		t.Errorf("got %v, want a CleatCallException carrying the service's failure", m)
	}
}

// A zero interval would make the host build a ticker that cannot exist (time.NewTicker panics on it, cleat#4105), so the SDK refuses it first.
func TestCSharpGroupACallHeartbeatRefusesAnIntervalTheHostCannotUse(t *testing.T) {
	for _, ms := range []int{0, -5} {
		in, _ := json.Marshal(map[string]any{"service": "svc", "operation": "long", "request": map[string]any{}, "intervalMs": ms})
		res := runConformanceEntry(t, "a_call_heartbeat", entryRun{Input: string(in)})
		m := objectResult(t, res)
		if m["invalid"] != "heartbeatInterval" {
			t.Errorf("interval %d ms: got %v, want the SDK to refuse it", ms, m)
		}
		if n := len(res.Calls); n != 0 {
			t.Errorf("interval %d ms: a refused interval reached the service %d time(s), want 0", ms, n)
		}
	}
}

type versionedState struct{ version, min int }

func (v versionedState) Version() int                    { return v.version }
func (v versionedState) MinVersion() int                 { return v.min }
func (v versionedState) ChildVersion(string) (int, bool) { return 0, false }
func (v versionedState) Priority() int                   { return 0 }

// The two versions differ on purpose, so a MinVersion that read the wrong import would show.
func TestCSharpGroupAMinVersionIsNotTheCurrentVersion(t *testing.T) {
	res := runConformanceEntry(t, "a_min_version", entryRun{Options: []engine.EngineOption{engine.WithWorkflowState(versionedState{version: 5, min: 3})}})
	m := objectResult(t, res)
	if m["min"] != float64(3) || m["version"] != float64(5) {
		t.Errorf("got %v, want min 3 and version 5", m)
	}
}

// Continue-as-new ends the run: the engine reports a suspension whose reason is continue_as_new and whose NewInput is what the entry passed,
// the call made BEFORE it is recorded, and the call written AFTER it never happens (the method does not return).
func TestCSharpGroupAContinueAsNewEndsTheRunWithTheNewInput(t *testing.T) {
	res := runConformanceEntry(t, "a_continue_as_new", entryRun{Input: `{"next":{"cursor":42}}`})
	if res.Err != nil {
		t.Fatalf("the entry failed: %v", res.Err)
	}
	if res.Suspended == nil || res.Suspended.Reason != "continue_as_new" {
		t.Fatalf("suspended = %+v (result %q), want a continue_as_new suspension", res.Suspended, res.Result)
	}
	if res.Suspended.NewInput != `{"cursor":42}` {
		t.Errorf("NewInput = %q, want the next run's input", res.Suspended.NewInput)
	}
	got := res.Calls
	if len(got) != 1 || got[0].Operation != "before" {
		t.Errorf("the service saw %v, want only the call made before ContinueAsNew (the one after it must not run)", got)
	}
}

func TestCSharpGroupAContinueAsNewVersionedCarriesTheVersion(t *testing.T) {
	res := runConformanceEntry(t, "a_continue_as_new_versioned", entryRun{Input: `{"next":{"v":2},"version":7}`})
	if res.Err != nil || res.Suspended == nil || res.Suspended.Reason != "continue_as_new" {
		t.Fatalf("err=%v suspended=%+v result=%q, want a continue_as_new suspension", res.Err, res.Suspended, res.Result)
	}
	if res.Suspended.NewVersion != 7 || res.Suspended.NewInput != `{"v":2}` {
		t.Errorf("NewVersion=%d NewInput=%q, want 7 and the next input", res.Suspended.NewVersion, res.Suspended.NewInput)
	}
}

// From inside a defer body the outcome is already decided, so a recorded continuation would never be taken: the SDK refuses before the
// host call. The body reports the refusal through a service call; the run completes normally instead of suspending.
func TestCSharpGroupAContinueAsNewIsRefusedInsideADeferBody(t *testing.T) {
	res := runConformanceEntry(t, "a_continue_in_defer", entryRun{})
	m := objectResult(t, res)
	if m["done"] != true {
		t.Fatalf("the run did not complete normally: %v", m)
	}
	got := res.Calls
	if len(got) != 1 || got[0].Service != "svc" || got[0].Operation != "refused" || !strings.Contains(got[0].Request, "defer body") {
		t.Errorf("the service saw %v, want one svc/refused call whose message says it was refused in a defer body", got)
	}
}
