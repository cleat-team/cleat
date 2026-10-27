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
	pe, ring, err := loadPayloadEncryption("", "", payloadKeyVersions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pe != nil {
		t.Fatal("want nil PayloadEncryption when no key file is configured")
	}
	if ring != nil {
		t.Fatal("want nil *ReloadableKeyRing when no key file is configured -- cleat#2298: nil exactly when pe is nil")
	}
}

func TestLoadPayloadEncryption_PreviousWithoutCurrentIsRefused(t *testing.T) {
	prev := writeKeyFile(t)
	_, _, err := loadPayloadEncryption("", prev, payloadKeyVersions{})
	if err == nil {
		t.Fatal("want an error: --encryption-key-file-previous with no --encryption-key-file")
	}
	if !strings.Contains(err.Error(), "requires --encryption-key-file") {
		t.Errorf("error = %q, want it to name the missing flag", err)
	}
}

func TestLoadPayloadEncryption_CurrentOnlySealsAndOpens(t *testing.T) {
	cur := writeKeyFile(t)
	pe, ring, err := loadPayloadEncryption(cur, "", payloadKeyVersions{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if pe == nil {
		t.Fatal("want a non-nil PayloadEncryption")
	}
	if ring == nil {
		t.Fatal("want a non-nil *ReloadableKeyRing alongside a non-nil PayloadEncryption -- cleat#2298")
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
	oldOnly, _, err := loadPayloadEncryption(oldKeyPath, "", payloadKeyVersions{})
	if err != nil {
		t.Fatalf("build the old-key-only worker: %v", err)
	}
	const tenant = "22222222-2222-2222-2222-222222222222"
	oldSealed, err := oldOnly.Encrypt(tenant, []byte("sealed-under-the-old-key"))
	if err != nil {
		t.Fatalf("seal under the old key: %v", err)
	}

	newKeyPath := writeKeyFile(t)
	rolled, _, err := loadPayloadEncryption(newKeyPath, oldKeyPath, payloadKeyVersions{})
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
	_, _, err := loadPayloadEncryption(filepath.Join(t.TempDir(), "does-not-exist"), "", payloadKeyVersions{})
	if err == nil {
		t.Fatal("want an error for a nonexistent --encryption-key-file")
	}
}

func TestLoadPayloadEncryption_MissingPreviousKeyFileIsAnError(t *testing.T) {
	cur := writeKeyFile(t)
	_, _, err := loadPayloadEncryption(cur, filepath.Join(t.TempDir(), "does-not-exist"), payloadKeyVersions{})
	if err == nil {
		t.Fatal("want an error for a nonexistent --encryption-key-file-previous")
	}
}

// cleat#3203: a payloadKeyVersions zero value must resolve to the same
// 2/1 labels loadPayloadKeyRing hardcoded before this issue -- every
// existing caller above that passes payloadKeyVersions{} depends on this
// without saying so, so it is worth pinning directly.
func TestLoadPayloadEncryption_ZeroValueVersionsAreTheHistoricalDefaults(t *testing.T) {
	cur := writeKeyFile(t)
	prev := writeKeyFile(t)
	ring, err := loadPayloadKeyRing(cur, prev, payloadKeyVersions{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := ring.Current().Version; got != 2 {
		t.Errorf("current version = %d, want 2 (the historical default)", got)
	}
	if _, ok := ring.Key(1); !ok {
		t.Error("previous version = want 1 (the historical default) to resolve")
	}
}

// cleat#3203's mechanism: an operator-set version number is honored, not
// just tolerated -- the property SIGHUP-driven rotation depends on.
func TestLoadPayloadEncryption_CustomVersionNumbersAreHonored(t *testing.T) {
	cur := writeKeyFile(t)
	prev := writeKeyFile(t)
	ring, err := loadPayloadKeyRing(cur, prev, payloadKeyVersions{current: "5", previous: "3"})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := ring.Current().Version; got != 5 {
		t.Errorf("current version = %d, want 5", got)
	}
	if _, ok := ring.Key(3); !ok {
		t.Error("version 3 (previous) should resolve")
	}
	if _, ok := ring.Key(2); ok {
		t.Error("version 2 (the old hardcoded default) should NOT resolve -- it was never configured")
	}
}

// The "-file" form exists so a SIGHUP can pick up a version change with no
// restart (same reason the KEY itself is a file and not an inline flag) --
// pinned here by changing the file's content BETWEEN two loadPayloadKeyRing
// calls and confirming the second call sees the new value.
func TestLoadPayloadEncryption_VersionFileIsReadFreshEveryCall(t *testing.T) {
	cur := writeKeyFile(t)
	versionFile := filepath.Join(t.TempDir(), "version")
	if err := os.WriteFile(versionFile, []byte("7"), 0o600); err != nil {
		t.Fatal(err)
	}

	ring1, err := loadPayloadKeyRing(cur, "", payloadKeyVersions{currentFile: versionFile})
	if err != nil {
		t.Fatalf("load (first): %v", err)
	}
	if got := ring1.Current().Version; got != 7 {
		t.Fatalf("first load: current version = %d, want 7", got)
	}

	if err := os.WriteFile(versionFile, []byte("8"), 0o600); err != nil {
		t.Fatal(err)
	}
	ring2, err := loadPayloadKeyRing(cur, "", payloadKeyVersions{currentFile: versionFile})
	if err != nil {
		t.Fatalf("load (second): %v", err)
	}
	if got := ring2.Current().Version; got != 8 {
		t.Fatalf("second load: current version = %d, want 8 -- the file must be re-read, not cached from the first call", got)
	}
}

// resolvePayloadKeyVersion's "both set is refused" contract, mirroring
// engine.SecretKeyRingFromEnv's resolveSecretKeyInput for
// CLEAT_SECRET_MASTER_KEY_VERSION[_FILE] -- an operator who set both flags
// is most likely mid-migration between the two forms, and silently
// preferring one would hide exactly that ambiguity.
func TestLoadPayloadEncryption_BothVersionFormsSetIsRefused(t *testing.T) {
	cur := writeKeyFile(t)
	versionFile := filepath.Join(t.TempDir(), "version")
	if err := os.WriteFile(versionFile, []byte("7"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadPayloadKeyRing(cur, "", payloadKeyVersions{current: "5", currentFile: versionFile})
	if err == nil {
		t.Fatal("want an error: both --encryption-key-file-version and --encryption-key-file-version-file are set")
	}
	if !strings.Contains(err.Error(), "set exactly one") {
		t.Errorf("error = %q, want it to say to set exactly one", err)
	}
}

// A non-numeric or non-positive version is refused with a message naming
// the flag, the same shape engine.KeyVersionFromEnv already gives the
// secrets side.
func TestLoadPayloadEncryption_InvalidVersionIsRefused(t *testing.T) {
	cur := writeKeyFile(t)
	_, err := loadPayloadKeyRing(cur, "", payloadKeyVersions{current: "not-a-number"})
	if err == nil {
		t.Fatal("want an error for a non-numeric --encryption-key-file-version")
	}
	if !strings.Contains(err.Error(), "encryption-key-file-version") {
		t.Errorf("error = %q, want it to name the flag", err)
	}
}
