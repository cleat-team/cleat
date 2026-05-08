package plugin

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// ---- Encode/Decode helpers ----

func TestEncodeOK(t *testing.T) {
	v := EncodeOK()
	if v != 0 {
		t.Errorf("EncodeOK() = %d, want 0", v)
	}
}

func TestEncodeOKWithLen(t *testing.T) {
	tests := []struct {
		len  uint32
		want uint64
	}{
		{0, 0},
		{1, 1 << 32},
		{255, 255 << 32},
		{4294967295, 4294967295 << 32}, // max uint32
	}
	for _, tc := range tests {
		got := EncodeOKWithLen(tc.len)
		if got != tc.want {
			t.Errorf("EncodeOKWithLen(%d) = %d, want %d", tc.len, got, tc.want)
		}
	}
}

func TestEncodeError(t *testing.T) {
	v := EncodeError(nil)
	if v != 1 {
		t.Errorf("EncodeError(nil) = %d, want 1 (errorCode=1)", v)
	}

	v = EncodeError(assertAnError)
	if v != 1 {
		t.Errorf("EncodeError(err) = %d, want 1", v)
	}
}

var assertAnError = &errType{}

type errType struct{}

func (e *errType) Error() string { return "something went wrong" }

func TestEncodeSuspend(t *testing.T) {
	v := EncodeSuspend()
	expected := uint64(1) << 62
	if v != expected {
		t.Errorf("EncodeSuspend() = %d, want %d", v, expected)
	}
}

// ---- CallContext helpers ----

func TestWithCallContextAndCallContextFromContext(t *testing.T) {
	ctx := context.Background()

	// Extract without setting should return nil.
	cc := CallContextFromContext(ctx)
	if cc != nil {
		t.Error("expected nil CallContext from background context")
	}

	tenantID := uuid.New()
	expected := &CallContext{
		TenantID:   tenantID,
		WorkflowID: "wf-123",
	}
	ctx = WithCallContext(ctx, expected)

	got := CallContextFromContext(ctx)
	if got == nil {
		t.Fatal("expected non-nil CallContext after WithCallContext")
	}
	if got.TenantID != tenantID {
		t.Errorf("TenantID = %v, want %v", got.TenantID, tenantID)
	}
	if got.WorkflowID != "wf-123" {
		t.Errorf("WorkflowID = %q, want %q", got.WorkflowID, "wf-123")
	}
}

func TestCallContextFromContextReturnsNilForWrongType(t *testing.T) {
	ctx := context.WithValue(context.Background(), callContextKeyType{}, "not-a-callcontext")
	cc := CallContextFromContext(ctx)
	if cc != nil {
		t.Error("expected nil for wrong value type")
	}
}

func TestWithCallContextMultipleTimes(t *testing.T) {
	ctx := context.Background()
	cc1 := &CallContext{TenantID: uuid.New(), WorkflowID: "wf-1"}
	cc2 := &CallContext{TenantID: uuid.New(), WorkflowID: "wf-2"}

	ctx = WithCallContext(ctx, cc1)
	ctx = WithCallContext(ctx, cc2)

	// Subsequent call should override.
	got := CallContextFromContext(ctx)
	if got.WorkflowID != "wf-2" {
		t.Errorf("expected wf-2, got %q", got.WorkflowID)
	}
}

