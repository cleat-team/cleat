package redacted

import (
	"strings"
	"testing"
)

// The expected texts are written out literally, and their sizes and hashes were computed outside Go (Python's hashlib, sha256 of the UTF-8 bytes, first 8
// bytes in hex). They are the SAME literals engine/a_divergence_names_its_bodies_and_never_prints_them_test.go asserts of its own copy: that is what holds
// the two copies equal (see the package comment).
func TestBodyIsItsSizeAndAShortHashAndNothingElse(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"empty", "", "[redacted: 0 bytes, sha256=e3b0c44298fc1c14]"},
		{"abc", "abc", "[redacted: 3 bytes, sha256=ba7816bf8f01cfea]"},
		{"a body past the old 4096-byte cut is named by its WHOLE size", strings.Repeat("x", 5000), "[redacted: 5000 bytes, sha256=c59d3c0480cc2d71]"},
		{"a JSON object", `{"a":1}`, "[redacted: 7 bytes, sha256=015abd7f5cc57a2d]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Body(tc.in); got != tc.want {
				t.Errorf("Body(%.40q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	// equal bodies read the same and different ones differently, which is what a message that names two bodies is read for
	if Body("same") != Body(strings.Clone("same")) || Body("same") == Body("other") {
		t.Error("two bodies must read the same when equal and differently when not")
	}
}
