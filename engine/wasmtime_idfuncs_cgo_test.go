//go:build cgo

// Host-side tests for the wasmtime ID functions, written to catch §2.18: a
// wrapper that fetches the guest memory buffer and then drops it on the floor.
//
// Under wasmtime the api.Module passed to a handler is always nil, so
// writeResult can only reach guest memory through the raw buffer that
// ctxWithMem puts in the context. A wrapper that calls the handler with a
// bare context.Background() therefore writes zero bytes, reports errCode 0,
// and looks like a success from every angle except the one that matters.
//
// §2.14 fixed exactly this in cleat_json_parse and cleat_json_stringify. The
// wrappers here had the same defect and nobody checked them, because the
// pre-existing wasmtime closure tests install mockHostHandler and assert only
// that the result is non-zero -- an assertion no implementation defect can
// fail (§2.16). These tests install the *real* execSession and assert on the
// bytes that landed in guest memory.

package engine

import (
	"strings"
	"testing"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

// Scratch offsets in the closure module's memory.
const (
	idFuncOut  = 256
	idFuncSeed = 1024
)

func idClosure(t *testing.T, name string, params []byte,
	register func(*wasmtimeBackend, *wasmtime.Linker) error, sess *execSession) *closureSetup {
	t.Helper()
	ft := wasmFunctype(params, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{name, ft}}, register)
	// The real session, not mockHostHandler. This is the whole point.
	s.backend.handler = sess
	return s
}

// assertWrote checks that a host function actually deposited the expected
// string in guest memory, rather than merely returning a plausible int64.
func assertWrote(t *testing.T, s *closureSetup, got int64, want string, at int) {
	t.Helper()
	errCode, written := decodeExportResult(uint64(got))
	if errCode != 0 {
		t.Fatalf("errCode = %d, want 0", errCode)
	}
	if written == 0 {
		t.Fatalf("host function reported success but wrote 0 bytes -- the wrapper is "+
			"not passing the guest memory buffer via ctxWithMem, so writeResult had "+
			"nowhere to write (raw result 0x%016x)", uint64(got))
	}
	if int(written) != len(want) {
		t.Fatalf("wrote %d bytes, want %d (%q)", written, len(want), want)
	}
	if got := string(s.data[at : at+int(written)]); got != want {
		t.Errorf("guest memory = %q, want %q", got, want)
	}
}

func TestWasmtimeWorkflowIDWritesGuestMemory(t *testing.T) {
	const wfID = "wf-0123456789abcdef"
	s := idClosure(t, "cleat_workflow_id", []byte{wasmValI32, wasmValI32},
		func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatWorkflowID(l) },
		&execSession{workflowID: wfID})

	got := s.call(t, "test_cleat_workflow_id", i32(idFuncOut), i32(512))
	assertWrote(t, s, got, wfID, idFuncOut)
}

func TestWasmtimeRunIDWritesGuestMemory(t *testing.T) {
	const runID = "run-fedcba9876543210"
	s := idClosure(t, "cleat_run_id", []byte{wasmValI32, wasmValI32},
		func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatRunID(l) },
		&execSession{execRunID: runID})

	got := s.call(t, "test_cleat_run_id", i32(idFuncOut), i32(512))
	assertWrote(t, s, got, runID, idFuncOut)
}

func TestWasmtimeUUIDWritesGuestMemory(t *testing.T) {
	const seed = "seed-1"
	s := idClosure(t, "cleat_uuid",
		[]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32},
		func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatUUID(l) },
		&execSession{workflowID: "wf-uuid-test"})
	s.writeString(idFuncSeed, seed)

	got := s.call(t, "test_cleat_uuid",
		i32(idFuncSeed), i32(int32(len(seed))), i32(idFuncOut), i32(512))

	errCode, written := decodeExportResult(uint64(got))
	if errCode != 0 {
		t.Fatalf("errCode = %d, want 0", errCode)
	}
	if written == 0 {
		t.Fatalf("cleat_uuid reported success but wrote 0 bytes (raw result 0x%016x)", uint64(got))
	}
	// A UUIDv5-shaped string: 36 chars, 8-4-4-4-12.
	uuid := string(s.data[idFuncOut : idFuncOut+int(written)])
	if len(uuid) != 36 {
		t.Fatalf("uuid = %q (len %d), want length 36", uuid, len(uuid))
	}
	parts := strings.Split(uuid, "-")
	wantLens := []int{8, 4, 4, 4, 12}
	if len(parts) != len(wantLens) {
		t.Fatalf("uuid = %q, want 5 hyphen-separated groups", uuid)
	}
	for i, p := range parts {
		if len(p) != wantLens[i] {
			t.Errorf("uuid group %d = %q (len %d), want len %d", i, p, len(p), wantLens[i])
		}
	}
	if parts[2][0] != '5' {
		t.Errorf("uuid = %q, want version 5 (third group starting '5')", uuid)
	}
}

// TestWasmtimeIDResultLayout pins the ABI contract that §2.19 got wrong on the
// guest side: packSimpleResult puts the byte count in the HIGH 32 bits and the
// error code in the low 32. wasm/adapter_metadata.go generated
// `idLen := uint32(result)` for cleat_workflow_id and cleat_run_id, which reads
// the error code instead -- always 0 on success, so WorkflowID() returned "".
//
// That guest-side bug and the host-side one above mask each other: fix either
// alone and WorkflowID() still comes back empty. This test fails if anyone
// flips the layout, which is what would silently un-fix the generated adapter.
func TestWasmtimeIDResultLayout(t *testing.T) {
	const wfID = "wf-layout-check"
	s := idClosure(t, "cleat_workflow_id", []byte{wasmValI32, wasmValI32},
		func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatWorkflowID(l) },
		&execSession{workflowID: wfID})

	got := s.call(t, "test_cleat_workflow_id", i32(idFuncOut), i32(512))
	raw := uint64(got)

	if lo := uint32(raw); lo != 0 {
		t.Errorf("low 32 bits = %d, want 0 (the error code on success)", lo)
	}
	if hi := uint32(raw >> 32); hi != uint32(len(wfID)) {
		t.Errorf("high 32 bits = %d, want %d (the byte count)", hi, len(wfID))
	}
}
