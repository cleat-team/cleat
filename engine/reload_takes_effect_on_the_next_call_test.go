package engine

import (
	"testing"
)

// TestPayloadEncryptionRespondsToReloadOnTheNextCall is cleat#2298's literal
// acceptance criterion at the engine level, for payload encryption: "sending
// SIGHUP changes the key used on the next operation without a restart".
// This test is the engine half of that -- no process, no signal, no
// restart, just a Reload call on the SAME *ReloadableKeyRing a live
// PayloadEncryption holds, proving the very next Encrypt call picks it up.
func TestPayloadEncryptionRespondsToReloadOnTheNextCall(t *testing.T) {
	const tenant = "11111111-1111-1111-1111-111111111111"
	ringA, err := NewKeyRing(VersionedKey{Version: 1, Key: key32(0xAA)})
	if err != nil {
		t.Fatalf("NewKeyRing(A): %v", err)
	}
	reloadable := NewReloadableKeyRing(ringA)
	pe, err := NewPayloadEncryptionWithReloadableRing(reloadable)
	if err != nil {
		t.Fatalf("NewPayloadEncryptionWithReloadableRing: %v", err)
	}

	before, err := pe.Encrypt(tenant, []byte("before reload"))
	if err != nil {
		t.Fatalf("Encrypt before reload: %v", err)
	}

	// Rotate: B becomes current, A demoted to previous -- no restart, no new
	// PayloadEncryption value, just Reload on the ring this one already
	// holds.
	ringB, err := NewKeyRing(VersionedKey{Version: 2, Key: key32(0xBB)}, VersionedKey{Version: 1, Key: key32(0xAA)})
	if err != nil {
		t.Fatalf("NewKeyRing(B): %v", err)
	}
	if _, err := reloadable.Reload(ringB); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	after, err := pe.Encrypt(tenant, []byte("after reload"))
	if err != nil {
		t.Fatalf("Encrypt after reload: %v", err)
	}

	// The pre-reload ciphertext must still open (A survives as previous)...
	if pt, _, err := pe.OpenAndClassify(tenant, before); err != nil || string(pt) != "before reload" {
		t.Errorf("open the pre-reload ciphertext after Reload: got (%q, %v), want (%q, nil)", pt, err, "before reload")
	}
	// ...and the post-reload ciphertext must be sealed under the NEW key:
	// opening it against a PayloadEncryption that only ever had A (never
	// reloaded) must fail, proving Encrypt actually used B, not a cached A.
	aOnly, err := NewPayloadEncryptionWithRing(ringA)
	if err != nil {
		t.Fatalf("NewPayloadEncryptionWithRing(A only): %v", err)
	}
	if pt, _, err := aOnly.OpenAndClassify(tenant, after); err == nil {
		t.Errorf("the post-reload ciphertext opened under key A alone (got %q) -- Encrypt did not pick up the reload", pt)
	}
	// And the live (reloaded) encryptor opens its own post-reload value.
	if pt, _, err := pe.OpenAndClassify(tenant, after); err != nil || string(pt) != "after reload" {
		t.Errorf("open the post-reload ciphertext: got (%q, %v), want (%q, nil)", pt, err, "after reload")
	}
}

// TestSecretStoreAndDeploymentSecretStoreShareOneReloadCall is cleat#2298's
// design property, not just the mechanism: SecretStore and
// DeploymentSecretStore built from the SAME *ReloadableKeyRing both see a
// single Reload call -- proving there is no window in which one store has
// rotated and the other has not, which is what sharing one instance
// (rather than two independently-fed ReloadableKeyRings) is FOR.
func TestSecretStoreAndDeploymentSecretStoreShareOneReloadCall(t *testing.T) {
	ringA, err := NewKeyRing(VersionedKey{Version: 1, Key: key32(0x01)})
	if err != nil {
		t.Fatalf("NewKeyRing(A): %v", err)
	}
	shared := NewReloadableKeyRing(ringA)
	secrets := NewSecretStoreWithReloadableRing(nil, "postgres", shared)
	deployment := NewDeploymentSecretStoreWithReloadableRing(nil, "postgres", shared)

	const tenant = "22222222-2222-2222-2222-222222222222"
	sealedBefore, err := secrets.seal(secrets.ring.Load(), tenant, "tenant-secret")
	if err != nil {
		t.Fatalf("secrets.seal before reload: %v", err)
	}
	depSealedBefore, err := deployment.seal(deployment.ring.Load(), "deployment-secret-name", "deployment-value")
	if err != nil {
		t.Fatalf("deployment.seal before reload: %v", err)
	}

	ringB, err := NewKeyRing(VersionedKey{Version: 2, Key: key32(0x02)}, VersionedKey{Version: 1, Key: key32(0x01)})
	if err != nil {
		t.Fatalf("NewKeyRing(B): %v", err)
	}
	if _, err := shared.Reload(ringB); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	// BOTH stores must now be on version 2 as current -- a single Reload
	// call, read back through two different store types.
	if got := secrets.ring.Load().Current().Version; got != 2 {
		t.Errorf("secrets.ring.Load().Current().Version = %d after Reload, want 2", got)
	}
	if got := deployment.ring.Load().Current().Version; got != 2 {
		t.Errorf("deployment.ring.Load().Current().Version = %d after Reload, want 2", got)
	}

	// And both can still open what they sealed under A -- A is still
	// present, as the previous key.
	if _, err := secrets.open(secrets.ring.Load(), tenant, sealedBefore, 1); err != nil {
		t.Errorf("secrets.open the pre-reload ciphertext (version 1): %v", err)
	}
	if _, err := deployment.open(deployment.ring.Load(), "deployment-secret-name", depSealedBefore, 1); err != nil {
		t.Errorf("deployment.open the pre-reload ciphertext (version 1): %v", err)
	}
}
