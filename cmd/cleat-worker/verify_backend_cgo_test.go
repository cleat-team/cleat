//go:build cgo

package main

import (
	"io"
	"runtime"
	"strings"
	"testing"
	"time"
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

// TestTheStartupBackendCheckDoesNotLeakItsBackend pins the review finding on
// cleat#3788: constructing the wasmtime backend starts an epoch-ticker
// goroutine that only Close(ctx) stops. The check used to "close" through a type
// assertion nothing satisfies, so each call leaked one -- unnoticeable in
// --verify-backend, which exits, and a real leak once the check ran at every
// worker start.
//
// One call first, so anything the runtime starts lazily on first use is already
// counted; then the goroutine count over several calls must not grow. Measured
// with the broken assertion: 2 -> 7 over five calls; with Close(ctx): 2 -> 2.
func TestTheStartupBackendCheckDoesNotLeakItsBackend(t *testing.T) {
	if err := wasmtimeBackendError(); err != nil {
		t.Fatalf("setup: %v", err)
	}
	before := runtime.NumGoroutine()
	for i := 0; i < 5; i++ {
		if err := wasmtimeBackendError(); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	// Close joins the ticker goroutine, so it is already gone; the short wait is
	// only for the scheduler's own bookkeeping.
	var after int
	for i := 0; i < 50; i++ {
		after = runtime.NumGoroutine()
		if after <= before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutines went from %d to %d over five wasmtimeBackendError() calls: the backend it "+
		"constructs is not being closed", before, after)
}
