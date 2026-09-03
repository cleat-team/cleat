package engine

import (
	"math/rand"
	"testing"
)

// Property tests over the host-call result words.
//
// CLAUDE.md asks for this shape explicitly: four real defects have come out of
// the ABI layer's integer-conversion sites (#318, #327, #341, #342) and none of
// them was an overflow -- in every case the value meant the wrong thing on one
// side of the boundary. Reading the remaining ~200 conversion sites finds those
// one at a time. A property over the boundary finds them by construction.
//
// The boundary has two halves. The host packs an i64; the guest decodes it. The
// decoders below are transcribed from the guest SDK, not from the host, because
// a test that re-derives the layout from the packer it is testing asserts only
// that the packer is self-consistent. The AS SDK is used as the reference
// because its layouts are written down as documented bit tables:
// packages/cleat-as/assembly/memory.ts, decodeSleepResult and friends.
//
// Three properties, and the second is the one that matters:
//
//  1. ROUND TRIP -- pack then decode returns what went in.
//  2. FIELD INDEPENDENCE -- changing one field perturbs no other. This is the
//     property that a 16-bit field receiving a 17-bit value violates, which is
//     exactly the documented defect in packAwaitSignalsResult, and the shape of
//     #341/#342 where a length meant something different on each side.
//  3. SENTINEL DISJOINTNESS -- no valid result can be mistaken for the
//     suspension sentinel 1<<62 that ABI.md reserves.
//
// Where a property does not hold, the test pins the ACTUAL boundary with a
// number rather than being weakened to pass. A pinned limit is a fact the next
// reader can check; a loosened assertion is one they cannot.

// ---- guest-side decoders, transcribed from packages/cleat-as/assembly/memory.ts ----

// decodeSleepResultGuest mirrors decodeSleepResult: bits 56-63 status,
// bits 0-55 durationMs.
func decodeSleepResultGuest(r int64) (status uint8, durationMs int64) {
	u := uint64(r)
	return uint8((u >> 56) & 0xFF), int64(u & 0x00FFFFFFFFFFFFFF)
}

// decodeAwaitSignalsResultGuest mirrors decodeAwaitSignalsResult: 48-63
// sigNameLen, 32-47 payloadLen, 16-31 timedOut, 0-15 errCode. Note the guest
// masks each field to 16 bits on the way out.
func decodeAwaitSignalsResultGuest(r int64) (sigNameLen, payloadLen uint32, timedOut bool, errCode uint32) {
	u := uint64(r)
	return uint32((u >> 48) & 0xFFFF),
		uint32((u >> 32) & 0xFFFF),
		((u >> 16) & 0xFFFF) != 0,
		uint32(u & 0xFFFF)
}

// decodeSimpleResultGuest mirrors decodeSimpleResult, used by defer,
// continue_as_new, child_workflow and await_child: bits 32-63 extra,
// bits 0-7 errCode. The guest reads EIGHT bits of error code.
func decodeSimpleResultGuest(r int64) (extra uint32, errCode uint8) {
	u := uint64(r)
	return uint32(u >> 32), uint8(u & 0xFF)
}

// decodeCallResultGuest mirrors decodeCallResult: bits 40-63 responseLen (24
// bits), bits 8-39 callErrorCode (32), bits 0-7 errCode (8).
func decodeCallResultGuest(r int64) (responseLen, callErrorCode uint32, errCode uint8) {
	u := uint64(r)
	return uint32((u >> 40) & 0xFFFFFF), uint32((u >> 8) & 0xFFFFFFFF), uint8(u & 0xFF)
}

// suspendSentinel is the value ABI.md reserves; the host must recognise it
// before decoding a result as a normal one.
const suspendSentinel int64 = 1 << 62

// prand is seeded from a constant so a failure reproduces exactly. A property
// test that cannot be re-run on the same inputs reports a defect nobody can
// then investigate.
func prand() *rand.Rand { return rand.New(rand.NewSource(0x0AB1)) }

// ---- 1. round trip ----

func TestPackSleepResult_RoundTrips(t *testing.T) {
	r := prand()
	// durationMs occupies 56 bits and is documented as a duration, so negative
	// values are out of contract; the domain is [0, 2^56).
	for i := 0; i < 2000; i++ {
		var status uint8
		if i%3 == 0 {
			status = 1
		}
		dur := r.Int63n(1 << 56)
		gotStatus, gotDur := decodeSleepResultGuest(packSleepResult(status, dur))
		if gotStatus != status || gotDur != dur {
			t.Fatalf("packSleepResult(%d, %d) decoded as (%d, %d)", status, dur, gotStatus, gotDur)
		}
	}
}

func TestPackDurableCallResult_RoundTrips(t *testing.T) {
	r := prand()
	// The host signature is (responseLen int, callErrorCode, errCode byte), so
	// the reachable domain is narrower than the guest's layout allows -- see
	// TestPackDurableCallResult_HostCannotFillCallErrorCode below.
	for i := 0; i < 2000; i++ {
		respLen := int(r.Int31n(1 << 24)) // 24-bit field
		callErr := byte(r.Int31n(1 << 8))
		errCode := byte(r.Int31n(1 << 8))
		gotLen, gotCallErr, gotErr := decodeCallResultGuest(
			packDurableCallResult(respLen, callErr, errCode))
		if gotLen != uint32(respLen) || gotCallErr != uint32(callErr) || gotErr != errCode {
			t.Fatalf("durable call (%d,%d,%d) decoded as (%d,%d,%d)",
				respLen, callErr, errCode, gotLen, gotCallErr, gotErr)
		}
	}
}

// TestPackDurableCallResult_HostCannotFillCallErrorCode records the asymmetry
// the signature creates. The guest's decodeCallResult reads a 32-bit
// callErrorCode from bits 8-39; the host parameter is a byte, so 24 of those
// bits are unreachable and always zero. Nothing is broken -- it means the
// error-code space is 8 bits wide in practice, not 32, and a future code above
// 255 cannot be delivered without widening the parameter.
func TestPackDurableCallResult_HostCannotFillCallErrorCode(t *testing.T) {
	_, callErr, _ := decodeCallResultGuest(packDurableCallResult(0, 0xFF, 0))
	if callErr != 0xFF {
		t.Fatalf("max byte callErrorCode decoded as %d, want 255", callErr)
	}
	t.Log("pinned: guest reads 32 bits of callErrorCode (bits 8-39); the host " +
		"parameter is a byte, so values above 255 are unrepresentable")
}

// TestPackDurableCallResult_ResponseLenTruncatesAboveItsField pins the top
// field's ceiling. responseLen is shifted to bits 40-63, so 2^24 and above is
// shifted off the end of the word and lost -- silently, since there is no field
// above it to corrupt. ABI.md caps max_out_len at 1048576, well inside 2^24, so
// this is a ceiling rather than a live defect.
func TestPackDurableCallResult_ResponseLenTruncatesAboveItsField(t *testing.T) {
	const field = 1 << 24
	if got, _, _ := decodeCallResultGuest(packDurableCallResult(field-1, 0, 0)); got != field-1 {
		t.Fatalf("responseLen=%d decoded as %d", field-1, got)
	}
	got, _, _ := decodeCallResultGuest(packDurableCallResult(field, 0, 0))
	if got != 0 {
		t.Fatalf("responseLen=%d decoded as %d; expected it to shift off the word", field, got)
	}
	t.Logf("pinned: responseLen holds %d bytes (2^24-1); ABI.md caps max_out_len "+
		"at %d, which keeps this unreachable", field-1, 1<<20)
}

// TestPackDurableCallResult_SentinelBitsTheHostCannotReach measures which bits
// of this layout are available for an out-of-band sentinel, because
// IMPROVEMENT-PLAN 3.81 proposed the wrong one.
//
// Phase 5 needs a way for the host to tell a guest "stop, do not do new work"
// on a call that has run past the end of recorded history. The obvious answer,
// and the one 3.81 wrote down, is bit 62 -- the bit cleat_await_child and
// cleat_await_any_child already decode as a suspend sentinel, so it looks like
// reusing an established convention.
//
// It is not available here. Those two pack a 32-bit length at bits 32-63, where
// bit 62 means a length of 1 GiB. This layout packs a 24-bit responseLen at
// bits 40-63, where bit 62 means 4 MiB -- and responseLen is bounded by
// MaxWasmStringLen and OutBufSize, which are package vars set from
// cmd/cleat-worker's -wasm-max-string-len and -wasm-output-buffer-size. Both
// are flag.Int with no upper bound, so 4 MiB is operator-reachable and a
// legitimate response would decode as a suspend.
//
// What IS available is bits 16-39, and structurally rather than by a bound:
// the guest decodes a 32-bit callErrorCode from bits 8-39, but the host
// parameter is a byte, so 24 of those bits cannot be filled by any input. That
// asymmetry is already pinned by
// TestPackDurableCallResult_HostCannotFillCallErrorCode above; this test states
// the consequence a sentinel can be built on.
//
// Measured 2026-09-02: the union over the whole reachable domain is
// 0xffffff000000ffff.
func TestPackDurableCallResult_SentinelBitsTheHostCannotReach(t *testing.T) {
	// Exhaustive over the two byte fields, strided over the 24-bit one. A
	// stride is enough because OR-ing cannot un-set a bit: any bit reachable
	// at all is reachable from some sampled responseLen, and the two byte
	// fields below are covered completely.
	var everSet uint64
	for rl := 0; rl < (1 << 24); rl += 997 {
		for ce := 0; ce < 256; ce++ {
			for ec := 0; ec < 256; ec++ {
				everSet |= uint64(packDurableCallResult(rl, byte(ce), byte(ec)))
			}
		}
	}

	const wantFree = 0x000000FFFFFF0000 // bits 16-39
	if everSet&wantFree != 0 {
		t.Fatalf("bits 16-39 are no longer free: union is %016x, overlap %016x.\n\n"+
			"A sentinel placed there would collide with a real result. If "+
			"callErrorCode was widened past a byte, pick a new range and update "+
			"IMPROVEMENT-PLAN 3.81.", everSet, everSet&wantFree)
	}

	// The other half, and the reason this test exists: the bit 3.81 named is
	// NOT free. If this ever stops holding, responseLen has been narrowed and
	// bit 62 becomes available after all -- which would be good news, but the
	// plan says something false until someone reads this.
	if everSet&(1<<62) == 0 {
		t.Fatalf("bit 62 is now unreachable by the host (union %016x); it sits "+
			"inside responseLen, so this means the layout changed. Bit 62 would "+
			"now be usable as a sentinel here.", everSet)
	}

	t.Logf("pinned: host-reachable bits are %016x; bits 16-39 are free for a "+
		"sentinel, bit 62 is not (it is responseLen's 4 MiB bit)", everSet)
}

// ---- 2. field independence: the property that finds the real defects ----

// TestPackAwaitSignalsResult_FieldsDoNotBleed pins the documented 16-bit limit.
//
// helpers.go says so in prose already: "a payload of more than 65535 bytes does
// not merely truncate: payloadLen << 32 runs into the signal-name field above
// it and corrupts that too." This asserts both halves of that sentence -- that
// it holds below the limit, and that it breaks above it -- so the day someone
// widens the fields, the second half fails and points at this comment.
func TestPackAwaitSignalsResult_FieldsDoNotBleed(t *testing.T) {
	r := prand()
	for i := 0; i < 2000; i++ {
		sigLen := uint32(r.Int31n(1 << 16))
		payLen := uint32(r.Int31n(1 << 16))
		errCode := uint32(r.Int31n(1 << 16))
		timedOut := i%2 == 0
		gotSig, gotPay, gotTO, gotErr := decodeAwaitSignalsResultGuest(
			packAwaitSignalsResult(sigLen, payLen, timedOut, errCode))
		if gotSig != sigLen || gotPay != payLen || gotTO != timedOut || gotErr != errCode {
			t.Fatalf("in-range (%d,%d,%v,%d) decoded as (%d,%d,%v,%d)",
				sigLen, payLen, timedOut, errCode, gotSig, gotPay, gotTO, gotErr)
		}
	}

	// The boundary, pinned. 65536 is the smallest payload that corrupts the
	// signal-name field above it.
	const overflow = 1 << 16
	gotSig, _, _, _ := decodeAwaitSignalsResultGuest(
		packAwaitSignalsResult(0, overflow, false, 0))
	if gotSig == 0 {
		t.Fatalf("payloadLen=%d no longer bleeds into sigNameLen -- if the fields "+
			"were widened, update packAwaitSignalsResult's comment and this test",
			overflow)
	}
	t.Logf("pinned: payloadLen=%d corrupts sigNameLen to %d (documented, unfixed: "+
		"the honest fix is an ABI decision, see packAwaitSignalsResult)", overflow, gotSig)
}

// TestPackAcquireLockResult_ErrCodeDoesNotSetAcquired is the property applied to
// a packer whose fields are adjacent with no gap: acquired is bit 8 and errCode
// occupies the bits below it, so an errCode of 256 or more sets the acquired
// flag. Callers pass small constants today; this pins the ceiling so that stops
// being true silently.
func TestPackAcquireLockResult_ErrCodeDoesNotSetAcquired(t *testing.T) {
	for errCode := uint32(0); errCode < 256; errCode++ {
		w := uint64(packAcquireLockResult(false, errCode))
		if acquired := (w>>8)&1 != 0; acquired {
			t.Fatalf("errCode=%d set the acquired flag with acquired=false", errCode)
		}
		if got := uint32(w & 0xFF); got != errCode {
			t.Fatalf("errCode=%d decoded as %d", errCode, got)
		}
	}
	w := uint64(packAcquireLockResult(false, 256))
	if (w>>8)&1 == 0 {
		t.Fatal("errCode=256 no longer collides with the acquired flag -- if the " +
			"layout was widened, update this test")
	}
	t.Log("pinned: packAcquireLockResult tolerates errCode <= 255; 256 sets `acquired`")
}

// TestPackAwaitChildResult_GuestSeesOnlyEightErrorBits records a real asymmetry
// rather than asserting the layout the host implies.
//
// The host packs errCode into bits 0-31. The guest's decodeSimpleResult reads
// `errCode: u8 = (r & 0xFF)` -- eight bits. Every caller in children.go passes
// 0, 1, 3 or 4, so nothing is broken today, and this test does not claim
// otherwise. It pins the width the guest can actually observe, so that adding a
// fifth error code above 255 fails here instead of arriving at the guest as a
// different, plausible code.
func TestPackAwaitChildResult_GuestSeesOnlyEightErrorBits(t *testing.T) {
	for _, errCode := range []uint32{0, 1, 3, 4, 255} {
		_, got := decodeSimpleResultGuest(packAwaitChildResult(7, errCode))
		if uint32(got) != errCode {
			t.Fatalf("errCode=%d decoded by guest as %d", errCode, got)
		}
	}
	if _, got := decodeSimpleResultGuest(packAwaitChildResult(7, 256)); got != 0 {
		t.Fatalf("errCode=256 decoded as %d; expected the guest's 8-bit mask to yield 0", got)
	}
	t.Log("pinned: the guest observes 8 bits of await-child error code, though the " +
		"host packs 32; callers use 0,1,3,4")
}

// ---- 3. sentinel disjointness ----

// TestNoValidResultCollidesWithSuspendSentinel checks the reserved value cannot
// be produced by a legitimate result.
//
// ABI.md reserves 1<<62 for suspension and says the host must check for it
// before decoding. 1<<62 is exactly written<<32 for written == 0x40000000, so
// the guarantee rests entirely on a write never reaching 1 GiB -- max_out_len is
// documented as 1048576. This asserts the reachable domain stays clear of it and
// pins the value that would collide.
func TestNoValidResultCollidesWithSuspendSentinel(t *testing.T) {
	const maxOutLen = 1 << 20 // ABI.md: out buffer capacity 1048576
	r := prand()
	for i := 0; i < 5000; i++ {
		written := uint32(r.Int31n(maxOutLen + 1))
		for _, errCode := range []uint32{0, 1, 3, 4} {
			if w := packAwaitChildResult(written, errCode); w == suspendSentinel {
				t.Fatalf("packAwaitChildResult(%d,%d) produced the suspend sentinel", written, errCode)
			}
		}
	}
	if packAwaitChildResult(1<<30, 0) != suspendSentinel {
		t.Fatal("0x40000000 bytes written no longer collides with the sentinel -- " +
			"if the sentinel or the layout moved, update this test")
	}
	t.Logf("pinned: a write of %d bytes would BE the suspend sentinel; ABI.md caps "+
		"max_out_len at %d, which is what keeps them disjoint", 1<<30, maxOutLen)
}
