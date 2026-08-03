package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tetratelabs/wazero/api"
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

// TestABIChildWorkflowInSchemaAcceptsEmptySchemaAndPolicy covers the third
// unreachable behaviour, and the one place the two backends actively disagreed:
// wazero guarded the policy parameter with an inline `policyLen > 0` check and
// wasmtime read it unconditionally, so the same guest call succeeded on one
// backend and was refused on the other.
func TestABIChildWorkflowInSchemaAcceptsEmptySchemaAndPolicy(t *testing.T) {
	rec := &childInSchemaRecorder{}
	// cleat_child_workflow_in_schema:
	//   (schemaPtr,schemaLen, namePtr,nameLen, inputPtr,inputLen,
	//    version i64, priority i64, policyPtr,policyLen, runIDPtr,runIDMaxLen)
	h := newTestHostFuncHarness(t, "cleat_child_workflow_in_schema",
		[]byte{wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32,
			wasmI64, wasmI64, wasmI32, wasmI32, wasmI32, wasmI32},
		[]byte{wasmI64}, true, rec)

	if !h.mem.Write(64, []byte("child")) || !h.mem.Write(128, []byte("{}")) {
		t.Fatal("write to memory failed")
	}

	// schemaLen = 0 and policyLen = 0: local schema, default policy.
	got, err := h.call(0, 0, 64, 5, 128, 2, 1, 0, 0, 0, 2048, 256)
	if err != nil {
		t.Fatalf("call cleat_child_workflow_in_schema: %v", err)
	}
	if got == errBadParam {
		t.Fatal("refused an empty targetSchema/policy; children.go documents " +
			"an empty targetSchema as the local-schema fallback")
	}
	if !rec.called {
		t.Fatal("did not reach the handler")
	}
	if rec.targetSchema != "" || rec.policy != "" {
		t.Errorf("handler got schema=%q policy=%q, want both empty",
			rec.targetSchema, rec.policy)
	}
	if rec.name != "child" {
		t.Errorf("handler got name=%q, want \"child\"", rec.name)
	}
}

type childInSchemaRecorder struct {
	stubHostHandler
	called       bool
	targetSchema string
	name         string
	policy       string
}

func (h *childInSchemaRecorder) ChildWorkflowInSchema(_ context.Context, _ api.Module,
	targetSchema, name, inputJSON string, _ int64, _ int64, parentClosePolicy string,
	_, _ uint32) int64 {
	h.called = true
	h.targetSchema, h.name, h.policy = targetSchema, name, parentClosePolicy
	return 0
}
