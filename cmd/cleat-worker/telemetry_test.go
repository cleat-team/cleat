package main

import (
	"context"
	"testing"
)

func TestSetupTelemetryDisabled(t *testing.T) {
	cleanup := setupTelemetry(context.Background(), "", true, "test-worker")
	cleanup() // should not panic
}

func TestSetupTelemetryEmptyEndpoint(t *testing.T) {
	cleanup := setupTelemetry(context.Background(), "", false, "test-worker")
	cleanup() // should not panic
}
