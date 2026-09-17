package engine

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"io"
	"testing"

	"golang.org/x/crypto/hkdf"
)

// cleat#1793: the payload key is derived per tenant, so a key recovered from one
// tenant's ciphertext does not decrypt another's.
//
// This is a DIFFERENT property from cleat#1776's AAD binding, and the difference
// is the reason both exist:
//
//	AAD binding      stops a ciphertext being MOVED into another tenant's row
//	key derivation   stops one recovered key READING every tenant's rows
//
// A test that only checks "tenant B cannot read tenant A's blob" passes on AAD
// alone and says nothing about derivation. So the tests below assert on the KEYS
// themselves, and on the form each ciphertext is in, rather than only on who can
// open what.

// sealWith is the three stored forms, built by hand.
//
// By hand because no API produces the two older ones any more: Encrypt derives,
// which is the change. Building them here is the only way the transition is
// tested against real artefacts instead of a mock of them.
func sealWith(t *testing.T, key []byte, aad, plaintext string) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(cryptorand.Reader, nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	var a []byte
	if aad != "" {
		a = []byte(aad)
	}
	return gcm.Seal(nonce, nonce, []byte(plaintext), a)
}

// TestTheThreeStoredFormsAreAllReadableAndClassified is the transition matrix.
//
// Every row is a form that exists on disk somewhere, and all three must open for
// their own tenant and be reported as what they are -- the sweep decides what to
// rewrite from the form, so a misclassification either leaves an old form in
// place or rewrites one that is already current.
func TestTheThreeStoredFormsAreAllReadableAndClassified(t *testing.T) {
	keyB64 := validKey(t)
	pe, err := NewPayloadEncryption(keyB64)
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}
	master := pe.key
	tc, err := pe.forTenant(tenantA)
	if err != nil {
		t.Fatalf("forTenant: %v", err)
	}

	const secret = "card 4111111111111111"
	cases := []struct {
		name string
		blob []byte
		want PayloadForm
	}{
		{"derived (current)", sealWith(t, tc.derived, tenantA, secret), PayloadFormDerived},
		{"bound (cleat#1792)", sealWith(t, master, tenantA, secret), PayloadFormBound},
		{"legacy (pre-1776)", sealWith(t, master, "", secret), PayloadFormLegacy},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, form, err := pe.OpenAndClassify(tenantA, c.blob)
			if err != nil {
				t.Fatalf("does not open for its own tenant: %v", err)
			}
			if string(got) != secret {
				t.Errorf("plaintext = %q, want %q", got, secret)
			}
			if form != c.want {
				t.Errorf("classified %s, want %s -- the sweep decides what to rewrite "+
					"from this, so a wrong form either skips an old value or rewrites a "+
					"current one", form, c.want)
			}
		})
	}

	// The two tenant-bound forms must refuse a foreign tenant. Legacy must NOT
	// -- that is the residual, and a test asserting otherwise would be asserting
	// a property the data does not have.
	t.Run("foreign tenant", func(t *testing.T) {
		for _, c := range cases[:2] {
			if _, _, err := pe.OpenAndClassify(tenantB, c.blob); err == nil {
				t.Errorf("%s opened for a foreign tenant", c.name)
			}
		}
		if _, form, err := pe.OpenAndClassify(tenantB, cases[2].blob); err != nil {
			t.Errorf("legacy blob did not open for a foreign tenant (%v) -- it is bound "+
				"to nothing, so it does, and pretending otherwise hides the residual", err)
		} else if form != PayloadFormLegacy {
			t.Errorf("legacy blob classified %s for a foreign tenant", form)
		}
	})
}

// TestEachTenantGetsADifferentPayloadKey asserts on the keys, which is the only
// place the derivation property is visible.
//
// Everything downstream of it -- who can open what -- is also satisfied by the
// AAD binding alone, so this is the test that would fail if forTenant returned
// pe.key unchanged while every other test kept passing.
func TestEachTenantGetsADifferentPayloadKey(t *testing.T) {
	pe, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}
	a, err := pe.forTenant(tenantA)
	if err != nil {
		t.Fatalf("forTenant A: %v", err)
	}
	b, err := pe.forTenant(tenantB)
	if err != nil {
		t.Fatalf("forTenant B: %v", err)
	}

	if bytes.Equal(a.derived, b.derived) {
		t.Error("two tenants derived the SAME key, so a key recovered from one " +
			"tenant's ciphertext decrypts the other's -- which is the whole point " +
			"of cleat#1793")
	}
	if bytes.Equal(a.derived, pe.key) {
		t.Error("the derived key IS the master key, so nothing was derived")
	}
	if len(a.derived) != 32 {
		t.Errorf("derived key is %d bytes, want 32 (AES-256)", len(a.derived))
	}

	// Deterministic, or a row written now cannot be read later.
	again, err := pe.forTenant(tenantA)
	if err != nil {
		t.Fatalf("forTenant A again: %v", err)
	}
	if !bytes.Equal(a.derived, again.derived) {
		t.Error("derivation is not deterministic: a row sealed now would not open later")
	}

	// An empty tenant is refused rather than deriving from "" -- same reasoning
	// as ErrNoTenantForEncryption, one layer down.
	if _, err := pe.forTenant(""); err == nil {
		t.Error("forTenant(\"\") did not refuse")
	}
}

// TestAPayloadKeyIsNotATenantSecretKey pins the info-string separation, which is
// otherwise a claim in a comment.
//
// If both subsystems are ever pointed at one master secret -- which nothing
// currently prevents -- a shared derivation would mean a payload key opens a
// tenant secret and the reverse. The only thing stopping that is the info
// string differing, and nothing else in the tree asserts it.
func TestAPayloadKeyIsNotATenantSecretKey(t *testing.T) {
	pe, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}
	payloadKey, err := pe.forTenant(tenantA)
	if err != nil {
		t.Fatalf("forTenant: %v", err)
	}

	// The same construction tenant_secrets.go uses, with ITS info string.
	secretKey := make([]byte, 32)
	r := hkdf.New(sha256.New, pe.key, []byte(tenantA), []byte("cleat-tenant-secret-v1"))
	if _, err := io.ReadFull(r, secretKey); err != nil {
		t.Fatalf("derive secret key: %v", err)
	}

	if bytes.Equal(payloadKey.derived, secretKey) {
		t.Error("the payload key and the tenant-secret key are identical for the same " +
			"master and tenant, so one subsystem's key opens the other's ciphertexts. " +
			"The info strings must differ -- payloadKeyInfo in encryption.go against " +
			"cleat-tenant-secret-v1 in tenant_secrets.go.")
	}

	// And the control: the same info string DOES reproduce the payload key, so
	// the inequality above is about the info string and not about this test
	// deriving something unrelated.
	same := make([]byte, 32)
	r2 := hkdf.New(sha256.New, pe.key, []byte(tenantA), []byte(payloadKeyInfo))
	if _, err := io.ReadFull(r2, same); err != nil {
		t.Fatalf("re-derive: %v", err)
	}
	if !bytes.Equal(payloadKey.derived, same) {
		t.Fatal("UNMEASURED: re-deriving with payloadKeyInfo did not reproduce the key, " +
			"so this test is not exercising the derivation it claims to")
	}
}
