//go:build cgo

package engine

import (
	"context"
	"testing"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

// cleat_poll_work was the only host function in the wasmtime layer that wrote
// to a guest-supplied pointer without checking it was in range.
//
// Every other writer bounds-checks: wasmtimeWriteString compares
// uint64(ptr)+uint64(len) against len(buf), cleat_complete does the same
// inline before slicing, and flush.go's writeResult was fixed explicitly with
// a comment naming this class. cleat_poll_work did
//
//	copy(buf[entryNamePtr:entryNamePtr+int32(entryLen)], ...)
//
// with entryNamePtr straight off the guest's stack. Measured 2026-09-01 against
// a one-page (65536-byte) guest, via the three subtests below:
//
//	ptr = -1       panic: slice bounds out of range [-1:]
//	ptr = 65530    panic: slice bounds out of range [:65538] with capacity 65536
//	argsPtr = 65530panic: slice bounds out of range [:65551] with capacity 65536
//
// Severity, stated precisely rather than dramatically: this is NOT a worker
// crash. All three sites that call into a guest -- backend_wasmtime.go:573
// (Go/_start), :700 (direct export) and :1383 (component) -- wrap the call in a
// recover, so the panic became a failed workflow reporting
// `wasmtime panic in "<entryPoint>"`. That message names the guest's entry
// point, so the fault appears to be in guest code rather than in a host
// function that was handed an argument it never validated. The recovers exist
// for "wasmtime-go internal panics", not for this; relying on them means every
// future direct writer is one unguarded copy away from the same thing.
//
// Why the property test in abi_output_buffer_property_test.go could not catch
// it: that test asserts the (ptr, maxLen) pair reaches the *handler* last and
// in order, and cleat_poll_work has no handler call -- it is the sole entry in
// directWriters. Its comment there already said this needed a behavioural test
// of its own, "exactly the shape where a mix-up writes past a guest's declared
// capacity". This is that test.

// pollWorkGuestWat forwards its four parameters to cleat_poll_work, so a test
// can hand the host function any pointer a real guest could.
const pollWorkGuestWat = `(module
  (import "env" "cleat_poll_work" (func $poll (param i32 i32 i32 i32) (result i64)))
  (memory (export "memory") 1)
  (func (export "poll") (param i32 i32 i32 i32) (result i64)
    (call $poll (local.get 0) (local.get 1) (local.get 2) (local.get 3)))
)`

// pollWorkGuestPageSize is the guest's linear memory: one WASM page.
const pollWorkGuestPageSize = 65536

const (
	pollWorkTestEntryPoint = "RunOrder"
	pollWorkTestInput      = `{"inputJSON":{"orderID":7}}`
)

// pollWorkHarness instantiates pollWorkGuestWat against the production linker
// and returns a function that calls cleat_poll_work, plus a reader for the
// guest's memory.
func pollWorkHarness(t *testing.T) (call func(entryPtr, entryMax, argsPtr, argsMax int32) (int64, error), memory func() []byte) {
	t.Helper()
	ctx := context.Background()

	wasmBytes, err := wasmtime.Wat2Wasm(pollWorkGuestWat)
	if err != nil {
		t.Fatalf("Wat2Wasm: %v", err)
	}

	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close(ctx) })

	// What backend_wasmtime.go:552 sets before calling _start.
	b.workEntryPoint = pollWorkTestEntryPoint
	b.workInput = []byte(pollWorkTestInput)

	linker := wasmtime.NewLinker(b.engine)
	var completeResult, completeErr string
	if err := b.registerAllImports(linker, &completeResult, &completeErr, false, nil); err != nil {
		t.Fatalf("registerAllImports: %v", err)
	}

	module, err := wasmtime.NewModule(b.engine, wasmBytes)
	if err != nil {
		t.Fatalf("NewModule: %v", err)
	}
	store := wasmtime.NewStore(b.engine)
	// Without this the store's epoch deadline is 0 and every call traps with
	// "wasm trap: interrupt" before reaching the host function -- a green-
	// looking harness that measures nothing. NewWasmtimeBackend enables epoch
	// interruption on the shared Config (backend_wasmtime.go:131).
	if _, err := b.configureStore(ctx, store); err != nil {
		t.Fatalf("configureStore: %v", err)
	}
	instance, err := linker.Instantiate(store, module)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}

	fn := instance.GetFunc(store, "poll")
	if fn == nil {
		t.Fatal("the probe module does not export poll")
	}
	mem := instance.GetExport(store, "memory").Memory()
	if mem == nil {
		t.Fatal("the probe module does not export memory")
	}
	if got := len(mem.UnsafeData(store)); got != pollWorkGuestPageSize {
		t.Fatalf("guest memory is %d bytes, expected %d; the out-of-range "+
			"pointers below are chosen relative to that size", got, pollWorkGuestPageSize)
	}

	return func(entryPtr, entryMax, argsPtr, argsMax int32) (int64, error) {
			res, err := fn.Call(store, entryPtr, entryMax, argsPtr, argsMax)
			if err != nil {
				return 0, err
			}
			return res.(int64), nil
		}, func() []byte {
			return mem.UnsafeData(store)
		}
}

// TestPollWorkRejectsOutOfRangeGuestPointers is the regression test.
//
// Before the fix each subtest panicked out of the host function, through
// wasmtime-go's enterWasm trampoline, and killed the test binary -- so they
// have to be run one at a time to see all three (`go test -run
// 'TestPollWorkRejectsOutOfRangeGuestPointers/negative'` and so on). After it,
// each returns errBadParamInt64 and leaves guest memory untouched.
func TestPollWorkRejectsOutOfRangeGuestPointers(t *testing.T) {
	cases := []struct {
		name                                 string
		entryPtr, entryMax, argsPtr, argsMax int32
	}{
		{
			// Negative is the dangerous one: it slices backwards, which no
			// bounds comparison written in signed arithmetic would catch
			// either.
			name: "negative entry pointer",
			// entryMax 64 is generous enough that entryLen is the full
			// "RunOrder", so a copy really would be attempted.
			entryPtr: -1, entryMax: 64, argsPtr: 256, argsMax: 1024,
		},
		{
			// In range as a pointer, out of range as a pointer plus length --
			// the case a naive `ptr < len(buf)` check would pass.
			name:     "entry buffer runs past the end of memory",
			entryPtr: pollWorkGuestPageSize - 2, entryMax: 64, argsPtr: 256, argsMax: 1024,
		},
		{
			// The second output buffer. Two buffers is why this needs its own
			// test: a check that covered only the first would pass here.
			name:     "args buffer runs past the end of memory",
			entryPtr: 0, entryMax: 64, argsPtr: pollWorkGuestPageSize - 2, argsMax: 1024,
		},
		{
			name:     "negative args pointer",
			entryPtr: 0, entryMax: 64, argsPtr: -8, argsMax: 1024,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call, memory := pollWorkHarness(t)
			got, err := call(tc.entryPtr, tc.entryMax, tc.argsPtr, tc.argsMax)
			if err != nil {
				t.Fatalf("calling cleat_poll_work(%d, %d, %d, %d): %v\n\n"+
					"The host must refuse an out-of-range pointer by returning, not by "+
					"trapping or panicking.",
					tc.entryPtr, tc.entryMax, tc.argsPtr, tc.argsMax, err)
			}
			if got != errBadParamInt64 {
				t.Errorf("cleat_poll_work(%d, %d, %d, %d) = %#x, want errBadParamInt64 (%#x).\n\n"+
					"An out-of-range destination must be refused, not clamped: a guest "+
					"that gets a partial write at a pointer it did not mean has no way "+
					"to tell.", tc.entryPtr, tc.entryMax, tc.argsPtr, tc.argsMax, got, errBadParamInt64)
			}
			for i, b := range memory() {
				if b != 0 {
					t.Fatalf("guest memory byte %d is %#x after a refused call; "+
						"the host wrote something before deciding to refuse. The two "+
						"range checks must both happen before either copy.", i, b)
					break
				}
			}
		})
	}
}

// TestPollWorkDeliversWorkAtValidPointers is the control.
//
// Without it, every assertion above would pass against a cleat_poll_work that
// returned errBadParamInt64 unconditionally -- which is the shape the fix could
// most easily take by accident, and would break every Go guest silently, since
// the dispatcher treats a short read as "no work".
func TestPollWorkDeliversWorkAtValidPointers(t *testing.T) {
	call, memory := pollWorkHarness(t)

	const entryPtr, argsPtr = 128, 512
	got, err := call(entryPtr, 64, argsPtr, 1024)
	if err != nil {
		t.Fatalf("cleat_poll_work at valid pointers: %v", err)
	}
	if got == errBadParamInt64 {
		t.Fatalf("cleat_poll_work refused a call whose pointers are both in range.\n\n" +
			"Every Go guest polls for its work this way; refusing here means no Go " +
			"workflow can start.")
	}

	entryLen := int(got >> 32)
	argsLen := int(int32(got))
	if entryLen != len(pollWorkTestEntryPoint) {
		t.Errorf("entry name length = %d, want %d (%q)", entryLen,
			len(pollWorkTestEntryPoint), pollWorkTestEntryPoint)
	}
	if argsLen != len(pollWorkTestInput) {
		t.Errorf("args length = %d, want %d", argsLen, len(pollWorkTestInput))
	}

	buf := memory()
	if entryLen > 0 {
		if got := string(buf[entryPtr : entryPtr+entryLen]); got != pollWorkTestEntryPoint {
			t.Errorf("entry name in guest memory = %q, want %q", got, pollWorkTestEntryPoint)
		}
	}
	if argsLen > 0 {
		if got := string(buf[argsPtr : argsPtr+argsLen]); got != pollWorkTestInput {
			t.Errorf("args in guest memory = %q, want %q", got, pollWorkTestInput)
		}
	}
}

// TestPollWorkTruncatesToTheCapacityTheGuestDeclared is the other half of the
// control: the host must still honour a small-but-valid buffer rather than
// refuse it, and must not write past the declared capacity.
//
// It passes with the fix removed as well -- truncation was never the broken
// part -- so it is a control, not a regression test. It is here to stop the
// refusal above from being satisfied by refusing more than it should, which is
// the cheapest wrong way to make this file green.
//
// The sentinel is what makes the overrun half assertable. Filling the byte just
// past the buffer with a known value and checking it survives is the only way
// to see a one-byte overrun; comparing the buffer contents alone cannot.
func TestPollWorkTruncatesToTheCapacityTheGuestDeclared(t *testing.T) {
	call, memory := pollWorkHarness(t)

	const entryPtr, entryMax = 128, 3 // "Run", not "RunOrder"
	const argsPtr = 512

	const sentinel = 0xAB
	memory()[entryPtr+entryMax] = sentinel

	got, err := call(entryPtr, entryMax, argsPtr, 1024)
	if err != nil {
		t.Fatalf("cleat_poll_work with a short entry buffer: %v", err)
	}
	if got == errBadParamInt64 {
		t.Fatal("cleat_poll_work refused an in-range buffer that is merely too small.\n\n" +
			"Truncation is the documented behaviour: the guest is told the length it " +
			"got back and can size a second call.")
	}
	if entryLen := int(got >> 32); entryLen != entryMax {
		t.Errorf("entry name length = %d, want it truncated to the declared capacity %d",
			entryLen, entryMax)
	}
	buf := memory()
	if got := string(buf[entryPtr : entryPtr+entryMax]); got != pollWorkTestEntryPoint[:entryMax] {
		t.Errorf("truncated entry name = %q, want %q", got, pollWorkTestEntryPoint[:entryMax])
	}
	if buf[entryPtr+entryMax] != sentinel {
		t.Errorf("the byte after the entry buffer is %#x, want the sentinel %#x.\n\n"+
			"The host wrote past the capacity the guest declared -- which in a real "+
			"guest is whatever the allocator put next.", buf[entryPtr+entryMax], sentinel)
	}
}

// TestPollWorkClampsNegativeCapacity covers the other i32 that can be
// nonsensical.
//
// A negative maxLen went straight into `if entryLen > int(entryNameMaxLen)`,
// setting entryLen to the negative value: the copy was skipped (guarded by
// `entryLen > 0`) but the negative length was still packed into the return
// value, so a guest reading it back as unsigned saw 0xFFFFFFFF bytes of entry
// name available. Zero is the honest answer.
func TestPollWorkClampsNegativeCapacity(t *testing.T) {
	call, _ := pollWorkHarness(t)

	got, err := call(128, -1, 512, 1024)
	if err != nil {
		t.Fatalf("cleat_poll_work with a negative entry capacity: %v", err)
	}
	if got == errBadParamInt64 {
		// Refusing is also defensible; assert the length only if it did not.
		return
	}
	if entryLen := int(got >> 32); entryLen != 0 {
		t.Errorf("entry name length = %d for a capacity of -1, want 0.\n\n"+
			"A guest reads the high word as unsigned, so a negative length is "+
			"reported as ~4GB of available name.", entryLen)
	}
}
