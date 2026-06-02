package plugin

import (
	"context"
	"testing"

	"github.com/cleat-team/cleat/wasmrw"
)

// ---- Encode/Decode helpers ----

func TestEncodeOK(t *testing.T) {
	v := wasmrw.OK()
	if v != 0 {
		t.Errorf("wasmrw.OK() = %d, want 0", v)
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
		got := wasmrw.OKWithLen(tc.len)
		if got != tc.want {
			t.Errorf("wasmrw.OKWithLen(%d) = %d, want %d", tc.len, got, tc.want)
		}
	}
}

func TestEncodeError(t *testing.T) {
	v := wasmrw.Error(nil)
	if v != 1 {
		t.Errorf("wasmrw.Error(nil) = %d, want 1 (errorCode=1)", v)
	}

	v = wasmrw.Error(assertAnError)
	if v != 1 {
		t.Errorf("wasmrw.Error(err) = %d, want 1", v)
	}
}

var assertAnError = &errType{}

type errType struct{}

func (e *errType) Error() string { return "something went wrong" }

func TestEncodeSuspend(t *testing.T) {
	v := wasmrw.Suspend()
	expected := uint64(1) << 62
	if v != expected {
		t.Errorf("wasmrw.Suspend() = %d, want %d", v, expected)
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

	tenantID := "550e8400-e29b-41d4-a716-446655440000"
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
	cc1 := &CallContext{TenantID: "550e8400-e29b-41d4-a716-446655440001", WorkflowID: "wf-1"}
	cc2 := &CallContext{TenantID: "550e8400-e29b-41d4-a716-446655440002", WorkflowID: "wf-2"}

	ctx = WithCallContext(ctx, cc1)
	ctx = WithCallContext(ctx, cc2)

	// Subsequent call should override.
	got := CallContextFromContext(ctx)
	if got.WorkflowID != "wf-2" {
		t.Errorf("expected wf-2, got %q", got.WorkflowID)
	}
}

