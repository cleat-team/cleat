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
	if ic, ok := s.engine.caller.(IdempotentCaller); ok {
		key := DurableCallIdempotencyKey(s.workflowID, s.execRunID, step)
		return ic.CallWithIdempotencyKey(ctx, service, operation, requestJSON, key)
	}
	return s.engine.caller.Call(ctx, service, operation, requestJSON)
}

// CallerHonoursIdempotencyKeys reports whether this engine's caller receives
// idempotency keys.
//
// Exported for operators and tests: whether a deployment has exactly-once
// behaviour against a key-honouring service is otherwise invisible until a crash
// duplicates something in production.
func (e *Engine) CallerHonoursIdempotencyKeys() bool {
	_, ok := e.caller.(IdempotentCaller)
	return ok
}
