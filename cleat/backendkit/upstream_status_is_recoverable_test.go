package backendkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// cleat#2718: classifyError used to return a plain fmt.Errorf with no status
// a caller could read back out, so every example backend mapped a refused
// request (401, 404, any 4xx) to the same 502 it used for a genuinely
// unreachable worker. This file asserts the two halves of the fix: the
// status survives as a typed error through classifyError AND the sentinel
// wrapping it doesn't shed it, and WriteUpstreamError reads it back out
// correctly on both sides of the 4xx/other boundary.
func TestUpstreamStatusIsRecoverable(t *testing.T) {
	t.Run("classifyError attaches the real status for a plain refusal", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid or revoked API key"}`))
		}))
		defer srv.Close()

		_, err := New(srv.URL).GetWorkflow(context.Background(), "run-1")
		if err == nil {
			t.Fatal("expected an error from a 401 response")
		}
		var se *UpstreamStatusError
		if !errors.As(err, &se) {
			t.Fatalf("errors.As(err, &UpstreamStatusError) found nothing; err=%v", err)
		}
		if se.Status != http.StatusUnauthorized {
			t.Errorf("Status = %d, want %d", se.Status, http.StatusUnauthorized)
		}
	})

	// THE CASE A NAME-ONLY CHECK WOULD MISS: a 409 that DOES carry a specific
	// sentinel must still expose the status via errors.As, not just the
	// sentinel via errors.Is. Before this fix's second commit, the sentinel
	// branches returned a plain fmt.Errorf wrapping ONLY the sentinel, with no
	// path back to the status at all.
	t.Run("a sentinel-wrapped 409 still exposes the status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"conflict","detail":"idempotency_key_input_mismatch"}`))
		}))
		defer srv.Close()

		_, err := New(srv.URL).StartWorkflowWithOptions(context.Background(), "wf",
			json.RawMessage(`{}`), StartOptions{IdempotencyKey: "k"})
		if !errors.Is(err, ErrIdempotencyKeyInputMismatch) {
			t.Fatalf("errors.Is(err, ErrIdempotencyKeyInputMismatch) = false; err=%v", err)
		}
		var se *UpstreamStatusError
		if !errors.As(err, &se) {
			t.Fatalf("errors.As(err, &UpstreamStatusError) found nothing on a sentinel-wrapped "+
				"error; err=%v", err)
		}
		if se.Status != http.StatusConflict {
			t.Errorf("Status = %d, want %d", se.Status, http.StatusConflict)
		}
	})

	t.Run("a transport failure (no HTTP response at all) has no recoverable status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		srv.Close() // closed before the call: connection refused, never reaches classifyError

		_, err := New(srv.URL).GetWorkflow(context.Background(), "run-1")
		if err == nil {
			t.Fatal("expected an error dialing a closed server")
		}
		var se *UpstreamStatusError
		if errors.As(err, &se) {
			t.Fatalf("errors.As found a status (%d) on a connection failure, which never reached "+
				"classifyError", se.Status)
		}
	})
}

// A recorder that always errors on WriteHeader would hide a bug where
// WriteUpstreamError never calls it; asserting through httptest.NewRecorder
// on the real status code is simpler and covers the same ground.
func TestWriteUpstreamError(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{
			name:       "a 401 passes through unchanged",
			err:        &UpstreamStatusError{Status: http.StatusUnauthorized, Body: []byte("nope")},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "a 404 passes through unchanged",
			err:        &UpstreamStatusError{Status: http.StatusNotFound, Body: []byte("no such run")},
			wantStatus: http.StatusNotFound,
		},
		{
			// THE CASE THIS FIX EXISTS FOR: before it, this was the ONLY
			// behavior WriteUpstreamError's call sites had, for every error.
			name: "a sentinel-wrapped 409 passes through, not just the bare type",
			err: fmt.Errorf("%w: %w", ErrIdempotencyKeyInputMismatch,
				&UpstreamStatusError{Status: http.StatusConflict, Body: []byte("{}")}),
			wantStatus: http.StatusConflict,
		},
		{
			name:       "a 5xx from the worker maps to 502, not passed through",
			err:        &UpstreamStatusError{Status: http.StatusServiceUnavailable, Body: []byte("down")},
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "an untyped error (transport failure) maps to 502",
			err:        errors.New("dial tcp: connection refused"),
			wantStatus: http.StatusBadGateway,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			WriteUpstreamError(rec, tc.err)
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}
