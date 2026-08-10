package main

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// setupTelemetry configures OpenTelemetry tracing. If disabled is true, a no-op
// tracer provider is installed and the returned cleanup function is a no-op.
// Otherwise, it creates an OTLP HTTP exporter, a batch span processor, and
// configures the global tracer provider. The returned function should be deferred
// to flush and shut down the tracer provider.
func setupTelemetry(ctx context.Context, endpoint string, disabled bool, workerID string) func() {
	// Guard on empty endpoint to prevent default export to localhost:4318.
	// Keep in sync with InitTracing() in internal/telemetry/tracing.go.
	if endpoint == "" || disabled {
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
		return func() {}
	}

	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		slog.Warn("failed to create OTLP trace exporter, falling back to no-op", "error", err)
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
		return func() {}
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			attribute.String("service.name", "cleat-worker"),
			attribute.String("worker.id", workerID),
		),
	)
	if err != nil {
		slog.Warn("failed to create OTel resource, using empty", "error", err)
		res = resource.Empty()
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	return func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			slog.Error("error shutting down tracer provider", "error", err)
		}
	}
}
