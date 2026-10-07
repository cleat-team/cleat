package engine

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

// envFromMap builds a getenv func from a plain map, the shape every
// SecretKeyRingFromEnv test in this file uses.
func envFromMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func writeTempFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func base64Key(fill byte) string {
	k := make([]byte, 32)
	for i := range k {
		k[i] = fill
	}
	return base64.StdEncoding.EncodeToString(k)
}

// TestSecretKeyRingFromEnvReadsTheCurrentKeyFromAFile is cleat#2298's M1:
// the whole point of the _FILE form is that its CONTENT, not the env var
// itself, is what a reload re-reads -- so the basic case is reading the key
// from a file at all, with no env var holding the key value directly.
func TestSecretKeyRingFromEnvReadsTheCurrentKeyFromAFile(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTempFile(t, dir, "current.key", base64Key(0x01)+"\n")

	ring, err := SecretKeyRingFromEnv(envFromMap(map[string]string{
		"CLEAT_SECRET_MASTER_KEY_FILE": keyPath,
	}))
	if err != nil {
		t.Fatalf("SecretKeyRingFromEnv: %v", err)
	}
	if ring == nil {
		t.Fatal("ring is nil, want a configured ring")
	}
	if ring.Current().Version != 1 {
		t.Errorf("version = %d, want 1 (the default)", ring.Current().Version)
	}
}

// TestSecretKeyRingFromEnvRereadsTheFileEveryCall is the property a reload
// actually depends on: calling SecretKeyRingFromEnv AGAIN, after the file's
// content changed on disk, must return the NEW key -- not a cached value
// from the first call. This is what makes the _FILE form usable for SIGHUP
// reload at all; a one-time read at process start would be no different
// from the plain env var it exists to replace.
func TestSecretKeyRingFromEnvRereadsTheFileEveryCall(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "current.key")
	if err := os.WriteFile(keyPath, []byte(base64Key(0x01)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	getenv := envFromMap(map[string]string{"CLEAT_SECRET_MASTER_KEY_FILE": keyPath})

	first, err := SecretKeyRingFromEnv(getenv)
	if err != nil {
		t.Fatalf("first SecretKeyRingFromEnv: %v", err)
	}
	firstKey := first.Current().Key

	// Simulate a mounted-secret update: rewrite the same path.
	if err := os.WriteFile(keyPath, []byte(base64Key(0x02)), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	second, err := SecretKeyRingFromEnv(getenv)
	if err != nil {
		t.Fatalf("second SecretKeyRingFromEnv: %v", err)
	}
	if string(second.Current().Key) == string(firstKey) {
		t.Fatal("second call returned the SAME key bytes as the first -- the file was not re-read")
	}
}

// TestSecretKeyRingFromEnvReadsAllFourFromFiles exercises the full ring:
// current key, current version, previous key, previous version, every one
// of them from a file rather than an inline value -- the shape a rotation
// via reload actually needs (cleat#2298's M1 doc comment on why version
// must be file-sourced too, not only the key).
func TestSecretKeyRingFromEnvReadsAllFourFromFiles(t *testing.T) {
	dir := t.TempDir()
	curKeyPath := writeTempFile(t, dir, "current.key", base64Key(0xAA))
	curVerPath := writeTempFile(t, dir, "current.version", "2")
	prevKeyPath := writeTempFile(t, dir, "previous.key", base64Key(0xBB))
	prevVerPath := writeTempFile(t, dir, "previous.version", "1")

	ring, err := SecretKeyRingFromEnv(envFromMap(map[string]string{
		"CLEAT_SECRET_MASTER_KEY_FILE":                  curKeyPath,
		"CLEAT_SECRET_MASTER_KEY_VERSION_FILE":          curVerPath,
		"CLEAT_SECRET_MASTER_KEY_PREVIOUS_FILE":         prevKeyPath,
		"CLEAT_SECRET_MASTER_KEY_PREVIOUS_VERSION_FILE": prevVerPath,
	}))
	if err != nil {
		t.Fatalf("SecretKeyRingFromEnv: %v", err)
	}
	if ring.Current().Version != 2 {
		t.Errorf("current version = %d, want 2", ring.Current().Version)
	}
	if _, ok := ring.Key(1); !ok {
		t.Error("version 1 (the previous key) is not in the ring")
	}
}

// TestSecretKeyRingFromEnvRefusesBothInlineAndFileForOneName is the
// ambiguity check: setting CLEAT_SECRET_MASTER_KEY and
// CLEAT_SECRET_MASTER_KEY_FILE together must be an error, not a silent
// pick -- an operator mid-migration from one form to the other needs to be
// told, not guessed for.
func TestSecretKeyRingFromEnvRefusesBothInlineAndFileForOneName(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeTempFile(t, dir, "current.key", base64Key(0x01))

	_, err := SecretKeyRingFromEnv(envFromMap(map[string]string{
		"CLEAT_SECRET_MASTER_KEY":      base64Key(0x02),
		"CLEAT_SECRET_MASTER_KEY_FILE": keyPath,
	}))
	if err == nil {
		t.Fatal("want an error when both the inline value and the _FILE path are set, got nil")
	}
}

// TestSecretKeyRingFromEnvFileNotFoundIsAnError confirms a missing file
// behind a _FILE variable is reported, not silently treated as "no key".
func TestSecretKeyRingFromEnvFileNotFoundIsAnError(t *testing.T) {
	_, err := SecretKeyRingFromEnv(envFromMap(map[string]string{
		"CLEAT_SECRET_MASTER_KEY_FILE": "/nonexistent/path/to/a/key/that/does/not/exist",
	}))
	if err == nil {
		t.Fatal("want an error for a _FILE path that does not exist, got nil")
	}
}

// TestSecretKeyRingFromEnvMixingInlineAndFileAcrossNamesIsFine confirms the
// refusal in TestSecretKeyRingFromEnvRefusesBothInlineAndFileForOneName is
// scoped to one NAME, not to the whole call: the current key can come from
// a file while the previous key stays inline (or vice versa), which is a
// realistic transitional state for an operator moving one variable at a
// time.
func TestSecretKeyRingFromEnvMixingInlineAndFileAcrossNamesIsFine(t *testing.T) {
	dir := t.TempDir()
	curKeyPath := writeTempFile(t, dir, "current.key", base64Key(0xAA))

	ring, err := SecretKeyRingFromEnv(envFromMap(map[string]string{
		"CLEAT_SECRET_MASTER_KEY_FILE":             curKeyPath,
		"CLEAT_SECRET_MASTER_KEY_VERSION":          "2",
		"CLEAT_SECRET_MASTER_KEY_PREVIOUS":         base64Key(0xBB),
		"CLEAT_SECRET_MASTER_KEY_PREVIOUS_VERSION": "1",
	}))
	if err != nil {
		t.Fatalf("SecretKeyRingFromEnv: %v", err)
	}
	if ring.Current().Version != 2 {
		t.Errorf("current version = %d, want 2", ring.Current().Version)
	}
	if _, ok := ring.Key(1); !ok {
		t.Error("version 1 (the inline previous key) is not in the ring")
	}
}
