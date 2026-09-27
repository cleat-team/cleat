package telemetry

import (
	"context"
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

// WorkflowSpan creates the span for a workflow execution, joined to the
// caller's trace when there is one.
//
// IT USED TO LINK RATHER THAN PARENT, and the difference is the whole of
// cleat#1669. trace.WithLinks does not make Start adopt the caller's trace: the
// span opens as a NEW ROOT IN A NEW TRACE-ID, with the caller reachable only by
// following a link. Meanwhile plugin.SetTraceparentFromContext sends the
// INBOUND trace-id downstream. So a collector held the caller's spans and the
// callee's spans correctly joined in one trace, and cleat -- the thing in the
// middle that orchestrated both -- in another. Measured with an in-memory
// exporter before the change:
//
//	INBOUND  traceparent   00-4bf92f35...4736-00f067aa0ba902b7-01
//	OUTBOUND traceparent   00-4bf92f35...4736-8f8109e582e314e2-01
//	cleat's own spans       trace bd8e8ad6...d1c1   <- a different trace
//
// ContextWithRemoteSpanContext makes Start adopt it, so the whole transaction
// is one trace. The per-worker tree underneath was always correct --
// workflow.execute over the event.* spans -- and is unchanged; this only
// attaches it to the right root.
//
// THE PARENT SPAN-ID IS STILL FABRICATED, and that is cleat#1597, not this.
// The inbound parse keeps the trace-id and discards parts[2], so there is no
// real caller span-id to name. A collector renders a parent it never receives
// as a second root WITHIN the trace, which is a far smaller loss than a missing
// trace: "show me everything in this transaction" now answers, and "what called
// what" across that one edge still does not.
//
// THE LINK IS DROPPED rather than kept alongside the parent. It would point at
// the same trace by a second span-id that does not exist either, so it would be
// a self-referential edge to nothing.
//
// This comment used to end "one fabricated parent is the honest minimum; two is
// noise". That was wrong: the honest minimum is NONE, and
// spanContextFromTraceID no longer mints one -- see its doc for why a zero span
// id still joins the trace.
//
// SAMPLING IS UNCHANGED, and this is the part worth checking rather than
// assuming, because the same constant changes job. TraceFlags(1) is inert on a
// link and decisive on a parent: the default sampler is
// ParentBased(AlwaysSample), so a sampled remote parent samples the child and
// an unsampled one does not. Hardcoding sampled therefore preserves exactly
// today's outcome -- a root span under ParentBased already falls through to
// AlwaysSample. Forwarding the CALLER's real flags would be a behaviour change
// and is not available anyway: they are discarded at the inbound parse, which
// is the same reason SetTraceparent's comment gives for hardcoding "01"
// outbound. That belongs with cleat#1597, which widens that parse.
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
			ctx = trace.ContextWithRemoteSpanContext(ctx, sc)
		}
	}
	// A run with NO inbound trace still gets one: traceID is empty only when
	// nothing upstream supplied it, and cmd/cleat-worker substitutes a fresh
	// generateTraceID() before reaching here. If it ever is empty, Start opens
	// a root in its own trace exactly as before -- a scheduled or swept run
	// must not become an orphan because this branch was skipped.
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

// spanContextFromTraceID creates a W3C SpanContext from a 32-char hex trace ID,
// carrying NO span ID. cleat#1669, corrected.
//
// IT USED TO INVENT ONE, and #1689 made that worse rather than better. Before
// #1689 cleat's spans were in a trace of their own, so a fabricated parent was
// invisible: nothing else was in that tree to be wrongly parented. Once #1689
// joined the caller's trace, every cleat span hung off a span-id that does not
// exist and never will arrive -- a collector shown the caller, cleat and the
// callee in one trace, with cleat rooted at a phantom. That is a falsehood
// sitting inside a trace that otherwise looks right, which is harder to notice
// than the obviously-disconnected state it replaced.
//
// A ZERO SPAN ID STILL JOINS THE TRACE, which is the fact that makes the
// fabrication unnecessary, and it is documented SDK behaviour rather than a
// quirk. otel/sdk/trace/tracer.go's newSpan branches on the TRACE id alone:
//
//	// If there is a valid parent trace ID, use it to ensure the continuity of
//	// the trace. Always generate a new span ID ...
//	if !psc.TraceID().IsValid() { tid, sid = ...NewIDs(ctx) } else { tid = psc.TraceID() ... }
//
// so a SpanContext whose own IsValid() is false -- which is what a zero span id
// makes it -- still contributes its trace id. Measured: workflow.execute lands
// in the caller's trace with parentValid=false and parentSpan all zeroes, and
// the event.* spans stay parented under it.
//
// WHAT A COLLECTOR IS TOLD, which is the whole point of the change:
//
//	before #1689   cleat's own trace, no parent   two unrelated traces
//	with a random  the caller's trace, INVENTED   a parent that never arrives
//	now            the caller's trace, none       cleat is a root WITHIN it
//
// The last row is true. cleat genuinely does not know its caller's span -- the
// inbound parse keeps the trace-id and discards parts[2] -- and saying so is
// better than naming a span nobody has. Recording the real edge is cleat#1597
// and needs the span-id persisted across three dialects; this does not.
//
// TRACE FLAGS STAY SAMPLED. With no parent the sampler treats the span as a
// root, and the default ParentBased falls through to AlwaysSample, so the
// outcome is unchanged either way -- measured, both rows sampled.
func spanContextFromTraceID(traceID string) (trace.SpanContext, error) {
	b, err := hex.DecodeString(traceID)
	if err != nil || len(b) != 16 {
		return trace.SpanContext{}, fmt.Errorf("invalid trace ID %q: must be 32 hex chars", traceID)
	}
	var tid trace.TraceID
	copy(tid[:], b)

	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		TraceFlags: trace.TraceFlags(1), // sampled
	}), nil
}
