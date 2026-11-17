package typescript

// The children-group bindings (cleat#3827, group C): start with options, await all, await any, poll, and the dispatch point
// the two waits give pending updates.
//
// THE STORE SCRIPTS BY NAME and reports an unscripted name as STILL RUNNING, which is the only honest thing a single-process
// test can do: a real child is a separate run, so "not finished yet" is what a name no script mentions means. That is what
// makes the suspend cases reachable, and it is the same divergence cmd/cleat-ts-run's memoryChildren documents.
//
// WHERE THE ASSERTIONS ARE. On the workflow's own result AND on what the store saw. A workflow that returned the right shape
// without the host ever being asked is exactly the failure "a green result that measured nothing" names, so `started` records
// every child the store was asked to create and the poll test checks the three ids it started are three distinct runs.

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

type memChildRun struct {
	defName string
	outcome engine.ChildOutcome
}

// memChildren is an engine.ChildWorkflowStore driven by a name -> outcome script. An unscripted name is RUNNING.
type memChildren struct {
	mu      sync.Mutex
	byID    map[string]*memChildRun
	next    int
	script  map[string]engine.ChildOutcome
	started []string
}

func newMemChildren(script map[string]engine.ChildOutcome) *memChildren {
	return &memChildren{byID: map[string]*memChildRun{}, script: script}
}

func (c *memChildren) start(childID, defName string) {
	outcome, ok := c.script[defName]
	if !ok {
		outcome = engine.ChildOutcome{Completed: false}
	}
	c.byID[childID] = &memChildRun{defName: defName, outcome: outcome}
	c.started = append(c.started, defName)
}

func (c *memChildren) StartChildWorkflow(_ context.Context, _, defName, _ string, _ int, _ string, _ int) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	id := fmt.Sprintf("child-%d", c.next)
	c.start(id, defName)
	return id, nil
}

func (c *memChildren) StartChildWorkflowAtomic(_ context.Context, childID, _, defName, _ string, _ int, _ string, _ engine.EventRecord, _ int) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// The engine calls this with an EMPTY id and expects the store to mint one; echoing the argument back produced "" and the
	// guest's await failed with "Check that the run ID is valid" (cmd/cleat-ts-run/stores.go, found by running it).
	if childID == "" {
		c.next++
		childID = fmt.Sprintf("child-%d", c.next)
	}
	c.start(childID, defName)
	return childID, nil
}

func (c *memChildren) GetChildResult(_ context.Context, runID string) (engine.ChildOutcome, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	row, ok := c.byID[runID]
	if !ok {
		return engine.ChildOutcome{}, fmt.Errorf("unknown child run id %q", runID)
	}
	return row.outcome, nil
}

func (c *memChildren) GetChildCompletedAtMs(_ context.Context, runID string) (int64, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	row, ok := c.byID[runID]
	if !ok {
		return 0, false, fmt.Errorf("unknown child run id %q", runID)
	}
	if !row.outcome.Completed && !row.outcome.Failed {
		return 0, false, nil
	}
	// Finished before this run began, which is true of a scripted child and keeps PollChild's durable-clock comparison
	// answering "terminal" on replay.
	return 1, true, nil
}

func (c *memChildren) ResolveVersionByTag(_ context.Context, name, tag string) (int, error) {
	return 0, fmt.Errorf("no version tags exist in this harness (asked %q for %q)", name, tag)
}

func (c *memChildren) startedList() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.started...)
}

// complete finishes every run started under `defName`, standing in for the child run doing its work between the parent's
// segments. It is the only lever a single-process test has on the replay path: a row minted while the name was unscripted
// stays running until this is called.
func (c *memChildren) complete(defName, result string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, row := range c.byID {
		if row.defName == defName {
			row.outcome = engine.ChildOutcome{Completed: true, Result: result}
		}
	}
}

// childScript is the script every test here runs against: a child that succeeds, one that fails, and no entry at all for a
// name that must stay running ("held").
func childScript() map[string]engine.ChildOutcome {
	return map[string]engine.ChildOutcome{
		"alpha": {Completed: true, Result: `{"v":1}`},
		"beta":  {Completed: true, Result: `{"v":2}`},
		"bad":   {Completed: true, Failed: true, Error: "the child refused"},
	}
}

func buildChildren(t *testing.T) []byte {
	t.Helper()
	root := needToolchain(t)
	src := "tests/typescript/testdata/workflows/children_group.ts"
	res := build(t, root, src, strings.Join([]string{
		"children_all", "children_all_suspends", "children_any", "children_any_fails",
		"children_poll", "children_poll_unknown", "children_with_update", "children_without_handler",
	}, ","))
	if res.exit != 0 {
		t.Fatalf("building %s failed (exit %d):\n%s", src, res.exit, res.output)
	}
	wasm, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	return wasm
}

// runChildren executes one entry and requires it not to suspend; the raw result is returned for the caller to decode.
func runChildren(t *testing.T, wasm []byte, entry string, extra ...engine.EngineOption) string {
	t.Helper()
	e, closeRT := newEngineWith(t, extra...)
	defer closeRT()
	raw, _, susp, _, _, err := e.Execute(context.Background(), wasm, entry, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("%s: running the workflow: %v", entry, err)
	}
	if susp != nil {
		t.Fatalf("%s: the workflow suspended (%s)", entry, susp.Reason)
	}
	return raw
}

// TestTypeScriptAwaitAllKeepsEveryOutcomeInTheCallersOrder is the case a sort-by-run-id or a fail-fast implementation gets
// wrong: the ids are awaited in the REVERSE of the order they were started, one child failed, and its sibling's value must
// still be there, in the position the caller asked for.
func TestTypeScriptAwaitAllKeepsEveryOutcomeInTheCallersOrder(t *testing.T) {
	wasm := buildChildren(t)
	store := newMemChildren(childScript())
	raw := runChildren(t, wasm, "children_all", engine.WithChildWorkflowStore(store))

	var got struct {
		Alpha    string `json:"alpha"`
		Bad      string `json:"bad"`
		Outcomes []struct {
			RunID  string          `json:"runId"`
			Status string          `json:"status"`
			Value  json.RawMessage `json:"value"`
			Reason string          `json:"reason"`
		} `json:"outcomes"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("the workflow's result is not the object this test expects: %v\n%.400s", err, raw)
	}
	if len(got.Outcomes) != 2 {
		t.Fatalf("awaitAll answered %d outcomes for 2 children; the host answers one per id: %.400s", len(got.Outcomes), raw)
	}
	// [bad, alpha] was asked for, so index 0 is the failing child and index 1 the successful one.
	if got.Outcomes[0].RunID != got.Bad {
		t.Errorf("outcomes[0].runId = %q, want the failing child's %q: the host answers in the caller's order and this layer must keep it", got.Outcomes[0].RunID, got.Bad)
	}
	if got.Outcomes[0].Status != "rejected" {
		t.Errorf("outcomes[0].status = %q, want \"rejected\" for the failing child", got.Outcomes[0].Status)
	}
	if !strings.Contains(got.Outcomes[0].Reason, "the child refused") {
		t.Errorf("outcomes[0].reason = %q, want the child's own message", got.Outcomes[0].Reason)
	}
	if got.Outcomes[1].RunID != got.Alpha {
		t.Errorf("outcomes[1].runId = %q, want alpha's %q", got.Outcomes[1].RunID, got.Alpha)
	}
	if got.Outcomes[1].Status != "fulfilled" {
		t.Errorf("outcomes[1].status = %q, want \"fulfilled\"", got.Outcomes[1].Status)
	}
	// The value is a JSON OBJECT, so comparing its decoded form is what separates "parsed" from "the text was handed through".
	if string(got.Outcomes[1].Value) != `{"v":1}` {
		t.Errorf("outcomes[1].value = %s, want alpha's parsed result {\"v\":1}: a sibling's failure must not cost the caller the others' values", got.Outcomes[1].Value)
	}
	if want := []string{"alpha", "bad"}; strings.Join(store.startedList(), ",") != strings.Join(want, ",") {
		t.Errorf("the store started %v, want %v", store.startedList(), want)
	}
}

// TestTypeScriptAwaitAllSuspendsOnARunningChild is the case the host got wrong once (IMPROVEMENT-PLAN 3.309): a running child
// must SUSPEND the workflow, because recording "not completed" as that child's outcome would make it permanent in history.
func TestTypeScriptAwaitAllSuspendsOnARunningChild(t *testing.T) {
	wasm := buildChildren(t)
	e, closeRT := newEngineWith(t, engine.WithChildWorkflowStore(newMemChildren(childScript())))
	defer closeRT()

	_, _, susp, _, _, err := e.Execute(context.Background(), wasm, "children_all_suspends", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("executing: %v", err)
	}
	if susp == nil {
		t.Fatal("the workflow COMPLETED with a child still running; awaitAll must suspend, not report the running child as an outcome")
	}
	if !strings.Contains(susp.Reason, "await_all_children") {
		t.Errorf("suspended for %q, want the reason to name await_all_children", susp.Reason)
	}
}

// TestTypeScriptAwaitAnyResolvesTheFinishedChild: one child is finished and one is running, so there is a winner and no hang.
func TestTypeScriptAwaitAnyResolvesTheFinishedChild(t *testing.T) {
	wasm := buildChildren(t)
	raw := runChildren(t, wasm, "children_any", engine.WithChildWorkflowStore(newMemChildren(childScript())))
	var got struct {
		RunID string          `json:"runId"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("the result is not the object this test expects: %v\n%.400s", err, raw)
	}
	// The store minted child-1 for alpha and child-2 for "held"; only alpha is terminal.
	if got.RunID != "child-1" {
		t.Errorf("awaitAny picked %q, want child-1 (alpha), the only finished child", got.RunID)
	}
	if string(got.Value) != `{"v":1}` {
		t.Errorf("the winner's value decoded to %s, want {\"v\":1}", got.Value)
	}
}

// TestTypeScriptAwaitAnyRejectsWithTheChildThatFailed: a failed child ENDS the wait, and the rejection names it.
func TestTypeScriptAwaitAnyRejectsWithTheChildThatFailed(t *testing.T) {
	wasm := buildChildren(t)
	raw := runChildren(t, wasm, "children_any_fails", engine.WithChildWorkflowStore(newMemChildren(childScript())))
	var got struct {
		Winner     string `json:"winner"`
		Message    string `json:"message"`
		ChildRunID string `json:"childRunId"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("the result is not the object this test expects: %v\n%.400s", err, raw)
	}
	if got.Winner != "" {
		t.Fatalf("awaitAny resolved winner %q although the only finished child had failed", got.Winner)
	}
	if !strings.Contains(got.Message, "the child refused") {
		t.Errorf("the rejection carried %q, want the child's own message", got.Message)
	}
	if got.ChildRunID != "child-1" {
		// child-1 is `bad`: it was started first. A missing tag is the case this asserts against, since a caller holding a
		// bare rejection cannot tell which of its children failed.
		t.Errorf("the rejection was tagged childRunId=%q, want child-1 (bad)", got.ChildRunID)
	}
}

// TestTypeScriptPollChildReportsAllThreeStatesWithoutWaiting: pending, fulfilled and rejected, in one synchronous pass.
func TestTypeScriptPollChildReportsAllThreeStatesWithoutWaiting(t *testing.T) {
	wasm := buildChildren(t)
	store := newMemChildren(childScript())
	raw := runChildren(t, wasm, "children_poll", engine.WithChildWorkflowStore(store))

	var got struct {
		Done struct {
			RunID  string          `json:"runId"`
			Status string          `json:"status"`
			Value  json.RawMessage `json:"value"`
		} `json:"done"`
		Failed struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"failed"`
		Running struct {
			RunID  string `json:"runId"`
			Status string `json:"status"`
		} `json:"running"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("the result is not the object this test expects: %v\n%.400s", err, raw)
	}
	if got.Done.Status != "fulfilled" || string(got.Done.Value) != `{"v":1}` {
		t.Errorf("polling the finished child gave %q with %s, want \"fulfilled\" and {\"v\":1}", got.Done.Status, got.Done.Value)
	}
	if got.Failed.Status != "rejected" || !strings.Contains(got.Failed.Reason, "the child refused") {
		t.Errorf("polling the failed child gave %q with %q, want \"rejected\" carrying the child's message", got.Failed.Status, got.Failed.Reason)
	}
	if got.Running.Status != "pending" {
		t.Errorf("polling the running child gave %q, want \"pending\": a still-running child is neither a value nor a failure", got.Running.Status)
	}
	if got.Done.RunID != "child-1" || got.Running.RunID != "child-3" {
		t.Errorf("the snapshots name %q and %q, want child-1 and child-3 (the ids the store minted)", got.Done.RunID, got.Running.RunID)
	}
	if n := len(store.startedList()); n != 3 {
		t.Errorf("the store started %d children, want 3 -- pollChild must not start any of its own", n)
	}
}

// TestTypeScriptAwaitDeliversPendingUpdatesBeforeItWaits is the dispatch point, and the falsification is removing
// `dispatchThen` from awaitAll: the request is then never delivered, `handled` stays 0 and the store shows no completion.
func TestTypeScriptAwaitDeliversPendingUpdatesBeforeItWaits(t *testing.T) {
	wasm := buildChildren(t)
	updates := newMemUpdates([2]string{"bump", `{"n":3}`})
	raw := runChildren(t, wasm, "children_with_update",
		engine.WithChildWorkflowStore(newMemChildren(childScript())), engine.WithUpdateStore(updates))

	var got struct {
		Handled  int `json:"handled"`
		Outcomes int `json:"outcomes"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("the result is not the object this test expects: %v\n%.400s", err, raw)
	}
	if got.Handled != 3 {
		t.Errorf("the update handler saw %d, want 3: awaitAll must dispatch pending updates before it waits", got.Handled)
	}
	if got.Outcomes != 1 {
		t.Errorf("awaitAll answered %d outcomes, want 1", got.Outcomes)
	}
	if row := updates.row(0); !row.completed {
		t.Error("the update request was never completed; a caller would be left holding a promise nothing settles")
	} else if !strings.Contains(row.result, "3") {
		t.Errorf("the request was completed with %q, want the handler's return value", row.result)
	}
}

// TestTypeScriptAwaitAnswersAnUpdateWithNoHandler is the negative half: the dispatch point must COMPLETE a delivery even when
// nothing can handle it. Without this, "delivers updates" and "delivers only the updates it can handle" are indistinguishable
// from the suite, and the second leaves real callers hanging forever.
func TestTypeScriptAwaitAnswersAnUpdateWithNoHandler(t *testing.T) {
	wasm := buildChildren(t)
	updates := newMemUpdates([2]string{"bump", `{"n":3}`})
	raw := runChildren(t, wasm, "children_without_handler",
		engine.WithChildWorkflowStore(newMemChildren(childScript())), engine.WithUpdateStore(updates))

	var got struct {
		Outcomes int `json:"outcomes"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("the result is not the object this test expects: %v\n%.400s", err, raw)
	}
	if got.Outcomes != 1 {
		t.Errorf("awaitAll answered %d outcomes, want 1", got.Outcomes)
	}
	row := updates.row(0)
	if !row.completed {
		t.Fatal("the update request was left pending when no handler was registered; the caller waits forever")
	}
	if !strings.Contains(row.errMsg, "no update handler registered") {
		t.Errorf("the request was answered with errMsg %q, want the \"no update handler registered\" refusal", row.errMsg)
	}
}

// TestTypeScriptAChildCallAfterASuspensionRecordsNothing is the group's late-call guard (groups/README.md convention 3). A
// segment that has suspended records nothing further, so a child call made after the sleep must not reach the host. The
// falsification is removing `guarded` from `__cleat_child_workflow_with_options`: the store then starts a child, and the
// history carries a child_workflow event, for a segment nobody will resume.
//
// THE OTHER THREE ARE GUARDED TOO, AND THIS TEST CANNOT PROVE IT -- measured by review, one binding at a time, each still
// green. `awaitAll` and `awaitAny` sit behind `dispatchThen`, whose poll is itself guarded, so a suspended segment never
// reaches the binding; and `pollChild` records no event even when it runs, which is the property `engine/children.go` states
// and relies on (pollChildIsDeterministic). Their `guarded` wrappers stay as defence in depth, and this comment is here so the
// next reader does not take the green as evidence they are unnecessary. The same limit is recorded on group H's late-call
// test, where three of four bindings are observable and one is not.
func TestTypeScriptAChildCallAfterASuspensionRecordsNothing(t *testing.T) {
	root := needToolchain(t)
	src := "tests/typescript/testdata/workflows/late_children.ts"
	res := build(t, root, src, "late_children")
	if res.exit != 0 {
		t.Fatalf("building %s failed (exit %d):\n%s", src, res.exit, res.output)
	}
	wasm, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	store := newMemChildren(childScript())
	e, closeRT := newEngineWith(t, engine.WithChildWorkflowStore(store))
	defer closeRT()

	_, history, susp, _, _, err := e.Execute(context.Background(), wasm, "late_children", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("running the workflow: %v", err)
	}
	// The precondition: the segment really did suspend, at the sleep. Without this the calls below were not "late" at all
	// and the test would pass for having measured nothing.
	if susp == nil {
		t.Fatalf("the workflow did not suspend; ctx.sleep(60000) is supposed to suspend the segment")
	}
	if !strings.Contains(susp.Reason, "cleat_sleep(60000ms)") {
		t.Fatalf("suspended for %q, not at the sleep; the ordering below is then unmeasured", susp.Reason)
	}
	if started := store.startedList(); len(started) != 0 {
		t.Errorf("the segment started %v after it suspended; startChildWithOptions must be guarded", started)
	}
	for _, ev := range history {
		switch ev.EventType {
		case engine.EventTypeChildWorkflow, engine.EventTypeAwaitAnyChild, engine.EventTypeAwaitAllChildren:
			t.Fatalf("the segment recorded a %s event (%+v) after the call that suspended it", ev.EventType, ev)
		}
	}
}

// TestTypeScriptAwaitAllResumesWhenTheChildFinishes is the half the suspend test cannot reach: the FIRST segment records an
// empty await_all event and stops, and the next segment must find the now-finished child and return its value. It is what
// separates a durable binding from a wrapper that happens to suspend correctly: without the replay path, every workflow
// holding a real child would suspend forever.
func TestTypeScriptAwaitAllResumesWhenTheChildFinishes(t *testing.T) {
	wasm := buildChildren(t)
	store := newMemChildren(childScript())
	e, closeRT := newEngineWith(t, engine.WithChildWorkflowStore(store))
	defer closeRT()

	_, history, susp, _, _, err := e.Execute(context.Background(), wasm, "children_all_suspends", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("the first pass failed rather than suspending: %v", err)
	}
	if susp == nil {
		t.Fatalf("the first pass did not suspend although one child is running")
	}
	// The precondition the replay depends on: the store really is holding that child as running, so the completion below is
	// a change and not a value the first pass already had.
	if _, running := store.byID["child-2"]; !running || store.byID["child-2"].outcome.Completed {
		t.Fatalf("child-2 is not held running after the first pass; the replay below would measure nothing")
	}

	store.complete("held", `{"v":2}`)

	raw, _, susp2, _, _, err := e.Replay(context.Background(), wasm, "children_all_suspends", json.RawMessage(`{}`), history)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if susp2 != nil {
		t.Fatalf("the replay suspended again (%s); both children are finished", susp2.Reason)
	}
	var got struct {
		Outcomes int `json:"outcomes"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("the replay's result is not the object this test expects: %v\n%.400s", err, raw)
	}
	if got.Outcomes != 2 {
		t.Errorf("the replay reported %d outcomes, want 2: a resumed awaitAll must answer for every child", got.Outcomes)
	}
}

// TestTypeScriptPollChildRejectsOnAHostError is the error channel the coordinator asked to match pollSignal's: a store that
// cannot answer is not a child that failed, and the two must be distinguishable. Without the promise this would have had to be
// a thrown plain Error, which no `instanceof CleatError` in a caller could catch.
func TestTypeScriptPollChildRejectsOnAHostError(t *testing.T) {
	wasm := buildChildren(t)
	raw := runChildren(t, wasm, "children_poll_unknown", engine.WithChildWorkflowStore(newMemChildren(childScript())))
	var got struct {
		Status       string `json:"status"`
		Message      string `json:"message"`
		IsCleatError bool   `json:"isCleatError"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("the result is not the object this test expects: %v\n%.400s", err, raw)
	}
	if got.Status != "" {
		t.Fatalf("polling an id the store never minted RESOLVED with status %q; a host error must reject", got.Status)
	}
	if !strings.Contains(got.Message, "no-such-run") {
		t.Errorf("the rejection carried %q, want the host's message naming the id", got.Message)
	}
	if !got.IsCleatError {
		t.Error("the rejection is not a CleatError; a caller cannot tell a host error from its own bug")
	}
}
