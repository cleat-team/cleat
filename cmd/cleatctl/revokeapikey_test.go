package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// The selector is the part worth testing without a database: it decides WHICH
// key gets revoked, and getting it wrong during an incident means revoking a
// working integration's credential instead of the leaked one.

func TestRevokeSelector_RequiresExactlyOne(t *testing.T) {
	tests := []struct {
		name       string
		keyID      string
		keyHash    string
		keyStdin   bool
		wantErrSub string
	}{
		{"none", "", "", false, "is required"},
		{"id and hash", "3f2a", "abcd", false, "mutually exclusive"},
		{"id and stdin", "3f2a", "", true, "mutually exclusive"},
		{"hash and stdin", "", "abcd", true, "mutually exclusive"},
		{"all three", "3f2a", "abcd", true, "mutually exclusive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := revokeSelector(tt.keyID, tt.keyHash, tt.keyStdin, strings.NewReader(""))
			if err == nil {
				t.Fatalf("expected an error, got none")
			}
			if !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErrSub)
			}
		})
	}
}

// The hash must be over the EXACT key bytes, including the cleat_sk_ prefix --
// auth/middleware.go's sha256Hash hashes the whole Authorization value, so a
// selector that stripped the prefix would compute a hash matching no row and
// silently report "no such key" during an incident.
func TestRevokeSelector_KeyStdinHashesFullKeyIncludingPrefix(t *testing.T) {
	const key = "cleat_sk_3740d7db465bf787d8589a1f023e03c64ba9f658821cee3f7506948869da07ba"
	want := sha256.Sum256([]byte(key))

	sel, err := revokeSelector("", "", true, strings.NewReader(key))
	if err != nil {
		t.Fatalf("revokeSelector: %v", err)
	}
	if got := hex.EncodeToString(sel.keyHash); got != hex.EncodeToString(want[:]) {
		t.Errorf("hash = %s, want %s (must be sha256 of the full key, prefix included)", got, hex.EncodeToString(want[:]))
	}
	if sel.keyID.String() != "00000000-0000-0000-0000-000000000000" {
		t.Errorf("keyID should be unset when selecting by hash, got %s", sel.keyID)
	}
}

// `echo` appends a newline and `printf` does not. An operator should not have
// to know which they used, so trailing newlines are trimmed -- but nothing
// else is, because trimming interior bytes would produce a hash matching
// nothing with no indication why.
func TestRevokeSelector_KeyStdinTrimsOnlyTrailingNewlines(t *testing.T) {
	const key = "cleat_sk_deadbeef"
	want := sha256.Sum256([]byte(key))

	for _, in := range []string{key, key + "\n", key + "\r\n", key + "\n\n"} {
		sel, err := revokeSelector("", "", true, strings.NewReader(in))
		if err != nil {
			t.Fatalf("revokeSelector(%q): %v", in, err)
		}
		if hex.EncodeToString(sel.keyHash) != hex.EncodeToString(want[:]) {
			t.Errorf("input %q produced a different hash than the bare key", in)
		}
	}

	// Leading whitespace is NOT trimmed: it would change the key, and silently
	// accepting it would hide an operator's copy-paste error.
	sel, err := revokeSelector("", "", true, strings.NewReader(" "+key))
	if err != nil {
		t.Fatalf("revokeSelector with leading space: %v", err)
	}
	if hex.EncodeToString(sel.keyHash) == hex.EncodeToString(want[:]) {
		t.Error("leading whitespace was trimmed; it must not be, or a mistyped key silently matches the right row")
	}
}

func TestRevokeSelector_KeyStdinRejectsEmpty(t *testing.T) {
	for _, in := range []string{"", "\n", "\r\n"} {
		if _, err := revokeSelector("", "", true, strings.NewReader(in)); err == nil {
			t.Errorf("input %q: expected an error for an empty key, got none", in)
		}
	}
}

func TestRevokeSelector_KeyHashMustBeSHA256Sized(t *testing.T) {
	full := strings.Repeat("ab", sha256.Size) // 32 bytes hex
	if _, err := revokeSelector("", full, false, nil); err != nil {
		t.Fatalf("valid sha256 hex rejected: %v", err)
	}

	for _, bad := range []string{"abcd", strings.Repeat("ab", sha256.Size+1), "nothex!!"} {
		if _, err := revokeSelector("", bad, false, nil); err == nil {
			t.Errorf("--key-hash %q: expected an error, got none", bad)
		}
	}
}

func TestRevokeSelector_KeyIDMustBeUUID(t *testing.T) {
	if _, err := revokeSelector("not-a-uuid", "", false, nil); err == nil {
		t.Fatal("expected an error for a non-uuid --key-id, got none")
	}
	sel, err := revokeSelector("3f2a7c1e-0000-4000-8000-000000000001", "", false, nil)
	if err != nil {
		t.Fatalf("valid uuid rejected: %v", err)
	}
	if sel.keyHash != nil {
		t.Error("keyHash should be unset when selecting by key-id")
	}
}
