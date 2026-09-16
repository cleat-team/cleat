package telemetry_test

import (
	"context"
	"testing"

	"github.com/cleat-team/cleat/internal/telemetry"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// cleat#1669. WorkflowSpan attached the caller with trace.WithLinks, which does
// not make Start adopt the caller's trace -- the span opened as a new root in a
// NEW trace-id. Meanwhile plugin.SetTraceparentFromContext sends the INBOUND
// trace-id downstream, so a collector held the caller and the callee joined in
// one trace and cleat in another.
//
// These assert what a collector RECEIVES, from an in-memory exporter, rather
// than what the code appears to do. The distinction earned its place: the
// defect was invisible from the inbound parse the originating issue quoted,
// because WithLinks is three frames away from it.

const (
	callerTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	callerSpan  = "00f067aa0ba902b7"
)

func exporterFor(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return exp
}

func TestAWorkflowSpanJoinsTheCallersTrace(t *testing.T) {
	exp := exporterFor(t)

	ctx, wf := telemetry.WorkflowSpan(context.Background(), "wf-1", "process_order", 1, "tenant-a", callerTrace)
	_, ev := telemetry.EventSpan(ctx, 0, "call", "payments", "Ship")
	ev.End()
	wf.End()

	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("exported %d spans, want 2 -- the rest of this test reads them by name", len(spans))
	}
	byName := map[string]tracetest.SpanStub{}
	for _, s := range spans {
		byName[s.Name] = s
	}

	root, ok := byName["workflow.execute"]
	if !ok {
		t.Fatal("no workflow.execute span was exported")
	}
	if got := root.SpanContext.TraceID().String(); got != callerTrace {
		t.Errorf("cleat's span is in trace %s, the caller is in %s.\n\n"+
			"They must be the same trace or the orchestrator is missing from the middle "+
			"of its own transaction: SetTraceparentFromContext sends the caller's "+
			"trace-id downstream, so the callee's spans land in the caller's trace and "+
			"cleat's do not. `WHERE trace_id = <caller>` in a collector then returns "+
			"both ends and not the middle.", got, callerTrace)
	}

	// The event span must be under it, in the same trace -- the per-worker tree
	// was always correct and this change must not disturb it.
	child, ok := byName["event.call"]
	if !ok {
		t.Fatal("no event.call span was exported")
	}
	if child.Parent.SpanID() != root.SpanContext.SpanID() {
		t.Errorf("event.call's parent is %s, want workflow.execute's %s",
			child.Parent.SpanID(), root.SpanContext.SpanID())
	}
	if got := child.SpanContext.TraceID().String(); got != callerTrace {
		t.Errorf("event.call is in trace %s, want %s", got, callerTrace)
	}

	// The parent context is remote -- it came from the inbound traceparent and
	// not from an ambient local span -- and it names NO span.
	if !root.Parent.IsRemote() {
		t.Errorf("workflow.execute's parent is not marked remote; it must come from the "+
			"inbound traceparent rather than from an ambient local span. parent=%v",
			root.Parent)
	}
	// THE STRONG FORM, and the weak one is why this is spelled out. This used
	// to assert only `!= callerSpan`, which a REINVENTED random span-id also
	// satisfies -- so it could not have caught a regression back to the phantom
	// cleat#1669 removed. Asserting the span-id is invalid catches both: a
	// random one and the caller's real one.
	if root.Parent.SpanID().IsValid() {
		got := root.Parent.SpanID().String()
		if got == callerSpan {
			t.Errorf("the parent span-id is the caller's real one (%s). That means the "+
				"inbound parse now keeps parts[2] -- cleat#1597 -- and this test should "+
				"be flipped to assert the edge is real, with spanContextFromTraceID's "+
				"doc updated to match", got)
		} else {
			t.Errorf("workflow.execute names a parent span %s that nothing will ever send. "+
				"cleat does not know its caller's span, so it must name none: a collector "+
				"told about a parent it cannot receive is being told something false, and "+
				"since cleat#1689 put these spans in the CALLER's trace that falsehood now "+
				"sits where it looks plausible. See spanContextFromTraceID", got)
		}
	}

	// No link: it would point at the same trace by a second, different
	// fabricated span-id. One fabricated parent is the honest minimum.
	if len(root.Links) != 0 {
		t.Errorf("workflow.execute carries %d link(s); the caller is the parent now, and a "+
			"link would be a self-referential edge to a span that also does not exist: %v",
			len(root.Links), root.Links)
	}
}

// Sampling must not change. TraceFlags(1) is inert on a link and decisive on a
// parent -- the default sampler is ParentBased(AlwaysSample) -- so the same
// constant changed job when the link became a parent. It preserves today's
// outcome, and this is the assertion that says so rather than the comment.
func TestJoiningTheTraceLeavesTheSpanSampled(t *testing.T) {
	exp := exporterFor(t)
	_, wf := telemetry.WorkflowSpan(context.Background(), "wf-2", "d", 1, "t", callerTrace)
	wf.End()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1", len(spans))
	}
	if !spans[0].SpanContext.IsSampled() {
		t.Error("workflow.execute is no longer sampled. The remote parent's TraceFlags now " +
			"decide it, and an unsampled parent makes the whole subtree unrecorded -- a " +
			"deployment would simply stop seeing cleat in its traces, with nothing failing")
	}
	// Exported at all is the stronger half: an unsampled span never reaches the
	// exporter, so the count above already proves recording. Both are kept
	// because they fail with different messages.
}

// A run with no inbound trace must still get a trace of its own. Scheduled and
// swept work has no caller to join, and the branch that adopts the parent is
// skipped for it -- it must open a root, not become an orphan.
func TestAWorkflowWithNoInboundTraceStillGetsOne(t *testing.T) {
	exp := exporterFor(t)
	_, wf := telemetry.WorkflowSpan(context.Background(), "wf-3", "d", 1, "t", "")
	wf.End()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1", len(spans))
	}
	if !spans[0].SpanContext.TraceID().IsValid() {
		t.Error("a run with no inbound trace produced no valid trace-id")
	}
	if spans[0].Parent.IsValid() {
		t.Errorf("it adopted a parent from nowhere: %v", spans[0].Parent)
	}
}

// A malformed inbound trace-id must not invent one. spanContextFromTraceID
// rejects anything that is not 32 hex chars, and the caller then starts a root
// -- emitting a syntactically valid header for a trace nobody is in is worse
// than emitting none, which is the reasoning SetTraceparent already records.
func TestAMalformedInboundTraceIsNotAdopted(t *testing.T) {
	for _, bad := range []string{"not-hex", "abc", "4bf92f3577b34da6a3ce929d0e0e47"} {
		t.Run(bad, func(t *testing.T) {
			exp := exporterFor(t)
			_, wf := telemetry.WorkflowSpan(context.Background(), "wf-4", "d", 1, "t", bad)
			wf.End()

			spans := exp.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("exported %d spans, want 1", len(spans))
			}
			if spans[0].Parent.IsValid() {
				t.Errorf("a malformed trace-id %q was adopted as a parent: %v", bad, spans[0].Parent)
			}
			if got := spans[0].SpanContext.TraceID().String(); got == bad {
				t.Errorf("the malformed value became the trace-id: %s", got)
			}
		})
	}
}
