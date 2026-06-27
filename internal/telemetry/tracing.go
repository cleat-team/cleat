package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"go.opentelemetry.io/otel/trace"
)

// Tracer is the global cleat tracer. It is set by InitTracing and used by
// WorkflowSpan and EventSpan.
var Tracer trace.Tracer

// InitTracing initializes OTLP trace export. Returns a shutdown function.
// endpoint is the OTLP HTTP endpoint, e.g., "localhost:4318".
// serviceName identifies this deployment (e.g., "cleat-worker").
// If endpoint is empty, a no-op tracer provider is used and the returned
// shutdown is a no-op.
func InitTracing(ctx context.Context, endpoint, serviceName string) (func(context.Context) error, error) {
	if endpoint == "" {
		// No tracing configured -- use a no-op provider.
		Tracer = otel.Tracer("cleat")
		return func(ctx context.Context) error { return nil }, nil
	}

	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithInsecure(), // allow HTTP for local dev
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: failed to create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: failed to create resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	Tracer = tp.Tracer("cleat")

	slog.InfoContext(context.Background(), "telemetry exporting traces", "endpoint", endpoint)
	return tp.Shutdown, nil
}

// WorkflowSpan creates a root span for a workflow execution.
// If traceID is a non-empty 32-char hex string, the span is created with a
// W3C TraceContext parent link for end-to-end propagation.
func WorkflowSpan(ctx context.Context, workflowID, defName string, defVersion int, tenantID string, traceID string) (context.Context, trace.Span) {
	opts := []trace.SpanStartOption{
		trace.WithAttributes(
			attribute.String("workflow.id", workflowID),
			attribute.String("workflow.def_name", defName),
			attribute.Int("workflow.def_version", defVersion),
			attribute.String("tenant.id", tenantID),
		),
	}
	if traceID != "" {
		opts = append(opts, trace.WithAttributes(attribute.String("trace.id", traceID)))
		if sc, err := spanContextFromTraceID(traceID); err == nil {
			opts = append(opts, trace.WithLinks(trace.Link{SpanContext: sc}))
		}
	}
	return otel.Tracer("cleat").Start(ctx, "workflow.execute", opts...)
}

// EventSpan creates a span for a single event in the workflow history.
func EventSpan(ctx context.Context, step int, eventType, service, operation string) (context.Context, trace.Span) {
	attrs := []attribute.KeyValue{
		attribute.Int("event.step", step),
		attribute.String("event.type", eventType),
	}
	if service != "" {
		attrs = append(attrs, attribute.String("event.service", service))
	}
	if operation != "" {
		attrs = append(attrs, attribute.String("event.operation", operation))
	}
	return otel.Tracer("cleat").Start(ctx, "event."+eventType, trace.WithAttributes(attrs...))
}

// spanContextFromTraceID creates a W3C SpanContext from a 32-char hex trace ID.
// A random span ID is generated and the trace flags are set to sampled (1).
func spanContextFromTraceID(traceID string) (trace.SpanContext, error) {
	b, err := hex.DecodeString(traceID)
	if err != nil || len(b) != 16 {
		return trace.SpanContext{}, fmt.Errorf("invalid trace ID %q: must be 32 hex chars", traceID)
	}
	var tid trace.TraceID
	copy(tid[:], b)

	var sid trace.SpanID
	_, _ = rand.Read(sid[:])

	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.TraceFlags(1), // sampled
	}), nil
}
