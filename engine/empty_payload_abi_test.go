package engine

import (
	"encoding/json"
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

func TestABIListStateEmptyPrefixListsEverything(t *testing.T) {
	s := newTestExecSession()
	s.stateStore = map[string]string{"a:1": "x", "b:2": "y", "c:3": "z"}

	// cleat_list_state: (prefixPtr,prefixLen, keysPtr,keysMaxLen)
	h := newTestHostFuncHarness(t, "cleat_list_state",
		[]byte{wasmI32, wasmI32, wasmI32, wasmI32}, []byte{wasmI64}, true, s)

	const keysPtr, keysMaxLen = 2048, 4096
	got, err := h.call(0, 0, keysPtr, keysMaxLen)
	if err != nil {
		t.Fatalf("call cleat_list_state: %v", err)
	}
	if got == errBadParam {
		t.Fatal("cleat_list_state refused an empty prefix; HasPrefix(k, \"\") is " +
			"true for every k, so an empty prefix means 'list everything'")
	}

	errCode, written := decodeExportResult(got)
	if errCode != 0 {
		t.Fatalf("errCode = %d, want 0", errCode)
	}
	raw, ok := h.mem.Read(keysPtr, written)
	if !ok {
		t.Fatal("could not read the keys buffer back")
	}
	var keys []string
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("keys buffer is not JSON (%q): %v", raw, err)
	}
	if len(keys) != 3 {
		t.Errorf("empty prefix listed %d keys (%v), want all 3", len(keys), keys)
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
