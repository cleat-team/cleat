package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#4340. A signal addressed to a run that has continued as new is delivered to the END of its chain, and the 200 says so: `workflow_id` is the run that now holds it and `forwarded_from` is the
// run the caller addressed, so a client holding the first id of a long chain learns where its signals go. Both keys are absent when the signal went where it was addressed, so a client that does not
// read them sees what it saw before.

// receiptStore is a store that can say where a signal went, which the plain mockStore deliberately cannot (it keeps the other delivery paths covered).
type receiptStore struct {
	*mockStore
	receipt engine.SignalReceipt
	gotKey  string
}

func (r *receiptStore) DeliverSignalReceipt(_ context.Context, _, _, _, key string) (engine.SignalReceipt, error) {
	r.gotKey = key
	return r.receipt, nil
}

func postSignalTo(t *testing.T, rs *receiptStore, key string) map[string]any {
	t.Helper()
	api := &apiServer{store: rs, worker: newTestWorker(rs.mockStore), maxBodySize: 1 << 20, factory: &fakeStoreFactory{fallback: rs}}
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-old/signal", strings.NewReader(`{"signal_name":"approve","payload":"{}"}`))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v: %s", err, rec.Body.String())
	}
	return body
}

func TestAForwardedSignalsReplyNamesTheRunThatHoldsIt(t *testing.T) {
	for _, keyed := range []bool{false, true} {
		keyed := keyed
		key := ""
		name := "keyless"
		if keyed {
			key, name = "key-1", "Idempotency-Key"
		}
		t.Run(name, func(t *testing.T) {
			fwd := &receiptStore{mockStore: &mockStore{}, receipt: engine.SignalReceipt{DeliveredTo: "wf-end", Forwarded: true}}
			body := postSignalTo(t, fwd, key)
			if body["status"] != "delivered" || body["workflow_id"] != "wf-end" || body["forwarded_from"] != "wf-old" {
				t.Errorf("a forwarded signal's reply is %v, want status=delivered workflow_id=wf-end forwarded_from=wf-old", body)
			}
			if fwd.gotKey != key {
				t.Errorf("the handler passed key %q to the store, want %q: the receipt path must carry the Idempotency-Key", fwd.gotKey, key)
			}

			// CONTROL: a signal that went where it was addressed carries neither key, so the reply is what it always was.
			plain := &receiptStore{mockStore: &mockStore{}, receipt: engine.SignalReceipt{DeliveredTo: "wf-old"}}
			body = postSignalTo(t, plain, key)
			if _, has := body["workflow_id"]; has {
				t.Errorf("an unforwarded reply carries workflow_id: %v", body)
			}
			if _, has := body["forwarded_from"]; has {
				t.Errorf("an unforwarded reply carries forwarded_from: %v", body)
			}
			if body["status"] != "delivered" {
				t.Errorf("an unforwarded reply is %v, want status=delivered", body)
			}
		})
	}
}
