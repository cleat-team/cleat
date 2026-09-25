package main

import (
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// writeKeyFile writes a fresh random 32-byte AES-256 key, base64-encoded, and returns its path.
func writeKeyFile(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "key.b64")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}

func TestLoadPayloadEncryption_NoCurrentKeyMeansEncryptionOff(t *testing.T) {
	pe, err := loadPayloadEncryption("", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pe != nil {
		t.Fatal("want nil PayloadEncryption when no key file is configured")
	}
}

func TestLoadPayloadEncryption_PreviousWithoutCurrentIsRefused(t *testing.T) {
	prev := writeKeyFile(t)
	_, err := loadPayloadEncryption("", prev)
	if err == nil {
		t.Fatal("want an error: --encryption-key-file-previous with no --encryption-key-file")
	}
	if !strings.Contains(err.Error(), "requires --encryption-key-file") {
		t.Errorf("error = %q, want it to name the missing flag", err)
	}
}

func TestLoadPayloadEncryption_CurrentOnlySealsAndOpens(t *testing.T) {
	cur := writeKeyFile(t)
	pe, err := loadPayloadEncryption(cur, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if pe == nil {
		t.Fatal("want a non-nil PayloadEncryption")
	}
	const tenant = "11111111-1111-1111-1111-111111111111"
	sealed, err := pe.Encrypt(tenant, []byte("plaintext"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, form, err := pe.OpenAndClassify(tenant, sealed)
	if err != nil {
		t.Fatalf("OpenAndClassify: %v", err)
	}
	if string(got) != "plaintext" {
		t.Errorf("opened %q, want %q", got, "plaintext")
	}
	if form != engine.PayloadFormDerived {
		t.Errorf("form = %v, want PayloadFormDerived (this is the current key, freshly used)", form)
	}
}

// This pins the mechanism docs/how-to/rotate-payload-encryption-key.md describes: a worker rolled
// onto --encryption-key-file B --encryption-key-file-previous A can still open a row a
// still-on-A-only worker wrote, via the previous-key fallback -- and the reverse fails closed.
func TestLoadPayloadEncryption_PreviousKeyOpensOldRowsAsPreviousKeyForm(t *testing.T) {
	oldKeyPath := writeKeyFile(t)
	oldOnly, err := loadPayloadEncryption(oldKeyPath, "")
	if err != nil {
		t.Fatalf("build the old-key-only worker: %v", err)
	}
	const tenant = "22222222-2222-2222-2222-222222222222"
	oldSealed, err := oldOnly.Encrypt(tenant, []byte("sealed-under-the-old-key"))
	if err != nil {
		t.Fatalf("seal under the old key: %v", err)
	}

	newKeyPath := writeKeyFile(t)
	rolled, err := loadPayloadEncryption(newKeyPath, oldKeyPath)
	if err != nil {
		t.Fatalf("build the rolled worker (new current, old previous): %v", err)
	}

	got, form, err := rolled.OpenAndClassify(tenant, oldSealed)
	if err != nil {
		t.Fatalf("a worker rolled onto the new key with the old one as --encryption-key-file-previous could not open a row the old-key-only worker wrote: %v", err)
	}
	if string(got) != "sealed-under-the-old-key" {
		t.Errorf("opened %q, want %q", got, "sealed-under-the-old-key")
	}
	if form != engine.PayloadFormPreviousKey {
		t.Errorf("form = %v, want PayloadFormPreviousKey: this row was sealed under the previous key, not the current one", form)
	}

	// The asymmetric half of the guarantee, stated explicitly in the how-to: a worker still on the
	// OLD key alone cannot read what the rolled worker writes NEW, until it restarts with the new
	// flags. Confirmed here rather than only in prose.
	newSealed, err := rolled.Encrypt(tenant, []byte("sealed-under-the-new-key"))
	if err != nil {
		t.Fatalf("seal under the rolled worker's current key: %v", err)
	}
	if _, _, err := oldOnly.OpenAndClassify(tenant, newSealed); err == nil {
		t.Fatal("a worker still on the old key alone opened a row sealed under the new key -- it should fail closed until it restarts with the new key")
	}
}

func TestLoadPayloadEncryption_MissingKeyFileIsAnError(t *testing.T) {
	_, err := loadPayloadEncryption(filepath.Join(t.TempDir(), "does-not-exist"), "")
	if err == nil {
		t.Fatal("want an error for a nonexistent --encryption-key-file")
	}
}

func TestLoadPayloadEncryption_MissingPreviousKeyFileIsAnError(t *testing.T) {
	cur := writeKeyFile(t)
	_, err := loadPayloadEncryption(cur, filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("want an error for a nonexistent --encryption-key-file-previous")
	}
}
