//go:build cgo

package engine

import (
	"context"
	"sync"
	"testing"

	"github.com/bytecodealliance/wasmtime-go/v44"
	"github.com/tetratelabs/wazero/api"
)

// The gap this closes.
//
// #515 fixed cleat_create_promise's arity -- the wasmtime host registered a
// fifth parameter nothing passed, so no guest could link against it -- and
// added a link-level regression test. Linking is only the first of the things
// that have to be right.
//
// TestClosure_CreatePromise, which already drives a guest through the wasmtime
// backend, cannot see the rest: newClosureSetup installs
// mockHostHandler{ret: 0}, whose CreatePromise records the *name* and returns
// h.ret. So the existing assertion is `got == 0`, and the real handler never
// returns 0 on success -- engine/promises.go:22 returns
// packSimpleResult(0, written), which is written<<32. Nothing checks that the
// promise ID reaches the guest's buffer, and nothing checks which pointer the
// wrapper passed.
//
// Those are exactly the failures an arity fix does not cover, and exactly the
// class CLAUDE.md names: "Four real defects have come out of the ABI layer's
// integer-conversion sites and none of them was an overflow -- in every case
// the value meant the wrong thing on one side of the boundary."
//
// The subject here is the wasmtime host-function wrapper in
// engine/wasmtime_hostfuncs_plugins.go, not the handler. The handler is the
// collaborator, so it is a fake -- but a fake that behaves the way
// engine/promises.go does, writing through the context memory buffer and
// packing its result the same way, so that a wrapper which mixes up
// promiseIDPtr and promiseIDMaxLen, or drops the return value, fails here.

// promiseABIHandler behaves like the real CreatePromise for the parts that
// cross the ABI boundary, and records what the wrapper handed it.
type promiseABIHandler struct {
	*mockHostHandler

	promiseID string

	mu        sync.Mutex
	gotName   string
	gotPtr    uint32
	gotMaxLen uint32
	sawCall   bool
	hadBuf    bool
}

func (h *promiseABIHandler) CreatePromise(ctx context.Context, _ api.Module, name string, promiseIDPtr, promiseIDMaxLen uint32) int64 {
	h.mu.Lock()
	h.gotName, h.gotPtr, h.gotMaxLen, h.sawCall = name, promiseIDPtr, promiseIDMaxLen, true
	h.mu.Unlock()

	// The same route engine/flush.go's writeResult takes: the wasmtime wrapper
	// puts the guest's linear memory on the context, and the handler writes
	// through it. If the wrapper stopped doing that, the guest would silently
	// receive nothing back.
	buf, ok := ctx.Value(wasmMemBufKey{}).([]byte)
	if !ok || buf == nil {
		return packSimpleResult(1)
	}
	h.mu.Lock()
	h.hadBuf = true
	h.mu.Unlock()

	data := []byte(h.promiseID)
	if uint32(len(data)) > promiseIDMaxLen {
		data = data[:promiseIDMaxLen]
	}
	copy(buf[promiseIDPtr:], data)
	return packSimpleResult(0, uint32(len(data)))
}

// TestCreatePromiseABICrossesTheBoundaryIntact checks what survives the
// wasmtime host-function wrapper: the name in, both output-buffer arguments in
// the right order, the promise ID back into guest memory, and the packed
// return.
func TestCreatePromiseABICrossesTheBoundaryIntact(t *testing.T) {
	const (
		namePtr    = 100
		promiseNm  = "my-promise"
		outPtr     = 200
		outMaxLen  = 64
		generatedI = "promise-abc-123"
	)

	// The documented signature: ABI.md 2.34, (param i32 i32 i32 i32) (result i64).
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_create_promise", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatCreatePromise(l)
	})

	h := &promiseABIHandler{mockHostHandler: s.mock, promiseID: generatedI}
	s.backend.handler = h

	s.writeString(namePtr, promiseNm)
	got := s.call(t, "test_cleat_create_promise", i32(namePtr), i32(int32(len(promiseNm))), i32(outPtr), i32(outMaxLen))

	h.mu.Lock()
	sawCall, gotName, gotPtr, gotMaxLen, hadBuf := h.sawCall, h.gotName, h.gotPtr, h.gotMaxLen, h.hadBuf
	h.mu.Unlock()

	if !sawCall {
		t.Fatal("the host handler's CreatePromise was never called.\n\n" +
			"Everything below asserts on what the wrapper passed, so without this " +
			"they would all compare zero values against zero values and pass.")
	}
	if !hadBuf {
		t.Error("the wrapper did not put the guest's linear memory on the context.\n\n" +
			"engine/flush.go's writeResult writes through ctx.Value(wasmMemBufKey{}); " +
			"without it a guest receives nothing back from any output-buffer call.")
	}

	// Argument identity, not just arity. Two i32 pointers next to each other is
	// where a swap hides: both link, both are in range, and the guest reads a
	// buffer the host never wrote.
	if gotName != promiseNm {
		t.Errorf("handler received name %q, want %q -- the wrapper read the name from "+
			"the wrong pointer or length", gotName, promiseNm)
	}
	if gotPtr != outPtr {
		t.Errorf("handler received promiseIDPtr = %d, want %d", gotPtr, outPtr)
	}
	if gotMaxLen != outMaxLen {
		t.Errorf("handler received promiseIDMaxLen = %d, want %d.\n\n"+
			"If this is %d, the wrapper passed the pointer and the max length in the "+
			"wrong order -- which links fine and corrupts guest memory.",
			gotMaxLen, outMaxLen, outPtr)
	}

	// The return, unpacked the way a guest unpacks it: bits 0-31 errCode,
	// bits 32-63 bytes written (engine/memory.go:280).
	wantRet := packSimpleResult(0, uint32(len(generatedI)))
	if got != wantRet {
		t.Errorf("call returned %#x, want %#x (errCode=0, written=%d).\n\n"+
			"errCode=%d written=%d was returned instead. A wrapper that drops or "+
			"truncates the handler's int64 leaves the guest unable to tell how many "+
			"bytes its buffer holds.",
			got, wantRet, len(generatedI), uint32(got), uint32(uint64(got)>>32))
	}

	// And the bytes really are in the guest's memory, which is the only part a
	// workflow author observes.
	if inMem := string(s.data[outPtr : outPtr+len(generatedI)]); inMem != generatedI {
		t.Errorf("guest memory at %d holds %q, want %q -- the promise ID never reached "+
			"the buffer the guest passed", outPtr, inMem, generatedI)
	}
}

// TestCreatePromiseABIHonoursTheGuestsBufferLimit covers the truncation path.
//
// promiseIDMaxLen is the guest's statement of how much room it has. A wrapper
// that passed a wrong or stale value here would let the host write past the
// buffer -- which is a guest-visible memory corruption, not an error return,
// and so would not show up as a failure anywhere else.
func TestCreatePromiseABIHonoursTheGuestsBufferLimit(t *testing.T) {
	const (
		namePtr   = 100
		promiseNm = "my-promise"
		outPtr    = 300
		outMaxLen = 4 // shorter than the ID below
		generated = "promise-abc-123"
		sentinel  = byte(0xAA)
	)

	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_create_promise", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatCreatePromise(l)
	})

	h := &promiseABIHandler{mockHostHandler: s.mock, promiseID: generated}
	s.backend.handler = h

	// Paint past the buffer so an overrun is visible rather than inferred.
	for i := outPtr; i < outPtr+32; i++ {
		s.data[i] = sentinel
	}

	s.writeString(namePtr, promiseNm)
	got := s.call(t, "test_cleat_create_promise", i32(namePtr), i32(int32(len(promiseNm))), i32(outPtr), i32(outMaxLen))

	if written := uint32(uint64(got) >> 32); written != outMaxLen {
		t.Errorf("call reported %d bytes written, want %d (the guest's stated capacity)", written, outMaxLen)
	}
	if inMem := string(s.data[outPtr : outPtr+outMaxLen]); inMem != generated[:outMaxLen] {
		t.Errorf("guest memory at %d holds %q, want %q", outPtr, inMem, generated[:outMaxLen])
	}
	for i := outPtr + outMaxLen; i < outPtr+32; i++ {
		if s.data[i] != sentinel {
			t.Fatalf("byte %d past the guest's %d-byte buffer was overwritten (%#x, want %#x) -- "+
				"the host wrote beyond the capacity the guest declared",
				i-outPtr, outMaxLen, s.data[i], sentinel)
		}
	}
}
