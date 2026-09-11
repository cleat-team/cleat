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

// ErrIdempotencyKeyInputMismatch is returned when an idempotency key was already
// used to start the SAME workflow definition with a DIFFERENT input.
//
// cleat#1170. The key matched, the definition matched, and the second request's
// arguments were silently dropped: the caller got 200 with already_started and
// a workflow id, and the run behind that id carries the FIRST request's input.
// Nothing in the response says so, and nothing in the store records that a
// second request was ever made -- which makes this quieter than the duplicate
// execution idempotency keys exist to prevent.
//
// The remedy is the one cleat#1047 already chose for the definition, and the
// argument carries over unchanged: this is one caller with one key namespace
// they control, so a collision means their key derivation does not capture
// something their requests distinguish. Refusing says so once, when the
// ambiguity first appears. Replaying silently would hide it and would shift
// behaviour under them later.
var ErrIdempotencyKeyInputMismatch = errors.New(
	"idempotency key already used to start the same workflow with a different input")
