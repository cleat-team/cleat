//go:build cgo

// The wasmtime half of the durable-call ABI tests. Split out because
// newClosureSetup, wasmtimeBackend and wasmtimeReadPayload all live behind
// //go:build cgo. See durablecall_abi_test.go for the contract these pin.

package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

func TestWasmtimeReadPayloadAcceptsEmptyButNotNegative(t *testing.T) {
	buf := make([]byte, 256)

	if s, ok := wasmtimeReadPayload(buf, 0, 0, int32(MaxWasmStringLen)); !ok || s != "" {
		t.Errorf("wasmtimeReadPayload(len=0) = (%q, %v), want (\"\", true)", s, ok)
	}

	// These parameters arrive as i32. Negative is a corrupt argument, not an
	// empty payload, and must still be refused.
	if _, ok := wasmtimeReadPayload(buf, 0, -1, int32(MaxWasmStringLen)); ok {
		t.Error("wasmtimeReadPayload accepted a negative length")
	}
	if _, ok := wasmtimeReadPayload(buf, 250, 100, int32(MaxWasmStringLen)); ok {
		t.Error("wasmtimeReadPayload accepted a read past the end of memory")
	}
}

// cleatCallFunctype is cleat_call's signature: (ptr,len x3, ptr,maxLen) -> i64.
func cleatCallFunctype() []byte {
	return wasmFunctype([]byte{
		wasmValI32, wasmValI32, wasmValI32, wasmValI32,
		wasmValI32, wasmValI32, wasmValI32, wasmValI32,
	}, []byte{wasmValI64})
}

func TestWasmtimeCleatCallAcceptsEmptyRequestPayload(t *testing.T) {
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_call", cleatCallFunctype()}},
		func(b *wasmtimeBackend, l *wasmtime.Linker) error {
			var res, errStr string
			return b.registerCleatCall(l, &res, &errStr)
		})

	rec := &recordingCallHandler{}
	s.backend.handler = rec

	s.writeString(8, "noop")
	s.writeString(32, "Noop")

	// reqPtr=0, reqLen=0 -- a durable call that takes no arguments.
	got := s.call(t, "test_cleat_call",
		i32(8), i32(4), i32(32), i32(4), i32(0), i32(0), i32(1024), i32(256))

	if got == badParamDurableCall {
		t.Fatal("cleat_call refused an empty request payload; a durable call " +
			"with no arguments is legitimate (IMPROVEMENT-PLAN.md 2.10)")
	}
	if !rec.called {
		t.Fatal("cleat_call did not reach the handler")
	}
	if rec.service != "noop" || rec.operation != "Noop" || rec.request != "" {
		t.Errorf("handler got (%q, %q, %q), want (\"noop\", \"Noop\", \"\")",
			rec.service, rec.operation, rec.request)
	}
}

func TestWasmtimeCleatCallRejectsEmptyOperation(t *testing.T) {
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_call", cleatCallFunctype()}},
		func(b *wasmtimeBackend, l *wasmtime.Linker) error {
			var res, errStr string
			return b.registerCleatCall(l, &res, &errStr)
		})

	rec := &recordingCallHandler{}
	s.backend.handler = rec

	s.writeString(8, "noop")

	// opPtr=0, opLen=0. An operation name is part of the call target, so
	// unlike the payload it may not be empty -- but the refusal must now be
	// expressed in the layout the guest decodes.
	got := s.call(t, "test_cleat_call",
		i32(8), i32(4), i32(0), i32(0), i32(64), i32(2), i32(1024), i32(256))

	if got != badParamDurableCall {
		t.Errorf("got %#x, want badParamDurableCall (%#x)", got, badParamDurableCall)
	}
	if rec.called {
		t.Error("cleat_call reached the handler with an empty operation name")
	}
	want := uint32(guestCodeNamed(t, "InvalidRequest"))
	if _, callErrorCode, errCode := decodeDurableCallResult(got); errCode == 0 ||
		callErrorCode != want {
		t.Errorf("guest would decode callErrorCode=%d errCode=%d; want %d and nonzero",
			callErrorCode, errCode, want)
	}
}

// isExecutionInterruptTrap reports whether err is the epoch-interruption trap
// the wasmtime backend raises when a workflow exceeds its wall-clock budget.
//
// It lives behind //go:build cgo, with a !cgo counterpart, so that
// integration_test.go can assert on the trap *type* without importing wasmtime
// -- that file compiles in both configurations. Matching the type rather than
// the message is what stops this assertion drifting back into the substring
// soup it used to be, where "error 1" counted as evidence of a timeout.
func isExecutionInterruptTrap(err error) bool {
	var trap *wasmtime.Trap
	if !errors.As(err, &trap) {
		return false
	}
	code := trap.Code()
	return code != nil && *code == wasmtime.Interrupt
}

// TestEpochTickIntervalMatchesDurationTestSlack pins the epoch tick against the
// literal slack TestIntegrationWorkflowMaxDuration allows. That test cannot
// reference epochTickInterval directly (it compiles without CGO, where the
// constant does not exist), so this is what stops the two drifting apart and
// quietly making the duration assertion either flaky or vacuous.
func TestEpochTickIntervalMatchesDurationTestSlack(t *testing.T) {
	if epochTickInterval != 50*time.Millisecond {
		t.Errorf("epochTickInterval = %v; TestIntegrationWorkflowMaxDuration allows "+
			"two ticks of slack as the literal 100ms and must be updated to match",
			epochTickInterval)
	}
}
