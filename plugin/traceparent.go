package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// TraceparentHeader is the W3C Trace Context header name. Lowercase, which is
// what the spec uses; Go canonicalises it on the way out either way.
const TraceparentHeader = "traceparent"

// SetTraceparent sets a W3C `traceparent` on an outbound request so the callee
// joins this run's trace instead of starting its own. cleat#1596.
//
// WHAT WAS BROKEN. cleat parses an inbound traceparent and never sends one.
// Two services in one transaction each get a perfectly good trace id and they
// are DIFFERENT ones, so the chain breaks at the first hop and nothing looks
// wrong -- cleat appears in a collector as a leaf that swallowed everything
// downstream.
//
// A SPAN-ID IS SYNTHESISED, AND THAT IS NOT cleat#1597 ARRIVING EARLY. A
// traceparent is `00-<trace-id>-<span-id>-<flags>` and there is no legal form
// that omits the span-id, so propagating AT ALL requires producing one. This
// one is random and is not recorded anywhere, which yields exactly the flat bag
// cleat#1597 describes: you learn that two hundred events share a transaction,
// not that B was called by A. That issue says to ship the flat version first
// and let it decide whether the tree is worth its price, so this is the
// intended intermediate state rather than a shortcut.
//
// RANDOMNESS IS SAFE HERE DESPITE REPLAY, and it is worth saying why in a
// durable engine. The span-id is only ever written to an outgoing header; it is
// not part of any recorded value. Outbound calls are durable steps, so a replay
// takes the recorded result and does not re-issue the request -- this function
// is not reached a second time. Nothing compares a replayed span-id to an
// original one because no span-id is stored.
//
// AN EXISTING traceparent IS NOT OVERWRITTEN. A guest that sets the header
// itself has said something deliberate about its own trace, and silently
// replacing it would be a second, quieter version of the bug being fixed here.
//
// A MALFORMED OR ABSENT trace-id IS A NO-OP rather than an invented trace.
// Emitting a syntactically valid header carrying a trace nobody is in is worse
// than emitting nothing: a collector believes it and the operator sees a tree
// that was never real.
func SetTraceparent(req *http.Request, traceID string) {
	if req == nil || !validTraceID(traceID) {
		return
	}
	if req.Header.Get(TraceparentHeader) != "" {
		return
	}
	span := make([]byte, 8)
	if _, err := rand.Read(span); err != nil {
		// Without a span-id there is no legal header to send. Silently sending
		// nothing is correct: a broken chain is the status quo this improves on
		// opportunistically, and failing an outbound call because a trace could
		// not be decorated would be a worse trade.
		return
	}
	// Flags "01" = sampled. STATED DECISION, and the reviewable one here:
	// cleat discards the inbound flags (it keeps only the trace-id -- see
	// cleat#1597), so it cannot faithfully forward the caller's sampling
	// choice. "01" is chosen because cleat has durably recorded this run, so a
	// collector asking "is this trace worth keeping" has a real answer. The
	// cost is that an unsampled caller can have its decision escalated
	// downstream; forwarding the flags is a change that belongs with #1597,
	// which is where the inbound parse is being widened.
	req.Header.Set(TraceparentHeader, "00-"+traceID+"-"+hex.EncodeToString(span)+"-01")
}

// validTraceID reports whether s is a W3C trace-id: 32 lowercase hex digits,
// not all zero.
//
// The all-zero case is REJECTED BY THE SPEC and is not a hypothetical -- it is
// what a caller sends when its own tracing is misconfigured, and forwarding it
// would propagate that fault to everything downstream.
func validTraceID(s string) bool {
	if len(s) != 32 {
		return false
	}
	allZero := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
		if c != '0' {
			allZero = false
		}
	}
	return !allZero
}

// SetTraceparentFromContext sets a traceparent on req using the trace-id the
// engine put on this call's CallContext. cleat#1596.
//
// THE FORM EVERY PLUGIN SHOULD USE. The two-argument SetTraceparent exists for
// callers that already hold a trace-id -- the worker's own fetch path does --
// but a plugin host function receives its trace through the context and
// nothing else. Making each of them unwrap CallContext by hand would be a dozen
// copies of the same nil-check, and the site that got it wrong would be silent
// rather than broken: a missing header looks exactly like a run with no trace.
//
// A nil CallContext is an ordinary state, not an error. Background sweeps and
// scheduled work run with no inbound request and therefore no trace to join;
// they need a trace ORIGINATED, which is a different mechanism and is not this.
func SetTraceparentFromContext(ctx context.Context, req *http.Request) {
	SetTraceparent(req, traceIDFor(ctx))
}

// originatedTraceKey carries a trace-id manufactured by a caller that had none
// to inherit. Separate from CallContext deliberately -- see WithNewTrace.
type originatedTraceKey struct{}

// WithNewTrace returns a context carrying a freshly originated W3C trace-id, for
// work that has no caller trace to join. cleat#1611.
//
// USE IT PER UNIT OF WORK, NEVER PER TICK. A background sweep that originates on
// every iteration floods a collector with empty single-span traces at the sweep
// interval, on every worker, forever -- burying the traces that mean something,
// at a cost that scales with worker count. Originate when there is something to
// do: a delivery to send, a batch actually fetched. An empty tick should produce
// no trace at all.
//
// AND THE UNIT IS THE ITEM, NOT THE BATCH, because the item is what a person
// asks about. "Why did this notification fail" is a question for a trace; "how
// long did the sweep take" is a question for a metric, which
// monitoring/prometheus already answers. A batch of N deliveries becomes N
// traces sharing a time window, which is legible; one trace with N children
// needs parentage that does not exist yet (cleat#1597).
//
// NOT STORED ON CallContext, and that is the one subtle choice here. CallContext
// means "the engine invoked a plugin function for a workflow", and a background
// sweep is none of those things. Fabricating one with only TraceID set would
// make CallContextFromContext return non-nil where it returns nil today, so any
// caller testing for presence would silently change behaviour. A separate key
// says what is true: a trace exists, a workflow call does not.
func WithNewTrace(ctx context.Context) context.Context {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		// No id means no trace. Returning ctx unchanged degrades to today's
		// behaviour -- no header -- rather than failing the work itself, which
		// would trade a missing trace for a missed delivery.
		return ctx
	}
	return context.WithValue(ctx, originatedTraceKey{}, hex.EncodeToString(id))
}

// traceIDFor returns the trace this context is in: the engine's CallContext
// trace first, then an originated one.
//
// ORDER MATTERS AND THIS IS THE SAFE DIRECTION. A real caller trace always wins
// over a manufactured one, so a path that acquires both -- a plugin host
// function that also calls WithNewTrace by mistake -- still propagates the
// customer's trace rather than a fabricated root that silently detaches the
// chain.
func traceIDFor(ctx context.Context) string {
	if cc := CallContextFromContext(ctx); cc != nil && cc.TraceID != "" {
		return cc.TraceID
	}
	id, _ := ctx.Value(originatedTraceKey{}).(string)
	return id
}
