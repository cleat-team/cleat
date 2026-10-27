//go:build cgo

// See the comment at the top of component_cgo.go: this file is part of the
// native wasmtime Component Model C API path. The `-I` to wasmtime's C
// headers it needs is declared once for the package there, pointing at the
// vendored engine/wasmtimeinc tree; cgo combines #cgo directives across every
// file in a package, so it does not need repeating here.

package engine

// #include <wasmtime.h>
// #include <wasmtime/component/component.h>
// #include <wasmtime/component/linker.h>
// #include <wasmtime/component/instance.h>
// #include <wasmtime/component/func.h>
// #include <wasmtime/component/val.h>
// #include <stdlib.h>
// #include <string.h>
//
// static void get_error_message(wasmtime_error_t *err, wasm_byte_vec_t *msg) {
//     wasmtime_error_message(err, msg);
// }
//
// // cbid_as_env turns a callback-registry id into the opaque `void *env`
// // wasmtime stores alongside a host function and hands back to
// // goComponentCallback, which converts it straight back with lookupCB.
// //
// // The cast lives here, in C, on purpose. Written on the Go side as
// // unsafe.Pointer(uintptr(cbID)) it is two things at once: a go vet unsafeptr
// // finding, and a value that is not a valid Go pointer sitting in a
// // pointer-typed Go slot, which the garbage collector is entitled to reject
// // when it scans the frame. The id is a small counter, never dereferenced by
// // anyone -- it is a token, and C is where a token may legally be spelled as
// // a pointer.
// //
// // go vet never said so before 2026-08-05 because this file was excluded from
// // every default build by the wasmtime_component_cgo tag, so nothing vetted
// // it. Compiling it by default is what surfaced this.
// static void *cbid_as_env(uintptr_t id) { return (void *)id; }
//
// extern wasmtime_error_t *goComponentCallback(
//     void *env, wasmtime_context_t *ctx,
//     wasmtime_component_func_type_t *ty,
//     wasmtime_component_val_t *args, size_t nargs,
//     wasmtime_component_val_t *results, size_t nresults);
import "C"
import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"unsafe"

	"github.com/cleat-team/cleat/wasm"
)

func (b *wasmtimeBackend) dispatchDurableCallString(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 3 || b.handler == nil {
		return nil
	}
	svc := readStrArg(args, 0, nargs)
	op := readStrArg(args, 1, nargs)
	req := readStrArg(args, 2, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.DurableCall(ctxWithMem(context.Background(), buf), nil, svc, op, req, 0, 65536)

	setResultCallOutcome(results, nresults,
		decodeCallOutcome(packed, buf, extractStringFromPacked))
	return nil
}

// dispatchDurableCallRetry handles (string,string,string,u64,u64,u64,u64,string) -> string.
func (b *wasmtimeBackend) dispatchDurableCallRetry(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 8 || b.handler == nil {
		return nil
	}
	svc := readStrArg(args, 0, nargs)
	op := readStrArg(args, 1, nargs)
	req := readStrArg(args, 2, nargs)
	maxAttempts := int64(readU64Arg(args, 3, nargs))
	initialInterval := int64(readU64Arg(args, 4, nargs))
	backoffCoeff := int64(readU64Arg(args, 5, nargs))
	maxInterval := int64(readU64Arg(args, 6, nargs))
	nonRetryable := readStrArg(args, 7, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.DurableCallWithRetry(ctxWithMem(context.Background(), buf), nil,
		svc, op, req, maxAttempts, initialInterval, backoffCoeff, maxInterval, nonRetryable, 0, 65536)

	setResultCallOutcome(results, nresults,
		decodeCallOutcome(packed, buf, extractStringFromPacked))
	return nil
}

// dispatchDurableCallHeartbeat handles (string,string,string,u64) -> string.
func (b *wasmtimeBackend) dispatchDurableCallHeartbeat(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 4 || b.handler == nil {
		return nil
	}
	svc := readStrArg(args, 0, nargs)
	op := readStrArg(args, 1, nargs)
	req := readStrArg(args, 2, nargs)
	heartbeatInterval := int64(readU64Arg(args, 3, nargs))

	buf := make([]byte, 65536)
	packed := b.handler.DurableCallWithHeartbeat(ctxWithMem(context.Background(), buf), nil,
		svc, op, req, heartbeatInterval, 0, 65536)

	setResultCallOutcome(results, nresults,
		decodeCallOutcome(packed, buf, extractStringFromPacked))
	return nil
}

// ---------------------------------------------------------------------------
// durable-sleep interface
// ---------------------------------------------------------------------------

// dispatchDurableSleep handles (u64) -> u64.
func (b *wasmtimeBackend) dispatchDurableSleep(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil {
		return nil
	}
	durationMs := int64(readU64Arg(args, 0, nargs))
	r := b.handler.DurableSleep(context.Background(), nil, durationMs)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchNow handles () -> u64.
func (b *wasmtimeBackend) dispatchNow(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if b.handler == nil {
		return nil
	}
	r := b.handler.Now(context.Background())
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchRandom handles () -> u64.
func (b *wasmtimeBackend) dispatchRandom(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if b.handler == nil {
		return nil
	}
	r := b.handler.Random(context.Background())
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchDurableLog handles (string) -> u64.
func (b *wasmtimeBackend) dispatchDurableLog(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil {
		return nil
	}
	msg := readStrArg(args, 0, nargs)
	r := b.handler.DurableLog(context.Background(), nil, msg)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// ---------------------------------------------------------------------------
// durable-version interface
// ---------------------------------------------------------------------------

// dispatchVersion handles () -> u64.
func (b *wasmtimeBackend) dispatchVersion(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if b.handler == nil {
		return nil
	}
	r := b.handler.Version(context.Background())
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchMinVersion handles () -> u64.
func (b *wasmtimeBackend) dispatchMinVersion(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if b.handler == nil {
		return nil
	}
	r := b.handler.MinVersion(context.Background())
	setResultU64(results, nresults, uint64(r))
	return nil
}

// ---------------------------------------------------------------------------
// durable-lifecycle interface
// ---------------------------------------------------------------------------

// dispatchDurableDefer handles (string) -> string.
func (b *wasmtimeBackend) dispatchDurableDefer(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil {
		return nil
	}
	desc := readStrArg(args, 0, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.DurableDefer(ctxWithMem(context.Background(), buf), nil, desc, 0, 65536)
	response := extractStringFromSimplePacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// dispatchContinueAsNew handles (string) -> u64.
func (b *wasmtimeBackend) dispatchContinueAsNew(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil {
		return nil
	}
	input := readStrArg(args, 0, nargs)
	r := b.handler.ContinueAsNew(context.Background(), nil, input)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchPollCancellation handles () -> string.
func (b *wasmtimeBackend) dispatchPollCancellation(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if b.handler == nil {
		return nil
	}

	buf := make([]byte, 65536)
	packed := b.handler.PollCancellation(ctxWithMem(context.Background(), buf), nil, 0, 65536)
	response := extractStringFromSimplePacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// ---------------------------------------------------------------------------
// durable-signals interface
// ---------------------------------------------------------------------------

// dispatchAwaitSignals handles (string,u64,u32,u32,u32,u32) -> u64.
// Output buffers are created via ctxWithMem; only the u64 return is surfaced.
func (b *wasmtimeBackend) dispatchAwaitSignals(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 6 || b.handler == nil {
		return nil
	}
	names := readStrArg(args, 0, nargs)
	timeoutMs := int64(readU64Arg(args, 1, nargs))

	// Create a double-sized buffer for two output params.
	buf := make([]byte, 131072)
	r := b.handler.DurableAwaitSignals(ctxWithMem(context.Background(), buf), nil,
		names, timeoutMs, 0, 65536, 65536, 65536)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchPollSignal handles (string) -> string.
func (b *wasmtimeBackend) dispatchPollSignal(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil {
		return nil
	}
	name := readStrArg(args, 0, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.PollSignal(ctxWithMem(context.Background(), buf), nil, name, 0, 65536)
	response := extractStringFromSimplePacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// dispatchSignalWorkflow handles (string,string,string) -> u64.
func (b *wasmtimeBackend) dispatchSignalWorkflow(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 3 || b.handler == nil {
		return nil
	}
	target := readStrArg(args, 0, nargs)
	sigName := readStrArg(args, 1, nargs)
	payload := readStrArg(args, 2, nargs)
	r := b.handler.SignalWorkflow(context.Background(), nil, target, sigName, payload)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// ---------------------------------------------------------------------------
// durable-children interface
// ---------------------------------------------------------------------------

// dispatchChildWorkflow handles (string,string) -> string.
func (b *wasmtimeBackend) dispatchChildWorkflow(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 2 || b.handler == nil {
		return nil
	}
	name := readStrArg(args, 0, nargs)
	input := readStrArg(args, 1, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.ChildWorkflow(ctxWithMem(context.Background(), buf), nil, name, input, 0, 65536)

	setResultCallOutcome(results, nresults,
		decodeCallOutcome(packed, buf, extractStringFromSimplePacked))
	return nil
}

// dispatchAwaitChild handles (string) -> string.
func (b *wasmtimeBackend) dispatchAwaitChild(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil {
		return nil
	}
	runID := readStrArg(args, 0, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.AwaitChild(ctxWithMem(context.Background(), buf), nil, runID, 0, 65536)
	if packed == packAwaitChildResultSuspend() {
		return b.stopAtPendingChildAwait("await_child", results, nresults)
	}
	response := extractStringFromSimplePacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// stopAtPendingChildAwait is what the three child awaits do when the host
// answered "that child has not finished": it has recorded the wait and set
// suspendErr, and the guest has to STOP, not run on.
//
// WIT types these calls `-> string`, so the suspend word cannot carry a stop.
// Before cleat#3429 it was flattened to 65536 zero bytes (extractStringFromSimplePacked
// reads the word's length field as 1<<30 and clamps it to the buffer) and the
// guest ran on: past a pending await_child it made the next side effect, and
// finished and drained its own defers, so a cleanup fired while the run was
// only suspended. await_all_children looked safe only because json.loads raised
// on the zeros.
//
// RETURNING "" IS NOT THE ANSWER outside the cases below, and was the issue's
// first suggestion: the Python SDK reads an empty await_all_children answer as
// [] (no children), so the guest would run on to its next side effect, and an
// empty string is a plausible child result besides.
//
// So the guest is stopped by TRAPPING: returning an error from the callback
// unwinds the guest at the call, the executor already lets a suspension win
// over a call error when suspendErr is set (executor.go), and no WIT or ABI
// changes, so components built against the old signatures keep linking.
//
// A trap kills the instance, and the host cannot then run `run-deferred`.
// That is harmless for an ordinary suspension, which deliberately drains
// nothing, and fatal in two places, where this answers "" instead:
//
//   - a DEFER SEGMENT (b.deferPhase): the workflow was terminated while it
//     waited on a child and the host replays it to run its defers. Measured:
//     the plain trap lost the cleanup (ops=[]) where today's code ran it.
//   - the guest's OWN drain (the session's inDeferPhase, which the guest sets
//     through durable-defer-phase, cleat#3428): a defer body that itself waits
//     on a pending child. A trap there ends every defer queued after it.
//
// What a defer body that awaits a pending child therefore sees: await_child
// returns "", await_all_children returns [] (Python's reading of ""), and
// await_any_child raises (Python's reading of ""). TestPythonGuestsDeferBodyAwaitingPendingChildren
// pins it. The run is still suspended: suspendErr was set by the host.
func (b *wasmtimeBackend) stopAtPendingChildAwait(
	call string, results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if !b.deferPhase && !b.guestIsDrainingDefers() {
		msg := C.CString("cleat: workflow suspended awaiting a child (" + call + ")")
		defer C.free(unsafe.Pointer(msg))
		return C.wasmtime_error_new(msg)
	}
	setResultString(results, nresults, "")
	return nil
}

// guestIsDrainingDefers reports whether the handler's session says the guest is
// running its own defer bodies. A handler with no session (a mock) says no.
func (b *wasmtimeBackend) guestIsDrainingDefers() bool {
	d, ok := b.handler.(interface{ guestIsDrainingDefers() bool })
	return ok && d.guestIsDrainingDefers()
}

// extractChildAnswer is extractStringFromSimplePacked for the two child calls
// that answer with a JSON document, PollChild and AwaitAnyChild.
//
// It exists for one packed word. AwaitAnyChild answers "no child has finished,
// the run is suspending" with packAwaitChildResultSuspend (1<<62), whose length
// field (bits 32-63) reads as 1<<30. extractStringFromSimplePacked clamps that
// to the buffer and returns 64 KiB of zero bytes, which a guest would hand to a
// JSON parser as if it were the winner's document. The empty string is what
// the Python SDK already treats as "nothing came back".
//
// WIT declares both functions `-> string`, so neither a suspend nor a host
// error (errCode in the low bits, the message in the buffer) can reach the
// guest as anything but text. An error therefore arrives as the host's message
// and fails the guest's JSON parse loudly, which is the most this signature
// can do; carrying them as a case of the type is what cleat#3425 did for
// durable-await-promise, and these two calls have not had it done.
func extractChildAnswer(packed int64, buf []byte) string {
	if packed == packAwaitChildResultSuspend() {
		return ""
	}
	return extractStringFromSimplePacked(packed, buf)
}

// dispatchPollChild handles (string) -> string.
func (b *wasmtimeBackend) dispatchPollChild(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil {
		return nil
	}
	runID := readStrArg(args, 0, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.PollChild(ctxWithMem(context.Background(), buf), nil, runID, 0, 65536)

	setResultString(results, nresults, extractChildAnswer(packed, buf))
	return nil
}

// dispatchAwaitAnyChild handles (string) -> string.
func (b *wasmtimeBackend) dispatchAwaitAnyChild(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil {
		return nil
	}
	runIDsJSON := readStrArg(args, 0, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.AwaitAnyChild(ctxWithMem(context.Background(), buf), nil, runIDsJSON, 0, 65536)
	if packed == packAwaitChildResultSuspend() {
		return b.stopAtPendingChildAwait("await_any_child", results, nresults)
	}

	setResultString(results, nresults, extractChildAnswer(packed, buf))
	return nil
}

// dispatchAwaitAllChildren handles (string) -> string.
func (b *wasmtimeBackend) dispatchAwaitAllChildren(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil {
		return nil
	}
	runIDsJSON := readStrArg(args, 0, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.AwaitAllChildren(ctxWithMem(context.Background(), buf), nil, runIDsJSON, 0, 65536)
	if packed == packAwaitChildResultSuspend() {
		return b.stopAtPendingChildAwait("await_all_children", results, nresults)
	}
	response := extractStringFromSimplePacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// dispatchChildWorkflowWithOptions handles (string,string,u64,u64,string) -> string.
func (b *wasmtimeBackend) dispatchChildWorkflowWithOptions(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 5 || b.handler == nil {
		return nil
	}
	name := readStrArg(args, 0, nargs)
	input := readStrArg(args, 1, nargs)
	version := int64(readU64Arg(args, 2, nargs))
	priority := int64(readU64Arg(args, 3, nargs))
	policy := readStrArg(args, 4, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.ChildWorkflowWithOptions(ctxWithMem(context.Background(), buf), nil,
		name, input, version, priority, policy, 0, 65536)

	setResultCallOutcome(results, nresults,
		decodeCallOutcome(packed, buf, extractStringFromSimplePacked))
	return nil
}

// ---------------------------------------------------------------------------
// durable-promises interface
// ---------------------------------------------------------------------------

// dispatchCreatePromise handles (string[,u64]) -> string.
// WIT defines a second arg (ttl-ms: u64) which the handler does not accept.
func (b *wasmtimeBackend) dispatchCreatePromise(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil {
		return nil
	}
	name := readStrArg(args, 0, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.CreatePromise(ctxWithMem(context.Background(), buf), nil, name, 0, 65536)
	response := extractStringFromSimplePacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// awaitRejectionReporter is implemented by the host handler that can say whether
// the AwaitPromise call it just answered ended in a REJECTION.
//
// The packed word cannot: a rejected promise and a host error (no promise
// store) both carry errCode 1 and a message, and five SDKs decode that word, so
// it is not changed to add a case. The component path needs the distinction
// because its WIT type has a case for each (cleat#3425). A handler that does
// not implement this -- the test mocks -- reports every errCode as a host error.
type awaitRejectionReporter interface {
	lastAwaitPromiseRejected() bool
}

// dispatchAwaitPromise handles (string,u64) -> result<await-outcome, call-failure>.
//
// This was `-> string`, decoded with extractStringFromSimplePacked, which reads
// the length field and nothing else (cleat#3425, measured on a real Python
// guest): a rejection arrived as its reason with rejected=False, a host error as
// its message, a genuine timeout as "" -- indistinguishable from a promise
// resolved with an empty value -- and a stop as "" too.
func (b *wasmtimeBackend) dispatchAwaitPromise(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 2 || b.handler == nil {
		return nil
	}
	id := readStrArg(args, 0, nargs)
	timeoutMs := int64(readU64Arg(args, 1, nargs))

	buf := make([]byte, 65536)
	packed := b.handler.AwaitPromise(ctxWithMem(context.Background(), buf), nil, id, timeoutMs, 0, 65536)

	// The stop is tested FIRST and by mask, for the reason decodeCallOutcome
	// gives. In this layout bit 31 sits in the byte at bits 24-31 (the
	// "rejected" flag the Python decoder documents) and the length field starts
	// at bit 32, so a stop reads as an ordinary result of length 0: measured
	// with the check removed, TestDispatchAwaitPromiseMapsEachPackedWordToItsOwnCase
	// reports the stop as "resolved" -- the same silent empty success this
	// import produced before it could carry a stop at all.
	if uint64(packed)&uint64(callSuspendSentinel) != 0 {
		setResultCallOutcome(results, nresults, callOutcome{suspended: true})
		return nil
	}

	u := uint64(packed)
	payload := extractStringFromSimplePacked(packed, buf)
	if errCode := uint16(u & 0xFFFF); errCode != 0 {
		if r, ok := b.handler.(awaitRejectionReporter); ok && r.lastAwaitPromiseRejected() {
			setResultAwaitOutcome(results, nresults, awaitRejected, payload)
			return nil
		}
		// code 0: the packed errCode here is this call's own ("1"), not the
		// call-error taxonomy the guest maps to retryable/permanent classes.
		setResultCallOutcome(results, nresults, callOutcome{failed: true, payload: payload})
		return nil
	}
	if byte(u>>16) != 0 {
		setResultAwaitOutcome(results, nresults, awaitTimedOut, "")
		return nil
	}
	setResultAwaitOutcome(results, nresults, awaitResolved, payload)
	return nil
}

// dispatchResolvePromise handles (string,string) -> u64.
func (b *wasmtimeBackend) dispatchResolvePromise(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 2 || b.handler == nil {
		return nil
	}
	id := readStrArg(args, 0, nargs)
	val := readStrArg(args, 1, nargs)
	r := b.handler.ResolvePromise(context.Background(), nil, id, val)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchRejectPromise handles (string,string) -> u64.
func (b *wasmtimeBackend) dispatchRejectPromise(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 2 || b.handler == nil {
		return nil
	}
	id := readStrArg(args, 0, nargs)
	errMsg := readStrArg(args, 1, nargs)
	r := b.handler.RejectPromise(context.Background(), nil, id, errMsg)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// ---------------------------------------------------------------------------
// durable-state interface
// ---------------------------------------------------------------------------

// dispatchSetQueryState handles (string,string) -> u64.
func (b *wasmtimeBackend) dispatchSetQueryState(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 2 || b.handler == nil {
		return nil
	}
	key := readStrArg(args, 0, nargs)
	val := readStrArg(args, 1, nargs)
	r := b.handler.SetQueryState(context.Background(), nil, key, val)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// ---------------------------------------------------------------------------
// durable-handlers interface
// ---------------------------------------------------------------------------

// dispatchRegisterUpdateHandler handles (string) -> u64.
func (b *wasmtimeBackend) dispatchRegisterUpdateHandler(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil {
		return nil
	}
	name := readStrArg(args, 0, nargs)
	r := b.handler.RegisterUpdateHandler(context.Background(), nil, name)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchRegisterQueryHandler handles (string) -> u64.
func (b *wasmtimeBackend) dispatchRegisterQueryHandler(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil {
		return nil
	}
	name := readStrArg(args, 0, nargs)
	r := b.handler.RegisterQueryHandler(context.Background(), nil, name)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// ---------------------------------------------------------------------------
// durable-messaging interface
// ---------------------------------------------------------------------------

// dispatchPollUpdate handles () -> string.
//
// The component signature returns the envelope directly rather than writing
// into a guest buffer, so "nothing pending" is the empty string. A component
// guest therefore cannot misread a not-found as a delivery, which is the same
// property the result<> calls have.
func (b *wasmtimeBackend) dispatchPollUpdate(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if b.handler == nil {
		return nil
	}
	buf := make([]byte, 65536)
	packed := b.handler.DurablePollUpdate(ctxWithMem(context.Background(), buf), nil, 0, 65536)
	// found=false packs as 0, and extractStringFromSimplePacked then yields "".
	envelope := extractStringFromSimplePacked(packed, buf)
	setResultString(results, nresults, envelope)
	return nil
}

// dispatchCompleteUpdate handles (string,string,string) -> u64.
func (b *wasmtimeBackend) dispatchCompleteUpdate(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 3 || b.handler == nil {
		return nil
	}
	requestID := readStrArg(args, 0, nargs)
	result := readStrArg(args, 1, nargs)
	errMsg := readStrArg(args, 2, nargs)
	r := b.handler.DurableCompleteUpdate(context.Background(), nil, requestID, result, errMsg)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchDurableSend handles (string,string,string) -> u64.
func (b *wasmtimeBackend) dispatchDurableSend(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 3 || b.handler == nil {
		return nil
	}
	svc := readStrArg(args, 0, nargs)
	op := readStrArg(args, 1, nargs)
	req := readStrArg(args, 2, nargs)
	r := b.handler.DurableSend(context.Background(), nil, svc, op, req)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchScheduleInvoke handles (string,string,string,u64) -> u64.
func (b *wasmtimeBackend) dispatchScheduleInvoke(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 4 || b.handler == nil {
		return nil
	}
	svc := readStrArg(args, 0, nargs)
	op := readStrArg(args, 1, nargs)
	req := readStrArg(args, 2, nargs)
	delayMs := int64(readU64Arg(args, 3, nargs))
	r := b.handler.DurableScheduleInvoke(context.Background(), nil, svc, op, req, delayMs)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// ---------------------------------------------------------------------------
// durable-identity interface
// ---------------------------------------------------------------------------

// dispatchWorkflowID handles () -> string.
func (b *wasmtimeBackend) dispatchWorkflowID(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if b.handler == nil {
		return nil
	}

	buf := make([]byte, 65536)
	packed := b.handler.WorkflowID(ctxWithMem(context.Background(), buf), nil, 0, 65536)
	response := extractStringFromSimplePacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// dispatchRunID handles () -> string.
func (b *wasmtimeBackend) dispatchRunID(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if b.handler == nil {
		return nil
	}

	buf := make([]byte, 65536)
	packed := b.handler.RunID(ctxWithMem(context.Background(), buf), nil, 0, 65536)
	response := extractStringFromSimplePacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// ---------------------------------------------------------------------------
// plugin interface
// ---------------------------------------------------------------------------

// dispatchPluginCall handles (string,string,string) -> string.
func (b *wasmtimeBackend) dispatchPluginCall(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 3 || b.handler == nil {
		return nil
	}
	pluginName := readStrArg(args, 0, nargs)
	funcName := readStrArg(args, 1, nargs)
	input := readStrArg(args, 2, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.PluginCall(ctxWithMem(context.Background(), buf), nil, pluginName, funcName, input, 0, 65536)

	setResultCallOutcome(results, nresults,
		decodeCallOutcome(packed, buf, extractStringFromPacked))
	return nil
}

// dispatchPluginCallStreaming handles (string,string,string) -> string.
func (b *wasmtimeBackend) dispatchPluginCallStreaming(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 3 || b.handler == nil {
		return nil
	}
	pluginName := readStrArg(args, 0, nargs)
	funcName := readStrArg(args, 1, nargs)
	input := readStrArg(args, 2, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.PluginCallStreaming(ctxWithMem(context.Background(), buf), nil, pluginName, funcName, input, 0, 65536)

	setResultCallOutcome(results, nresults,
		decodeCallOutcome(packed, buf, extractStringFromPacked))
	return nil
}

// ---------------------------------------------------------------------------
// durable-lock interface
// ---------------------------------------------------------------------------

// dispatchAcquireLock handles (string,s64) -> s64.
func (b *wasmtimeBackend) dispatchAcquireLock(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 2 || b.handler == nil {
		return nil
	}
	key := readStrArg(args, 0, nargs)
	ttlMs := int64(readU64Arg(args, 1, nargs))
	r := b.handler.AcquireLock(context.Background(), nil, key, ttlMs)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchReleaseLock handles (string) -> s64.
func (b *wasmtimeBackend) dispatchReleaseLock(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil {
		return nil
	}
	key := readStrArg(args, 0, nargs)
	r := b.handler.ReleaseLock(context.Background(), nil, key)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// ---------------------------------------------------------------------------
// durable-scope interface
// ---------------------------------------------------------------------------

// dispatchSetScope handles (string,string) -> string.
func (b *wasmtimeBackend) dispatchSetScope(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 2 || b.handler == nil {
		return nil
	}
	objType := readStrArg(args, 0, nargs)
	instKey := readStrArg(args, 1, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.SetScope(ctxWithMem(context.Background(), buf), nil, objType, instKey, 0, 65536)
	response := extractStringFromSimplePacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// dispatchGetScope handles (u32,u32,u32,u32) -> u64.
// Output buffers are created via ctxWithMem; only the u64 return is surfaced.
func (b *wasmtimeBackend) dispatchGetScope(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 4 || b.handler == nil {
		return nil
	}

	// Create a double-sized buffer for two output params.
	buf := make([]byte, 131072)
	r := b.handler.GetScope(ctxWithMem(context.Background(), buf), nil, 0, 65536, 65536, 65536)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchUUID handles (string) -> string.
func (b *wasmtimeBackend) dispatchUUID(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil {
		return nil
	}
	seed := readStrArg(args, 0, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.UUID(ctxWithMem(context.Background(), buf), nil, seed, 0, 65536)
	response := extractStringFromSimplePacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// ---------------------------------------------------------------------------
// durable-stream-state interface
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// durable-extended-lifecycle interface
// ---------------------------------------------------------------------------

// dispatchRunDetached handles (string,string) -> u64.
//
// The WIT result is a u64, and the deprecated fallback also writes a u64, so
// until cleat#3428 this call type-checked while starting nothing: no trap and no
// error, a zero that reads as success. The word is returned untouched, so a stop
// sentinel (bit 31) reaches the Python SDK's _raise_if_stopped the way it does
// for durable-signal-workflow.
func (b *wasmtimeBackend) dispatchRunDetached(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 2 || b.handler == nil {
		return nil
	}
	name := readStrArg(args, 0, nargs)
	input := readStrArg(args, 1, nargs)
	r := b.handler.RunDetached(context.Background(), nil, name, input)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchDeferPhase handles (bool) -> ().
//
// It reports that the guest has started (true) or finished (false) draining its
// defer table, so the host can mark the events recorded in between. It records no
// event and returns nothing, which is why the fallback was harmless in shape and
// wrong in effect: nothing to type-check, and the mark never reached the host
// (cleat#1155, cleat#1237, cleat#3428).
func (b *wasmtimeBackend) dispatchDeferPhase(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil {
		return nil
	}
	b.handler.SetDeferPhase(context.Background(), readBoolArg(args, 0, nargs))
	return nil
}

// dispatchContinueAsNewVersioned handles (string,u32) -> u64.
func (b *wasmtimeBackend) dispatchContinueAsNewVersioned(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 2 || b.handler == nil {
		return nil
	}
	input := readStrArg(args, 0, nargs)
	newVersion := int(readU32Arg(args, 1, nargs))
	r := b.handler.ContinueAsNewWithVersion(context.Background(), nil, input, newVersion)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchSideEffect handles (string) -> string.
func (b *wasmtimeBackend) dispatchSideEffect(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil {
		return nil
	}
	result := readStrArg(args, 0, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.SideEffect(ctxWithMem(context.Background(), buf), nil, result, 0, 65536)
	// result<string, call-failure>, not a bare string: decodeCallOutcome tests
	// the stop sentinel by mask BEFORE reading any field, so a refusal reaches
	// the component guest as the `suspended` case rather than as a response the
	// service could also have produced. setResultString discarded those bits.
	setResultCallOutcome(results, nresults,
		decodeCallOutcome(packed, buf, extractStringFromSimplePacked))
	return nil
}

// ---------------------------------------------------------------------------
// durable-fetch interface
// ---------------------------------------------------------------------------

// dispatchFetch handles (string,string,string,string) -> string.
func (b *wasmtimeBackend) dispatchFetch(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 4 || b.handler == nil {
		return nil
	}
	method := readStrArg(args, 0, nargs)
	url := readStrArg(args, 1, nargs)
	headers := readStrArg(args, 2, nargs)
	body := readStrArg(args, 3, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.Fetch(ctxWithMem(context.Background(), buf), nil, method, url, headers, body, 0, 65536)

	setResultCallOutcome(results, nresults,
		decodeCallOutcome(packed, buf, extractStringFromSimplePacked))
	return nil
}

// ---------------------------------------------------------------------------
// durable-cron interface
// ---------------------------------------------------------------------------

// extractStringFromSimplePacked decodes the output string from a handler
// result packed by packSimpleResult (memory.go):
//
//	bits 32-63 = written length
//	bits 8-31  = unused
//	bits 0-7   = errCode
//
// This is a different layout from extractStringFromPacked, which decodes
// packDurableCallResult's bits-40-63-for-length layout. ScheduleCron and
// ListCrons (engine/schedules.go) both return packSimpleResult, like most
// string-returning host calls other than the durable-call/plugin-call family
// -- reusing extractStringFromPacked here would right-shift the written
// length by an extra 8 bits and silently return "" for any response under
// 256 bytes. Confirmed against packSimpleResult's actual bit layout; this
// helper is scoped to the two cron dispatchers below rather than changed
// package-wide.
func extractStringFromSimplePacked(packed int64, buf []byte) string {
	r := uint64(packed)
	actualLen := uint32(r >> 32)
	if actualLen > uint32(len(buf)) {
		actualLen = uint32(len(buf))
	}
	return string(buf[:actualLen])
}

// dispatchScheduleCron handles (string,string,string,string) -> string.
func (b *wasmtimeBackend) dispatchScheduleCron(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 4 || b.handler == nil {
		return nil
	}
	workflowName := readStrArg(args, 0, nargs)
	cronExpr := readStrArg(args, 1, nargs)
	timezone := readStrArg(args, 2, nargs)
	input := readStrArg(args, 3, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.ScheduleCron(ctxWithMem(context.Background(), buf), nil,
		workflowName, cronExpr, timezone, input, 0, 65536)
	// result<string, call-failure>, not a bare string: decodeCallOutcome tests
	// the stop sentinel by mask BEFORE reading any field, so a refusal reaches
	// the component guest as the `suspended` case rather than as a response the
	// service could also have produced. setResultString discarded those bits.
	setResultCallOutcome(results, nresults,
		decodeCallOutcome(packed, buf, extractStringFromSimplePacked))
	return nil
}

// dispatchDeleteCron handles (string) -> void, per the WIT signature
// (durable-delete-cron has no return type -- the only void host call in
// cleat.wit). DeleteCron (engine/schedules.go) writes no output buffer, only
// an errCode via packSimpleResult, so there is nothing to extract. results
// still gets a best-effort u64 write: setResultU64 already no-ops when
// nresults is 0, which is what wasmtime asks for on a component function
// that returns nothing, so this stays the same shape as every other
// dispatch* method here instead of special-casing void.
func (b *wasmtimeBackend) dispatchDeleteCron(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil {
		return nil
	}
	scheduleID := readStrArg(args, 0, nargs)
	r := b.handler.DeleteCron(context.Background(), nil, scheduleID)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchListCrons handles () -> string.
func (b *wasmtimeBackend) dispatchListCrons(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if b.handler == nil {
		return nil
	}

	buf := make([]byte, 65536)
	packed := b.handler.ListCrons(ctxWithMem(context.Background(), buf), nil, 0, 65536)
	response := extractStringFromSimplePacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// ---------------------------------------------------------------------------
// deprecated fallback
// ---------------------------------------------------------------------------

// dispatchComponentDefault is the deprecated fallback for unregistered functions.
// It returns 0 for all results.
func (b *wasmtimeBackend) dispatchComponentDefault(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	setResultU64(results, nresults, 0)
	return nil
}

// ---------------------------------------------------------------------------
// wasi:clocks/wall-clock and wasi:random/random  (cleat#1410)
// ---------------------------------------------------------------------------

// dispatchWasiWallClockNow handles wasi:clocks/wall-clock#now() -> datetime.
//
// Backed by the same durable clock cleat's own `durable-now` returns, so a
// Python guest's time.time() and its h.now() agree and a replay reproduces
// both. Before this a guest read the HOST's clock: measured, two executions of
// one workflow id with the durable clock pinned returned 1.7895622924e9 and
// 1.7895622951e9 seconds while h.now() returned the pinned value both times.
func (b *wasmtimeBackend) dispatchWasiWallClockNow(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if b.handler == nil {
		return nil
	}
	setResultDatetime(results, nresults, b.handler.Now(context.Background()))
	return nil
}

// dispatchWasiWallClockResolution handles
// wasi:clocks/wall-clock#resolution() -> datetime.
//
// One millisecond, because that is the durable clock's unit. Shadowed rather
// than left to wasmtime for the same reason as now(): the real one reports the
// HOST's resolution, which is a property of the machine a workflow happens to
// be replayed on. A guest that scales a measurement by the reported resolution
// would then compute different numbers on two workers.
func (b *wasmtimeBackend) dispatchWasiWallClockResolution(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	setResultDatetime(results, nresults, 1)
	return nil
}

// dispatchWasiRandomU64 handles wasi:random/random#get-random-u64() -> u64.
func (b *wasmtimeBackend) dispatchWasiRandomU64(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if b.handler == nil {
		return nil
	}
	setResultU64(results, nresults, uint64(b.handler.Random(context.Background())))
	return nil
}

// maxWasiRandomBytes bounds get-random-bytes.
//
// A component list is a vec of wasmtime_component_val_t -- one 16-byte val per
// byte returned, not a byte buffer -- so a guest asking for 1 GiB would ask the
// host for 16. The guest supplies the length, so this is a host-side bound on a
// guest-controlled allocation rather than a policy about entropy.
//
// 1 MiB is far above any real use: CPython seeds its Mersenne Twister from
// 2500 bytes and os.urandom callers ask for tens.
const maxWasiRandomBytes = 1 << 20

// dispatchWasiRandomBytes handles
// wasi:random/random#get-random-bytes(len: u64) -> list<u8>.
//
// THIS IS THE ONE THAT MATTERS AND IT IS NOT THE OBVIOUS ONE. CPython seeds
// random.random()'s Mersenne Twister from os.urandom at import, so a guest
// whose get-random-bytes is still live has a non-deterministic
// random.random() -- while get-random-u64, the function that looks like the
// RNG, is shadowed and every check of it passes. Shadowing the scalar alone is
// a slice that compiles, registers, and achieves nothing observable.
//
// The bytes come from the same seeded source as cleat's durable-random, drawn
// eight at a time, so they reproduce on replay for the same reason it does.
func (b *wasmtimeBackend) dispatchWasiRandomBytes(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if b.handler == nil {
		return nil
	}
	if int(nargs) < 1 {
		return nil
	}
	requested := readU64Arg(args, 0, nargs)
	n := int(requested)
	// `int` is 64-bit here, so a length above 2^63 arrives NEGATIVE rather
	// than large. Both bounds are needed and the negative one is the easy
	// mistake -- `n > max` alone lets 2^63 through as a negative length.
	if n < 0 || n > maxWasiRandomBytes {
		n = maxWasiRandomBytes
		// TRUNCATION IS NEVER SILENT. A short read of an entropy buffer is the
		// kind of thing that behaves for years and then does not, so it is
		// logged even though no real guest reaches it: CPython seeds from 2500
		// bytes and os.urandom callers ask for tens.
		//
		// TRAPPING WOULD BE MORE HONEST AND IS NOT DONE HERE. A callback
		// reports failure by returning a wasmtime_error_t, and no dispatcher
		// in this package has ever returned one -- so it would be the first
		// use of wasmtime_error_new in the tree, with ownership semantics
		// nothing here establishes and no way to exercise it from a real
		// guest. Recorded rather than silently preferred; if a guest is ever
		// found asking for more than this, trap instead of raising the bound.
		slog.Warn("wasi get-random-bytes truncated",
			"requested", requested, "returned", n)
	}
	out := make([]byte, n)
	ctx := context.Background()
	for i := 0; i < n; i += 8 {
		var word [8]byte
		binary.BigEndian.PutUint64(word[:], uint64(b.handler.Random(ctx)))
		copy(out[i:], word[:])
	}
	setResultListU8(results, nresults, out)
	return nil
}

// -- WIT module/function -> cbType map ---------------------------------------

// witTypeMap maps WIT module-name / function-name pairs to their cbType,
// used by registerCleatComponentImports to register each import correctly.
var witTypeMap = map[string]map[string]cbType{
	"cleat:host-calls/durable-call": {
		"durable-call":           cbTypeDurableCallString,
		"durable-call-retry":     cbTypeDurableCallRetry,
		"durable-call-heartbeat": cbTypeDurableCallHeartbeat,
	},
	"cleat:host-calls/durable-sleep": {
		"durable-sleep":  cbTypeDurableSleep,
		"durable-now":    cbTypeNow,
		"durable-random": cbTypeRandom,
		"durable-log":    cbTypeDurableLog,
	},
	"cleat:host-calls/durable-version": {
		"durable-version":     cbTypeVersion,
		"durable-min-version": cbTypeMinVersion,
	},
	"cleat:host-calls/durable-lifecycle": {
		"durable-defer":             cbTypeDurableDefer,
		"durable-continue-as-new":   cbTypeContinueAsNew,
		"durable-poll-cancellation": cbTypePollCancellation,
		"durable-defer-phase":       cbTypeDeferPhase,
	},
	"cleat:host-calls/durable-signals": {
		"durable-await-signals":   cbTypeAwaitSignals,
		"durable-poll-signal":     cbTypePollSignal,
		"durable-signal-workflow": cbTypeSignalWorkflow,
	},
	"cleat:host-calls/durable-children": {
		"durable-child-workflow":              cbTypeChildWorkflow,
		"durable-await-child":                 cbTypeAwaitChild,
		"durable-await-all-children":          cbTypeAwaitAllChildren,
		"durable-poll-child":                  cbTypePollChild,
		"durable-await-any-child":             cbTypeAwaitAnyChild,
		"durable-child-workflow-with-options": cbTypeChildWorkflowWithOptions,
	},
	"cleat:host-calls/durable-promises": {
		"durable-create-promise":  cbTypeCreatePromise,
		"durable-await-promise":   cbTypeAwaitPromise,
		"durable-resolve-promise": cbTypeResolvePromise,
		"durable-reject-promise":  cbTypeRejectPromise,
	},
	"cleat:host-calls/durable-state": {
		"set-query-state": cbTypeSetQueryState,
	},
	"cleat:host-calls/durable-handlers": {
		"durable-register-update-handler": cbTypeRegisterUpdateHandler,
		"durable-poll-update":             cbTypePollUpdate,
		"durable-complete-update":         cbTypeCompleteUpdate,
		"durable-register-query-handler":  cbTypeRegisterQueryHandler,
	},
	"cleat:host-calls/durable-messaging": {
		"durable-send":            cbTypeDurableSend,
		"durable-schedule-invoke": cbTypeScheduleInvoke,
	},
	"cleat:host-calls/durable-identity": {
		"durable-workflow-id": cbTypeWorkflowID,
		"durable-run-id":      cbTypeRunID,
	},
	"cleat:host-calls/plugin": {
		"plugin-call":           cbTypePluginCall,
		"plugin-call-streaming": cbTypePluginCallStreaming,
	},
	"cleat:host-calls/durable-lock": {
		"durable-acquire-lock": cbTypeAcquireLock,
		"durable-release-lock": cbTypeReleaseLock,
	},
	"cleat:host-calls/durable-scope": {
		"set-scope": cbTypeSetScope,
		"get-scope": cbTypeGetScope,
		"uuid":      cbTypeUUID,
	},
	"cleat:host-calls/durable-extended-lifecycle": {
		"continue-as-new-versioned": cbTypeContinueAsNewVersioned,
		"side-effect":               cbTypeSideEffect,
		"durable-run-detached":      cbTypeRunDetached,
	},
	"cleat:host-calls/durable-fetch": {
		"fetch": cbTypeFetch,
	},
	"cleat:host-calls/durable-cron": {
		"durable-schedule-cron": cbTypeScheduleCron,
		"durable-delete-cron":   cbTypeDeleteCron,
		"durable-list-crons":    cbTypeListCrons,
	},
}

// -- register cleat WIT functions in component linker -------------------------

// wasiDeterminismVersion is the WASI Preview 2 version componentize-py's
// guests import.
//
// IT IS PART OF THE NAME, AND A WRONG ONE FAILS SILENTLY. The linker matches
// the import name EXACTLY, so registering `wasi:clocks/wall-clock@0.2.0`
// against a guest importing `@0.2.9` shadows nothing, returns no error, and
// leaves the guest on the host's real clock -- with every "registration
// succeeded" check passing. That is the empty green cleat#1410 is named after.
//
// It is also a property of the componentize-py BUILD rather than of cleat, so
// a toolchain bump un-shadows a working implementation with nothing failing.
// TestAPythonGuestsWasiImportsAreAllShadowed is what makes that loud: it walks
// a freshly compiled guest's import section and fails if it imports a
// wall-clock or random interface this list does not name.
const wasiDeterminismVersion = "@0.2.9"

// wasiDeterminismInterfaces are the WASI interfaces cleat shadows so a
// component guest's clock and entropy are the workflow's rather than the
// host's.
//
// NOT monotonic-clock, deliberately. cleat#1386 measured why: realtime is the
// workflow's and may jump, monotonic is the runtime's and must advance
// smoothly, and a durable-sourced monotonic clock ran 3 GC cycles instead of
// 17 and reached a 256 MB heap against a 32 MB limit. It is also the interface
// whose functions return resource-typed pollables, so leaving it alone is both
// correct and the cheap option -- which is a coincidence worth stating, since
// the cheap reason would otherwise look like the whole reason.
//
// The two `insecure` interfaces are included because CPython does not promise
// which it uses and "insecure" is a statement about entropy quality, not about
// determinism. A guest reaching randomness through either must replay.
var wasiDeterminismInterfaces = map[string]map[string]cbType{
	"wasi:clocks/wall-clock" + wasiDeterminismVersion: {
		"now":        cbTypeWasiWallClockNow,
		"resolution": cbTypeWasiWallClockResolution,
	},
	"wasi:random/random" + wasiDeterminismVersion: {
		"get-random-bytes": cbTypeWasiRandomBytes,
		"get-random-u64":   cbTypeWasiRandomU64,
	},
	"wasi:random/insecure" + wasiDeterminismVersion: {
		"get-insecure-random-bytes": cbTypeWasiRandomBytes,
		"get-insecure-random-u64":   cbTypeWasiRandomU64,
	},
	"wasi:random/insecure-seed" + wasiDeterminismVersion: {
		"insecure-seed": cbTypeWasiRandomU64,
	},
}

// registerWasiDeterminismImports shadows the WASI interfaces a guest would
// otherwise satisfy from the host's real clock and entropy.
//
// MUST RUN AFTER add_wasip2 AND INSIDE THE allow_shadowing BRACKET. Both are
// already true at the one call site; this comment exists because the ordering
// is invisible at a glance and reversing it produces no error, only a guest
// that keeps the real clock.
func (b *wasmtimeBackend) registerWasiDeterminismImports(linker *C.wasmtime_component_linker_t) error {
	root := C.wasmtime_component_linker_root(linker)
	if root == nil {
		return fmt.Errorf("component linker root is nil")
	}
	for iface, funcs := range wasiDeterminismInterfaces {
		nameBytes := []byte(iface)
		var sub *C.wasmtime_component_linker_instance_t
		if err := C.wasmtime_component_linker_instance_add_instance(
			root, (*C.char)(unsafe.Pointer(&nameBytes[0])), C.size_t(len(iface)), &sub); err != nil {
			return fmt.Errorf("shadow %s: %s", iface, componentErrorMessage(err))
		}
		for fnName, fnType := range funcs {
			fnBytes := []byte(fnName)
			cbID := registerCB(b, fnType)
			if err := C.wasmtime_component_linker_instance_add_func(
				sub, (*C.char)(unsafe.Pointer(&fnBytes[0])), C.size_t(len(fnName)),
				C.wasmtime_component_func_callback_t(C.goComponentCallback),
				C.cbid_as_env(C.uintptr_t(cbID)), nil); err != nil {
				return fmt.Errorf("shadow %s.%s: %s", iface, fnName, componentErrorMessage(err))
			}
		}
	}
	return nil
}

// componentErrorMessage extracts and frees a wasmtime error's message.
func componentErrorMessage(err *C.wasmtime_error_t) string {
	var msg C.wasm_byte_vec_t
	C.get_error_message(err, &msg)
	s := C.GoStringN(msg.data, C.int(msg.size))
	C.wasm_byte_vec_delete(&msg)
	C.wasmtime_error_delete(err)
	return s
}

func (b *wasmtimeBackend) registerCleatComponentImports(linker *C.wasmtime_component_linker_t) error {
	root := C.wasmtime_component_linker_root(linker)
	if root == nil {
		return fmt.Errorf("component linker root is nil")
	}
	for witModule, funcs := range wasm.WitToEnvImport {
		nameBytes := []byte(witModule)
		var namePtr *C.char
		if len(nameBytes) > 0 {
			namePtr = (*C.char)(unsafe.Pointer(&nameBytes[0]))
		}
		var sub *C.wasmtime_component_linker_instance_t
		err := C.wasmtime_component_linker_instance_add_instance(root, namePtr, C.size_t(len(witModule)), &sub)
		if err != nil {
			var msg C.wasm_byte_vec_t
			C.get_error_message(err, &msg)
			s := C.GoStringN(msg.data, C.int(msg.size))
			C.wasm_byte_vec_delete(&msg)
			C.wasmtime_error_delete(err)
			return fmt.Errorf("register %s: %s", witModule, s)
		}
		for witFuncName := range funcs {
			// Look up the cbType from witTypeMap, falling back to cbTypeDefault.
			fnType := cbTypeDefault
			if moduleTypes, ok := witTypeMap[witModule]; ok {
				if t, ok := moduleTypes[witFuncName]; ok {
					fnType = t
				}
			}
			fnBytes := []byte(witFuncName)
			var fnPtr *C.char
			if len(fnBytes) > 0 {
				fnPtr = (*C.char)(unsafe.Pointer(&fnBytes[0]))
			}
			cbID := registerCB(b, fnType)
			err := C.wasmtime_component_linker_instance_add_func(sub, fnPtr, C.size_t(len(witFuncName)), C.wasmtime_component_func_callback_t(C.goComponentCallback), C.cbid_as_env(C.uintptr_t(cbID)), nil)
			if err != nil {
				var msg C.wasm_byte_vec_t
				C.get_error_message(err, &msg)
				s := C.GoStringN(msg.data, C.int(msg.size))
				C.wasm_byte_vec_delete(&msg)
				C.wasmtime_error_delete(err)
				return fmt.Errorf("register %s.%s: %s", witModule, witFuncName, s)
			}
		}
	}
	return nil
}

// -- ExecuteComponentCGo ------------------------------------------------------
