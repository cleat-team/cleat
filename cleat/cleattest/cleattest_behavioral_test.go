package cleattest

import (
	"testing"
)

// ---------------------------------------------------------------------------
// SetCancelled / ClearCancelled
// ---------------------------------------------------------------------------

func TestSetCancelledAndClearCancelled(t *testing.T) {
	env := NewTestEnv()

	// Initially, no cancellation.
	cancelled, reason := env.H().PollCancellation()
	if cancelled {
		t.Fatal("expected no cancellation initially")
	}
	if reason != "" {
		t.Fatalf("expected empty reason initially, got %q", reason)
	}

	// Set cancellation with a reason.
	env.SetCancelled("test reason")
	cancelled, reason = env.H().PollCancellation()
	if !cancelled {
		t.Fatal("expected cancellation after SetCancelled")
	}
	if reason != "test reason" {
		t.Fatalf("expected reason %q, got %q", "test reason", reason)
	}

	// Clear cancellation.
	env.ClearCancelled()
	cancelled, reason = env.H().PollCancellation()
	if cancelled {
		t.Fatal("expected no cancellation after ClearCancelled")
	}
	if reason != "" {
		t.Fatalf("expected empty reason after ClearCancelled, got %q", reason)
	}
}

func TestSetCancelledRoundTrip(t *testing.T) {
	env := NewTestEnv()

	// Set multiple times, verifying latest reason.
	env.SetCancelled("reason-1")
	cancelled, reason := env.H().PollCancellation()
	if !cancelled || reason != "reason-1" {
		t.Fatalf("expected (true, reason-1), got (%v, %q)", cancelled, reason)
	}

	env.SetCancelled("reason-2")
	cancelled, reason = env.H().PollCancellation()
	if !cancelled || reason != "reason-2" {
		t.Fatalf("expected (true, reason-2), got (%v, %q)", cancelled, reason)
	}

	// Clear and verify.
	env.ClearCancelled()
	cancelled, reason = env.H().PollCancellation()
	if cancelled || reason != "" {
		t.Fatalf("expected (false, \"\") after ClearCancelled, got (%v, %q)", cancelled, reason)
	}
}
