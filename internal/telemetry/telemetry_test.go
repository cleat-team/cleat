package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestInitTracingEmptyEndpoint(t *testing.T) {
	ctx := context.Background()
	shutdown, err := InitTracing(ctx, "", "test-service")
	if err != nil {
		t.Fatalf("InitTracing with empty endpoint failed: %v", err)
	}
	if shutdown == nil {
		t.Fatal("expected non-nil shutdown function")
	}
	if err := shutdown(ctx); err != nil {
		t.Errorf("shutdown should not error for no-op: %v", err)
	}
	if Tracer == nil {
		t.Error("Tracer should be set even with no-op provider")
	}
}

func TestWorkflowSpan(t *testing.T) {
	ctx := context.Background()
	shutdown, err := InitTracing(ctx, "", "test-service")
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)

	ctx, span := WorkflowSpan(ctx, "wf-123", "my-workflow", 1, "tenant-1", "trace-abc")
	if span == nil {
		t.Fatal("expected non-nil span")
	}
	defer span.End()

	// With a no-op tracer, the span context is valid.
	sc := span.SpanContext()
	if sc.IsValid() {
		t.Log("span context is valid (expected for no-op tracer)")
	}
}

func TestEventSpanWithoutServiceOrOperation(t *testing.T) {
	ctx := context.Background()
	shutdown, _ := InitTracing(ctx, "", "test-service")
	defer shutdown(ctx)

	ctx, span := EventSpan(ctx, 1, "http_request", "", "")
	if span == nil {
		t.Fatal("expected non-nil span")
	}
	defer span.End()
}

func TestEventSpanWithServiceAndOperation(t *testing.T) {
	ctx := context.Background()
	shutdown, _ := InitTracing(ctx, "", "test-service")
	defer shutdown(ctx)

	ctx, span := EventSpan(ctx, 2, "db_query", "postgres", "SELECT")
	if span == nil {
		t.Fatal("expected non-nil span")
	}
	defer span.End()
}

func TestEventSpanWithServiceOnly(t *testing.T) {
	ctx := context.Background()
	shutdown, _ := InitTracing(ctx, "", "test-service")
	defer shutdown(ctx)

	ctx, span := EventSpan(ctx, 0, "start", "my-service", "")
	if span == nil {
		t.Fatal("expected non-nil span")
	}
	defer span.End()
}

func TestEventSpanWithOperationOnly(t *testing.T) {
	ctx := context.Background()
	shutdown, _ := InitTracing(ctx, "", "test-service")
	defer shutdown(ctx)

	ctx, span := EventSpan(ctx, 5, "custom", "", "do-thing")
	if span == nil {
		t.Fatal("expected non-nil span")
	}
	defer span.End()
}

func TestSpanContextFromTraceIDValid(t *testing.T) {
	sc, err := spanContextFromTraceID("abcdef0123456789abcdef0123456789")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sc.IsValid() {
		t.Error("expected valid SpanContext")
	}
	if sc.TraceFlags() != trace.TraceFlags(1) {
		t.Error("expected TraceFlags to be 1 (sampled)")
	}
}

func TestSpanContextFromTraceIDInvalid(t *testing.T) {
	tests := []struct {
		name    string
		traceID string
	}{
		{"empty", ""},
		{"too short", "abc"},
		{"non-hex", "ghijklmnopqrstuvwxyz0123456789"},
		{"wrong length 31", "abcdef0123456789abcdef012345678"},
		{"wrong length 33", "abcdef0123456789abcdef0123456789a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := spanContextFromTraceID(tt.traceID)
			if err == nil {
				t.Error("expected error for invalid trace ID")
			}
		})
	}
}

func TestTracerIsSetAfterInit(t *testing.T) {
	// Reset Tracer to nil to verify InitTracing sets it.
	Tracer = nil
	ctx := context.Background()
	_, err := InitTracing(ctx, "", "reset-test")
	if err != nil {
		t.Fatal(err)
	}
	if Tracer == nil {
		t.Error("expected Tracer to be set after InitTracing")
	}
}
