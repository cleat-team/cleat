package engine

import (
	"crypto/rand"
	"testing"
)

// cleat#1992. PayloadEncryption held exactly one key, read once at worker
// startup, and openClassify tried three FORMS of that one key -- never a
// different key. So a payload key rotation had no way off the old key: rows
// sealed under it became unreadable the moment the operator swapped keys, and
// `cleatctl reseal-payloads` only ever upgraded the FORM under one key, never
// moved data to a new one (that half is reseal_moves_across_keys_test.go).
//
// This file proves the READ half: a PayloadEncryption built around a ring with
// a previous key can still open data sealed before the rotation, in whichever
// of the three forms that data was written in, and reports it as
// PayloadFormPreviousKey so the sweep knows to convert it.

// randKey is validKey (encryption_test.go) minus the base64 encoding step --
// the ring-based tests want raw key bytes to build VersionedKeys directly,
// not a base64 string to pass through NewPayloadEncryption.
func randKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return key
}

// TestRotationPreviousKeyStillReadable is the known-positive from cleat#1992's
// own acceptance criteria: "payloads written under key A read correctly with A
// as previous and B as current."
func TestRotationPreviousKeyStillReadable(t *testing.T) {
	keyA := randKey(t)
	keyB := randKey(t)

	ringA, err := NewKeyRing(VersionedKey{Version: 1, Key: keyA})
	if err != nil {
		t.Fatalf("build ring A: %v", err)
	}
	peA, err := NewPayloadEncryptionWithRing(ringA)
	if err != nil {
		t.Fatalf("NewPayloadEncryptionWithRing(A): %v", err)
	}
	const secret = "written before rotation"
	sealed, err := peA.Encrypt(tenantA, []byte(secret))
	if err != nil {
		t.Fatalf("seal under key A: %v", err)
	}

	// Deploy: B current, A previous.
	ringBA, err := NewKeyRing(VersionedKey{Version: 2, Key: keyB}, VersionedKey{Version: 1, Key: keyA})
	if err != nil {
		t.Fatalf("build ring B+A: %v", err)
	}
	peBA, err := NewPayloadEncryptionWithRing(ringBA)
	if err != nil {
		t.Fatalf("NewPayloadEncryptionWithRing(B+A): %v", err)
	}

	got, form, err := peBA.OpenAndClassify(tenantA, sealed)
	if err != nil {
		t.Fatalf("value sealed under the previous key did not open: %v", err)
	}
	if string(got) != secret {
		t.Errorf("plaintext = %q, want %q", got, secret)
	}
	if form != PayloadFormPreviousKey {
		t.Errorf("classified %s, want previous-key -- the sweep needs this to know the "+
			"row still needs converting", form)
	}

	// Same tenantID, foreign key: rejected. A ring with B+A must not open
	// data sealed under some THIRD key it was never given.
	keyC := randKey(t)
	ringC, err := NewKeyRing(VersionedKey{Version: 1, Key: keyC})
	if err != nil {
		t.Fatalf("build ring C: %v", err)
	}
	peC, err := NewPayloadEncryptionWithRing(ringC)
	if err != nil {
		t.Fatalf("NewPayloadEncryptionWithRing(C): %v", err)
	}
	sealedC, err := peC.Encrypt(tenantA, []byte("sealed under an unconfigured key"))
	if err != nil {
		t.Fatalf("seal under key C: %v", err)
	}
	if _, form, err := peBA.OpenAndClassify(tenantA, sealedC); err == nil {
		t.Errorf("opened a value sealed under a key this ring never configured, form=%s", form)
	}
}

// TestRotationNewWritesUseTheCurrentKey is the write half: Encrypt always
// seals under ring.Current(), never a previous key, and the result does NOT
// open under a PayloadEncryption that only knows the OLD key -- proving the
// write moved, not just that the ring can read both.
func TestRotationNewWritesUseTheCurrentKey(t *testing.T) {
	keyA := randKey(t)
	keyB := randKey(t)
	ringBA, err := NewKeyRing(VersionedKey{Version: 2, Key: keyB}, VersionedKey{Version: 1, Key: keyA})
	if err != nil {
		t.Fatalf("build ring: %v", err)
	}
	peBA, err := NewPayloadEncryptionWithRing(ringBA)
	if err != nil {
		t.Fatalf("NewPayloadEncryptionWithRing: %v", err)
	}

	const secret = "written after rotation"
	sealed, err := peBA.Encrypt(tenantA, []byte(secret))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	got, form, err := peBA.OpenAndClassify(tenantA, sealed)
	if err != nil {
		t.Fatalf("did not open under its own ring: %v", err)
	}
	if string(got) != secret || form != PayloadFormDerived {
		t.Errorf("got=%q form=%s, want %q derived", got, form, secret)
	}

	ringAOnly, err := NewKeyRing(VersionedKey{Version: 1, Key: keyA})
	if err != nil {
		t.Fatalf("build ring A-only: %v", err)
	}
	peAOnly, err := NewPayloadEncryptionWithRing(ringAOnly)
	if err != nil {
		t.Fatalf("NewPayloadEncryptionWithRing(A-only): %v", err)
	}
	if _, _, err := peAOnly.OpenAndClassify(tenantA, sealed); err == nil {
		t.Error("a value sealed after rotation opened under the OLD key alone -- " +
			"Encrypt must be sealing under the current key, not the previous one")
	}
}

// TestRotationRemovingThePreviousKeyLeavesCurrentReadable is the third
// acceptance criterion: once reseal has moved a row to the current key, the
// previous key can be dropped from the ring entirely and the row still opens.
func TestRotationRemovingThePreviousKeyLeavesCurrentReadable(t *testing.T) {
	keyB := randKey(t)
	ringBOnly, err := NewKeyRing(VersionedKey{Version: 2, Key: keyB})
	if err != nil {
		t.Fatalf("build ring B-only: %v", err)
	}
	peBOnly, err := NewPayloadEncryptionWithRing(ringBOnly)
	if err != nil {
		t.Fatalf("NewPayloadEncryptionWithRing(B-only): %v", err)
	}

	const secret = "resealed under B, A retired"
	sealed, err := peBOnly.Encrypt(tenantA, []byte(secret))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, form, err := peBOnly.OpenAndClassify(tenantA, sealed)
	if err != nil {
		t.Fatalf("did not open once the previous key was removed: %v", err)
	}
	if string(got) != secret || form != PayloadFormDerived {
		t.Errorf("got=%q form=%s, want %q derived", got, form, secret)
	}
}

// TestNewPayloadEncryptionWithRing_NilRing refuses rather than building a
// PayloadEncryption that silently has no current key -- the same "half
// configured is an error, not a guess" rule KeyRing and SecretKeyRingFromEnv
// already apply.
func TestNewPayloadEncryptionWithRing_NilRing(t *testing.T) {
	if _, err := NewPayloadEncryptionWithRing(nil); err == nil {
		t.Fatal("expected an error for a nil ring")
	}
}
