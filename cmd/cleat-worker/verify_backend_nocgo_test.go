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

// TestAWorkerWithoutWasmtimeRefusesAndSaysWhatToDo is the startup half of the
// check (cleat#3787). The person who reaches it ran `go install` on a machine
// with no C compiler -- Go turned cgo off by itself and the install exited 0 --
// so the message has to say what happened and what to run, not only name a
// variable they never set.
func TestAWorkerWithoutWasmtimeRefusesAndSaysWhatToDo(t *testing.T) {
	var out strings.Builder
	if !refuseWithoutWasmtimeBackend(&out) {
		t.Fatalf("refuseWithoutWasmtimeBackend() = false on a binary built without cgo; it must stop the worker.\n%s", out.String())
	}
	for _, want := range []string{
		"refusing to start",
		"no wasmtime backend",
		"built without cgo",
		"no C compiler",
		"build-essential",
		"gcc alone is not enough",
		"GOTOOLCHAIN=go1.27.3",
		"releases",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the refusal does not contain %q:\n%s", want, out.String())
		}
	}
}
