package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// The stores decide what a workflow sees, so each test here pins one ANSWER rather than the absence of an error. A
// store that returned nothing for everything would satisfy "no error" on all of them.

func TestMemoryPromisesSettlesAScriptedNameOnCreate(t *testing.T) {
	m := newMemoryPromises(map[string]string{"answered": `{"greeting":"pong"}`}, nil)
	if err := m.CreatePromise(context.Background(), "wf", "answered", "id-1"); err != nil {
		t.Fatalf("CreatePromise: %v", err)
	}
	status, result, errMsg, err := m.GetPromise(context.Background(), "wf", "id-1")
	if err != nil {
		t.Fatalf("GetPromise: %v", err)
	}
	if status != "resolved" {
		t.Fatalf("status = %q, want %q", status, "resolved")
	}
	// The JSON TEXT is compared, not a decoded value: the engine hands this string straight to the guest, and a store
	// that re-encoded it would be a different answer for anything whose byte form matters.
	if result != `{"greeting":"pong"}` {
		t.Errorf("result = %q, want the scripted text unchanged", result)
	}
	if errMsg != "" {
		t.Errorf("errMsg = %q, want empty", errMsg)
	}
}

func TestMemoryPromisesLeavesAnUnscriptedNamePending(t *testing.T) {
	// The other direction, and the one that matters for a workflow expected to SUSPEND: a promise nobody settles must
	// stay pending, or the workflow would complete on a value no test asked for.
	m := newMemoryPromises(nil, nil)
	if err := m.CreatePromise(context.Background(), "wf", "pending", "id-2"); err != nil {
		t.Fatalf("CreatePromise: %v", err)
	}
	status, result, _, err := m.GetPromise(context.Background(), "wf", "id-2")
	if err != nil {
		t.Fatalf("GetPromise: %v", err)
	}
	if status != "pending" {
		t.Fatalf("status = %q, want pending", status)
	}
	if result != "" {
		t.Errorf("result = %q, want empty for a pending promise", result)
	}
}

func TestMemoryPromisesRejectsAScriptedName(t *testing.T) {
	m := newMemoryPromises(nil, map[string]string{"refused": "not today"})
	if err := m.CreatePromise(context.Background(), "wf", "refused", "id-3"); err != nil {
		t.Fatalf("CreatePromise: %v", err)
	}
	status, _, errMsg, err := m.GetPromise(context.Background(), "wf", "id-3")
	if err != nil {
		t.Fatalf("GetPromise: %v", err)
	}
	if status != "rejected" || errMsg != "not today" {
		t.Fatalf("got status=%q errMsg=%q, want rejected/not today", status, errMsg)
	}
}

func TestMemoryPromisesRefusesAFixtureThatSettlesOneNameBothWays(t *testing.T) {
	// Neither order is defensible, so the store refuses rather than picking one -- otherwise which way the test read
	// would depend on which branch this function happened to check first.
	m := newMemoryPromises(map[string]string{"both": `{}`}, map[string]string{"both": "no"})
	err := m.CreatePromise(context.Background(), "wf", "both", "id-4")
	if err == nil {
		t.Fatal("a name scripted as both resolved and rejected was accepted")
	}
}

func TestMemoryPromisesRefusesAnUnknownIDAndAcceptsItsOwn(t *testing.T) {
	m := newMemoryPromises(nil, nil)
	if err := m.ResolvePromise(context.Background(), "nope", `1`); err == nil {
		t.Error("ResolvePromise accepted an id the store never issued")
	}
	if err := m.RejectPromise(context.Background(), "nope", "x"); err == nil {
		t.Error("RejectPromise accepted an id the store never issued")
	}
	// GetPromise too: answering "pending" for an unknown id would make the workflow wait on the fixture's mistake
	// rather than reporting it.
	if _, _, _, err := m.GetPromise(context.Background(), "wf", "nope"); err == nil {
		t.Error("GetPromise answered for an id the store never issued")
	}

	if err := m.CreatePromise(context.Background(), "wf", "mine", "id-5"); err != nil {
		t.Fatalf("CreatePromise: %v", err)
	}
	if err := m.ResolvePromise(context.Background(), "id-5", `{"v":7}`); err != nil {
		t.Fatalf("ResolvePromise on its own id: %v", err)
	}
	status, result, _, err := m.GetPromise(context.Background(), "wf", "id-5")
	if err != nil || status != "resolved" || result != `{"v":7}` {
		t.Fatalf("after ResolvePromise: status=%q result=%q err=%v, want resolved/{\"v\":7}/nil", status, result, err)
	}
}

func TestMemoryPromisesRecordsCreatedNamesInOrder(t *testing.T) {
	// This is what the envelope carries, so a test can assert the workflow created the promises it meant to -- an
	// assertion that does not depend on the workflow's own output.
	m := newMemoryPromises(nil, nil)
	for i, name := range []string{"first", "second", "third"} {
		if err := m.CreatePromise(context.Background(), "wf", name, string(rune('a'+i))); err != nil {
			t.Fatalf("CreatePromise(%q): %v", name, err)
		}
	}
	got := m.nameList()
	want := []string{"first", "second", "third"}
	if len(got) != len(want) {
		t.Fatalf("nameList = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("nameList = %v, want %v", got, want)
		}
	}
}

func TestMemorySignalsDeliversQueuedPayloadsInOrderAndConsumesThem(t *testing.T) {
	m := newMemorySignals(map[string][]string{"approve": {`{"ok":true}`, `{"ok":false}`}})
	ctx := context.Background()

	d, found, err := m.PollSignal(ctx, "wf", "approve")
	if err != nil || !found {
		t.Fatalf("first PollSignal: found=%v err=%v, want found", found, err)
	}
	if d.Payload != `{"ok":true}` {
		t.Fatalf("first payload = %q, want the oldest", d.Payload)
	}
	// Polling does not consume: two polls in a row see the same delivery, which is the interface's contract (the
	// guest's completion call is what consumes).
	again, found, _ := m.PollSignal(ctx, "wf", "approve")
	if !found || again.ID != d.ID {
		t.Fatalf("a second poll consumed the delivery: found=%v id=%d want %d", found, again.ID, d.ID)
	}

	if err := m.ConsumeSignal(ctx, "wf", d.ID); err != nil {
		t.Fatalf("ConsumeSignal: %v", err)
	}
	second, found, _ := m.PollSignal(ctx, "wf", "approve")
	if !found || second.Payload != `{"ok":false}` {
		t.Fatalf("after consuming the first, poll gave found=%v payload=%q, want the second", found, second.Payload)
	}

	// Consuming an id that is already gone is not an error, by the interface's own wording.
	if err := m.ConsumeSignal(ctx, "wf", d.ID); err != nil {
		t.Errorf("consuming an already-consumed id errored: %v", err)
	}
	// An unknown name is simply not found.
	if _, found, err := m.PollSignal(ctx, "wf", "other"); found || err != nil {
		t.Errorf("PollSignal on an unknown name: found=%v err=%v, want not found", found, err)
	}
	if got := m.consumedList(); len(got) != 1 || got[0] != "approve" {
		t.Errorf("consumedList = %v, want [approve]", got)
	}
}

func TestMemorySignalsIsEmptyWhenNothingWasScripted(t *testing.T) {
	// The control for the test above: the same poll on a store with no fixture must find nothing, or "found" would be
	// a property of the store rather than of the script.
	m := newMemorySignals(nil)
	if _, found, err := m.PollSignal(context.Background(), "wf", "approve"); found || err != nil {
		t.Fatalf("an empty store found a signal: found=%v err=%v", found, err)
	}
	if c, _, _ := m.PollCancellation(context.Background(), "wf"); c {
		t.Error("an empty store reported the workflow cancelled")
	}
}

func TestLoadStringMapRefusesAValueThatIsNotJSON(t *testing.T) {
	// The value goes to the guest as JSON text, so a malformed one surfaces as a parse error INSIDE the workflow --
	// which reads like a workflow bug rather than a broken fixture. Refusing here is what keeps the two apart.
	dir := t.TempDir()
	bad := filepath.Join(dir, "promises.json")
	if err := os.WriteFile(bad, []byte(`{"answered":"{not json}"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadStringMap(bad, "promises"); err == nil {
		t.Fatal("a promise value that is not JSON was accepted")
	}

	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(`{"answered":"{\"ok\":1}"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadStringMap(good, "promises")
	if err != nil {
		t.Fatalf("a valid file was refused: %v", err)
	}
	if got["answered"] != `{"ok":1}` {
		t.Fatalf("loaded %q, want the original text", got["answered"])
	}
}

func TestLoadSignalScriptReadsArraysOfPayloads(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "signals.json")
	if err := os.WriteFile(p, []byte(`{"approve":[{"ok":true},{"ok":false}],"cancel":["{}"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadSignalScript(p)
	if err != nil {
		t.Fatalf("loadSignalScript: %v", err)
	}
	if len(got["approve"]) != 2 || got["approve"][0] != `{"ok":true}` || got["approve"][1] != `{"ok":false}` {
		t.Fatalf("approve = %v, want two payloads in order", got["approve"])
	}
	if len(got["cancel"]) != 1 {
		t.Fatalf("cancel = %v, want one payload", got["cancel"])
	}
}

func TestMemorySignalsImplementsTheEngineInterface(t *testing.T) {
	// Compile-time, but stated as a test so a change to the interface is a failing test rather than a build error in
	// whichever package happens to reference the store first.
	var _ engine.SignalStore = (*memorySignals)(nil)
	var _ engine.PromiseStore = (*memoryPromises)(nil)
}

// The bytes below are what `packages/cleat-ts/testing.ts` actually wrote when driven with a stub runner, copied
// verbatim rather than written by hand from the same intention. The two halves are written in different languages, so
// a shape that only the Go side agrees with would pass every unit test here and produce a fixture the runner rejects.
func TestTheHarnessScriptsAreTheShapeTheseLoadersRead(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	promises, err := loadStringMap(write("promises.json", `{"answered":"{\"greeting\":\"pong\"}"}`), "promises")
	if err != nil {
		t.Fatalf("the harness's promises file was refused: %v", err)
	}
	if promises["answered"] != `{"greeting":"pong"}` {
		t.Fatalf("promises[answered] = %q, want the JSON text the guest receives", promises["answered"])
	}

	// loadMessageMap, not loadStringMap: a rejection is an error MESSAGE, not JSON the guest parses. Using the wrong
	// one here is what this test caught -- the first version of it did, and the runner would have refused every
	// realistic rejection fixture in CI.
	rejects, err := loadMessageMap(write("promise-rejects.json", `{"refused":"nope"}`), "promise-rejects")
	if err != nil {
		t.Fatalf("the harness's promise-rejects file was refused: %v", err)
	}
	if rejects["refused"] != "nope" {
		t.Fatalf("rejects[refused] = %q, want nope", rejects["refused"])
	}

	signals, err := loadSignalScript(write("signals.json", `{"approve":[{"ok":true},{"ok":false}]}`))
	if err != nil {
		t.Fatalf("the harness's signals file was refused: %v", err)
	}
	if len(signals["approve"]) != 2 || signals["approve"][0] != `{"ok":true}` {
		t.Fatalf("signals[approve] = %v, want the payloads in order", signals["approve"])
	}
}

// The WIRING, which the loader tests cannot reach: a review note pointed out that putting the original wrong loader
// back for -promise-rejects (`loadStringMap`, which demands JSON where a rejection is an error message) left
// `go test ./cmd/cleat-ts-run/` green, because every test above calls a loader directly and none asked which one the
// runner chose.
func TestBuildStoresWiresEachFlagToTheLoaderThatCanReadIt(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	promises := write("promises.json", `{"answered":"{\"greeting\":\"pong\"}"}`)
	// "nope" is a MESSAGE, not JSON. loadStringMap refuses exactly this, so this file is the one that fails if the
	// wiring regresses to it.
	rejects := write("promise-rejects.json", `{"refused":"nope"}`)
	signals := write("signals.json", `{"approve":[{"ok":true}]}`)

	ps, ss, _, err := buildStores(promises, rejects, signals, "")
	if err != nil {
		t.Fatalf("buildStores refused fixtures the runner must accept: %v", err)
	}

	if err := ps.CreatePromise(context.Background(), "wf", "answered", "id-a"); err != nil {
		t.Fatalf("CreatePromise(answered): %v", err)
	}
	if status, result, _, _ := ps.GetPromise(context.Background(), "wf", "id-a"); status != "resolved" || result != `{"greeting":"pong"}` {
		t.Errorf("scripted resolution did not survive the wiring: status=%q result=%q", status, result)
	}
	if err := ps.CreatePromise(context.Background(), "wf", "refused", "id-b"); err != nil {
		t.Fatalf("CreatePromise(refused): %v", err)
	}
	if status, _, errMsg, _ := ps.GetPromise(context.Background(), "wf", "id-b"); status != "rejected" || errMsg != "nope" {
		t.Errorf("scripted rejection did not survive the wiring: status=%q errMsg=%q", status, errMsg)
	}
	if d, found, _ := ss.PollSignal(context.Background(), "wf", "approve"); !found || d.Payload != `{"ok":true}` {
		t.Errorf("scripted signal did not survive the wiring: found=%v payload=%q", found, d.Payload)
	}
}

func TestBuildStoresWithNoFixturesStillReturnsUsableStores(t *testing.T) {
	// The control for the wiring test: no paths must still produce stores rather than nil, or a workflow touching a
	// promise would fail with errNoPromiseStore instead of parking.
	ps, ss, _, err := buildStores("", "", "", "")
	if err != nil {
		t.Fatalf("buildStores with no fixtures: %v", err)
	}
	if ps == nil || ss == nil {
		t.Fatal("buildStores returned a nil store for an unscripted run")
	}
	if err := ps.CreatePromise(context.Background(), "wf", "anything", "id-c"); err != nil {
		t.Fatalf("CreatePromise: %v", err)
	}
	if status, _, _, _ := ps.GetPromise(context.Background(), "wf", "id-c"); status != "pending" {
		t.Errorf("an unscripted promise settled: status=%q, want pending", status)
	}
	if _, found, _ := ss.PollSignal(context.Background(), "wf", "approve"); found {
		t.Error("an unscripted store found a signal")
	}
}

func TestMemoryChildrenReturnsAScriptedSuccess(t *testing.T) {
	c := newMemoryChildren(map[string]engine.ChildOutcome{
		"charge": {Completed: true, Result: `{"id":"ch_1"}`},
	})
	id, err := c.StartChildWorkflow(context.Background(), "parent", "charge", `{}`, 0, "", 0)
	if err != nil {
		t.Fatalf("StartChildWorkflow: %v", err)
	}
	out, err := c.GetChildResult(context.Background(), id)
	if err != nil {
		t.Fatalf("GetChildResult: %v", err)
	}
	if !out.Completed || out.Failed || out.Result != `{"id":"ch_1"}` {
		t.Fatalf("outcome = %+v, want completed, not failed, with the scripted result", out)
	}
	if ms, ok, err := c.GetChildCompletedAtMs(context.Background(), id); err != nil || !ok || ms == 0 {
		t.Fatalf("GetChildCompletedAtMs = (%d, %v, %v), want a non-zero instant with ok", ms, ok, err)
	}
}

// The error path, which a harness that could only script success would leave untestable by construction: a child that
// FAILED is a completed call with Failed set, NOT a store error. Conflating those is cleat#1115 -- a parent told "your
// child failed" because the store blinked branches wrongly.
func TestMemoryChildrenReturnsAScriptedFailure(t *testing.T) {
	c := newMemoryChildren(map[string]engine.ChildOutcome{
		"charge": {Completed: true, Failed: true, Error: "card declined"},
	})
	id, err := c.StartChildWorkflow(context.Background(), "parent", "charge", `{}`, 0, "", 0)
	if err != nil {
		t.Fatalf("StartChildWorkflow: %v", err)
	}
	out, err := c.GetChildResult(context.Background(), id)
	if err != nil {
		t.Fatalf("a failed child came back as a store error: %v", err)
	}
	if !out.Completed || !out.Failed || out.Error != "card declined" {
		t.Fatalf("outcome = %+v, want completed+failed with the scripted error", out)
	}
	if out.Result != "" {
		t.Errorf("a failed child reported a result: %q", out.Result)
	}
}

func TestMemoryChildrenLeavesAnUnscriptedChildRunning(t *testing.T) {
	// The control: the parent must PARK on a child nobody scripted, not be handed an invented outcome.
	c := newMemoryChildren(nil)
	id, err := c.StartChildWorkflow(context.Background(), "parent", "anything", `{}`, 0, "", 0)
	if err != nil {
		t.Fatalf("StartChildWorkflow: %v", err)
	}
	out, err := c.GetChildResult(context.Background(), id)
	if err != nil {
		t.Fatalf("GetChildResult: %v", err)
	}
	if out.Completed || out.Failed {
		t.Fatalf("outcome = %+v, want neither completed nor failed", out)
	}
	if _, ok, err := c.GetChildCompletedAtMs(context.Background(), id); err != nil || ok {
		t.Fatalf("GetChildCompletedAtMs = (_, %v, %v), want ok=false for a running child", ok, err)
	}
}

func TestMemoryChildrenRefusesAnUnknownRunID(t *testing.T) {
	c := newMemoryChildren(nil)
	if _, err := c.GetChildResult(context.Background(), "nope"); err == nil {
		t.Error("GetChildResult answered for a run id the store never issued")
	}
	if _, ok, err := c.GetChildCompletedAtMs(context.Background(), "nope"); err == nil || ok {
		t.Error("GetChildCompletedAtMs answered for a run id the store never issued")
	}
}

func TestLoadChildScriptAcceptsBothSpellingsAndRefusesAmbiguity(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	ok, err := loadChildScript(write("ok.json", `{"charge":{"result":{"id":"ch_1"}},"refund":{"error":"not allowed"}}`))
	if err != nil {
		t.Fatalf("a valid child script was refused: %v", err)
	}
	if out := ok["charge"]; !out.Completed || out.Failed || out.Result != `{"id":"ch_1"}` {
		t.Errorf("charge = %+v, want completed with the result", out)
	}
	if out := ok["refund"]; !out.Completed || !out.Failed || out.Error != "not allowed" {
		t.Errorf("refund = %+v, want completed+failed with the error", out)
	}

	// A child scripted both ways is a broken fixture, not a case to pick a winner for.
	if _, err := loadChildScript(write("both.json", `{"x":{"result":1,"error":"no"}}`)); err == nil {
		t.Error("a child scripted as both a result and an error was accepted")
	}
	// Neither is also a broken fixture: it would silently mean "running", which the script is supposed to say by
	// omission rather than by an empty object.
	if _, err := loadChildScript(write("neither.json", `{"x":{}}`)); err == nil {
		t.Error("a child with neither a result nor an error was accepted")
	}
}
