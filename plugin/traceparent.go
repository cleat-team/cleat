package plugin

import (
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
