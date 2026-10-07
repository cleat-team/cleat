package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// TestReplayUnderOriginalKey_StatusCodeDecidesTheOutcome exercises
// dbServiceCaller.ReplayUnderOriginalKey directly, one case per status code
// -- the same shape as TestResolveCall_StatusCodeDecidesTheOutcome, for the
// sibling mechanism cleat#2897 adds. Not the acceptance-table coverage: see
// TestIdempotencyKeyReplayEndToEnd_* for a real crash, a real worker and a
// real second dispatch.
func TestReplayUnderOriginalKey_StatusCodeDecidesTheOutcome(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		body         string
		wantOutcome  engine.IdempotencyReplayOutcome
		wantResponse string
		wantErr      bool
	}{
		{
			name:         "200: the service's own key table answers",
			status:       http.StatusOK,
			body:         `{"charge_id":"ch_1"}`,
			wantOutcome:  engine.IdempotencyReplayResolved,
			wantResponse: `{"charge_id":"ch_1"}`,
		},
		{
			name:        "409: a request under this key is still being processed",
			status:      http.StatusConflict,
			wantOutcome: engine.IdempotencyReplayRetryLater,
		},
		{
			name:        "500: cannot say",
			status:      http.StatusInternalServerError,
			wantOutcome: engine.IdempotencyReplayCannotSay,
		},
		{
			name:        "404: cannot say -- NOT the same meaning as --ambiguity-lookup's 404",
			status:      http.StatusNotFound,
			wantOutcome: engine.IdempotencyReplayCannotSay,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotKey, gotBody string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotKey = r.Header.Get("Idempotency-Key")
				buf, _ := io.ReadAll(r.Body)
				gotBody = string(buf)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			ops, err := parseIdempotencyKeyOps("payment.charge")
			if err != nil {
				t.Fatalf("parseIdempotencyKeyOps: %v", err)
			}
			c := &dbServiceCaller{
				serviceEndpoints:  map[string]string{"payment": srv.URL},
				idempotencyKeyOps: ops,
				egress:            allowLoopbackForTest(),
			}

			resp, outcome, err := c.ReplayUnderOriginalKey(context.Background(), "payment", "charge", `{"amount":100}`, "key-abc")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if outcome != tc.wantOutcome {
				t.Errorf("outcome = %v, want %v", outcome, tc.wantOutcome)
			}
			if resp != tc.wantResponse {
				t.Errorf("response = %q, want %q", resp, tc.wantResponse)
			}
			if gotPath != "/call/payment/charge" {
				t.Errorf("path = %q, want /call/payment/charge -- the SAME operation, "+
					"not a separate lookup", gotPath)
			}
			if gotKey != "key-abc" {
				t.Errorf("Idempotency-Key = %q, want the ORIGINAL attempt's key", gotKey)
			}
			if gotBody != `{"amount":100}` {
				t.Errorf("request body = %q, want the ORIGINAL request -- re-dispatching means "+
					"sending the same call again, not an empty probe", gotBody)
			}
		})
	}
}

// TestReplayUnderOriginalKey_UnconfiguredOperationCannotSay mirrors
// TestResolveCall_UnconfiguredOperationCannotSay: this mechanism only
// answers for an operation --idempotency-key-ops named.
func TestReplayUnderOriginalKey_UnconfiguredOperationCannotSay(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ops, err := parseIdempotencyKeyOps("payment.charge")
	if err != nil {
		t.Fatalf("parseIdempotencyKeyOps: %v", err)
	}
	c := &dbServiceCaller{
		serviceEndpoints:  map[string]string{"payment": srv.URL},
		idempotencyKeyOps: ops,
		egress:            allowLoopbackForTest(),
	}

	resp, outcome, err := c.ReplayUnderOriginalKey(context.Background(), "payment", "refund", "{}", "key-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome != engine.IdempotencyReplayCannotSay {
		t.Errorf("outcome = %v, want IdempotencyReplayCannotSay", outcome)
	}
	if resp != "" {
		t.Errorf("response = %q, want empty", resp)
	}
	if reached {
		t.Error("the service was called for an operation not declared --idempotency-key-ops")
	}
}
