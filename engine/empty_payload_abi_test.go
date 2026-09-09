package engine

import (
	"testing"
)

// Three host functions document behaviour selected by passing an empty name or
// prefix, and all three were unreachable from a guest: readServiceName and
// readWasmStringValidated refuse a zero length, and every caller turns that
// into errBadParam. The handlers implemented the behaviour correctly and were
// tested -- TestSetScopeEmptyClears in host_dispatch_test.go has asserted the
// clear-scope path for a long time, calling execSession.SetScope directly.
// Nothing could ask for it through the ABI.
//
// That gap is the point. A handler test plus a wrapper test that substitutes a
// mock handler leaves the seam between them uncovered, and the seam is where
// this lived (IMPROVEMENT-PLAN.md 2.16). So these tests deliberately drive the
// *registered host function* with a real execSession behind it, which is the
// path a guest actually takes.
//
// See IMPROVEMENT-PLAN.md 2.13.

func TestABISetScopeEmptyPairClearsScope(t *testing.T) {
	s := newTestExecSession()
	s.scopeSet = true
	s.scopePrefix = "vo:cart:c1:"
	s.scopeObjType = "cart"
	s.scopeInstKey = "c1"

	// cleat_set_scope: (objTypePtr,objTypeLen, instKeyPtr,instKeyLen, prevPtr,prevMaxLen)
	h := newTestHostFuncHarness(t, "cleat_set_scope",
		[]byte{wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32}, []byte{wasmI64}, true, s)

	got, err := h.call(0, 0, 0, 0, 2048, 256)
	if err != nil {
		t.Fatalf("call cleat_set_scope: %v", err)
	}
	if got == errBadParam {
		t.Fatal("cleat_set_scope refused the empty pair; clearing the scope is " +
			"documented behaviour (scope.go freshSetScope) and must be reachable")
	}
	if s.scopeSet || s.scopePrefix != "" || s.scopeObjType != "" || s.scopeInstKey != "" {
		t.Errorf("scope not cleared: set=%v prefix=%q objType=%q instKey=%q",
			s.scopeSet, s.scopePrefix, s.scopeObjType, s.scopeInstKey)
	}
}

// ---- Reader-level rules ----

func TestReadOptionalServiceNameAllowsEmptyOnly(t *testing.T) {
	mem := newTestMemory(t, []byte("cart\x00bad name"))

	if s, ok := readOptionalServiceName(mem, 0, 0); !ok || s != "" {
		t.Errorf("readOptionalServiceName(len=0) = (%q, %v), want (\"\", true)", s, ok)
	}
	if s, ok := readOptionalServiceName(mem, 0, 4); !ok || s != "cart" {
		t.Errorf("readOptionalServiceName(\"cart\") = (%q, %v), want (\"cart\", true)", s, ok)
	}
	// Relaxing emptiness must not relax the character set.
	if _, ok := readOptionalServiceName(mem, 5, 8); ok {
		t.Error("readOptionalServiceName accepted a name with a space in it")
	}
}

// TestABISetScopeReportsPreviousScopeLength pins the half of cleat_set_scope's
// contract that had no test: the guest must be able to READ the previous scope,
// not merely have it written somewhere.
//
// freshSetScope writes prevScope into the guest buffer and discards the length
// (`_, _ = s.writeResult(...)`), then returns 0 on every success path. The
// bytes land in guest memory and nothing reports how many. Every SDK that binds
// this call decodes the length out of the high 32 bits -- Rust's clear_scope
// does `let (prev_len, _err) = memory::decode_simple_result(result)` and
// returns String::new() whenever prev_len is 0, which is always.
//
// So the call succeeds, the memory is correct, and the documented return value
// is unreachable. TestABISetScopeEmptyPairClearsScope already passed a real
// 256-byte buffer at 2048 with a scope set, and asserted nothing about either
// the length or the contents -- the write was exercised and never read back.
func TestABISetScopeReportsPreviousScopeLength(t *testing.T) {
	s := newTestExecSession()
	s.scopeSet = true
	s.scopePrefix = "vo:cart:c1:"
	s.scopeObjType = "cart"
	s.scopeInstKey = "c1"

	h := newTestHostFuncHarness(t, "cleat_set_scope",
		[]byte{wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32}, []byte{wasmI64}, true, s)

	const objTypePtr, instKeyPtr, prevPtr, prevMax = 1024, 1100, 2048, 256
	if !h.mem.Write(objTypePtr, []byte("order")) || !h.mem.Write(instKeyPtr, []byte("o1")) {
		t.Fatal("could not stage the input strings in guest memory")
	}

	got, err := h.call(objTypePtr, 5, instKeyPtr, 2, prevPtr, prevMax)
	if err != nil {
		t.Fatalf("call cleat_set_scope: %v", err)
	}
	if errCode := uint32(got & 0xFFFF); errCode != 0 {
		t.Fatalf("cleat_set_scope reported error code %d; the replacement should succeed", errCode)
	}

	want := "vo:cart:c1:"

	// The bytes are there -- this half already worked.
	buf, ok := h.mem.Read(prevPtr, uint32(len(want)))
	if !ok {
		t.Fatal("could not read the previous-scope buffer")
	}
	if string(buf) != want {
		t.Errorf("previous scope bytes = %q, want %q", string(buf), want)
	}

	// This is the half that did not: the guest has no way to learn how many.
	gotLen := uint32(uint64(got) >> 32)
	if gotLen != uint32(len(want)) {
		t.Errorf("cleat_set_scope returned previous-scope length %d, want %d "+
			"(raw result %#x). The host wrote %q into the buffer and reported no "+
			"length, so every SDK that decodes it -- rust, java, assemblyscript -- "+
			"reads an empty previous scope no matter what was there.",
			gotLen, len(want), uint64(got), want)
	}
}
