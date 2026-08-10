//go:build !cgo

package main

import (
	"strings"
	"testing"
)

// TestVerifyBackendFailsWithoutCGO is the half that makes the guard meaningful.
// A --verify-backend that returned 0 unconditionally would let the Dockerfile's
// build step pass while shipping a binary with no execution fence.
//
// It also asserts the message names the cause, because the person who hits this
// is looking at a failed Docker build with no other context.
func TestVerifyBackendFailsWithoutCGO(t *testing.T) {
	var out strings.Builder
	got := runVerifyBackend(&out)
	if got == 0 {
		t.Fatalf("runVerifyBackend() = 0, want non-zero -- this binary is built "+
			"without cgo, so it has only the unfenced wazero backend. Output: %s", out.String())
	}
	if !strings.Contains(out.String(), "CGO_ENABLED=0") {
		t.Errorf("failure message does not mention CGO_ENABLED=0, which is the "+
			"likeliest cause and the whole diagnostic value of this check:\n%s", out.String())
	}
}
