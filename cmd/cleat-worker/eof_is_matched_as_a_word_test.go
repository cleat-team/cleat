package main

import (
	"errors"
	"testing"
)

// cleat#1465: isConnectionError matched "EOF" as a substring on a lowercased
// message, so any error text containing "typeof" -- t-y-p-e-o-f -- classified
// as a connection error.
//
// At setup.go:2127 a match means releaseWorkflow and a re-claim. Guest errors
// reproduce deterministically, so a TypeError produced the same message every
// retry: the run never reached a terminal state and never recorded an
// error_code. Liveness, not message quality.
//
// BOTH HALVES ARE ASSERTED. The false rows are the collision. The true rows
// are the control, and they are what stops the fix being "return false for
// everything" -- which would turn every transient disconnect into a terminal
// failure, the same defect with the sign flipped and worse.
func TestEOFIsMatchedAsAWordNotASubstring(t *testing.T) {
	for _, tc := range []struct {
		msg  string
		want bool
		why  string
	}{
		// The collision, from real guest-error shapes. cleat ships an
		// AssemblyScript SDK, so `typeof` is ordinary text here.
		{"TypeError: undefined is not a function (typeof x)", false,
			"a guest TypeError is not a connection failure"},
		{"unexpected token in typeof expression", false,
			"`typeof` contains `eof` and must not match"},

		// Controls that were never at risk, so the test distinguishes rather
		// than merely matching less.
		{"RangeError: Maximum call stack size exceeded", false, "unrelated guest error"},
		{"validation failed: field 'name' is required", false, "unrelated application error"},

		// Genuine connection errors: these MUST still match.
		{"unexpected EOF", true, "the case the EOF pattern exists for"},
		{"read tcp 10.0.0.1:5432: eof", true, "lowercase standalone token still counts"},
		{"dial tcp 127.0.0.1:5432: connect: connection refused", true, "multi-word pattern"},
		{"write tcp: broken pipe", true, "multi-word pattern"},
		{"driver: bad connection", true, "multi-word pattern"},
	} {
		t.Run(tc.msg, func(t *testing.T) {
			got := isConnectionError(errors.New(tc.msg))
			if got != tc.want {
				t.Errorf("isConnectionError(%q) = %v, want %v -- %s", tc.msg, got, tc.want, tc.why)
			}
		})
	}

	if isConnectionError(nil) {
		t.Error("a nil error is not a connection error")
	}
}
