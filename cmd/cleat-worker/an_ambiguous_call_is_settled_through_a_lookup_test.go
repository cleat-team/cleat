package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// TestResolveCall_StatusCodeDecidesTheOutcome is the acceptance table from
// cleat#1984, exercised against dbServiceCaller.ResolveCall directly: one
// case per row.
func TestResolveCall_StatusCodeDecidesTheOutcome(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		body         string
		wantOutcome  engine.AmbiguityOutcome
		wantResponse string
		wantErr      bool
	}{
		{
			name:         "200: the call happened",
			status:       http.StatusOK,
			body:         `{"charge_id":"ch_1"}`,
			wantOutcome:  engine.AmbiguityResolved,
			wantResponse: `{"charge_id":"ch_1"}`,
		},
		{
			name:        "404: the call never arrived",
			status:      http.StatusNotFound,
			wantOutcome: engine.AmbiguityNotSent,
		},
		{
			name:        "500: cannot say",
			status:      http.StatusInternalServerError,
			wantOutcome: engine.AmbiguityCannotSay,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotKey string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotKey = r.Header.Get("Idempotency-Key")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			lookups, err := parseAmbiguityLookup("payment.charge=payment.get_by_key")
			if err != nil {
				t.Fatalf("parseAmbiguityLookup: %v", err)
			}
			c := &dbServiceCaller{
				serviceEndpoints: map[string]string{"payment": srv.URL},
				ambiguityLookup:  lookups,
				egress:           allowLoopbackForTest(),
			}

			resp, outcome, err := c.ResolveCall(context.Background(), "payment", "charge", "key-abc")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if outcome != tc.wantOutcome {
				t.Errorf("outcome = %v, want %v", outcome, tc.wantOutcome)
			}
			if resp != tc.wantResponse {
				t.Errorf("response = %q, want %q", resp, tc.wantResponse)
			}
			if gotPath != "/call/payment/get_by_key" {
				t.Errorf("path = %q, want /call/payment/get_by_key -- the LOOKUP operation, "+
					"not the original one", gotPath)
			}
			if gotKey != "key-abc" {
				t.Errorf("Idempotency-Key = %q, want the ORIGINAL attempt's key -- the whole "+
					"point is asking about the same attempt", gotKey)
			}
		})
	}
}

// TestResolveCall_UnconfiguredOperationCannotSay pins that ResolveCall only
// answers for an operation --ambiguity-lookup named. A resolver that guessed
// about an unconfigured operation would be answering a question nobody asked
// it to.
func TestResolveCall_UnconfiguredOperationCannotSay(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	lookups, err := parseAmbiguityLookup("payment.charge=payment.get_by_key")
	if err != nil {
		t.Fatalf("parseAmbiguityLookup: %v", err)
	}
	c := &dbServiceCaller{
		serviceEndpoints: map[string]string{"payment": srv.URL},
		ambiguityLookup:  lookups,
		egress:           allowLoopbackForTest(),
	}

	resp, outcome, err := c.ResolveCall(context.Background(), "payment", "refund", "key-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome != engine.AmbiguityCannotSay {
		t.Errorf("outcome = %v, want AmbiguityCannotSay", outcome)
	}
	if resp != "" {
		t.Errorf("response = %q, want empty", resp)
	}
	if reached {
		t.Error("the service was called for an operation no lookup was configured for; " +
			"a resolver answering about something it was never asked to look up is " +
			"guessing, not resolving")
	}
}

// TestResolveCall_BodyIsLimited pins that a pathological lookup response
// cannot exhaust memory -- the same 1MB cap forwardToService already applies
// to an ordinary call's response.
func TestResolveCall_BodyIsLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		buf := make([]byte, 1<<21) // 2MB, twice the cap
		_, _ = w.Write(buf)
	}))
	defer srv.Close()

	lookups, err := parseAmbiguityLookup("payment.charge=payment.get_by_key")
	if err != nil {
		t.Fatalf("parseAmbiguityLookup: %v", err)
	}
	c := &dbServiceCaller{
		serviceEndpoints: map[string]string{"payment": srv.URL},
		ambiguityLookup:  lookups,
		egress:           allowLoopbackForTest(),
	}

	resp, outcome, err := c.ResolveCall(context.Background(), "payment", "charge", "key-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome != engine.AmbiguityResolved {
		t.Errorf("outcome = %v, want AmbiguityResolved", outcome)
	}
	if len(resp) != 1<<20 {
		t.Errorf("response length = %d, want the 1MB cap (%d)", len(resp), 1<<20)
	}
}
