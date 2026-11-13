//go:build cgo

package main

import (
	"io"
	"strings"
	"testing"
)

// TestVerifyBackendSucceedsUnderCGO pins the guard to the thing it is supposed
// to detect. Built with cgo, the wasmtime backend must be present -- if this
// fails, --verify-backend would pass a build that silently ships the unfenced
// wazero fallback, which is exactly IMPROVEMENT-PLAN.md 2.28.
//
// The !cgo half is in verify_backend_nocgo_test.go. Between them the guard is
// asserted in both directions; a check that only ever reports "OK" would be no
// check at all.
func TestVerifyBackendSucceedsUnderCGO(t *testing.T) {
	if got := runVerifyBackend(io.Discard); got != 0 {
		t.Fatalf("runVerifyBackend() = %d, want 0 -- this binary is built with cgo, "+
			"so the wasmtime backend must be constructible", got)
	}
}

// TestAWorkerWithWasmtimeIsNotRefused is the known-positive for the startup
// check (cleat#3787): built with cgo, the worker must NOT stop, and must say
// nothing. A refusal that fired on every build would pass every test that only
// asks whether it refuses.
func TestAWorkerWithWasmtimeIsNotRefused(t *testing.T) {
	var out strings.Builder
	if refuseWithoutWasmtimeBackend(&out) {
		t.Fatalf("refuseWithoutWasmtimeBackend() = true on a cgo build; the wasmtime backend is constructible.\n%s", out.String())
	}
	if out.Len() != 0 {
		t.Errorf("a worker that is not refused wrote %q", out.String())
	}
}
