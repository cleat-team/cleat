// Package telemetry provides OpenTelemetry tracing initialization and
// span helpers for cleat workflows.
//
// It configures OTLP HTTP export, manages a global tracer, and provides
// convenience functions for creating workflow-level and event-level spans.
//
// Key functions:
//   - InitTracing — initializes OTLP trace export with configurable endpoint
//   - WorkflowSpan — creates a span for workflow execution
//   - EventSpan — creates a span for workflow events
package telemetry
