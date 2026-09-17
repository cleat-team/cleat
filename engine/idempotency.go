package engine

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"strconv"
)

// IdempotentCaller is an optional interface a ServiceCaller may implement to
// receive a per-call idempotency key.
//
// IMPROVEMENT-PLAN §1.4 phase B, and docs/durable-call-intent-design.md §4. The
// design there prescribes adding the key as a parameter to ServiceCaller.Call
// itself, and names the cost honestly: "a breaking change for external callers
// and plugin authors. That is the main expense of this tier."
//
// This is the same mechanism without that expense. A caller that can deduplicate
// implements this interface and gets the key; one that cannot is untouched, and
// no existing implementation stops compiling. The engine picks the richer method
// when it is available. The trade is that a caller which *could* honour keys but
// has not been updated goes on silently not honouring them — visible in a type
// switch rather than in a compile error. That is a worse failure mode than a
// breaking change in a codebase where nobody would notice; it is a better one
// here, because CallerHonoursIdempotencyKeys makes the distinction testable and
// tests/crash asserts on the observable outcome rather than on the wiring.
//
// If the interface is ever collapsed into ServiceCaller, delete this and take
// the breaking change. Nothing here forecloses that.
type IdempotentCaller interface {
	ServiceCaller

	// CallWithIdempotencyKey makes the call, passing a key that is stable
	// across every replay of the same logical step. Implementations should
	// forward it to the service — an `Idempotency-Key` header for HTTP, an
	// explicit argument for anything else — so that a call repeated after a
	// crash returns the original outcome instead of performing the work twice.
	CallWithIdempotencyKey(ctx context.Context, service, operation, requestJSON, idempotencyKey string) (responseJSON string, err error)
}

// idempotencyKeyEncoding is unpadded base32. Chosen over hex for length and over
// base64 because the key travels in an HTTP header value, where base64's '+' and
// '/' would need escaping and its '=' padding invites truncation by
// intermediaries.
var idempotencyKeyEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// DurableCallIdempotencyKey derives the key for one durable call.
//
//	key = base32(sha256(workflowID || 0x00 || runID || 0x00 || step))
//
// Every input is deterministic on replay, so the key for a given logical step is
// identical on the original run and on every replay of it. That is the whole
// mechanism: after a crash, replay re-issues the call with the same key and a
// service that honours keys returns the original outcome rather than doing the
// work again.
//
// runID is included so that ContinueAsNew — genuinely new work — gets fresh keys
// instead of colliding with the run it continues from.
//
// The 0x00 separators matter. Without them the concatenation is ambiguous:
// workflow "ab" run "c" step 1 and workflow "a" run "bc" step 1 would hash
// identically, and two unrelated calls would silently deduplicate against each
// other. A NUL cannot appear in any of these identifiers, so the encoding is
// injective.
func DurableCallIdempotencyKey(workflowID, runID string, step int) string {
	h := sha256.New()
	h.Write([]byte(workflowID))
	h.Write([]byte{0})
	h.Write([]byte(runID))
	h.Write([]byte{0})
	h.Write([]byte(strconv.Itoa(step)))
	return idempotencyKeyEncoding.EncodeToString(h.Sum(nil))
}

// callService invokes the caller for one durable call, passing an idempotency
// key when the caller can use one.
//
// Every durable-call path goes through here so that the key is derived in
// exactly one place. Deriving it at each call site is how the step number and
// the recorded event drift apart.
func (s *execSession) callService(ctx context.Context, service, operation, requestJSON string, step int) (string, error) {
	// Scope the tenant, so a caller can answer "whose workflow is this".
	// cleat#1565: http.fetch's egress allowlist is per tenant, and without
	// this the service path carried no tenant at all -- the same
	// one-path-only shape cleat#1278 records for plugins.
	ctx = s.tenantScopedContext(ctx)
	if ic, ok := s.engine.caller.(IdempotentCaller); ok {
		key := DurableCallIdempotencyKey(s.workflowID, s.execRunID, step)
		return ic.CallWithIdempotencyKey(ctx, service, operation, requestJSON, key)
	}
	return s.engine.caller.Call(ctx, service, operation, requestJSON)
}

// CallerHonoursIdempotencyKeys reports whether this engine's caller CAN receive
// a per-call idempotency key — that is, whether it implements IdempotentCaller.
//
// # What this does NOT establish, stated because it was read as establishing it
//
// It is a type assertion. It cannot see whether any particular path forwards the
// key it receives, and for most of this project's life exactly one did not:
// dbServiceCaller implemented IdempotentCaller, took the key, forwarded it to
// forwardToBenchSvc, and DROPPED it for http.fetch — the only route a workflow
// has to an external service without a Go plugin and a worker rebuild. This
// predicate returned true throughout (cleat#1837).
//
// So the comment above IdempotentCaller is half right. The distinction it calls
// "testable" is testable at the level of the TYPE, which is not the level the
// failure lives at. A caller that could honour keys and does not still returns
// true here.
//
// # What establishes the property this is usually quoted for
//
// Whether the key reaches the wire is a question about a request, and only a
// test that reads a header off a delivered request can answer it.
// cmd/cleat-worker/the_idempotency_key_reaches_the_request_test.go does that
// against an httptest listener, for the engine's key, for its stability across
// replays, and for a guest-set key winning. Cite those, not this, for
// "this deployment sends idempotency keys".
//
// Making this predicate answer the real question needs IdempotentCaller to
// expose which service/operation pairs it forwards for, which is a change to a
// published interface and a decision this comment deliberately does not take.
func (e *Engine) CallerHonoursIdempotencyKeys() bool {
	_, ok := e.caller.(IdempotentCaller)
	return ok
}
