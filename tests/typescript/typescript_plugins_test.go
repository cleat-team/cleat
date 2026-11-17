package typescript

// The plugin-call bindings, cleat#3827 group I: `plugin_call`, `plugin_call_streaming` and `cleat_wait_for_event`.
//
// These run a BUILT module against two registries this file configures, so every answer here came from the script
// below and not from a plugin. What that buys is the binding: a binding that decoded the wrong field, or reached the
// wrong registry, would return a plausible value rather than an error.
//
// `cleat_wait_for_event` has TWO exits and both are tested, because they are decided by the same claim loop: a
// scripted function reporting `found: true` must return the event, and one reporting `found: false` with a zero budget
// must surface the host's timeout as a rejection. A test of the first alone passes against a loop that never gives up.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
)

// pluginScripts is what the two registries answer, and what they were asked.
type pluginScripts struct {
	// claimFound decides demo/claim's answer: true reports a claimable event, false reports none.
	claimFound bool
}

func (s *pluginScripts) options(t *testing.T) engine.EngineOption {
	t.Helper()
	pr := engine.NewPluginRegistry()
	psr := engine.NewPluginStreamRegistry()
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("registering %s: %v", what, err)
		}
	}
	must("demo/echo", pr.Register("demo", "echo", func(_ context.Context, inputJSON string) (string, error) {
		return `{"said":"hello","input":` + inputJSON + `}`, nil
	}))
	must("demo/claim", pr.Register("demo", "claim", func(_ context.Context, _ string) (string, error) {
		if s.claimFound {
			return `{"found":true,"payload":"e1"}`, nil
		}
		return `{"found":false}`, nil
	}))
	// A chunk list is registered on the STREAM registry only: registering it on the plain one as well would answer
	// plugin_call for a function whose scripted answer is a stream.
	must("demo/stream", psr.Register("demo", "stream", func(_ context.Context, _ string) (<-chan plugin.StreamEvent, error) {
		ch := make(chan plugin.StreamEvent, 2)
		ch <- plugin.StreamEvent{Index: 0, Content: "a"}
		ch <- plugin.StreamEvent{Index: 1, Content: "b", Finish: true}
		close(ch)
		return ch, nil
	}))
	return func(e *engine.Engine) {
		engine.WithPluginRegistry(pr)(e)
		engine.WithPluginStreamRegistry(psr)(e)
	}
}

// buildAndRun builds the plugins fixture for one entry and runs it with the scripted registries.
func buildAndRun(t *testing.T, entry string, found bool) (result string, history []engine.EventRecord, susp *engine.SuspendResult) {
	t.Helper()
	root := needToolchain(t)
	src := "tests/typescript/testdata/workflows/plugins.ts"
	res := build(t, root, src, entry)
	if res.exit != 0 {
		t.Fatalf("building %s for %s failed (exit %d):\n%s", src, entry, res.exit, res.output)
	}
	wasm, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	scripts := &pluginScripts{claimFound: found}
	e, closeRT := newEngineWith(t, scripts.options(t))
	defer closeRT()
	runResult, runHistory, runSusp, _, _, err := e.Execute(context.Background(), wasm, entry, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("running %s: %v", entry, err)
	}
	return runResult, runHistory, runSusp
}

func TestTypeScriptPluginCallReturnsTheScriptedAnswer(t *testing.T) {
	result, _, _ := buildAndRun(t, "plugin_calls", true)
	var got struct {
		Echo struct {
			Said  string         `json:"said"`
			Input map[string]int `json:"input"`
		} `json:"echo"`
		// The streaming binding returns the CHUNK ARRAY as the host emits it, not a joined string: each chunk carries
		// its index `i`, content `c` and finish flag `f`. Measured, not assumed -- the first draft of this test
		// expected a joined answer and the workflow returned this instead.
		Streamed []struct {
			Index   int    `json:"i"`
			Content string `json:"c"`
			Finish  bool   `json:"f"`
		} `json:"streamed"`
	}
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("the workflow returned %s, which is not the expected shape: %v", result, err)
	}
	if got.Echo.Said != "hello" {
		t.Errorf("echo.said = %q, want the scripted answer", got.Echo.Said)
	}
	// The input must arrive as the workflow's own JSON, encoded by h.encode: a binding that passed the object's own
	// toString would send "[object Object]" and this would catch it.
	if got.Echo.Input["n"] != 1 {
		t.Errorf("the plugin saw input %v, want the encoded argument", got.Echo.Input)
	}
	if len(got.Streamed) != 2 || got.Streamed[0].Content != "a" || got.Streamed[1].Content != "b" {
		t.Fatalf("streamed = %+v, want the two scripted chunks in order", got.Streamed)
	}
	if !got.Streamed[1].Finish {
		t.Error("the last chunk is not the finish chunk; the binding dropped the flag")
	}
}

func TestTypeScriptWaitForEventReturnsAClaimedEvent(t *testing.T) {
	result, _, _ := buildAndRun(t, "plugin_wait_found", true)
	var got struct {
		Event struct {
			Found   bool   `json:"found"`
			Payload string `json:"payload"`
		} `json:"event"`
	}
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("the workflow returned %s: %v", result, err)
	}
	if !got.Event.Found || got.Event.Payload != "e1" {
		t.Fatalf("event = %+v, want the scripted claim", got.Event)
	}
}

func TestTypeScriptWaitForEventTimesOut(t *testing.T) {
	result, _, _ := buildAndRun(t, "plugin_wait_times_out", false)
	var got struct{ Message string }
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("the workflow returned %s: %v", result, err)
	}
	// The host reports a timeout as a CALL FAILURE, so the binding must reject rather than resolve with a flag. The
	// message is the host's own, which is the convention every other error follows.
	if !strings.Contains(got.Message, "no event for demo/claim") {
		t.Fatalf("message = %q, want the host's timeout message; a wait that resolved instead would leave this empty", got.Message)
	}
}

// The late-call guard, in the shape of TestTypeScriptACallAfterASuspensionRecordsNothing: a segment records nothing
// past the call that suspended it. Both plugin bindings are exercised, because they are two separate `guarded` calls
// and removing it from one leaves the other passing.
func TestTypeScriptAPluginCallAfterASuspensionRecordsNothing(t *testing.T) {
	_, history, susp := buildAndRun(t, "plugin_late", true)
	if susp == nil {
		t.Fatal("the workflow did not suspend; ctx.sleep(60000) is supposed to suspend the segment")
	}
	if !strings.Contains(susp.Reason, "cleat_sleep(60000ms)") {
		t.Fatalf("suspended for %q, not at the sleep; the ordering below is then unmeasured", susp.Reason)
	}
	for _, ev := range history {
		if ev.EventType == engine.EventTypePluginCall || ev.EventType == engine.EventTypePluginCallStreamChunk {
			t.Fatalf("the segment recorded a plugin event (%+v) AFTER the call that suspended it; the binding must be guarded", ev)
		}
	}
}

// An update that arrives while the workflow is blocked in `waitForEvent` must reach the guest's handler.
//
// This is the test that would fail without `dispatchThen` around the wait, and it is the one no scripted registry can
// substitute for: the update is delivered at a fixed program position, so what is under test is WHERE the dispatch
// happens, not whether a registry answers. The precondition is stated first -- the workflow really did suspend inside the
// wait -- because a run that never reached the wait would leave the store untouched for a reason that has nothing to do
// with the dispatch.
func TestTypeScriptAnUpdateIsDeliveredWhileBlockedInWaitForEvent(t *testing.T) {
	root := needToolchain(t)
	src := "tests/typescript/testdata/workflows/plugins.ts"
	res := build(t, root, src, "plugin_wait_update")
	if res.exit != 0 {
		t.Fatalf("building %s for plugin_wait_update failed (exit %d):\n%s", src, res.exit, res.output)
	}
	wasm, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}

	updates := newMemUpdates([2]string{"add", `{"n":5}`})
	// claimFound=false: the scripted claim reports no event, so the wait registers and the segment suspends inside it.
	scripts := &pluginScripts{claimFound: false}
	e, closeRT := newEngineWith(t, scripts.options(t), engine.WithUpdateStore(updates))
	defer closeRT()

	_, _, susp, _, _, err := e.Execute(context.Background(), wasm, "plugin_wait_update", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("running plugin_wait_update: %v", err)
	}
	if susp == nil {
		t.Fatal("the workflow did not suspend; the wait was supposed to register and suspend, so the dispatch below is unmeasured")
	}

	// The delimiter, and the whole point: the update was pending when the workflow entered the wait, and it must have been
	// HANDLED -- completed, with its handler's result -- before the wait began.
	r := updates.row(0)
	if !r.completed {
		t.Fatalf("the update pending as the workflow entered waitForEvent was never completed: it must run through "+
			"dispatchThen as sleep and waitForSignal do, or the handler is asked for it and never gets it (store: %v)", updates.complete)
	}
	if r.result != `{"total":5}` || r.errMsg != "" {
		t.Fatalf("the update completed with result %q, error %q, want its handler's answer and no error", r.result, r.errMsg)
	}
	if n := updates.pending(); n != 0 {
		t.Fatalf("%d update(s) still pending: the dispatch point in front of waitForEvent did not run", n)
	}
}
