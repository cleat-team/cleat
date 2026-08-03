package engine

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero/api"

	"github.com/cleat-team/cleat/cleat"
)

// The tests in this file pin the ABI contract for the five host functions whose
// guest adapter decodes the packDurableCallResult layout: cleat_call,
// cleat_call_retry, cleat_call_heartbeat, plugin_call and
// plugin_call_streaming.
//
// Two separate defects lived here, and both were invisible because a durable
// call that was *refused* looked, from the guest, like a durable call that had
// *timed out*. See IMPROVEMENT-PLAN.md 2.10.

// decodeDurableCallResult decodes a packed result exactly as the generated
// guest adapter does. The three expressions are copied verbatim from
// wasm/adapter_metadata.go's DurableCall ResultStmts -- if the codegen changes
// its unpacking, these tests must be updated in lockstep, and the copy is here
// so that divergence shows up as a test edit rather than as silence.
func decodeDurableCallResult(result int64) (responseLen uint32, callErrorCode cleat.CallErrorCode, errCode uint32) {
	responseLen = uint32(uint64(result) >> 40)
	callErrorCode = cleat.CallErrorCode((uint64(result) >> 8) & 0xFFFFFFFF)
	errCode = uint32(result & 0xFF)
	return
}

// TestBadParamDurableCallIsDecodableByGuest is the regression test for the
// headline defect: a parameter-validation refusal used to be returned as the
// raw errBadParam sentinel, which this layout cannot carry.
func TestBadParamDurableCallIsDecodableByGuest(t *testing.T) {
	responseLen, callErrorCode, errCode := decodeDurableCallResult(badParamDurableCall)

	if errCode == 0 {
		t.Errorf("errCode = 0, so the guest would treat a refused call as success")
	}
	if callErrorCode != cleat.CallErrorInvalidRequest {
		t.Errorf("callErrorCode = %d, want %d (cleat.CallErrorInvalidRequest)",
			callErrorCode, cleat.CallErrorInvalidRequest)
	}
	if responseLen != 0 {
		t.Errorf("responseLen = %d, want 0: nothing was written to the response buffer",
			responseLen)
	}
}

// TestErrBadParamIsNotDecodableByGuest documents *why* the constant above has
// to exist, by showing what the raw sentinel decodes to. If someone ever
// "simplifies" the five host functions back to returning errBadParam, this test
// is the explanation of what that costs.
func TestErrBadParamIsNotDecodableByGuest(t *testing.T) {
	// Via a variable: errBadParam is a uint64 constant, and converting it to
	// int64 in constant context is a compile-time overflow.
	sentinel := errBadParam
	responseLen, callErrorCode, _ := decodeDurableCallResult(int64(sentinel))

	// 0xFF000000. Not a cleat.CallErrorCode, so every `switch e.Code` on the
	// guest falls through to default and the structured classification that
	// CallErrorCode exists to provide is lost.
	if callErrorCode <= cleat.CallErrorPermissionDenied {
		t.Errorf("errBadParam now decodes to the valid CallErrorCode %d; if the "+
			"sentinel changed, badParamDurableCall may no longer be needed",
			callErrorCode)
	}

	// 0xFFFFFF -- 16 MB, against a 64 KB response buffer. The generated
	// callErrorMessage bounds-checks responseLen against len(responseBuf)
	// before slicing, which is the only reason this degraded to a wrong
	// message rather than an out-of-range read.
	if responseLen <= _cleatDefaultOutBufSize {
		t.Errorf("errBadParam decodes to responseLen %d, which now fits the "+
			"guest response buffer; the hazard this documents has changed",
			responseLen)
	}
}

// _cleatDefaultOutBufSize mirrors _cleatOutBufSize in the generated host
// adapter (wasm/adapter_component.go).
const _cleatDefaultOutBufSize = 65536

// TestCallErrorInvalidRequestMatchesGuestSDK keeps the engine-local copy of the
// enum value honest. engine does not import cleat in non-test code, so nothing
// but this test stops the two drifting apart.
func TestCallErrorInvalidRequestMatchesGuestSDK(t *testing.T) {
	if got, want := callErrorInvalidRequest, byte(cleat.CallErrorInvalidRequest); got != want {
		t.Errorf("callErrorInvalidRequest = %d, but cleat.CallErrorInvalidRequest = %d", got, want)
	}
}

// ---- The empty-payload rules, at the reader level ----

func TestReadWasmPayloadAcceptsEmpty(t *testing.T) {
	mem := newTestMemory(t, make([]byte, 256))

	// The point of the whole exercise: length 0 is a value, not a fault.
	if s, ok := readWasmPayload(mem, 0, 0, MaxWasmStringLen); !ok || s != "" {
		t.Errorf("readWasmPayload(len=0) = (%q, %v), want (\"\", true)", s, ok)
	}

	// Everything else must still behave exactly like the strict reader.
	if _, ok := readWasmPayload(mem, 0, MaxWasmStringLen+1, MaxWasmStringLen); ok {
		t.Error("readWasmPayload accepted a length over maxLen")
	}
	if _, ok := readWasmPayload(mem, 1<<30, 8, MaxWasmStringLen); ok {
		t.Error("readWasmPayload accepted an out-of-range pointer")
	}
}

func TestReadWasmStringValidatedStillRejectsEmpty(t *testing.T) {
	// Names and keys keep the strict rule; only payloads were relaxed.
	mem := newTestMemory(t, make([]byte, 256))
	if _, ok := readWasmStringValidated(mem, 0, 0, MaxWasmStringLen); ok {
		t.Error("readWasmStringValidated now accepts an empty string; service and " +
			"operation names depend on it rejecting one")
	}
	if ok := validServiceName(""); ok {
		t.Error("validServiceName(\"\") = true, want false")
	}
}

// ---- The same rules, through the registered host closures, on both backends ----

// recordingCallHandler captures what cleat_call actually handed the handler, so
// the tests below can tell "the host accepted the empty payload and passed it
// through" apart from "the host returned something that merely isn't an error".
type recordingCallHandler struct {
	stubHostHandler
	called    bool
	service   string
	operation string
	request   string
}

func (h *recordingCallHandler) DurableCall(_ context.Context, _ api.Module,
	service, operation, requestJSON string, _, _ uint32) int64 {
	h.called = true
	h.service, h.operation, h.request = service, operation, requestJSON
	return packDurableCallResult(0, 0, 0)
}

func TestWazeroCleatCallAcceptsEmptyRequestPayload(t *testing.T) {
	rec := &recordingCallHandler{}
	h := newTestHostFuncHarness(t, "cleat_call",
		[]byte{wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32},
		[]byte{wasmI64}, true, rec)

	if !h.mem.Write(8, []byte("noop")) || !h.mem.Write(32, []byte("Noop")) {
		t.Fatal("write to memory failed")
	}

	got, err := h.call(8, 4, 32, 4, 0, 0, 1024, 256)
	if err != nil {
		t.Fatalf("call cleat_call: %v", err)
	}
	if got == uint64(badParamDurableCall) {
		t.Fatal("cleat_call refused an empty request payload under wazero")
	}
	if !rec.called {
		t.Fatal("cleat_call did not reach the handler")
	}
	if rec.service != "noop" || rec.operation != "Noop" || rec.request != "" {
		t.Errorf("handler got (%q, %q, %q), want (\"noop\", \"Noop\", \"\")",
			rec.service, rec.operation, rec.request)
	}
}

func TestWazeroCleatCallRejectsEmptyOperation(t *testing.T) {
	rec := &recordingCallHandler{}
	h := newTestHostFuncHarness(t, "cleat_call",
		[]byte{wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32},
		[]byte{wasmI64}, true, rec)

	if !h.mem.Write(8, []byte("noop")) {
		t.Fatal("write to memory failed")
	}

	got, err := h.call(8, 4, 0, 0, 64, 2, 1024, 256)
	if err != nil {
		t.Fatalf("call cleat_call: %v", err)
	}
	if got != uint64(badParamDurableCall) {
		t.Errorf("got %#x, want badParamDurableCall (%#x)", got, uint64(badParamDurableCall))
	}
	if rec.called {
		t.Error("cleat_call reached the handler with an empty operation name")
	}
}
