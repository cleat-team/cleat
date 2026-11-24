package typescript

// The durable promise bindings (cleat#3827). A promise exists so that something OTHER than the waiter can settle it, so
// the store below decides each promise's outcome by the NAME it was created under -- standing in for the other run --
// rather than being settled by the workflow under test. Everything else is the same harness the metadata test uses: a
// built module, run in the in-process engine, compared against what the store was told to answer.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// memPromises is an in-memory engine.PromiseStore. "answered" is created already resolved with a JSON object (so a
// binding that passed the raw text through would fail to compare equal), "refused" is created already rejected, and
// anything else stays pending. Settling an id it does not hold is an error, which is what the workflow's last two cases
// need.
type memPromises struct {
	mu   sync.Mutex
	byID map[string]promiseRow
}

type promiseRow struct{ status, result, errMsg string }

func newMemPromises() *memPromises { return &memPromises{byID: map[string]promiseRow{}} }

func (m *memPromises) CreatePromise(_ context.Context, _, promiseName, promiseID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch promiseName {
	case "answered":
		m.byID[promiseID] = promiseRow{status: "resolved", result: `{"greeting":"pong"}`}
	case "refused":
		m.byID[promiseID] = promiseRow{status: "rejected", errMsg: "the store refused it"}
	default:
		m.byID[promiseID] = promiseRow{status: "pending"}
	}
	return nil
}

func (m *memPromises) GetPromise(_ context.Context, _, promiseID string) (status, result, errMsg string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.byID[promiseID]
	if !ok {
		return "", "", "", fmt.Errorf("no promise %s", promiseID)
	}
	return row.status, row.result, row.errMsg, nil
}

func (m *memPromises) ResolvePromise(_ context.Context, promiseID, result string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byID[promiseID]; !ok {
		return fmt.Errorf("no promise %s", promiseID)
	}
	m.byID[promiseID] = promiseRow{status: "resolved", result: result}
	return nil
}

func (m *memPromises) RejectPromise(_ context.Context, promiseID, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byID[promiseID]; !ok {
		return fmt.Errorf("no promise %s", promiseID)
	}
	m.byID[promiseID] = promiseRow{status: "rejected", errMsg: errMsg}
	return nil
}

func TestTypeScriptHostCallBindingsPromises(t *testing.T) {
	root := needToolchain(t)
	src := "tests/typescript/testdata/workflows/promises.ts"
	res := build(t, root, src, "promises,promise_timeout")
	if res.exit != 0 {
		t.Fatalf("building %s failed (exit %d):\n%s", src, res.exit, res.output)
	}
	wasm, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	e, closeRT := newEngineWith(t, engine.WithPromiseStore(newMemPromises()))
	defer closeRT()
	raw, _, susp, _, _, err := e.Execute(context.Background(), wasm, "promises", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("running the workflow: %v", err)
	}
	if susp != nil {
		t.Fatalf("the workflow suspended (%s); every promise it awaits is already settled by the store", susp.Reason)
	}
	var got struct {
		ID           string          `json:"id"`
		Value        json.RawMessage `json:"value"`
		TimedOut     bool            `json:"timedOut"`
		Rejection    string          `json:"rejection"`
		MineValue    int             `json:"mineValue"`
		ResolveError string          `json:"resolveError"`
		RejectError  string          `json:"rejectError"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("the workflow's result is not the object this test expects: %v\n%.400s", err, raw)
	}

	if got.ID == "" {
		t.Errorf("createPromise resolved to an empty id; the host returns one and the binding must pass it through")
	}
	// The store answers a JSON OBJECT. Comparing the decoded form is what distinguishes "the payload was parsed" from
	// "the raw text was handed through as a string", which is a mistake that reads as success.
	if string(got.Value) != `{"greeting":"pong"}` {
		t.Errorf("awaitPromise resolved with %s, want the parsed value {\"greeting\":\"pong\"}", got.Value)
	}
	if got.TimedOut {
		t.Errorf("timedOut = true for a promise the store had already resolved")
	}
	if !strings.Contains(got.Rejection, "the store refused it") {
		t.Errorf("awaiting a rejected promise gave %q, want it to carry the store's message", got.Rejection)
	}
	// resolvePromise's SUCCESS path: the workflow resolved its own promise and read the value back. Without this the
	// only resolvePromise exercised anywhere is the one that fails.
	if got.MineValue != 7 {
		t.Errorf("a promise resolved by the workflow read back as %d, want 7", got.MineValue)
	}
	if got.Rejection == "" {
		t.Errorf("awaiting a rejected promise resolved instead of rejecting")
	}
	// The Rust SDK builds these two messages itself, so their text is deterministic and assertable.
	for name, msg := range map[string]string{"resolvePromise": got.ResolveError, "rejectPromise": got.RejectError} {
		if !strings.Contains(msg, "no-such-promise") {
			t.Errorf("%s on an unknown id gave %q, want a rejection naming the id (the host's error path must not be swallowed)", name, msg)
		}
	}
}

// TestTypeScriptPromiseTimeoutIsReachedOnReplay closes the gap the first version of this file declared as unmeasurable.
// The host answers a first await with the STOP SENTINEL rather than a timeout-shaped word, so a single Execute cannot
// report timedOut; but Execute → wait past the deadline → Replay does, and engine's own promise-timeout test proves the
// same path with a Go guest. So the branch is reachable in-process, and asserting it here also asserts the suspend
// branch: `susp != nil` with no error is the 3-tuple stop path reaching this binding.
func TestTypeScriptPromiseTimeoutIsReachedOnReplay(t *testing.T) {
	root := needToolchain(t)
	src := "tests/typescript/testdata/workflows/promises.ts"
	res := build(t, root, src, "promises,promise_timeout")
	if res.exit != 0 {
		t.Fatalf("building %s failed (exit %d):\n%s", src, res.exit, res.output)
	}
	wasm, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	e, closeRT := newEngineWith(t, engine.WithPromiseStore(newMemPromises()))
	defer closeRT()

	_, history, susp, _, _, err := e.Execute(context.Background(), wasm, "promise_timeout", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("the first pass failed rather than suspending: %v", err)
	}
	if susp == nil {
		t.Fatalf("the first pass did not suspend awaiting a promise nothing will settle")
	}
	// Past the fixture's 200ms timeout. AwaitPromise's deadline check uses the real clock (engine/promises.go), so this
	// is a real wait, not a fake one.
	time.Sleep(250 * time.Millisecond)

	result, _, susp2, _, _, err := e.Replay(context.Background(), wasm, "promise_timeout", json.RawMessage(`{}`), history)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if susp2 != nil {
		t.Fatalf("expected the timeout to fire on replay, got a second suspend (%s)", susp2.Reason)
	}
	var got struct {
		TimedOut bool            `json:"timedOut"`
		Value    json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("the replay's result is not the object this test expects: %v\n%.400s", err, result)
	}
	if !got.TimedOut {
		t.Fatalf("timedOut = false after the deadline passed; the binding is dropping the host's timeout word")
	}
}
