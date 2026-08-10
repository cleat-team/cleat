//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

// ---------------------------------------------------------------------------
// Output pointers come from the guest, and the guest may be wrong about them.
//
// Every host function that returns a string takes a (ptr, maxLen) pair chosen
// by the guest and writes through it. WS-3's theme is what the host does when a
// guest misbehaves; this is the smaller sibling of the execution fence -- not a
// guest that will not stop, but a guest that asks the host to write somewhere
// that does not exist.
// ---------------------------------------------------------------------------

// TestWriteResult_GuestPointerOutOfRange pins the bounds check on the raw-buffer
// (wasmtime) branch of writeResult.
//
// Without it, `rawBuf[ptr:]` panics for any ptr past the end of guest memory.
// That did not crash the worker -- the recover around fn.Call in
// backend_wasmtime.go caught it -- but it surfaced as
//
//	wasmtime panic in "run": runtime error: slice bounds out of range [4294967280:12582912]
//
// which reads as an engine defect rather than a guest passing a bad pointer,
// and it leaned on a recover to handle ordinary malformed guest input.
//
// The other two writers already got this right: wazero's path returns an error
// because mem.Write reports out-of-bounds, and wasmtimeWriteString does this
// exact check. writeResult was the one raw-buffer writer that skipped it.
func TestWriteResult_GuestPointerOutOfRange(t *testing.T) {
	s := &execSession{}
	const bufLen = 1024
	ctx := contextWithRawMemBuf(context.Background(), make([]byte, bufLen))

	tests := []struct {
		name    string
		ptr     uint32
		wantErr bool
	}{
		{"well inside", 100, false},
		{"last byte that fits", bufLen - 5, false},
		// The write is 5 bytes ("hello"), so a ptr 4 bytes from the end is the
		// first that cannot fit. Checking ptr alone would let this one through.
		{"start in range but end past it", bufLen - 4, true},
		{"exactly at the end", bufLen, true},
		{"one past the end", bufLen + 1, true},
		// The value that motivated this: near-max uint32, which is what a
		// guest passing -16 as an i32 becomes.
		{"near max uint32", 0xFFFFFFF0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Any panic here is the defect itself, so name it rather than
			// letting the panic fail the test with a stack trace.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("writeResult panicked on a guest-supplied ptr (%v) -- "+
						"the bounds check is missing", r)
				}
			}()
			n, err := s.writeResult(ctx, nil, tt.ptr, "hello", 64)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ptr=%d: got n=%d, err=nil; want an error", tt.ptr, n)
				}
				return
			}
			if err != nil {
				t.Errorf("ptr=%d: unexpected error: %v", tt.ptr, err)
			}
		})
	}
}

// TestGuestBadOutputPointerIsNotAPanic is the same defect from the guest's
// side, which is the side that matters: it proves the bad pointer is reachable
// through the published ABI rather than only by calling writeResult directly.
//
// cleat_workflow_id is used because it writes unconditionally. The first
// attempt at this test used cleat_poll_cancellation and passed while proving
// nothing -- that one only writes when a workflow has actually been cancelled,
// so it returned before reaching the write at all.
func TestGuestBadOutputPointerIsNotAPanic(t *testing.T) {
	// -16 as an i32 is 0xFFFFFFF0 once the host widens it to uint32, which is
	// far outside any linear memory the guest could have grown.
	wasmBytes, err := wasmtime.Wat2Wasm(`(module
	  (import "env" "cleat_workflow_id" (func $wfid (param i32 i32) (result i64)))
	  (func (export "run") (param i32 i32 i32 i32) (result i64)
	    (drop (call $wfid (i32.const -16) (i32.const 64)))
	    (i64.const 0))
	  (memory (export "memory") 1))`)
	if err != nil {
		t.Fatalf("Wat2Wasm: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	eng := NewEngine(rt, &mockCaller{}, WithBackends([]string{"go"}, wt))

	_, _, _, _, _, execErr := eng.Execute(ctx, wasmBytes, "run", json.RawMessage(`{}`))

	// The assertion is specifically on the *shape* of the failure, not on
	// whether it failed. A bad output pointer may legitimately fail the
	// workflow; what it must not do is arrive as a Go runtime panic that
	// happened to be recovered somewhere up the stack.
	if execErr != nil {
		if strings.Contains(execErr.Error(), "slice bounds out of range") ||
			strings.Contains(execErr.Error(), "wasmtime panic") {
			t.Fatalf("a guest-supplied output pointer panicked the host: %v", execErr)
		}
		t.Logf("guest failed cleanly, which is fine: %v", execErr)
	}
}
