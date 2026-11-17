package typescript

// The continue-as-new binding (cleat#3827, group G). What the HOST does is not what "suspends" suggests: it records the event, sets
// the segment's suspension reason to continue_as_new with the new input, and returns SUCCESS to the guest, which would run on until it
// returns. The binding never lets the promise settle, so these tests read the history (not only the result) for what the guest did
// AFTER the call, and the suspension (reason and new input) for what the host was told.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

const continuationEntries = "continuation,continuation_versioned,continuation_bad_version,continuation_late,continuation_versioned_once"

func buildContinuation(t *testing.T) []byte {
	t.Helper()
	root := needToolchain(t)
	src := "tests/typescript/testdata/workflows/continuation.ts"
	res := build(t, root, src, continuationEntries)
	if res.exit != 0 {
		t.Fatalf("building %s failed (exit %d):\n%s", src, res.exit, res.output)
	}
	wasm, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	return wasm
}

func continuationTypes(h []engine.EventRecord) []string {
	var out []string
	for _, ev := range h {
		out = append(out, string(ev.EventType))
	}
	return out
}

func TestTypeScriptContinueAsNewEndsTheSegmentWithTheNewInputAndRunsNothingAfterIt(t *testing.T) {
	wasm := buildContinuation(t)
	e, closeRT := newEngineWith(t)
	defer closeRT()
	raw, history, susp, _, _, err := e.Execute(context.Background(), wasm, "continuation", json.RawMessage(`{"n":1,"limit":5}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if susp == nil || susp.Reason != "continue_as_new" {
		t.Fatalf("suspension = %+v, want reason continue_as_new", susp)
	}
	// The new input is what the guest passed, JSON-encoded: the worker starts the next run with exactly this text.
	var in map[string]any
	if err := json.Unmarshal([]byte(susp.NewInput), &in); err != nil || in["n"] != float64(2) || in["limit"] != float64(5) {
		t.Fatalf("new input %q, want {n:2, limit:5}", susp.NewInput)
	}
	if raw != "" {
		t.Errorf("the segment returned a result %q: a run that continues as new has no result of its own", raw)
	}
	// NOTHING AFTER THE CALL ran: the host returned success to the guest, so a binding that let the promise settle would have gone on
	// to createPromise and recorded an event for a run that has already been replaced.
	if got := strings.Join(continuationTypes(history), ","); got != "continue_as_new" {
		t.Fatalf("history = %s, want exactly continue_as_new: the guest ran on past a continuation", got)
	}
	if history[0].NewVersion != 0 {
		t.Errorf("the plain form recorded NewVersion %d, want 0 (the current version)", history[0].NewVersion)
	}
}

func TestTypeScriptContinueAsNewIsNotRecordedAgainOnReplay(t *testing.T) {
	wasm := buildContinuation(t)
	e, closeRT := newEngineWith(t)
	defer closeRT()
	input := json.RawMessage(`{"n":1,"limit":5}`)
	_, history, _, _, _, err := e.Execute(context.Background(), wasm, "continuation", input)
	if err != nil {
		t.Fatal(err)
	}
	_, history2, susp, _, _, err := e.Replay(context.Background(), wasm, "continuation", input, history)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if susp == nil || susp.Reason != "continue_as_new" || !strings.Contains(susp.NewInput, `"n":2`) {
		t.Fatalf("replay suspension = %+v, want the same continue_as_new with the recorded input", susp)
	}
	if a, b := strings.Join(continuationTypes(history), ","), strings.Join(continuationTypes(history2), ","); a != b {
		t.Errorf("replay recorded a different history: first %s, replay %s", a, b)
	}
}

func TestTypeScriptContinueAsNewRecordsTheVersionItWasGiven(t *testing.T) {
	wasm := buildContinuation(t)
	e, closeRT := newEngineWith(t)
	defer closeRT()
	_, history, susp, _, _, err := e.Execute(context.Background(), wasm, "continuation_versioned", json.RawMessage(`{"version":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if susp == nil || susp.Reason != "continue_as_new" || susp.NewVersion != 3 {
		t.Fatalf("suspension = %+v, want continue_as_new carrying version 3", susp)
	}
	if len(history) != 1 || history[0].NewVersion != 3 || !strings.Contains(history[0].NewInput, `"carried":true`) {
		t.Fatalf("history = %+v, want one continue_as_new event with version 3 and the carried input", history)
	}
}

func TestTypeScriptAVersionThatCannotBeOneIsRefusedBeforeTheHost(t *testing.T) {
	wasm := buildContinuation(t)
	e, closeRT := newEngineWith(t)
	defer closeRT()
	raw, history, susp, _, _, err := e.Execute(context.Background(), wasm, "continuation_bad_version", json.RawMessage(`{}`))
	if err != nil || susp != nil {
		t.Fatalf("susp=%v err=%v: a refused version must not continue the run", susp, err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"zero", "negative", "fractional", "nan"} {
		if !strings.Contains(got[k], "options.version must be a whole number from 1") {
			t.Errorf("%s: %q, want the version refusal; accepted, it would be truncated or wrapped into another version", k, got[k])
		}
	}
	if len(history) != 0 {
		t.Errorf("a refused version recorded %v: it must not reach the host", continuationTypes(history))
	}
}

// What `guarded` buys, and the strongest case in this PR: a continue_as_new made after the segment suspended would record an event
// AND overwrite the suspension's reason, so the run would restart instead of waiting out its sleep.
func TestTypeScriptAContinueAsNewAfterASuspensionNeitherRecordsNorReplacesTheSuspension(t *testing.T) {
	wasm := buildContinuation(t)
	e, closeRT := newEngineWith(t)
	defer closeRT()
	_, history, susp, _, _, err := e.Execute(context.Background(), wasm, "continuation_late", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if susp == nil || !strings.Contains(susp.Reason, "cleat_sleep(60000ms)") {
		t.Fatalf("suspension = %+v, want the sleep's (cleat_sleep(60000ms)); a continue_as_new reason here means the late call replaced it", susp)
	}
	if len(history) != 0 {
		t.Fatalf("the segment recorded %v AFTER the call that suspended it; cleat_continue_as_new must be guarded", continuationTypes(history))
	}
}
