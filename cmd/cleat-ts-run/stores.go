// In-memory stores for cleat-ts-run (cleat#3828).
//
// WHY THESE EXIST. A TypeScript workflow reaches durable state through host calls -- promises, signals, and (later)
// children and updates -- and the engine refuses every one of them when no store is configured: `create_promise` and
// `await_promise` return errNoPromiseStore rather than suspending, which is IMPROVEMENT-PLAN 3.231's fix and is what
// stops a workflow waiting forever on a promise nothing can settle. So without stores this harness can run a workflow
// that CALLS a service and nothing else.
//
// The gap that made this worth building rather than leaving to each test: tests/typescript/typescript_promises_test.go
// already carries its own memPromises, and a second test that needs a signal would carry a second one. A TypeScript
// author writing a test in TypeScript could not have either. These put the same idea where `testing.ts` can reach it.
//
// WHAT A STORE HERE IS NOT. It is not a claim about the shipped stores. It answers from what the TEST said, keyed the
// way the test said it, and it settles a promise on CREATE rather than when another run would have resolved it -- a
// stand-in for the other run, which is the only thing a single-process test can be. Where that differs from a deployed
// store in a way a workflow could observe, the difference belongs in DIVERGENCES.md beside the language differences,
// not silently here.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/cleat-team/cleat/engine"
)

// promiseRow is one promise's state.
type promiseRow struct {
	name   string
	status string // "pending" | "resolved" | "rejected"
	result string
	errMsg string
}

// memoryPromises is an engine.PromiseStore driven by the test's script.
//
// SETTLEMENT IS ON CREATE, keyed by NAME. A promise exists so that something other than the waiter can settle it, and
// this store is that something. The alternative -- leave every promise pending and make the test resolve it afterwards
// -- would need the harness to re-enter the engine between segments, which is resume support and a different change.
type memoryPromises struct {
	mu       sync.Mutex
	byID     map[string]*promiseRow
	resolves map[string]string // promise name -> JSON text it resolves with
	rejects  map[string]string // promise name -> rejection message
	created  []string          // creation order, for the envelope
}

func newMemoryPromises(resolves, rejects map[string]string) *memoryPromises {
	if resolves == nil {
		resolves = map[string]string{}
	}
	if rejects == nil {
		rejects = map[string]string{}
	}
	return &memoryPromises{byID: map[string]*promiseRow{}, resolves: resolves, rejects: rejects}
}

func (m *memoryPromises) CreatePromise(_ context.Context, _, promiseName, promiseID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row := &promiseRow{name: promiseName, status: "pending"}
	// A name in BOTH scripts is a contradiction in the fixture rather than a case to pick a winner for: neither order
	// is defensible, and a test that hit it would be asserting on whichever branch this happened to prefer.
	if _, ok := m.resolves[promiseName]; ok {
		if _, both := m.rejects[promiseName]; both {
			return fmt.Errorf("the fixture settles %q as both resolved and rejected", promiseName)
		}
		row.status, row.result = "resolved", m.resolves[promiseName]
	}
	if msg, ok := m.rejects[promiseName]; ok {
		row.status, row.errMsg = "rejected", msg
	}
	m.byID[promiseID] = row
	m.created = append(m.created, promiseName)
	return nil
}

// knows reports whether the store holds this id. Settling an unknown id must fail: the workflow's own error paths are
// worth testing, and a store that accepted anything would make them untestable.
func (m *memoryPromises) knows(promiseID string) bool {
	_, ok := m.byID[promiseID]
	return ok
}

func (m *memoryPromises) ResolvePromise(_ context.Context, promiseID, result string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.knows(promiseID) {
		return fmt.Errorf("unknown promise id %q", promiseID)
	}
	row := m.byID[promiseID]
	row.status, row.result, row.errMsg = "resolved", result, ""
	return nil
}

func (m *memoryPromises) RejectPromise(_ context.Context, promiseID, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.knows(promiseID) {
		return fmt.Errorf("unknown promise id %q", promiseID)
	}
	row := m.byID[promiseID]
	row.status, row.errMsg, row.result = "rejected", errMsg, ""
	return nil
}

func (m *memoryPromises) GetPromise(_ context.Context, _, promiseID string) (string, string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.byID[promiseID]
	if !ok {
		// A missing row is an error, not "pending": the workflow asked about an id this store never issued, and
		// answering "pending" would make it wait instead of reporting the fixture's mistake.
		return "", "", "", fmt.Errorf("unknown promise id %q", promiseID)
	}
	return row.status, row.result, row.errMsg, nil
}

func (m *memoryPromises) nameList() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]string(nil), m.created...)
	return out
}

// memorySignals is an engine.SignalStore seeded from the test's script, standing in for a caller that delivered before
// the run started.
//
// DeliveredAtMs is deliberately left ZERO. The engine treats a zero as "this store did not populate it", which is true
// here, and signalIsVisibleNow then answers that the delivery is visible rather than comparing it against a durable
// clock this process does not have (engine/signaller.go).
type memorySignals struct {
	mu        sync.Mutex
	queue     map[string][]engine.SignalDelivery // signal name -> deliveries, oldest first
	nextID    int64
	consumed  []string
	cancelled bool
	reason    string
}

func newMemorySignals(payloads map[string][]string) *memorySignals {
	m := &memorySignals{queue: map[string][]engine.SignalDelivery{}}
	// Sorted so a fixture with several names builds the same queue every run: map iteration order is random, and the
	// ids below are the queue's ordering.
	names := make([]string, 0, len(payloads))
	for name := range payloads {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, payload := range payloads[name] {
			m.nextID++
			m.queue[name] = append(m.queue[name], engine.SignalDelivery{ID: m.nextID, Payload: payload})
		}
	}
	return m
}

func (m *memorySignals) DeliverSignal(_ context.Context, _, signalName, payload string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	m.queue[signalName] = append(m.queue[signalName], engine.SignalDelivery{ID: m.nextID, Payload: payload})
	return nil
}

func (m *memorySignals) PollSignal(_ context.Context, _, signalName string) (engine.SignalDelivery, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.queue[signalName]
	if len(q) == 0 {
		return engine.SignalDelivery{}, false, nil
	}
	return q[0], true, nil
}

func (m *memorySignals) ConsumeSignal(_ context.Context, _ string, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Removing an id that is already gone is not an error (the engine's interface says so), and neither is an id this
	// store never issued: the queue is per-name and the id is global, so a miss is ordinary.
	for name, q := range m.queue {
		for i, d := range q {
			if d.ID == id {
				m.queue[name] = append(q[:i], q[i+1:]...)
				m.consumed = append(m.consumed, name)
				return nil
			}
		}
	}
	return nil
}

func (m *memorySignals) PollCancellation(context.Context, string) (bool, string, error) {
	return m.cancelled, m.reason, nil
}

func (m *memorySignals) consumedList() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.consumed...)
}

// loadStringMap reads a JSON object of strings, for the promise scripts.
func loadStringMap(path, what string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the %s file: %w", what, err)
	}
	var out map[string]string
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("the %s file is not a JSON object of strings: %w", what, err)
	}
	// Each value is handed to the guest as JSON text, so it must BE JSON: a malformed value would surface as a
	// parse error inside the workflow, which reads like a workflow bug rather than a broken fixture.
	for name, value := range out {
		if !json.Valid([]byte(value)) {
			return nil, fmt.Errorf("the %s entry %q is not valid JSON: %s", what, name, value)
		}
	}
	return out, nil
}

// loadMessageMap reads a JSON object of plain strings, for the promise-REJECTION scripts.
//
// SEPARATE FROM loadStringMap ON PURPOSE, and the difference is the whole reason it exists: a promise's RESOLUTION
// value is handed to the guest as JSON text, so it must BE JSON; a REJECTION is an error message the guest turns into
// an Error, so it is ordinary text -- `"nope"` is valid here and `{"why":"nope"}` would be delivered to the workflow as
// the string with braces in it. Validating these as JSON refused every realistic fixture, which the cross-language
// test below caught before it reached CI.
func loadMessageMap(path, what string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the %s file: %w", what, err)
	}
	var out map[string]string
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("the %s file is not a JSON object of strings: %w", what, err)
	}
	return out, nil
}

// loadSignalScript reads the signal fixture: {"<signal name>": [<payload JSON>, ...]}.
func loadSignalScript(path string) (map[string][]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the signals file: %w", err)
	}
	var raw map[string][]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("the signals file is not a JSON object of arrays: %w", err)
	}
	out := make(map[string][]string, len(raw))
	for name, payloads := range raw {
		for _, p := range payloads {
			out[name] = append(out[name], string(p))
		}
	}
	return out, nil
}
