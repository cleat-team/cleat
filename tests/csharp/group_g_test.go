//go:build cgo

package csharp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// Group G of cleat#4080's host-call coverage: fetch, side-effect and durable-run-detached, each driven through the real engine and wasmtime
// backend by a named entry of the conformance component (packages/cleat-csharp/conformance/GroupG.cs). Nothing here claims C# is supported.

type fetchCall struct{ Method, URL, Headers, Body string }

type fakeFetcher struct {
	mu    sync.Mutex
	calls []fetchCall
	reply func(c fetchCall) (string, error)
}

func (f *fakeFetcher) Fetch(_ context.Context, method, url, headers, body string) (string, error) {
	f.mu.Lock()
	c := fetchCall{method, url, headers, body}
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	return f.reply(c)
}

func (f *fakeFetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func okReply(string) func(fetchCall) (string, error) {
	return func(fetchCall) (string, error) { return `{"status":200,"headers":{},"body":""}`, nil }
}

func withFetcher(f *fakeFetcher) []engine.EngineOption {
	return []engine.EngineOption{engine.WithFetcher(f)}
}

func TestCSharpGroupGFetchSendsTheRequestAndDecodesTheReply(t *testing.T) {
	f := &fakeFetcher{reply: func(fetchCall) (string, error) {
		return `{"status":201,"headers":{"X-Id":"7","Content-Type":"text/plain"},"body":"made"}`, nil
	}}
	in := `{"url":"https://example.test/items","method":"POST","headers":{"Authorization":"Bearer t","X-Note":"a\"b"},"body":"{\"n\":1}"}`
	m := mustFinish(t, runConformanceEntry(t, "g_fetch", entryRun{Input: in, Options: withFetcher(f)}))
	if m["status"] != float64(201) || m["body"] != "made" {
		t.Fatalf("got %v, want status 201 and body made", m)
	}
	h := m["headers"].(map[string]any)
	if h["X-Id"] != "7" || h["Content-Type"] != "text/plain" {
		t.Errorf("headers = %v, want the reply's headers", h)
	}
	if f.count() != 1 {
		t.Fatalf("the fetcher was called %d times, want 1", f.count())
	}
	c := f.calls[0]
	if c.Method != "POST" || c.URL != "https://example.test/items" || c.Body != `{"n":1}` {
		t.Errorf("the fetcher saw %+v, want POST, the URL and the body unchanged", c)
	}
	// The request headers cross as one JSON object, with the quote in X-Note escaped.
	if !strings.Contains(c.Headers, `"Authorization":"Bearer t"`) || !strings.Contains(c.Headers, `"X-Note":"a\"b"`) {
		t.Errorf("request headers = %s, want both headers as JSON with the quote escaped", c.Headers)
	}
}

// A reply that is valid JSON but not an object (a string, an array, a number) is an error, not an empty response (cleat-review2: removing the
// ValueKind check left every other fetch test green). The host's own fetcher always answers an object; an embedder's may not.
func TestCSharpGroupGFetchAReplyThatIsValidJSONButNotAnObjectIsAnError(t *testing.T) {
	for _, reply := range []string{`"hello"`, `[1,2]`, `42`, `null`, `true`} {
		f := &fakeFetcher{reply: func(fetchCall) (string, error) { return reply, nil }}
		m := mustFinish(t, runConformanceEntry(t, "g_fetch", entryRun{Input: `{"url":"https://example.test/"}`, Options: withFetcher(f)}))
		if m["failed"] != true || m["operation"] != "fetch" || !strings.Contains(fmt.Sprint(m["message"]), "not a JSON object") {
			t.Errorf("reply %s: got %v, want an error saying the reply is not a JSON object", reply, m)
		}
	}
}

// Defaults: GET, no headers (an empty JSON object, which is what the worker's fetcher parses), empty body.
func TestCSharpGroupGFetchDefaultsToAGetWithNoHeadersOrBody(t *testing.T) {
	f := &fakeFetcher{reply: okReply("")}
	mustFinish(t, runConformanceEntry(t, "g_fetch", entryRun{Input: `{"url":"https://example.test/"}`, Options: withFetcher(f)}))
	if f.count() != 1 {
		t.Fatalf("the fetcher was called %d times, want 1", f.count())
	}
	if got := f.calls[0]; got.Method != "GET" || got.Headers != "{}" || got.Body != "" {
		t.Errorf("the fetcher saw %+v, want GET, {} and an empty body", got)
	}
}

// A non-2xx status is a response, not an exception.
func TestCSharpGroupGFetchNon2xxIsAResponseNotAnError(t *testing.T) {
	f := &fakeFetcher{reply: func(fetchCall) (string, error) { return `{"status":404,"headers":{},"body":"nope"}`, nil }}
	m := mustFinish(t, runConformanceEntry(t, "g_fetch", entryRun{Input: `{"url":"https://example.test/x"}`, Options: withFetcher(f)}))
	if m["failed"] != nil || m["status"] != float64(404) || m["body"] != "nope" {
		t.Errorf("got %v, want status 404 with its body and no failure", m)
	}
}

// A reply that carries no usable status reports 0, never 200 (cleat#3413): the SDK must not invent a success the host did not report.
func TestCSharpGroupGFetchNeverDefaultsAMissingStatusTo200(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		want        float64
	}{
		{"status_code fallback", `{"status_code":503,"body":"x"}`, 503},
		{"status wins over status_code", `{"status":200,"status_code":503,"body":"x"}`, 200},
		{"no status at all", `{"body":"x"}`, 0},
		{"status is a string", `{"status":"200","body":"x"}`, 0},
		{"status is a boolean", `{"status":true,"body":"x"}`, 0},
		{"status is a fraction", `{"status":200.5,"body":"x"}`, 0},
	} {
		f := &fakeFetcher{reply: func(fetchCall) (string, error) { return tc.reply, nil }}
		m := mustFinish(t, runConformanceEntry(t, "g_fetch", entryRun{Input: `{"url":"https://example.test/"}`, Options: withFetcher(f)}))
		if m["status"] != tc.want {
			t.Errorf("%s: status = %v, want %v", tc.name, m["status"], tc.want)
		}
	}
}

func TestCSharpGroupGFetchAFetcherFailureIsAnErrorCarryingTheHostsText(t *testing.T) {
	f := &fakeFetcher{reply: func(fetchCall) (string, error) { return "", errors.New("egress denied for example.test") }}
	m := mustFinish(t, runConformanceEntry(t, "g_fetch", entryRun{Input: `{"url":"https://example.test/"}`, Options: withFetcher(f)}))
	// The exception CLASS and message, never the code: the code of a simple-packed failure is garbage (cleat#4107).
	if m["failed"] != true || !strings.HasPrefix(fmt.Sprint(m["type"]), "CleatCall") || m["service"] != "http" || m["operation"] != "fetch" ||
		!strings.Contains(fmt.Sprint(m["message"]), "egress denied") {
		t.Errorf("got %v, want a CleatCall exception for http/fetch carrying the egress message", m)
	}
}

// With no fetcher the host says so and names the flag: the message is what an operator reads, so the SDK must not replace it.
func TestCSharpGroupGFetchWithNoFetcherNamesTheFlag(t *testing.T) {
	m := mustFinish(t, runConformanceEntry(t, "g_fetch", entryRun{Input: `{"url":"https://example.test/"}`}))
	if m["failed"] != true || !strings.Contains(fmt.Sprint(m["message"]), "--enable-workflow-fetch") {
		t.Errorf("got %v, want an error whose message names --enable-workflow-fetch", m)
	}
}

// A reply that is not a JSON object is raised with its text; it is not read as an empty 0-status response.
func TestCSharpGroupGFetchAReplyThatIsNotJSONIsAnErrorNotAnEmptyResponse(t *testing.T) {
	f := &fakeFetcher{reply: func(fetchCall) (string, error) { return "<html>gateway timeout</html>", nil }}
	m := mustFinish(t, runConformanceEntry(t, "g_fetch", entryRun{Input: `{"url":"https://example.test/"}`, Options: withFetcher(f)}))
	if m["failed"] != true || !strings.Contains(fmt.Sprint(m["message"]), "gateway timeout") || m["status"] != nil {
		t.Errorf("got %v, want an error carrying the reply's text", m)
	}
}

// The host refuses a method that is not a valid name before the fetcher is reached.
func TestCSharpGroupGFetchAnInvalidMethodNeverReachesTheFetcher(t *testing.T) {
	f := &fakeFetcher{reply: okReply("")}
	m := mustFinish(t, runConformanceEntry(t, "g_fetch", entryRun{Input: `{"url":"https://example.test/","method":"GET /x HTTP/1.1"}`, Options: withFetcher(f)}))
	if m["failed"] != true {
		t.Errorf("got %v, want the invalid method refused", m)
	}
	if f.count() != 0 {
		t.Errorf("the fetcher was called %d time(s) for a refused method", f.count())
	}
}

// Replay returns the RECORDED response and sends nothing: the fetcher is made to answer differently for the replay, and is counted.
func TestCSharpGroupGFetchReplayReturnsTheRecordedResponseWithoutAskingTheFetcher(t *testing.T) {
	in := `{"url":"https://example.test/r"}`
	first := &fakeFetcher{reply: func(fetchCall) (string, error) { return `{"status":200,"headers":{},"body":"recorded"}`, nil }}
	fresh := runConformanceEntry(t, "g_fetch", entryRun{WorkflowID: "wf-g-1", Input: in, Options: withFetcher(first)})
	if mustFinish(t, fresh)["body"] != "recorded" {
		t.Fatal("the fresh run did not return the reply")
	}
	second := &fakeFetcher{reply: func(fetchCall) (string, error) { return `{"status":500,"headers":{},"body":"LIVE"}`, nil }}
	replay := runConformanceEntry(t, "g_fetch", entryRun{WorkflowID: "wf-g-1", Input: in, Replay: fresh.History, Options: withFetcher(second)})
	if m := mustFinish(t, replay); m["body"] != "recorded" || m["status"] != float64(200) {
		t.Errorf("the replay returned %v, want the recorded response", m)
	}
	if second.count() != 0 {
		t.Errorf("the replay asked the fetcher %d time(s), want none", second.count())
	}
}

func TestCSharpGroupGSideEffectRecordsTheValueAndReturnsIt(t *testing.T) {
	res := runConformanceEntry(t, "g_side_effect", entryRun{WorkflowID: "wf-g-2", Input: `{"value":"{\"n\":42}"}`})
	if mustFinish(t, res)["recorded"] != `{"n":42}` {
		t.Fatalf("got %s, want the value back", res.Result)
	}
	rec := groupGRecord(t, res, engine.EventTypeSideEffect)
	if rec.SideEffectResult != `{"n":42}` {
		t.Errorf("the recorded side effect is %q, want the value", rec.SideEffectResult)
	}
}

func TestCSharpGroupGSideEffectReplayOfTheSameValueReturnsIt(t *testing.T) {
	in := `{"value":"stable"}`
	fresh := runConformanceEntry(t, "g_side_effect", entryRun{WorkflowID: "wf-g-3", Input: in})
	replay := runConformanceEntry(t, "g_side_effect", entryRun{WorkflowID: "wf-g-3", Input: in, Replay: fresh.History})
	if mustFinish(t, replay)["recorded"] != "stable" {
		t.Errorf("the replay returned %s, want the recorded value", replay.Result)
	}
}

// cleat#4113: on a replay the host returns the RECORDED value whatever the guest passes, so a value computed from a clock cannot make a replay
// fail. (Until #4119 this test pinned the opposite: the host failed the replay with a divergence naming both values.)
func TestCSharpGroupGSideEffectReplayOfADifferentValueReturnsTheRecordedOne(t *testing.T) {
	fresh := runConformanceEntry(t, "g_side_effect", entryRun{WorkflowID: "wf-g-4", Input: `{"value":"first"}`})
	replay := runConformanceEntry(t, "g_side_effect", entryRun{WorkflowID: "wf-g-4", Input: `{"value":"second"}`, Replay: fresh.History})
	m := mustFinish(t, replay)
	if m["failed"] == true || m["recorded"] != "first" {
		t.Errorf("got %v, want the recorded value \"first\" and no failure", m)
	}
}

// The function form runs its function once, on the first execution, and never on a replay.
func TestCSharpGroupGSideEffectFunctionRunsOnceAndNeverOnReplay(t *testing.T) {
	fresh := runConformanceEntry(t, "g_side_effect_once", entryRun{WorkflowID: "wf-g-8", Input: `{"value":"frozen"}`})
	if m := mustFinish(t, fresh); m["recorded"] != "frozen" || m["ran"] != float64(1) {
		t.Fatalf("first run: got %v, want recorded=frozen after the function ran once", m)
	}
	if rec := groupGRecord(t, fresh, engine.EventTypeSideEffect); rec.SideEffectResult != "frozen" {
		t.Errorf("the recorded side effect is %q, want the function's value", rec.SideEffectResult)
	}
	// The replay's function would compute something else; the recorded value must win and the function must not run.
	replay := runConformanceEntry(t, "g_side_effect_once", entryRun{WorkflowID: "wf-g-8", Input: `{"value":"DIFFERENT"}`, Replay: fresh.History})
	if m := mustFinish(t, replay); m["recorded"] != "frozen" || m["ran"] != float64(0) {
		t.Errorf("replay: got %v, want the recorded value with the function run 0 times", m)
	}
}

// A history whose record at this step is a different event is a divergence the SDK raises, and the function must NOT run: the lookup fails
// before it (cleat-review2's design: step 0 is a foreign event type). This is what keeps the failure path pinned now that a changed value no
// longer fails.
func TestCSharpGroupGSideEffectOverAForeignEventIsADivergenceAndTheFunctionDoesNotRun(t *testing.T) {
	st := &fakeDetachedStore{id: "run-x"}
	foreign := runConformanceEntry(t, "g_run_detached", entryRun{WorkflowID: "wf-g-9", Input: `{"name":"audit.report","input":"{}"}`, Options: withChildStore(st)})
	groupGRecord(t, foreign, engine.EventTypeRunDetached)

	for _, entry := range []string{"g_side_effect", "g_side_effect_once"} {
		replay := runConformanceEntry(t, entry, entryRun{WorkflowID: "wf-g-9", Input: `{"value":"v"}`, Replay: foreign.History})
		m := mustFinish(t, replay)
		if m["failed"] != true || m["operation"] != "side_effect" || !strings.Contains(fmt.Sprint(m["message"]), "replay divergence at step 0") {
			t.Errorf("%s over a run_detached history: got %v, want a divergence raised for side_effect", entry, m)
		}
		if m["recorded"] != nil {
			t.Errorf("%s returned %v despite the divergence", entry, m["recorded"])
		}
		if entry == "g_side_effect_once" && m["ran"] != float64(0) {
			t.Errorf("the function ran %v time(s) over a foreign event, want 0", m["ran"])
		}
	}
}

// fakeDetachedStore is the one method run-detached uses. The rest of the interface is the embedded nil value.
type fakeDetachedStore struct {
	engine.ChildWorkflowStore
	mu    sync.Mutex
	calls []string
	id    string
}

func (f *fakeDetachedStore) StartChildWorkflow(_ context.Context, parentID, defName, inputJSON string, defVersion int, _ string, _ int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s|%s|%s|v%d", parentID, defName, inputJSON, defVersion))
	return f.id, nil
}

func (f *fakeDetachedStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func withChildStore(f *fakeDetachedStore) []engine.EngineOption {
	return []engine.EngineOption{engine.WithChildWorkflowStore(f)}
}

func TestCSharpGroupGRunDetachedStartsTheRunAndRecordsItsId(t *testing.T) {
	st := &fakeDetachedStore{id: "run-from-the-store"}
	res := runConformanceEntry(t, "g_run_detached", entryRun{WorkflowID: "wf-g-5", Input: `{"name":"audit.report","input":"{\"k\":1}"}`, Options: withChildStore(st)})
	if mustFinish(t, res)["started"] != true {
		t.Fatalf("got %s", res.Result)
	}
	if st.count() != 1 || st.calls[0] != `wf-g-5|audit.report|{"k":1}|v0` {
		t.Errorf("the store saw %v, want one start of audit.report by wf-g-5 with the input unchanged", st.calls)
	}
	rec := groupGRecord(t, res, engine.EventTypeRunDetached)
	if rec.DetachedName != "audit.report" || rec.DetachedRunID != "run-from-the-store" || rec.DetachedInput != `{"k":1}` {
		t.Errorf("the recorded run is %+v, want the name, the input and the id the store issued", rec)
	}
}

// A name that is not a valid name is refused before the store is reached, as a host status the SDK raises.
func TestCSharpGroupGRunDetachedARefusedNameNeverReachesTheStore(t *testing.T) {
	st := &fakeDetachedStore{id: "x"}
	m := mustFinish(t, runConformanceEntry(t, "g_run_detached", entryRun{Input: `{"name":"bad name","input":"{}"}`, Options: withChildStore(st)}))
	if m["failed"] != true || m["operation"] != "run-detached" {
		t.Errorf("got %v, want a refusal for run-detached", m)
	}
	if st.count() != 0 {
		t.Errorf("the store was asked %d time(s) for a refused name", st.count())
	}
}

// Replay returns from the record and does not start the run a second time.
func TestCSharpGroupGRunDetachedReplayStartsNothing(t *testing.T) {
	in := `{"name":"audit.report","input":"{}"}`
	st := &fakeDetachedStore{id: "r1"}
	fresh := runConformanceEntry(t, "g_run_detached", entryRun{WorkflowID: "wf-g-6", Input: in, Options: withChildStore(st)})
	groupGRecord(t, fresh, engine.EventTypeRunDetached)
	again := &fakeDetachedStore{id: "r2"}
	replay := runConformanceEntry(t, "g_run_detached", entryRun{WorkflowID: "wf-g-6", Input: in, Replay: fresh.History, Options: withChildStore(again)})
	if mustFinish(t, replay)["started"] != true {
		t.Fatal("the replay did not complete")
	}
	if again.count() != 0 {
		t.Errorf("the replay started the run %d time(s), want none", again.count())
	}
}

// A history whose record at this step is a detached run of a DIFFERENT name is a divergence the SDK raises.
func TestCSharpGroupGRunDetachedReplayOfADifferentNameIsAnError(t *testing.T) {
	st := &fakeDetachedStore{id: "r1"}
	fresh := runConformanceEntry(t, "g_run_detached", entryRun{WorkflowID: "wf-g-7", Input: `{"name":"one","input":"{}"}`, Options: withChildStore(st)})
	res := runConformanceEntry(t, "g_run_detached", entryRun{WorkflowID: "wf-g-7", Input: `{"name":"two","input":"{}"}`, Replay: fresh.History, Options: withChildStore(&fakeDetachedStore{id: "r2"})})
	m := mustFinish(t, res)
	if m["failed"] != true || m["operation"] != "run-detached" {
		t.Errorf("got %v, want a divergence raised for run-detached", m)
	}
}

// groupGRecord returns the one history record of this type, failing if there is not exactly one. (Group F has the same helper under another
// name; they merge into one when both are on develop, and the two names cannot collide meanwhile.)
func groupGRecord(t *testing.T, res conformanceResult, typ engine.EventType) engine.EventRecord {
	t.Helper()
	var got []engine.EventRecord
	for _, r := range res.History {
		if r.EventType == typ {
			got = append(got, r)
		}
	}
	if len(got) != 1 {
		t.Fatalf("history has %d %s record(s), want exactly 1: %+v", len(got), typ, res.History)
	}
	return got[0]
}
