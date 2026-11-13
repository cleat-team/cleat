//go:build cgo

package xlang

// Q4, the part place_order cannot answer: the events a run records when it WAITS. place_order only makes calls. Here a Go and a
// TypeScript twin each do   sleep -> wait for the signal "go" -> run the child "xsleeper"   and their histories cross in every
// direction: complete histories (signal_received, child_workflow, await_child with its recorded result), histories cut at each
// suspension point, and a run suspended in one language that is RESUMED in the other. All in process (wasmtest), so it needs no
// worker and no database and can run beside anyone else's run.
//
// What is and is not covered is stated in PHASE3.md. In short: a sleep records NO event in either language (measured here, not
// assumed), so there is no sleep state to carry across; resuming a Go run on the TS module is covered for the signal wait and for
// complete histories, not for "child still running, then finishes", which this in-memory child store cannot model.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/cleat/wasmtest"
	"github.com/cleat-team/cleat/engine"
)

const (
	xjGoEntry = "x_journey"
	xjResult  = `{"signal":"hello","slept":1500}`
	xjWF      = "00000000-0000-0000-0000-00000000c0de" // a fixed workflow id: the engine's random/uuid streams are derived from it
)

var runIDRe = regexp.MustCompile(`child-xsleeper-[0-9a-f]{8}`)

type segment struct {
	result string
	hist   []engine.EventRecord
	reason string // the suspension reason, with the child run id masked ("" if the run completed)
	err    error
}

func xjWasm(t *testing.T, lang string) []byte {
	t.Helper()
	dir := os.Getenv("XLANG_DIR")
	if dir == "" {
		t.Skip("UNMEASURED: XLANG_DIR is not set (run through phase3_verify.py)")
	}
	name := map[string]string{"go": "gocontrol.wasm", "ts": "xjourney_ts.wasm"}[lang]
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("UNMEASURED: %v", err)
	}
	return b
}

func xjInput(lang string, sleepMs int) string {
	if lang == "go" {
		return `{"in":{"sleepMs":` + itoa(sleepMs) + `,"childMs":1500}}`
	}
	return `{"sleepMs":` + itoa(sleepMs) + `,"childMs":1500}`
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// run executes (hist == nil) or replays a history on one language's module, in a fresh environment with the fixed workflow id.
// signal delivers "go" to that environment first; the child store completes any child it STARTS with {"slept":1500}, and knows
// nothing about a child that history says was already started (its run id is unknown to it, so it reads as still running).
func run(t *testing.T, lang string, hist []engine.EventRecord, signal bool, sleepMs int) segment {
	t.Helper()
	env := wasmtest.NewWasmTestEnv(t, wasmtest.WithWorkflowID(xjWF), wasmtest.WithDefName("xj-"+lang))
	defer env.Close()
	env.ChildWorkflowStore.SetAutoResult(`{"slept":1500}`)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if signal {
		if err := env.SignalStore.DeliverSignal(ctx, xjWF, "go", `"hello"`); err != nil {
			t.Fatalf("deliver: %v", err)
		}
	}
	var (
		res  string
		h    []engine.EventRecord
		susp *engine.SuspendResult
		err  error
	)
	if hist == nil {
		res, h, susp, _, _, err = env.H().Execute(ctx, xjWasm(t, lang), xjGoEntry, json.RawMessage(xjInput(lang, sleepMs)))
	} else {
		res, h, susp, _, _, err = env.H().Replay(ctx, xjWasm(t, lang), xjGoEntry, json.RawMessage(xjInput(lang, sleepMs)), hist)
	}
	s := segment{result: res, hist: h, err: err}
	if susp != nil {
		s.reason = runIDRe.ReplaceAllString(susp.Reason, "child-xsleeper-<id>")
	}
	return s
}

func shapeOf(h []engine.EventRecord) string {
	var out []string
	for _, e := range h {
		out = append(out, string(e.EventType))
	}
	return strings.Join(out, ",")
}

func TestSuspensionEventsCrossReplay(t *testing.T) {
	langs := []string{"go", "ts"}
	other := map[string]string{"go": "ts", "ts": "go"}

	// 1. The complete run in each language, produced the way a real one is: segment one SUSPENDS at the signal wait (recording the
	// await_signals marker), then the run is resumed with the signal delivered. (With the signal already there, no marker is
	// recorded -- the first version of this test assumed otherwise and its own guard caught it.)
	full := map[string]segment{}
	waiting := map[string]segment{}
	for _, l := range langs {
		waiting[l] = run(t, l, nil, false, 0)
		if waiting[l].err != nil || !strings.Contains(waiting[l].reason, "await_signals") {
			t.Fatalf("%s segment one: expected a suspension at the signal wait, got err=%v suspended=%q", l, waiting[l].err, waiting[l].reason)
		}
		t.Logf("%-3s segment one: suspended=%q events=[%s]", l, waiting[l].reason, shapeOf(waiting[l].hist))
		full[l] = run(t, l, waiting[l].hist, true, 0)
		if full[l].err != nil || full[l].reason != "" {
			t.Fatalf("%s resumed run: err=%v suspended=%q", l, full[l].err, full[l].reason)
		}
		t.Logf("%-3s complete: result=%s events=[%s]", l, full[l].result, shapeOf(full[l].hist))
		if full[l].result != xjResult {
			t.Errorf("%s result %q != %q", l, full[l].result, xjResult)
		}
	}
	if waiting["go"].reason != waiting["ts"].reason || shapeOf(waiting["go"].hist) != shapeOf(waiting["ts"].hist) {
		t.Errorf("segment one differs:\n go: %q [%s]\n ts: %q [%s]", waiting["go"].reason, shapeOf(waiting["go"].hist), waiting["ts"].reason, shapeOf(waiting["ts"].hist))
	}
	if shapeOf(full["go"].hist) != shapeOf(full["ts"].hist) {
		t.Errorf("recorded events differ:\n go: %s\n ts: %s", shapeOf(full["go"].hist), shapeOf(full["ts"].hist))
	}
	for _, want := range []string{"await_signals", "signal_received", "child_workflow", "await_child"} {
		if !strings.Contains(shapeOf(full["go"].hist), want) {
			t.Errorf("UNMEASURED: the complete history has no %s event, so the cross-replay below would not cross it: %s", want, shapeOf(full["go"].hist))
		}
	}

	// 2. COMPLETE histories cross in both directions, and the other language replays to the same bytes.
	for _, from := range langs {
		to := other[from]
		got := run(t, to, full[from].hist, false, 0)
		t.Logf("%-3s history -> %-3s replay: err=%v suspended=%q result=%s", from, to, got.err, got.reason, got.result)
		if got.err != nil || got.reason != "" || got.result != xjResult {
			t.Errorf("%s history on the %s module: err=%v suspended=%q result=%q", from, to, got.err, got.reason, got.result)
		}
	}

	// 3. SUSPENSION POINTS. Each history cut at a wait is replayed on BOTH languages; all four reasons must be one string.
	cuts := []struct {
		name string
		keep string // event type of the last event kept
	}{{"signal wait", "await_signals"}, {"child wait", "child_workflow"}}
	for _, c := range cuts {
		reasons := map[string]string{}
		for _, from := range langs {
			cut := truncateAfter(full[from].hist, c.keep)
			if len(cut) == len(full[from].hist) || len(cut) == 0 {
				t.Fatalf("UNMEASURED: could not cut the %s history at %s: kept %d of %d", from, c.keep, len(cut), len(full[from].hist))
			}
			for _, to := range langs {
				s := run(t, to, cut, false, 0)
				if s.err != nil || s.reason == "" {
					t.Errorf("cut at %s, %s history on %s: expected a suspension, got err=%v result=%q", c.name, from, to, s.err, s.result)
				}
				reasons[from+"->"+to] = s.reason
			}
		}
		t.Logf("%-11s reasons: %v", c.name, reasons)
		first := reasons["go->go"]
		for k, v := range reasons {
			if v != first {
				t.Errorf("the %s suspends differently: %s gives %q, go->go gives %q", c.name, k, v, first)
			}
		}
	}

	// 4. A run suspended at the signal wait in one language is RESUMED in the other: the signal arrives in the second language's
	// environment and the run continues, new events and all, to the same result.
	for _, from := range langs {
		to := other[from]
		waiting := truncateAfter(full[from].hist, "await_signals")
		got := run(t, to, waiting, true, 0)
		t.Logf("suspended in %-3s at the signal wait, signal delivered, resumed in %-3s: err=%v suspended=%q result=%s", from, to, got.err, got.reason, got.result)
		if got.err != nil || got.reason != "" || got.result != xjResult {
			t.Errorf("resume %s -> %s: err=%v suspended=%q result=%q", from, to, got.err, got.reason, got.result)
		}
	}

	// 5. A SLEEP records nothing, in either language: same suspension reason, no events to carry.
	sl := map[string]segment{}
	for _, l := range langs {
		sl[l] = run(t, l, nil, false, 300)
		t.Logf("%-3s with a 300 ms sleep: suspended=%q events=%d", l, sl[l].reason, len(sl[l].hist))
		if !strings.Contains(sl[l].reason, "sleep") || len(sl[l].hist) != 0 {
			t.Errorf("%s: expected a sleep suspension with an empty history, got suspended=%q events=%d", l, sl[l].reason, len(sl[l].hist))
		}
	}
	if sl["go"].reason != sl["ts"].reason {
		t.Errorf("the sleep suspends differently: go %q, ts %q", sl["go"].reason, sl["ts"].reason)
	}

	// 6. RED CONTROL: a complete history with its child_workflow record retyped must be REFUSED, with a replay divergence, by BOTH
	// languages, or 2 and 4 prove nothing. (child_workflow is the record whose wrong-type guard reports; cleat#1506.)
	for _, from := range langs {
		tam := append([]engine.EventRecord(nil), full[from].hist...)
		idx := -1
		for i, e := range tam {
			if e.EventType == "child_workflow" {
				idx = i
			}
		}
		if idx < 0 {
			t.Fatalf("UNMEASURED: no child_workflow event to tamper with")
		}
		tam[idx].EventType = "signal_received"
		for _, to := range langs {
			s := run(t, to, tam, false, 0)
			t.Logf("%-3s history with child_workflow -> signal_received, replayed on %-3s: refused=%v", from, to, s.err != nil)
			if s.err == nil {
				t.Errorf("a tampered %s history was not refused on %s (suspended=%q result=%q)", from, to, s.reason, s.result)
			} else if !strings.Contains(s.err.Error(), "divergence") {
				t.Errorf("refused, but not as a replay divergence (%s on %s): %.300s", from, to, s.err.Error())
			}
		}
	}

	// OBSERVATION, deliberately NOT asserted. Retyping signal_received is not a divergence: the engine reads [await_signals, <anything
	// that is not a delivery>] as "the wait TIMED OUT" (engine/signaller.go, cleat#947), which is a valid history. The first version
	// of this test used it as its red control; a tightened assertion showed it was never refused, in either language, and the loose
	// version had only passed because the result happened to differ. What each language does with that "timed out" history differs
	// (Go suspends; TS fails a step later on an empty child run id), which is a guest-side difference, not a replay refusal.
	for _, from := range langs {
		tam := append([]engine.EventRecord(nil), full[from].hist...)
		for i, e := range tam {
			if e.EventType == "signal_received" {
				tam[i].EventType = "child_workflow"
			}
		}
		for _, to := range langs {
			s := run(t, to, tam, false, 0)
			t.Logf("OBSERVATION %-3s history with signal_received -> child_workflow on %-3s: err=%v suspended=%q result=%q", from, to, s.err != nil, s.reason, s.result)
		}
	}
}

// truncateAfter keeps events up to and including the first of the given type.
func truncateAfter(h []engine.EventRecord, typ string) []engine.EventRecord {
	for i, e := range h {
		if string(e.EventType) == typ {
			return append([]engine.EventRecord(nil), h[:i+1]...)
		}
	}
	return h
}
