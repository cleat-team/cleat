//go:build cgo

// The wasmtime half of the JSON host-function tests. Split out because
// newClosureSetup and wasmtimeBackend live behind //go:build cgo. See
// json_hostfuncs_test.go for why the pre-existing tests did not catch this.

package engine

import (
	"testing"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

// jsonClosure builds a single-import module for one of the two JSON host
// functions and installs the *real* execSession as the handler. The handler
// choice is the entire point: with mockHostHandler these tests pass no matter
// what the implementation does.
func jsonClosure(t *testing.T, name string,
	register func(*wasmtimeBackend, *wasmtime.Linker) error) *closureSetup {
	t.Helper()
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{name, ft}}, register)
	s.backend.handler = &execSession{}
	return s
}

func TestWasmtimeJsonParseNormalises(t *testing.T) {
	s := jsonClosure(t, "cleat_json_parse",
		func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatJsonParse(l) })

	input := `{"b":2, "a":  1}`
	s.writeString(jsonParseIn, input)

	got := s.call(t, "test_cleat_json_parse",
		i32(jsonParseIn), i32(int32(len(input))), i32(jsonParseOut), i32(512))

	errCode, written := decodeExportResult(uint64(got))
	if errCode != 0 {
		t.Fatalf("errCode = %d, want 0", errCode)
	}
	assertJSONWritten(t, s.data[jsonParseOut:jsonParseOut+int(written)], true)
}

func TestWasmtimeJsonStringifyNormalises(t *testing.T) {
	s := jsonClosure(t, "cleat_json_stringify",
		func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatJsonStringify(l) })

	input := `{"b":2, "a":  1}`
	s.writeString(jsonParseIn, input)

	got := s.call(t, "test_cleat_json_stringify",
		i32(jsonParseIn), i32(int32(len(input))), i32(jsonParseOut), i32(512))

	errCode, written := decodeExportResult(uint64(got))
	if errCode != 0 {
		t.Fatalf("errCode = %d, want 0", errCode)
	}
	assertJSONWritten(t, s.data[jsonParseOut:jsonParseOut+int(written)], true)
}
