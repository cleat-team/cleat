package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
)

// cleat#4340. A signal to a run that can never be reopened (done, cancelled, continued as new) is a 409 whose body says which state refused and, for a run that continued as new, which run is current; on BOTH delivery
// paths, the keyless one and the Idempotency-Key one, because they are two statements and either could have been left behind. A run that is failed, terminated or dead_lettered is NOT refused: the signal is stored
// and the reply says so instead of claiming delivery (cleat#4345 is the owner's question whether those should refuse).

func postSettledSignal(t *testing.T, ms *mockStore, key string) *httptest.ResponseRecorder {
	t.Helper()
	api := newTestAPIServer(ms)
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-old/signal", strings.NewReader(`{"signal_name":"approve","payload":"{}"}`))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, req)
	return rec
}

func TestASignalToARunClosedForGoodIs409WithItsSuccessor(t *testing.T) {
	notOpen := func(status, to string) error {
		return fmt.Errorf("deliver: %w", &engine.WorkflowNotOpenError{WorkflowID: "wf-old", Status: status, ContinuedTo: to})
	}
	for _, tc := range []struct {
		name, status, to string
	}{
		{"a run that continued as new", "done", "wf-current"},
		{"a run that finished", "done", ""},
		{"a cancelled run", "cancelled", ""},
	} {
		for _, keyed := range []bool{false, true} {
			tc, keyed := tc, keyed
			path := map[bool]string{false: "keyless", true: "Idempotency-Key"}[keyed]
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				ms := &mockStore{}
				key := ""
				if keyed {
					key = "key-1"
					ms.deliverSignalIdempotentFn = func(context.Context, string, string, string, string) (bool, error) {
						return false, notOpen(tc.status, tc.to)
					}
				} else {
					ms.deliverSignalFn = func(context.Context, string, string, string) error { return notOpen(tc.status, tc.to) }
				}
				rec := postSettledSignal(t, ms, key)
				if rec.Code != http.StatusConflict {
					t.Fatalf("status %d, want 409; body %s", rec.Code, rec.Body.String())
				}
				var body map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("body is not a flat JSON object of strings: %v: %s", err, rec.Body.String())
				}
				if body["detail"] != "workflow_not_open" || body["status"] != tc.status || body["error"] == "" {
					t.Errorf("body %v, want detail=workflow_not_open status=%s and a message", body, tc.status)
				}
				if got, has := body["continued_to"]; got != tc.to || has != (tc.to != "") {
					t.Errorf("continued_to = %q (present=%v), want %q (present only for a continued run)", got, has, tc.to)
				}
			})
		}
	}
	// CONTROL: a refusal that is NOT this error is still what it was (404 for not-found, 500 for a plain store error), so the new branch has not swallowed the others.
	for _, keyed := range []bool{false, true} {
		ms := &mockStore{}
		key := ""
		if keyed {
			key = "key-1"
			ms.deliverSignalIdempotentFn = func(context.Context, string, string, string, string) (bool, error) {
				return false, engine.ErrWorkflowNotFound
			}
		} else {
			ms.deliverSignalFn = func(context.Context, string, string, string) error { return engine.ErrWorkflowNotFound }
		}
		if rec := postSettledSignal(t, ms, key); rec.Code != http.StatusNotFound {
			t.Errorf("CONTROL (keyed=%v): not-found came back %d, want 404", keyed, rec.Code)
		}
	}
}

// A plugin treats a settled target as TERMINAL, as it does a missing one: plugins/eventtriggers unregisters an awaiter on plugin.ErrWorkflowNotFound and retries anything else, so a bare store error
// here would retry a signal that can never be read.
func TestAPluginSeesASettledTargetAsTerminalAndKeepsTheReason(t *testing.T) {
	ne := &engine.WorkflowNotOpenError{WorkflowID: "wf-old", Status: "cancelled"}
	got := translateSignalDeliveryError(fmt.Errorf("deliver: %w", ne))
	if !errors.Is(got, plugin.ErrWorkflowNotFound) {
		t.Fatalf("a settled target translated to %v, want it to satisfy plugin.ErrWorkflowNotFound so the awaiter is dropped", got)
	}
	if !errors.Is(got, engine.ErrWorkflowNotOpen) || !strings.Contains(got.Error(), "cancelled") {
		t.Errorf("the translation lost the reason: %q (errors.Is NotOpen = %v)", got, errors.Is(got, engine.ErrWorkflowNotOpen))
	}
	if got := translateSignalDeliveryError(engine.ErrWorkflowNotFound); got != plugin.ErrWorkflowNotFound {
		t.Errorf("not-found translated to %v, want plugin.ErrWorkflowNotFound unchanged", got)
	}
	boom := errors.New("connection reset")
	if got := translateSignalDeliveryError(boom); got != boom || errors.Is(got, plugin.ErrWorkflowNotFound) {
		t.Errorf("an ordinary error came back as %v, want it unchanged and still retryable", got)
	}
}

// The three redrivable statuses: accepted and stored, and the reply must not say "delivered" without qualification. Same 200, so a client that checks only the code is unaffected.
func TestAStoredSignalToARedrivableRunIsReportedAsStoredNotDelivered(t *testing.T) {
	for _, status := range []string{"failed", "terminated", "dead_lettered"} {
		for _, keyed := range []bool{false, true} {
			status, keyed := status, keyed
			path := map[bool]string{false: "keyless", true: "Idempotency-Key"}[keyed]
			t.Run(status+"/"+path, func(t *testing.T) {
				ms := &mockStore{}
				ms.getWorkflowByIDFn = func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
					return &engine.WorkflowInstance{ID: id, DefName: "d", DefVersion: 1, Status: status}, nil
				}
				key := ""
				if keyed {
					key = "key-1"
				}
				rec := postSettledSignal(t, ms, key)
				if rec.Code != http.StatusOK {
					t.Fatalf("status %d, want 200 (the same success code, so a client checking only the code is unaffected); body %s", rec.Code, rec.Body.String())
				}
				var body map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("body is not JSON: %v: %s", err, rec.Body.String())
				}
				if body["status"] == "delivered" {
					t.Errorf("body %v says \"delivered\" for a signal to a %s run that reads it only if it is redriven", body, status)
				}
				if body["status"] != "stored" || body["run_status"] != status || !strings.Contains(fmt.Sprint(body["detail"]), "retried or re-replayed") {
					t.Errorf("body %v, want status=stored run_status=%s and a detail saying the run reads it only if it is retried or re-replayed", body, status)
				}
				if got, has := body["idempotent_replay"]; !has || got != false {
					t.Errorf("idempotent_replay = %v (present=%v), want present and false: the standard flag must still be there", got, has)
				}
			})
		}
	}

	// CONTROLS: an open run is still "delivered"; and the status lookup is ADVISORY, so a read that fails AFTER the store accepted the signal must not turn it into a failure. The ownership check in front of
	// the handler reads the same run BEFORE delivery, so the failing read is armed only once the store has accepted the signal, or the request would be refused earlier for a different reason.
	for name, afterDelivery := range map[string]func(context.Context, string) (*engine.WorkflowInstance, error){
		"an open run": func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
			return &engine.WorkflowInstance{ID: id, DefName: "d", DefVersion: 1, Status: "running"}, nil
		},
		"a status read that fails after the signal was accepted": func(context.Context, string) (*engine.WorkflowInstance, error) {
			return nil, errors.New("read failed")
		},
		"a run the read cannot find after the signal was accepted": func(context.Context, string) (*engine.WorkflowInstance, error) { return nil, nil },
	} {
		afterDelivery := afterDelivery
		delivered := false
		ms := &mockStore{}
		ms.deliverSignalFn = func(context.Context, string, string, string) error { delivered = true; return nil }
		ms.getWorkflowByIDFn = func(ctx context.Context, id string) (*engine.WorkflowInstance, error) {
			if delivered {
				return afterDelivery(ctx, id)
			}
			return &engine.WorkflowInstance{ID: id, DefName: "d", DefVersion: 1, Status: "running"}, nil
		}
		rec := postSettledSignal(t, ms, "")
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if !delivered {
			t.Fatalf("CONTROL (%s): the store was never asked to deliver (got %d %v): the control did not reach the code it is about", name, rec.Code, body)
		}
		if rec.Code != http.StatusOK || body["status"] != "delivered" {
			t.Errorf("CONTROL (%s): got %d %v, want 200 with status=delivered", name, rec.Code, body)
		}
	}
}
