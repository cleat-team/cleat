//go:build cgo && wasmtime_component_cgo

// See the comment at the top of component_cgo.go: this file is part of the
// opt-in native wasmtime Component Model C API fast path, gated behind the
// wasmtime_component_cgo build tag because the required `-I` to wasmtime's
// C headers cannot be derived portably in a #cgo directive.

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
// extern wasmtime_error_t *goComponentCallback(
//     void *env, wasmtime_context_t *ctx,
//     wasmtime_component_func_type_t *ty,
//     wasmtime_component_val_t *args, size_t nargs,
//     wasmtime_component_val_t *results, size_t nresults);
import "C"
import (
	"context"
	"fmt"
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
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
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
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
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
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
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
	response := extractStringFromPacked(packed, buf)

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
	response := extractStringFromPacked(packed, buf)

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
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// dispatchSendSignalAndWait handles (string,string,string,u64) -> string.
func (b *wasmtimeBackend) dispatchSendSignalAndWait(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 4 || b.handler == nil {
		return nil
	}
	target := readStrArg(args, 0, nargs)
	sigName := readStrArg(args, 1, nargs)
	payload := readStrArg(args, 2, nargs)
	timeoutMs := int64(readU64Arg(args, 3, nargs))

	buf := make([]byte, 65536)
	packed := b.handler.SendSignalAndWait(ctxWithMem(context.Background(), buf), nil,
		target, sigName, payload, timeoutMs, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// dispatchReplyToSignal handles (string,string) -> u64.
func (b *wasmtimeBackend) dispatchReplyToSignal(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 2 || b.handler == nil {
		return nil
	}
	correlationID := readStrArg(args, 0, nargs)
	response := readStrArg(args, 1, nargs)
	r := b.handler.ReplyToSignal(context.Background(), nil, correlationID, response)
	setResultU64(results, nresults, uint64(r))
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
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
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
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
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
	response := extractStringFromPacked(packed, buf)

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
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
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
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// dispatchAwaitPromise handles (string,u64) -> string.
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
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
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
	response := extractStringFromPacked(packed, buf)

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
	response := extractStringFromPacked(packed, buf)

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
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
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
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
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
	response := extractStringFromPacked(packed, buf)

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
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// ---------------------------------------------------------------------------
// durable-stream-state interface
// ---------------------------------------------------------------------------

// dispatchSetState handles (string,string) -> u64.
func (b *wasmtimeBackend) dispatchSetState(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 2 || b.handler == nil {
		return nil
	}
	key := readStrArg(args, 0, nargs)
	val := readStrArg(args, 1, nargs)
	r := b.handler.SetState(context.Background(), nil, key, val)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchGetState handles (string) -> string.
func (b *wasmtimeBackend) dispatchGetState(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil {
		return nil
	}
	key := readStrArg(args, 0, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.GetState(ctxWithMem(context.Background(), buf), nil, key, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// dispatchDeleteState handles (string) -> u64.
func (b *wasmtimeBackend) dispatchDeleteState(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil {
		return nil
	}
	key := readStrArg(args, 0, nargs)
	r := b.handler.DeleteState(context.Background(), nil, key)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchIncrState handles (string,u64) -> u64.
func (b *wasmtimeBackend) dispatchIncrState(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 2 || b.handler == nil {
		return nil
	}
	key := readStrArg(args, 0, nargs)
	delta := int64(readU64Arg(args, 1, nargs))
	r := b.handler.IncrState(context.Background(), nil, key, delta)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchHasState handles (string) -> u64.
func (b *wasmtimeBackend) dispatchHasState(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil {
		return nil
	}
	key := readStrArg(args, 0, nargs)
	r := b.handler.HasState(context.Background(), nil, key)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchListState handles (string) -> string.
func (b *wasmtimeBackend) dispatchListState(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil {
		return nil
	}
	prefix := readStrArg(args, 0, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.ListState(ctxWithMem(context.Background(), buf), nil, prefix, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// ---------------------------------------------------------------------------
// durable-extended-lifecycle interface
// ---------------------------------------------------------------------------

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
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// ---------------------------------------------------------------------------
// durable-extended-children interface
// ---------------------------------------------------------------------------

// dispatchChildWorkflowInSchema handles (string,string,string,u64,u64,string) -> string.
func (b *wasmtimeBackend) dispatchChildWorkflowInSchema(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 6 || b.handler == nil {
		return nil
	}
	schema := readStrArg(args, 0, nargs)
	name := readStrArg(args, 1, nargs)
	input := readStrArg(args, 2, nargs)
	version := int64(readU64Arg(args, 3, nargs))
	priority := int64(readU64Arg(args, 4, nargs))
	policy := readStrArg(args, 5, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.ChildWorkflowInSchema(ctxWithMem(context.Background(), buf), nil,
		schema, name, input, version, priority, policy, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
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
	response := extractStringFromPacked(packed, buf)

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
	},
	"cleat:host-calls/durable-signals": {
		"durable-await-signals":        cbTypeAwaitSignals,
		"durable-poll-signal":          cbTypePollSignal,
		"durable-send-signal-and-wait": cbTypeSendSignalAndWait,
		"durable-reply-to-signal":      cbTypeReplyToSignal,
		"durable-signal-workflow":      cbTypeSignalWorkflow,
	},
	"cleat:host-calls/durable-children": {
		"durable-child-workflow":              cbTypeChildWorkflow,
		"durable-await-child":                 cbTypeAwaitChild,
		"durable-await-all-children":          cbTypeAwaitAllChildren,
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
	"cleat:host-calls/durable-stream-state": {
		"set-state":    cbTypeSetState,
		"get-state":    cbTypeGetState,
		"delete-state": cbTypeDeleteState,
		"incr-state":   cbTypeIncrState,
		"has-state":    cbTypeHasState,
		"list-state":   cbTypeListState,
	},
	"cleat:host-calls/durable-extended-lifecycle": {
		"continue-as-new-versioned": cbTypeContinueAsNewVersioned,
		"side-effect":               cbTypeSideEffect,
	},
	"cleat:host-calls/durable-extended-children": {
		"child-workflow-in-schema": cbTypeChildWorkflowInSchema,
	},
	"cleat:host-calls/durable-fetch": {
		"fetch": cbTypeFetch,
	},
}

// -- register cleat WIT functions in component linker -------------------------

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
			err := C.wasmtime_component_linker_instance_add_func(sub, fnPtr, C.size_t(len(witFuncName)), C.wasmtime_component_func_callback_t(C.goComponentCallback), unsafe.Pointer(uintptr(cbID)), nil)
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
