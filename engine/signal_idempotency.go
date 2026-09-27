package engine

import (
	"context"
	"crypto/sha256"
)

// SignalIdempotencyStore absorbs a duplicate signal identified by a
// client-supplied token.
//
// A separate interface asserted at run time, rather than a fifth parameter on
// WorkflowStore.DeliverSignal, because that signature has fifty call sites in
// tests and the churn would bury the change. ChildWorkflowStore is the existing
// precedent for this shape.
//
// The cost of an optional interface is that a store which forgets to implement
// it silently loses idempotency, so
// TestEveryRealStoreCanAbsorbADuplicateSignal asserts all three do.
//
// cleat#1121.
type SignalIdempotencyStore interface {
	// DeliverSignalIdempotent delivers a signal unless idempotencyKey has
	// already delivered one, reporting which happened.
	//
	// The key and the delivery are written in ONE transaction. A key recorded
	// outside it would let a crash between the two leave a token claiming a
	// delivery that never happened, which is worse than no key at all: the
	// retry it exists to permit would then be refused.
	//
	// An empty key is the ABSENCE of a token, not a token equal to "". It
	// delivers unconditionally and records nothing, so two callers who both
	// send no header cannot collide.
	DeliverSignalIdempotent(ctx context.Context, workflowID, signalName, payload, idempotencyKey string) (alreadyDelivered bool, err error)
}

// signalIdempotencyHash is the idempotency_keys.key_hash for a signal.
//
// THE OPERATION IS FOLDED INTO THE HASH, and the asymmetry below is deliberate.
//
// idempotency_keys is shared with the start path, and its identity is
// (key_hash, tenant_id). One client request id legitimately produces a start
// AND a signal -- Cadence's dedup table registers exactly that pair under one
// request id, deliberately, because one logical request drives several
// persisted operations. So the two must COEXIST rather than collide, which
// means the operation belongs in the identity.
//
// It is folded into the hash rather than added as a column because a column
// means ALTER TABLE plus a backfill plus a primary-key change on three
// dialects, and because folding makes a collision impossible rather than
// merely refused.
//
// Start keeps sha256(key) with no prefix, and cannot be changed to match: the
// table stores the hash and not the key, so existing rows cannot be rehashed
// and every in-flight key would break on deploy. The encoding is therefore
// asymmetric by necessity, and a third operation must prefix like this one.
//
// Contrast def_name (#1047) and input_digest (#1170), which are columns
// compared on a hit and REFUSE when they differ. That is right for both: a key
// naming a different definition, or carrying a different payload, is a client
// mistake worth reporting. An operation type is not a mistake.
func signalIdempotencyHash(idempotencyKey string) []byte {
	sum := sha256.Sum256([]byte("signal\x00" + idempotencyKey))
	return sum[:]
}
