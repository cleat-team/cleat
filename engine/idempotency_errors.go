package engine

import "errors"

// ErrIdempotencyKeyDefMismatch is returned when an idempotency key was already
// used to start a DIFFERENT workflow definition.
//
// The key is keyed by (key_hash, tenant_id) and carries no definition, so
// before cleat#1047 a second request naming another definition matched the
// first row and was handed that workflow's ID with alreadyExisted = true --
// while its own workflow was never started. A caller awaiting the returned id
// reads the other workflow's result as its own.
//
// REFUSING RATHER THAN SCOPING, and the distinction is who collides. Migration
// 010 records the tenant version of this as "the expected outcome of ordinary
// naming rather than an attack" -- two customers who have never heard of each
// other, cannot coordinate, and both reasonably chose "order-123". Silent
// scoping is the only correct answer there.
//
// Here it is ONE caller with one key namespace they control, using the same key
// for two operations on the same entity. They can coordinate with themselves,
// and a collision means their key derivation does not capture something their
// workflows distinguish. Scoping silently would hide that -- and shift
// behaviour under them later if they add a third workflow on that entity or
// move an operation between definitions. Refusing says so once, when the
// ambiguity first appears.
var ErrIdempotencyKeyDefMismatch = errors.New(
	"idempotency key already used to start a different workflow definition")
