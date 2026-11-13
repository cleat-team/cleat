//go:build cgo

// Q2 (typescript-javy prototype plan): "history is identical across 3 replays".
//
// This loads an IN-FLIGHT run's persisted history through the engine's own store (not a hand-written row mapping),
// replays it three times on the wasmtime backend with the same module and input, and compares what each replay
// produced. A tampered copy of the same history is the red control: if replay accepted that too, "identical" would
// mean nothing. Driven by phase1_replay.sh; skips with a stated reason when the environment is not set.
package replay

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/cleat/wasmtest"
	"github.com/cleat-team/cleat/engine"
	_ "github.com/lib/pq"
)

func need(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Skipf("UNMEASURED: %s is not set (run through phase1_replay.sh)", k)
	}
	return v
}

// shape strips the fields that legitimately differ between two loads of one history (wall-clock stamps), so what is
// compared is the recorded content: step, type, call/signal/child fields, payloads.
func shape(h []engine.EventRecord) string {
	cp := make([]engine.EventRecord, len(h))
	copy(cp, h)
	for i := range cp {
		cp[i].CreatedAt = cp[i].CreatedAt.UTC()
	}
	b, err := json.Marshal(cp)
	if err != nil {
		panic(err)
	}
	return string(b)
}

type outcome struct {
	Result    string
	Suspended string
	History   string
	Err       string
}

func replayOnce(t *testing.T, env *wasmtest.WasmTestEnv, wasm []byte, entry, input string, h []engine.EventRecord) outcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60e9)
	defer cancel()
	res, hist, susp, _, _, err := env.H().Replay(ctx, wasm, entry, json.RawMessage(input), h)
	// Events at or beyond len(h) were recorded BY this replay and carry its wall clock (TimestampMs is "the wall-clock
	// time when the event was recorded"), so two replays legitimately differ there by a millisecond or three. Those
	// stamps are zeroed; every persisted event (index < len(h)) is compared exactly, timestamps included.
	for i := len(h); i < len(hist); i++ {
		hist[i].TimestampMs, hist[i].CreatedAt = 0, time.Time{}
	}
	o := outcome{Result: res, History: shape(hist)}
	if susp != nil {
		o.Suspended = susp.Reason
	}
	if err != nil {
		o.Err = err.Error()
	}
	return o
}

func TestReplayOfAnInFlightJSRunIsIdenticalThreeTimes(t *testing.T) {
	dsn, id := need(t, "REPLAY_DSN"), need(t, "REPLAY_WORKFLOW_ID")
	wasmPath, entry, input := need(t, "REPLAY_WASM"), need(t, "REPLAY_ENTRY"), need(t, "REPLAY_INPUT")
	wasm, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	loaded, err := engine.NewPostgresStore(db).LoadEventHistory(context.Background(), id)
	if err != nil {
		t.Fatalf("loading the persisted history: %v", err)
	}
	t.Logf("loaded %d persisted events for %s: %s", len(loaded), id, types(loaded))
	if len(loaded) < 2 {
		t.Fatalf("UNMEASURED: only %d events loaded; a replay of that proves nothing about suspension points", len(loaded))
	}

	env := wasmtest.NewWasmTestEnv(t)
	defer env.Close()

	var outs [3]outcome
	for i := range outs {
		outs[i] = replayOnce(t, env, wasm, entry, input, loaded)
		t.Logf("replay %d: err=%q suspended=%q resultLen=%d historyEvents=%d", i+1, outs[i].Err, outs[i].Suspended, len(outs[i].Result), strings.Count(outs[i].History, `"step"`))
	}
	if outs[0].Err != "" {
		t.Fatalf("replay 1 of a genuine history failed: %s", outs[0].Err)
	}
	for i := 1; i < 3; i++ {
		if !reflect.DeepEqual(outs[0], outs[i]) {
			t.Errorf("replay %d differs from replay 1 in: %s (result %q/%q, suspended %q/%q)", i+1, fieldDiff(outs[0].History, outs[i].History), outs[0].Result, outs[i].Result, outs[0].Suspended, outs[i].Suspended)
		}
	}
	// The replay must not have rewritten what was recorded: the history it hands back begins with the loaded events.
	if !strings.HasPrefix(strings.TrimSuffix(outs[0].History, "]"), strings.TrimSuffix(shape(loaded), "]")) {
		t.Errorf("replayed history does not begin with the persisted history\n loaded:   %s\n replayed: %s", shape(loaded), outs[0].History)
	}
	t.Logf("IDENTICAL x3: %v", t.Failed() == false)

	genuine := outs[0]
	detected := func(o outcome) bool { return o.Err != "" || !reflect.DeepEqual(o, genuine) }

	// RED CONTROL A: swap one recorded event's type. The module asks for a signal wait/child at that step, so replay
	// must refuse it or visibly diverge. If this replayed exactly like the genuine history, "identical" says nothing.
	swapped := append([]engine.EventRecord(nil), loaded...)
	swapped[1].EventType = "child_workflow"
	a := replayOnce(t, env, wasm, entry, input, swapped)
	t.Logf("RED CONTROL A (step 1 signal_received -> child_workflow): detected=%v err=%q suspended=%q", detected(a), a.Err, a.Suspended)
	if !detected(a) {
		t.Errorf("red control A failed: a history with a swapped event type replayed exactly like the genuine one")
	}

	// RED CONTROL B: remove an event from the middle (the perforated shape the engine's density check is for).
	if len(loaded) >= 3 {
		perforated := append([]engine.EventRecord(nil), loaded[:1]...)
		perforated = append(perforated, loaded[2:]...)
		p := replayOnce(t, env, wasm, entry, input, perforated)
		t.Logf("RED CONTROL B (one event removed from the middle): detected=%v err=%q", detected(p), p.Err)
		if !detected(p) {
			t.Errorf("red control B failed: a perforated history replayed exactly like the genuine one")
		}
	}

	// OBSERVATION, deliberately not a control: rename the recorded child. Whether the engine checks the child's NAME on
	// replay is a property of the engine's child-workflow guard, identical for every guest language. Recorded as found.
	renamed := append([]engine.EventRecord(nil), loaded...)
	for i := range renamed {
		if renamed[i].EventType == "child_workflow" {
			renamed[i].ChildName = "not-the-child-the-module-asked-for"
		}
	}
	r := replayOnce(t, env, wasm, entry, input, renamed)
	t.Logf("OBSERVATION (renamed child, not asserted): detected=%v err=%q", detected(r), r.Err)
}

// fieldDiff names every top-level JSON field that differs, per step, between two shaped histories.
func fieldDiff(a, b string) string {
	var x, y []map[string]any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return "unparseable"
	}
	var out []string
	for i := 0; i < len(x) || i < len(y); i++ {
		if i >= len(x) || i >= len(y) {
			out = append(out, "event "+itoa(i)+" present in only one")
			continue
		}
		for k := range x[i] {
			if !reflect.DeepEqual(x[i][k], y[i][k]) {
				out = append(out, "event "+itoa(i)+" field "+k+": "+toS(x[i][k])+" vs "+toS(y[i][k]))
			}
		}
		for k := range y[i] {
			if _, ok := x[i][k]; !ok {
				out = append(out, "event "+itoa(i)+" field "+k+" only in the second")
			}
		}
	}
	return strings.Join(out, "; ")
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }
func toS(v any) string  { b, _ := json.Marshal(v); return string(b) }

func types(h []engine.EventRecord) string {
	var s []string
	for _, e := range h {
		s = append(s, string(e.EventType))
	}
	return strings.Join(s, ",")
}
