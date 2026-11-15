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
	res := build(t, root, src, "promises")
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
	// The Rust SDK builds these two messages itself, so their text is deterministic and assertable.
	for name, msg := range map[string]string{"resolvePromise": got.ResolveError, "rejectPromise": got.RejectError} {
		if !strings.Contains(msg, "no-such-promise") {
			t.Errorf("%s on an unknown id gave %q, want a rejection naming the id (the host's error path must not be swallowed)", name, msg)
		}
	}
}
