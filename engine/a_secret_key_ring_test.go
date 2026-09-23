package engine

// The key ring (cleat#1991): the configuration a worker or cleatctl reads, and
// what seal and open do with it. No database here -- the rows, the version
// column and the reseal are in a_secret_rotation_test.go on all three dialects.

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func b64Key(fill byte) string {
	k := make([]byte, 32)
	for i := range k {
		k[i] = fill
	}
	return base64.StdEncoding.EncodeToString(k)
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestSecretKeyRingFromEnv(t *testing.T) {
	const (
		cur, curV   = "CLEAT_SECRET_MASTER_KEY", "CLEAT_SECRET_MASTER_KEY_VERSION"
		prev, prevV = "CLEAT_SECRET_MASTER_KEY_PREVIOUS", "CLEAT_SECRET_MASTER_KEY_PREVIOUS_VERSION"
	)
	for _, tc := range []struct {
		name     string
		env      map[string]string
		wantNil  bool
		wantCur  int
		wantVers []int
		wantErr  string
	}{
		{name: "nothing configured is a legitimate deployment that uses no secrets", env: map[string]string{}, wantNil: true},
		{name: "a lone key is version 1, which is what every existing row already carries",
			env: map[string]string{cur: b64Key(1)}, wantCur: 1, wantVers: []int{1}},
		{name: "the current key can be given a version",
			env: map[string]string{cur: b64Key(2), curV: "2"}, wantCur: 2, wantVers: []int{2}},
		{name: "a rotation in flight: new current, old previous",
			env: map[string]string{cur: b64Key(2), curV: "2", prev: b64Key(1), prevV: "1"}, wantCur: 2, wantVers: []int{1, 2}},
		{name: "previous key with no version is refused rather than guessed",
			env:     map[string]string{cur: b64Key(2), curV: "2", prev: b64Key(1)},
			wantErr: "CLEAT_SECRET_MASTER_KEY_PREVIOUS_VERSION is not"},
		{name: "a version with no key",
			env: map[string]string{curV: "2"}, wantErr: "needs its current key"},
		{name: "a previous key with no current key",
			env: map[string]string{prev: b64Key(1), prevV: "1"}, wantErr: "needs its current key"},
		{name: "a previous version with no previous key",
			env:     map[string]string{cur: b64Key(2), curV: "2", prevV: "1"},
			wantErr: "is set but CLEAT_SECRET_MASTER_KEY_PREVIOUS is not"},
		{name: "the same version twice",
			env:     map[string]string{cur: b64Key(2), curV: "1", prev: b64Key(1), prevV: "1"},
			wantErr: "configured twice"},
		{name: "the same key under two versions protects nothing new",
			env:     map[string]string{cur: b64Key(7), curV: "2", prev: b64Key(7), prevV: "1"},
			wantErr: "carry the same key"},
		{name: "a version that is not a positive integer",
			env: map[string]string{cur: b64Key(1), curV: "zero"}, wantErr: "must be a positive integer"},
		{name: "version zero",
			env: map[string]string{cur: b64Key(1), curV: "0"}, wantErr: "must be a positive integer"},
		{name: "a key that is not base64", env: map[string]string{cur: "not base64!"}, wantErr: "not valid base64"},
		{name: "a key of the wrong length",
			env: map[string]string{cur: base64.StdEncoding.EncodeToString([]byte("short"))}, wantErr: "32 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ring, err := SecretKeyRingFromEnv(envOf(tc.env))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantNil {
				if ring != nil {
					t.Fatalf("ring = %+v, want nil", ring)
				}
				return
			}
			if ring.Current().Version != tc.wantCur {
				t.Errorf("current version = %d, want %d", ring.Current().Version, tc.wantCur)
			}
			got := ring.Versions()
			if len(got) != len(tc.wantVers) {
				t.Fatalf("versions = %v, want %v", got, tc.wantVers)
			}
			for i := range got {
				if got[i] != tc.wantVers[i] {
					t.Errorf("versions = %v, want %v", got, tc.wantVers)
				}
			}
		})
	}
}

func ringOf(t *testing.T, cur VersionedKey, prev ...VersionedKey) *KeyRing {
	t.Helper()
	r, err := NewKeyRing(cur, prev...)
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}
	return r
}

func rawKey(fill byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = fill
	}
	return k
}

// A value sealed before a rotation opens after it, under the previous key, and
// a value sealed after is sealed under the new one -- and the two are not
// interchangeable.
func TestASecretOpensUnderTheKeyItsVersionNames(t *testing.T) {
	const tenant = "11111111-1111-1111-1111-111111111111"
	v1, v2 := VersionedKey{1, rawKey(1)}, VersionedKey{2, rawKey(2)}

	before := NewSecretStoreWithRing(nil, "postgres", ringOf(t, v1))
	after := NewSecretStoreWithRing(nil, "postgres", ringOf(t, v2, v1))

	old, err := before.seal(tenant, "sk-sealed-before")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if got, err := after.open(tenant, old, 1); err != nil || got != "sk-sealed-before" {
		t.Fatalf("a row sealed before the rotation must open under the previous key: got %q, %v", got, err)
	}

	fresh, err := after.seal(tenant, "sk-sealed-after")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if got, err := after.open(tenant, fresh, 2); err != nil || got != "sk-sealed-after" {
		t.Fatalf("a row sealed after the rotation must open under the current key: got %q, %v", got, err)
	}

	// The version is what selects the key. Labelling a v2 ciphertext as v1 must
	// fail to decrypt, not silently succeed: if it opened, the version would be
	// decoration and a mislabelled row would go unnoticed.
	if _, err := after.open(tenant, fresh, 1); err == nil {
		t.Fatal("a ciphertext sealed under version 2 opened when labelled version 1: the version does not select the key")
	}
}

// After the previous key is removed, a row still on it fails with an error that
// names the version -- not the generic "could not be decrypted", which is what
// this replaced and which told an operator nothing about which key to restore.
func TestARowOnAnUnconfiguredVersionNamesTheMissingVersion(t *testing.T) {
	const tenant = "11111111-1111-1111-1111-111111111111"
	v1, v2 := VersionedKey{1, rawKey(1)}, VersionedKey{2, rawKey(2)}
	old, _ := NewSecretStoreWithRing(nil, "postgres", ringOf(t, v1)).seal(tenant, "x")

	_, err := NewSecretStoreWithRing(nil, "postgres", ringOf(t, v2)).open(tenant, old, 1)
	var verr *SecretKeyVersionError
	if !errors.As(err, &verr) {
		t.Fatalf("err = %v (%T), want a *SecretKeyVersionError", err, err)
	}
	if verr.Version != 1 {
		t.Errorf("Version = %d, want 1", verr.Version)
	}
	if len(verr.Configured) != 1 || verr.Configured[0] != 2 {
		t.Errorf("Configured = %v, want [2]", verr.Configured)
	}
	if !strings.Contains(err.Error(), "key_version 1") {
		t.Errorf("the message must name the version; got %q", err.Error())
	}
	if strings.Contains(err.Error(), base64.StdEncoding.EncodeToString(rawKey(1))) {
		t.Error("the message contains key material")
	}
}
