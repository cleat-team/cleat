package main

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
)

func TestSetupTelemetryDisabled(t *testing.T) {
	cleanup := setupTelemetry(context.Background(), "", true, "test-worker")
	defer cleanup() //nolint:errcheck

	tp := otel.GetTracerProvider()
	if tp == nil {
		t.Error("expected non-nil tracer provider (noop)")
	}
}

func TestSetupTelemetryEmptyEndpoint(t *testing.T) {
	cleanup := setupTelemetry(context.Background(), "", false, "test-worker")
	defer cleanup() //nolint:errcheck

	tp := otel.GetTracerProvider()
	if tp == nil {
		t.Error("expected non-nil tracer provider (noop)")
	}
}

func TestSetupTelemetry_OTLPEndpointUnreachable(t *testing.T) {
	// Use an endpoint that is extremely unlikely to have a collector
	// listening.  The function should either fall back to a no-op tracer
	// gracefully or create a tracer provider whose cleanup does not panic.
	cleanup := setupTelemetry(context.Background(), "127.0.0.1:1", false, "test-worker")
	cleanup() // should not panic
}
