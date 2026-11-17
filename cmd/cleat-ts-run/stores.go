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
	// self is the run's own id, set by main. A signal the workflow SENDS is recorded in sent; it joins this store's queue (and
	// so becomes visible to this run's own polls) only when it was addressed to this run. Left empty, as in a unit test of
	// the store, every delivery joins the queue, which is what this store did before it recorded sends.
	self string
	sent []sentSignal
}

// sentSignal is one signal the workflow sent with signalWorkflow, as the store saw it.
type sentSignal struct {
	Target  string `json:"target"`
	Name    string `json:"name"`
	Payload string `json:"payload"`
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

func (m *memorySignals) DeliverSignal(_ context.Context, target, signalName, payload string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, sentSignal{Target: target, Name: signalName, Payload: payload})
	if m.self != "" && target != m.self {
		return nil // addressed to another run: recorded, and nobody here to receive it
	}
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

// reportInto adds `signalsSent` to the envelope's extras: what the workflow SENT, in order, as the store saw it. Omitted when
// nothing was sent, so a run that sends nothing is unchanged.
func (m *memorySignals) reportInto(extras map[string]any) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sent) == 0 {
		return extras
	}
	if extras == nil {
		extras = map[string]any{}
	}
	extras["signalsSent"] = append([]sentSignal(nil), m.sent...)
	return extras
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

// memoryChildren is an engine.ChildWorkflowStore driven by the test's script (cleat#3828).
//
// WHAT IT IS FOR. A TypeScript workflow reaches a child through `Ctx.child(name, input)`, which is bound
// (`__cleat_child_workflow` / `__cleat_await_child`); without a store the engine answers "no child workflow store" and
// the call fails. With this, a test says what a child NAMED <name> returns, and the workflow runs against it.
//
// THE CHILD IS ALREADY FINISHED, and that is the divergence worth stating loudly. A real child is a workflow: it
// starts, runs, and completes at some instant the parent may have to wait for. This store scripts the OUTCOME and
// reports the child as having completed long ago, so the parent never waits. It therefore tests "the parent handles
// what the child returned", not "the parent survives waiting for a child" -- the same limit memoryPromises has, for
// the same reason: one process, one segment, no resume.
//
// GetChildCompletedAtMs returns a small non-zero instant for exactly this reason. The engine compares it against the
// parent's durable clock to answer "was this child terminal as of the parent's recorded time" (issue #847), and a zero
// would mean "the store did not populate it" / "not complete". A near-zero value says "completed before this run
// began", which is true of a scripted child and is the only honest answer this process can give.
type memoryChildren struct {
	mu      sync.Mutex
	byID    map[string]*memoryChildRun
	next    int64
	script  map[string]engine.ChildOutcome // def name -> outcome
	started []string                       // def names, in start order
}

type memoryChildRun struct {
	defName string
	outcome engine.ChildOutcome
}

func newMemoryChildren(script map[string]engine.ChildOutcome) *memoryChildren {
	if script == nil {
		script = map[string]engine.ChildOutcome{}
	}
	return &memoryChildren{byID: map[string]*memoryChildRun{}, script: script}
}

// start records a child and hands back its id. Shared by both Start methods so the two cannot drift: the Atomic variant
// differs only in taking the id and the event from the caller.
func (c *memoryChildren) start(childID, defName string) engine.ChildOutcome {
	outcome, scripted := c.script[defName]
	if !scripted {
		// An unscripted child stays RUNNING, so the parent suspends rather than being handed an invented result. The
		// same rule as an unscripted promise and an unqueued signal.
		outcome = engine.ChildOutcome{Completed: false}
	}
	c.byID[childID] = &memoryChildRun{defName: defName, outcome: outcome}
	c.started = append(c.started, defName)
	return outcome
}

func (c *memoryChildren) StartChildWorkflow(_ context.Context, _, defName, _ string, _ int, _ string, _ int) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	childID := fmt.Sprintf("child-%d", c.next)
	c.start(childID, defName)
	return childID, nil
}

func (c *memoryChildren) StartChildWorkflowAtomic(_ context.Context, childID, _, defName, _ string, _ int, _ string, _ engine.EventRecord, _ int) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// AN EMPTY ID MEANS "GENERATE ONE", and that is the case the engine actually uses: it calls this with childID ""
	// (engine/children.go), so echoing the argument produced a run id of "" and the guest's await_child failed with
	// "Check that the run ID is valid". Found by running the module, not by reading -- the store returned a string and
	// no unit test asked whether it was a USABLE one.
	if childID == "" {
		c.next++
		childID = fmt.Sprintf("child-%d", c.next)
	}
	c.start(childID, defName)
	return childID, nil
}

func (c *memoryChildren) GetChildResult(_ context.Context, runID string) (engine.ChildOutcome, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	row, ok := c.byID[runID]
	if !ok {
		// A store error, NOT a failed child: the interface distinguishes them because a parent told "your child failed"
		// when the store merely blinked branches wrongly (cleat#1115's neighbour).
		return engine.ChildOutcome{}, fmt.Errorf("unknown child run id %q", runID)
	}
	return row.outcome, nil
}

func (c *memoryChildren) GetChildCompletedAtMs(_ context.Context, runID string) (int64, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	row, ok := c.byID[runID]
	if !ok {
		return 0, false, fmt.Errorf("unknown child run id %q", runID)
	}
	// Not terminal at all: ok=false is the interface's "still running".
	if !row.outcome.Completed && !row.outcome.Failed {
		return 0, false, nil
	}
	// Terminal: finished before this run began. See the type's comment.
	return 1, true, nil
}

func (c *memoryChildren) ResolveVersionByTag(_ context.Context, workflowName, tag string) (int, error) {
	return 0, fmt.Errorf("no version tags exist in this harness (asked %q for tag %q)", workflowName, tag)
}

func (c *memoryChildren) startedList() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.started...)
}

// loadChildScript reads the child fixture: {"<child name>": {"result": <any>}} or {"<child name>": {"error": "..."}}.
//
// Both spellings are one file because a test that only ever scripts a SUCCESS cannot exercise the parent's error path,
// and a harness that could not express a failing child would leave that path untested by construction.
func loadChildScript(path string) (map[string]engine.ChildOutcome, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the children file: %w", err)
	}
	var raw map[string]struct {
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("the children file is not a JSON object of {result|error}: %w", err)
	}
	out := make(map[string]engine.ChildOutcome, len(raw))
	for name, entry := range raw {
		switch {
		case entry.Error != "":
			if entry.Result != nil {
				return nil, fmt.Errorf("the child %q is scripted as both a result and an error", name)
			}
			out[name] = engine.ChildOutcome{Completed: true, Failed: true, Error: entry.Error}
		case entry.Result != nil:
			out[name] = engine.ChildOutcome{Completed: true, Result: string(entry.Result)}
		default:
			return nil, fmt.Errorf("the child %q has neither a result nor an error", name)
		}
	}
	return out, nil
}
