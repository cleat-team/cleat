package engine

import (
	"errors"
	"testing"
)

func TestErrorCode_String(t *testing.T) {
	tests := []struct {
		code ErrorCode
		want string
	}{
		{ErrUnknown, "unknown"},
		{ErrTransient, "transient"},
		{ErrPermanent, "permanent"},
		{ErrCancelled, "cancelled"},
		{ErrTimeout, "timeout"},
		{ErrAmbiguous, "ambiguous"},
		{ErrRetriesExhausted, "retries_exhausted"},
		{ErrorCode(99), "unknown"}, // unknown code falls through to default
	}
	for _, tt := range tests {
		got := tt.code.String()
		if got != tt.want {
			t.Errorf("ErrorCode(%d).String() = %q, want %q", tt.code, got, tt.want)
		}
	}
}

func TestCleatError_Error(t *testing.T) {
	base := errors.New("connection refused")

	t.Run("with workflow ID", func(t *testing.T) {
		e := &CleatError{Code: ErrTransient, Op: "connect", WorkflowID: "wf-123", Err: base}
		want := "connect: workflow=wf-123: connection refused"
		if got := e.Error(); got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
	})

	t.Run("without workflow ID", func(t *testing.T) {
		e := &CleatError{Code: ErrPermanent, Op: "validate", Err: base}
		want := "validate: connection refused"
		if got := e.Error(); got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
	})
}

func TestCleatError_Unwrap(t *testing.T) {
	base := errors.New("underlying")
	e := &CleatError{Code: ErrTransient, Op: "op", Err: base}
	if !errors.Is(e, base) {
		t.Error("errors.Is(e, base) = false, want true")
	}
}

func TestCleatError_Retryable(t *testing.T) {
	t.Run("transient is retryable", func(t *testing.T) {
		e := &CleatError{Code: ErrTransient}
		if !e.Retryable() {
			t.Error("Retryable() = false, want true for ErrTransient")
		}
	})
	t.Run("permanent is not retryable", func(t *testing.T) {
		e := &CleatError{Code: ErrPermanent}
		if e.Retryable() {
			t.Error("Retryable() = true, want false for ErrPermanent")
		}
	})
	t.Run("timeout is not retryable", func(t *testing.T) {
		e := &CleatError{Code: ErrTimeout}
		if e.Retryable() {
			t.Error("Retryable() = true, want false for ErrTimeout")
		}
	})
}

func TestNewTransientError(t *testing.T) {
	base := errors.New("timeout")
	e := NewTransientError("dial", "wf-1", base)
	if e.Code != ErrTransient {
		t.Errorf("Code = %v, want ErrTransient", e.Code)
	}
	if e.Op != "dial" {
		t.Errorf("Op = %q, want %q", e.Op, "dial")
	}
	if e.WorkflowID != "wf-1" {
		t.Errorf("WorkflowID = %q, want %q", e.WorkflowID, "wf-1")
	}
	if !errors.Is(e, base) {
		t.Error("errors.Is(e, base) = false")
	}
}

func TestNewPermanentError(t *testing.T) {
	base := errors.New("not found")
	e := NewPermanentError("lookup", "wf-2", base)
	if e.Code != ErrPermanent {
		t.Errorf("Code = %v, want ErrPermanent", e.Code)
	}
	if e.Retryable() {
		t.Error("Retryable() = true, want false")
	}
}

func TestNewTimeoutError(t *testing.T) {
	base := errors.New("deadline exceeded")
	e := NewTimeoutError("run", "wf-3", base)
	if e.Code != ErrTimeout {
		t.Errorf("Code = %v, want ErrTimeout", e.Code)
	}
	if e.Op != "run" || e.WorkflowID != "wf-3" {
		t.Errorf("fields mismatch")
	}
}

func TestNewCancelledError(t *testing.T) {
	base := errors.New("cancelled")
	e := NewCancelledError("exec", "wf-4", base)
	if e.Code != ErrCancelled {
		t.Errorf("Code = %v, want ErrCancelled", e.Code)
	}
}

func TestNewAmbiguousError(t *testing.T) {
	base := errors.New("outcome unknown")
	e := NewAmbiguousError("call", "wf-5", base)
	if e.Code != ErrAmbiguous {
		t.Errorf("Code = %v, want ErrAmbiguous", e.Code)
	}
	if e.Retryable() {
		t.Error("Ambiguous error should not be retryable")
	}
}

func TestNewRetriesExhaustedError(t *testing.T) {
	base := errors.New("max retries")
	e := NewRetriesExhaustedError("retry", "wf-6", base)
	if e.Code != ErrRetriesExhausted {
		t.Errorf("Code = %v, want ErrRetriesExhausted", e.Code)
	}
}
