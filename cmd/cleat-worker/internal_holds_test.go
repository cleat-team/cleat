package main

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// A real listener and a real client, not a direct call into the handler --
// cleat#2196's design says this channel must be "testable on its own (ask a
// worker about a run it holds vs. doesn't)", and askInternalHolds/
// handleInternalHolds agreeing about the wire format is exactly what a
// direct call would not exercise.
func TestInternalHoldsRoundTrip(t *testing.T) {
	const secret = "test-shared-secret-abc123"

	w := &Worker{}
	w.inflight.Store("run-held", &engine.WorkflowInstance{ID: "run-held", Generation: 7})
	w.inflight.Store("run-old-generation", &engine.WorkflowInstance{ID: "run-old-generation", Generation: 3})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := newInternalHoldsServer(secret, w)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	addr := ln.Addr().String()
	client := &http.Client{Timeout: 5 * time.Second}
	ctx := context.Background()

	for _, tc := range []struct {
		name       string
		runID      string
		generation int64
		wantHeld   bool
	}{
		{"held: matching run and generation", "run-held", 7, true},
		{"not held: right run, stale generation asked about", "run-old-generation", 1, false},
		{"not held: run this worker never claimed", "run-unknown", 1, false},
		{"not held: right run, generation this worker is not on", "run-held", 6, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			held, err := askInternalHolds(ctx, client, addr, secret, tc.runID, tc.generation)
			if err != nil {
				t.Fatalf("askInternalHolds: %v", err)
			}
			if held != tc.wantHeld {
				t.Errorf("askInternalHolds(%q, generation=%d) = %v, want %v", tc.runID, tc.generation, held, tc.wantHeld)
			}
		})
	}

	t.Run("wrong secret is refused, not just unanswered", func(t *testing.T) {
		held, err := askInternalHolds(ctx, client, addr, "wrong-secret", "run-held", 7)
		if err != nil {
			t.Fatalf("askInternalHolds: %v", err)
		}
		if held {
			t.Error("a wrong secret returned held=true -- the auth check did not run, or did not refuse")
		}
	})

	t.Run("no Authorization header at all is refused", func(t *testing.T) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/internal/holds/run-held?generation=7", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("no Authorization header: status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
		}
	})

	t.Run("an empty configured secret refuses every caller, including one presenting an empty bearer value", func(t *testing.T) {
		if validInternalAuth(mustRequest(t, "run-held", ""), "") {
			t.Error("validInternalAuth(_, \"\") = true for an empty bearer value against an empty secret, want false")
		}
	})
}

func mustRequest(t *testing.T, runID, bearer string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://example/internal/holds/"+runID, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	return req
}
