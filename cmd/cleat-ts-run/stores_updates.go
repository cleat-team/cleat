// The `updates` store for cleat-ts-run (cleat#3827, group A): an in-memory engine.UpdateStore driven by the test's script.
//
// WHY THIS EXISTS AND WHY IT IS NOT OPTIONAL. Without an UpdateStore the engine answers `cleat_poll_update` with "nothing
// pending" and says so to nobody (engine/updater.go:98), so a test of an update handler would run to completion, deliver
// nothing, and PASS. The store is what makes an update arrive, and its report (`extras.updates`) is what lets a test assert
// on the store's side of the exchange -- that the request was completed, and with what -- rather than only on what the
// workflow returned.
//
// WHAT IT IS NOT. It is not the shipped store. Requests are the ones the test scripted, all pending from the start of the
// run, delivered oldest first; there is no HTTP caller, so a request's promise settles into a record this store keeps and not
// into a waiter. Where that differs from a deployed store in a way a workflow could observe, packages/cleat-ts/DIVERGENCES.md
// says so (the group's closing PR collects it).
//
// FIXTURE: a JSON array, one object per request, in delivery order:
//
//	[{"name": "add", "payload": {"n": 5}}, {"name": "ping"}]
//
// `payload` is any JSON value (absent means the empty payload) and is handed to the handler as that value's JSON text.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/cleat-team/cleat/engine"
)

func init() { registerStore("updates", loadUpdatesStore) }

type updateScript struct {
	Name    string          `json:"name"`
	Payload json.RawMessage `json:"payload"`
}

// updateRow is one scripted request and what became of it.
type updateRow struct {
	info      engine.UpdateRequestInfo
	completed bool
	result    string
	errMsg    string
}

// settledPromise is what the store was asked to do to a caller's promise.
type settledPromise struct {
	PromiseID string `json:"promiseId"`
	Resolved  bool   `json:"resolved"`
	Value     string `json:"value,omitempty"`
	Error     string `json:"error,omitempty"`
}

// completedUpdate is the envelope's record of one completed request.
type completedUpdate struct {
	Name      string `json:"name"`
	RequestID string `json:"requestId"`
	Result    string `json:"result,omitempty"`
	Error     string `json:"error,omitempty"`
}

type memoryUpdates struct {
	mu      sync.Mutex
	rows    []*updateRow
	order   []string // request ids in completion order
	settled []settledPromise
}

func loadUpdatesStore(path string) (storeSpec, error) {
	m, err := newMemoryUpdatesFromFile(path)
	if err != nil {
		return storeSpec{}, err
	}
	return updatesSpec(m), nil
}

// updatesSpec is what the registry is handed for a store: the engine option, and the report read after the run.
func updatesSpec(m *memoryUpdates) storeSpec {
	return storeSpec{
		Option: engine.WithUpdateStore(m),
		Extra:  func() (string, any) { return "updates", m.report() },
	}
}

// newMemoryUpdatesFromFile is the loader without the spec around it, so a test can hold the store itself.
func newMemoryUpdatesFromFile(path string) (*memoryUpdates, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var script []updateScript
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields() // a misspelt key ("payloads", "id") would otherwise script a request with nothing in it
	if err := dec.Decode(&script); err != nil {
		return nil, fmt.Errorf("not a JSON array of {name, payload} objects: %w", err)
	}
	m := &memoryUpdates{}
	for i, s := range script {
		if s.Name == "" {
			return nil, fmt.Errorf("request %d has no name", i)
		}
		payload := ""
		if len(s.Payload) > 0 {
			payload = string(s.Payload)
		}
		m.rows = append(m.rows, &updateRow{info: engine.UpdateRequestInfo{
			WorkflowID: "",
			RequestID:  fmt.Sprintf("req-%d", i+1),
			UpdateName: s.Name,
			Payload:    payload,
			PromiseID:  fmt.Sprintf("prom-%d", i+1),
			Status:     "pending",
		}})
	}
	return m, nil
}

// GetPendingUpdateRequests returns the requests still awaiting an answer, oldest first. Delivery does not consume a row:
// completing it does, as in the shipped stores, so a handler that never finishes leaves its request to be redelivered.
func (m *memoryUpdates) GetPendingUpdateRequests(_ context.Context, workflowID string) ([]engine.UpdateRequestInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []engine.UpdateRequestInfo
	for _, r := range m.rows {
		if !r.completed {
			info := r.info
			info.WorkflowID = workflowID
			out = append(out, info)
		}
	}
	return out, nil
}

// CompleteUpdateRequest records the handler's outcome on the request row. An id this store never issued is an error, so a
// guest that completes the wrong request is visible rather than absorbed.
func (m *memoryUpdates) CompleteUpdateRequest(_ context.Context, _, requestID, result, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.info.RequestID == requestID {
			r.completed, r.result, r.errMsg = true, result, errMsg
			m.order = append(m.order, requestID)
			return nil
		}
	}
	return fmt.Errorf("unknown update request id %q", requestID)
}

func (m *memoryUpdates) ResolvePromise(_ context.Context, promiseID, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settled = append(m.settled, settledPromise{PromiseID: promiseID, Resolved: true, Value: value})
	return nil
}

func (m *memoryUpdates) RejectPromise(_ context.Context, promiseID, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settled = append(m.settled, settledPromise{PromiseID: promiseID, Error: errMsg})
	return nil
}

// report is what the store saw, read AFTER the run (see extrasFrom).
func (m *memoryUpdates) report() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	completed := make([]completedUpdate, 0, len(m.order))
	for _, id := range m.order {
		for _, r := range m.rows {
			if r.info.RequestID == id {
				completed = append(completed, completedUpdate{Name: r.info.UpdateName, RequestID: id, Result: r.result, Error: r.errMsg})
			}
		}
	}
	pending := 0
	for _, r := range m.rows {
		if !r.completed {
			pending++
		}
	}
	settled := m.settled
	if settled == nil {
		settled = []settledPromise{}
	}
	return map[string]any{"completed": completed, "pending": pending, "settled": settled}
}
