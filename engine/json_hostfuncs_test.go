package engine

import (
	"testing"
)

// cleat_json_parse and cleat_json_stringify validate and canonicalise a JSON
// string using the host's encoding/json. The guest decodes the result as
// (written, errCode) and treats written == 0 as failure -- see
// crates/cleat-sdk/src/host_calls.rs json_parse -- so a test that only checks
// the returned error code has not checked anything the caller relies on.
//
// TestClosure_JsonParse and TestClosure_JsonStringify did exactly that, and
// were vacuous: newClosureSetup installs mockHostHandler, whose JsonParse
// returns a canned 0 without touching memory, so `got != 0` could never fire.
// They passed while the real handler panicked on the wasmtime backend. The
// tests here drive the real execSession and assert on the bytes it wrote.
//
// See IMPROVEMENT-PLAN.md 2.14.

const jsonParseIn = 64   // where the input JSON is written
const jsonParseOut = 512 // where the host should write the normalised JSON

func TestWazeroJsonParseNormalises(t *testing.T) {
	h := newTestHostFuncHarness(t, "cleat_json_parse",
		[]byte{wasmI32, wasmI32, wasmI32, wasmI32}, []byte{wasmI64}, true, &execSession{})

	// Deliberately not canonical: keys out of order, with whitespace.
	input := `{"b":2, "a":  1}`
	if !h.mem.Write(jsonParseIn, []byte(input)) {
		t.Fatal("write to memory failed")
	}

	got, err := h.call(jsonParseIn, uint64(len(input)), jsonParseOut, 512)
	if err != nil {
		t.Fatalf("call cleat_json_parse: %v", err)
	}

	errCode, written := decodeExportResult(got)
	if errCode != 0 {
		t.Fatalf("errCode = %d, want 0", errCode)
	}
	data, readOK := h.mem.Read(jsonParseOut, written)
	assertJSONWritten(t, data, readOK)
}

func TestWazeroJsonStringifyNormalises(t *testing.T) {
	h := newTestHostFuncHarness(t, "cleat_json_stringify",
		[]byte{wasmI32, wasmI32, wasmI32, wasmI32}, []byte{wasmI64}, true, &execSession{})

	input := `{"b":2, "a":  1}`
	if !h.mem.Write(jsonParseIn, []byte(input)) {
		t.Fatal("write to memory failed")
	}

	got, err := h.call(jsonParseIn, uint64(len(input)), jsonParseOut, 512)
	if err != nil {
		t.Fatalf("call cleat_json_stringify: %v", err)
	}

	errCode, written := decodeExportResult(got)
	if errCode != 0 {
		t.Fatalf("errCode = %d, want 0", errCode)
	}
	data, readOK := h.mem.Read(jsonParseOut, written)
	assertJSONWritten(t, data, readOK)
}

// assertJSONWritten checks the host actually wrote canonicalised JSON, which
// is the whole point of these two host functions.
func assertJSONWritten(t *testing.T, data []byte, ok bool) {
	t.Helper()
	if !ok {
		t.Fatal("could not read the output region back")
	}
	if got, want := string(data), `{"a":1,"b":2}`; got != want {
		t.Errorf("host wrote %q, want %q (keys sorted, whitespace removed)", got, want)
	}
}
