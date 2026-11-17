package typescript

// The detached-group bindings (cleat#3827, group D): start a workflow nobody waits for.
//
// THE STORE IS THE MEASUREMENT, not the workflow's return value. A detached start goes through the same
// ChildWorkflowStore method a child does (engine/children.go: runDetached calls StartChildWorkflow), so the store sees it and
// can say whether one was created, under what name, and whether anything then waited for it. The store scripts NOTHING, so
// every run it mints stays running: a binding that awaited the run it started would suspend, and the tests below assert the
// absence of that suspension rather than trusting a result that would look identical either way.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

type memDetachedRun struct {
	defName string
	outcome engine.ChildOutcome
}

// memDetached is an engine.ChildWorkflowStore that records every start and never completes one.
type memDetached struct {
	mu      sync.Mutex
	byID    map[string]*memDetachedRun
	next    int
	started []string
}

func newMemDetached() *memDetached {
	return &memDetached{byID: map[string]*memDetachedRun{}}
}

func (d *memDetached) start(childID, defName string) {
	d.byID[childID] = &memDetachedRun{defName: defName, outcome: engine.ChildOutcome{Completed: false}}
	d.started = append(d.started, defName)
}

func (d *memDetached) StartChildWorkflow(_ context.Context, _, defName, _ string, _ int, _ string, _ int) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.next++
	id := fmt.Sprintf("child-%d", d.next)
	d.start(id, defName)
	return id, nil
}

func (d *memDetached) StartChildWorkflowAtomic(_ context.Context, childID, _, defName, _ string, _ int, _ string, _ engine.EventRecord, _ int) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if childID == "" {
		d.next++
		childID = fmt.Sprintf("child-%d", d.next)
	}
	d.start(childID, defName)
	return childID, nil
}

func (d *memDetached) GetChildResult(_ context.Context, runID string) (engine.ChildOutcome, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	row, ok := d.byID[runID]
	if !ok {
		return engine.ChildOutcome{}, fmt.Errorf("unknown child run id %q", runID)
	}
	return row.outcome, nil
}

func (d *memDetached) GetChildCompletedAtMs(_ context.Context, runID string) (int64, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.byID[runID]; !ok {
		return 0, false, fmt.Errorf("unknown child run id %q", runID)
	}
	return 0, false, nil
}

func (d *memDetached) ResolveVersionByTag(_ context.Context, name, tag string) (int, error) {
	return 0, fmt.Errorf("no version tags exist in this harness (asked %q for %q)", name, tag)
}

func (d *memDetached) startedList() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.started...)
}

func buildDetached(t *testing.T) []byte {
	t.Helper()
	root := needToolchain(t)
	src := "tests/typescript/testdata/workflows/detached_group.ts"
	res := build(t, root, src, "detached_run,detached_start")
	if res.exit != 0 {
		t.Fatalf("building %s failed (exit %d):\n%s", src, res.exit, res.output)
	}
	wasm, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	return wasm
}

// runDetachedEntry executes one entry and fails if it suspends: neither detached call may park the workflow.
func runDetachedEntry(t *testing.T, wasm []byte, entry string, store *memDetached) (result string, history []engine.EventRecord) {
	t.Helper()
	e, closeRT := newEngineWith(t, engine.WithChildWorkflowStore(store))
	defer closeRT()
	raw, hist, susp, _, _, err := e.Execute(context.Background(), wasm, entry, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("%s: running the workflow: %v", entry, err)
	}
	if susp != nil {
		t.Fatalf("%s: the workflow suspended (%s). The run it started is still running, so a binding that WAITED for it would "+
			"look exactly like this", entry, susp.Reason)
	}
	return string(raw), hist
}

// runDetachedEvents returns every run_detached event in the history.
func runDetachedEvents(history []engine.EventRecord) []engine.EventRecord {
	var out []engine.EventRecord
	for _, ev := range history {
		if ev.EventType == engine.EventTypeRunDetached {
			out = append(out, ev)
		}
	}
	return out
}

// TestTypeScriptRunDetachedStartsAndDoesNotWait: `runDetached` starts the workflow, records it, and returns without waiting.
func TestTypeScriptRunDetachedStartsAndDoesNotWait(t *testing.T) {
	wasm := buildDetached(t)
	store := newMemDetached()
	raw, history := runDetachedEntry(t, wasm, "detached_run", store)

	var got struct {
		Reached bool `json:"reached"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("the result is not the object this test expects: %v\n%.400s", err, raw)
	}
	if !got.Reached {
		t.Errorf("the workflow did not report reaching its end: %.400s", raw)
	}
	if started := store.startedList(); strings.Join(started, ",") != "background" {
		t.Errorf("the store started %v, want exactly one run of \"background\"", started)
	}
	events := runDetachedEvents(history)
	if len(events) != 1 {
		t.Fatalf("the history holds %d run_detached events, want 1", len(events))
	}
	if events[0].DetachedName != "background" {
		t.Errorf("the recorded event names the workflow %q, want \"background\"", events[0].DetachedName)
	}
	// runDetached computes the id and discards it, so this is the only place it is observable -- and it must be the store's id,
	// not the "detached-<name>-<step>" fallback the engine invents when no store answered.
	if events[0].DetachedRunID != "child-1" {
		t.Errorf("the recorded run id is %q, want the store's child-1 (a fallback id means the store was not consulted)",
			events[0].DetachedRunID)
	}
}

// TestTypeScriptStartDetachedReturnsTheRecordedRunId: `startDetached` hands back the id the record holds, which is the handle
// a workflow keeps. A binding that returned anything else would make the documented use -- poll it, signal it -- impossible.
func TestTypeScriptStartDetachedReturnsTheRecordedRunId(t *testing.T) {
	wasm := buildDetached(t)
	store := newMemDetached()
	// Built here rather than through runDetachedEntry because the replay pass below needs the engine, and the replay is half of
	// the claim: the id the workflow sees is the RECORDED one, so a second segment must produce it without starting a second run.
	e, closeRT := newEngineWith(t, engine.WithChildWorkflowStore(store))
	defer closeRT()
	raw, history, susp, _, _, err := e.Execute(context.Background(), wasm, "detached_start", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("detached_start: running the workflow: %v", err)
	}
	if susp != nil {
		t.Fatalf("detached_start: the workflow suspended (%s)", susp.Reason)
	}

	var got struct {
		RunID   string `json:"runId"`
		Reached bool   `json:"reached"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("the result is not the object this test expects: %v\n%.400s", err, raw)
	}
	if !got.Reached {
		t.Errorf("the workflow did not report reaching its end: %.400s", raw)
	}
	if got.RunID != "child-1" {
		t.Errorf("startDetached resolved with %q, want the store's child-1", got.RunID)
	}
	if started := store.startedList(); strings.Join(started, ",") != "background" {
		t.Errorf("the store started %v, want exactly one run of \"background\"", started)
	}
	events := runDetachedEvents(history)
	if len(events) != 1 {
		t.Fatalf("the history holds %d run_detached events, want 1", len(events))
	}
	if events[0].DetachedRunID != got.RunID {
		t.Errorf("the recorded run id %q and the id handed to the workflow %q differ; a replay would hand back the record's",
			events[0].DetachedRunID, got.RunID)
	}

	// The replay: same id, and NO second run. Without this the test asserts only that the first execution recorded the id, which
	// a binding that started a fresh run on every segment would also satisfy -- and a detached start is exactly the call a
	// duplicate would be worst for.
	replayed, _, susp2, _, _, err := e.Replay(context.Background(), wasm, "detached_start", json.RawMessage(`{}`), history)
	if err != nil {
		t.Fatalf("replaying detached_start: %v", err)
	}
	if susp2 != nil {
		t.Fatalf("the replay suspended (%s); nothing in this workflow waits", susp2.Reason)
	}
	var again struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal([]byte(replayed), &again); err != nil {
		t.Fatalf("the replay's result is not the object this test expects: %v\n%.400s", err, replayed)
	}
	if again.RunID != got.RunID {
		t.Errorf("the replay resolved with %q, want the recorded %q: the id must come from history, not from a new start",
			again.RunID, got.RunID)
	}
	if started := store.startedList(); len(started) != 1 {
		t.Errorf("the store started %d run(s) across both segments, want 1: a replayed detached start must not start another: %v",
			len(started), started)
	}
}

// TestTypeScriptADetachedCallAfterASuspensionRecordsNothing is the group's late-call guard (groups/README.md convention 3).
// The falsification is removing `guarded` from either plugin binding: the store then starts a workflow, and the history carries
// a run_detached event, for a segment nobody will resume.
func TestTypeScriptADetachedCallAfterASuspensionRecordsNothing(t *testing.T) {
	root := needToolchain(t)
	src := "tests/typescript/testdata/workflows/late_detached.ts"
	res := build(t, root, src, "late_detached")
	if res.exit != 0 {
		t.Fatalf("building %s failed (exit %d):\n%s", src, res.exit, res.output)
	}
	wasm, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	store := newMemDetached()
	e, closeRT := newEngineWith(t, engine.WithChildWorkflowStore(store))
	defer closeRT()

	_, history, susp, _, _, err := e.Execute(context.Background(), wasm, "late_detached", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("running the workflow: %v", err)
	}
	// The precondition: the segment really did suspend, at the sleep. Without this the calls below were not "late" and the test
	// would pass for having measured nothing.
	if susp == nil {
		t.Fatalf("the workflow did not suspend; ctx.sleep(60000) is supposed to suspend the segment")
	}
	if !strings.Contains(susp.Reason, "cleat_sleep(60000ms)") {
		t.Fatalf("suspended for %q, not at the sleep; the ordering below is then unmeasured", susp.Reason)
	}
	if started := store.startedList(); len(started) != 0 {
		t.Errorf("the segment started %v after it suspended; both detached bindings must be guarded", started)
	}
	if events := runDetachedEvents(history); len(events) != 0 {
		t.Fatalf("the segment recorded a run_detached event (%+v) after the call that suspended it", events[0])
	}
}
