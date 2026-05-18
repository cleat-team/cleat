//go:build cgo

package host

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

// Compile-time check: wasmtimeBackend implements WasmBackend.
var _ WasmBackend = (*wasmtimeBackend)(nil)

// wasmtimeBackend implements WasmBackend using the wasmtime WASM runtime.
// It loads core WASM modules (post-decompose Component Model binaries).
//
// Build constraint: this file requires CGO because wasmtime-go wraps the
// wasmtime Rust runtime via CGo.
type wasmtimeBackend struct {
	engine *wasmtime.Engine
}

// NewWasmtimeBackend creates a new wasmtimeBackend with a fresh engine.
func NewWasmtimeBackend(ctx context.Context) (*wasmtimeBackend, error) {
	engine := wasmtime.NewEngine()
	return &wasmtimeBackend{engine: engine}, nil
}

// Name returns "wasmtime" for diagnostics.
func (b *wasmtimeBackend) Name() string { return "wasmtime" }

// Close releases the wasmtime engine resources.
func (b *wasmtimeBackend) Close(ctx context.Context) error {
	b.engine.Close()
	return nil
}

// Execute compiles, instantiates, and runs a core WASM module via wasmtime.
//
// The session provides the HostHandler for all host function calls. The
// wasmtime backend registers flat "env" module imports matching the names
// produced by component decomposition (e.g. cleat_call, cleat_sleep).
//
// Like the wazero backend, it reserves scratch space in linear memory at a
// high offset (10 MB) for input/output buffers and uses the same encoding
// conventions for export return values.
func (b *wasmtimeBackend) Execute(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, session HostHandler) (*ExecResult, error) {
	// Create per-execution store with WASI configuration.
	store := wasmtime.NewStore(b.engine)
	defer store.Close()

	// Configure WASI for Go wasip1 module support.
	// The module may need WASI for stack/goroutine management even though we
	// override time/random functions for determinism.
	wasiConfig := wasmtime.NewWasiConfig()
	wasiConfig.InheritStderr()
	store.SetWasi(wasiConfig)

	// Wrap context so host functions can find the session.
	ctx = withHandler(ctx, session)

	// Compile the WASM module.
	module, err := wasmtime.NewModule(b.engine, wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("host: compile: %w", err)
	}
	defer module.Close()

	// Create linker and register host functions.
	linker := wasmtime.NewLinker(b.engine)
	if err := b.registerWasiStubs(linker); err != nil {
		return nil, fmt.Errorf("host: register WASI stubs: %w", err)
	}
	if err := b.registerEnvStubs(linker); err != nil {
		return nil, fmt.Errorf("host: register env stubs: %w", err)
	}
	if err := b.registerTeavmStubs(linker); err != nil {
		return nil, fmt.Errorf("host: register teavm stubs: %w", err)
	}

	// Register cleat_* host functions. We use a closure-based approach:
	// each function captures a result/error holder so that cleat_complete
	// can store the workflow result and the Execute method can retrieve
	// it even when the module subsequently traps (e.g. via proc_exit).
	var completeResult, completeErr string

	if err := b.registerCleatCall(linker, &completeResult, &completeErr); err != nil {
		return nil, fmt.Errorf("host: register cleat_call: %w", err)
	}
	if err := b.registerCleatSleep(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_sleep: %w", err)
	}
	if err := b.registerCleatNow(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_now: %w", err)
	}
	if err := b.registerCleatRandom(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_random: %w", err)
	}
	if err := b.registerCleatLog(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_log: %w", err)
	}
	if err := b.registerCleatVersion(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_version: %w", err)
	}
	if err := b.registerCleatMinVersion(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_min_version: %w", err)
	}
	if err := b.registerCleatDefer(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_defer: %w", err)
	}
	if err := b.registerCleatPollCancellation(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_poll_cancellation: %w", err)
	}
	if err := b.registerCleatPollSignal(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_poll_signal: %w", err)
	}
	if err := b.registerCleatContinueAsNew(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_continue_as_new: %w", err)
	}
	if err := b.registerCleatContinueAsNewVersioned(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_continue_as_new_versioned: %w", err)
	}
	if err := b.registerCleatChildWorkflow(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_child_workflow: %w", err)
	}
	if err := b.registerCleatChildWorkflowWithOptions(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_child_workflow_with_options: %w", err)
	}
	if err := b.registerCleatChildWorkflowInSchema(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_child_workflow_in_schema: %w", err)
	}
	if err := b.registerCleatAwaitChild(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_await_child: %w", err)
	}
	if err := b.registerCleatAwaitAllChildren(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_await_all_children: %w", err)
	}
	if err := b.registerCleatCallRetry(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_call_retry: %w", err)
	}
	if err := b.registerCleatAwaitSignals(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_await_signals: %w", err)
	}
	if err := b.registerCleatSetQueryState(linker); err != nil {
		return nil, fmt.Errorf("host: register set_query_state: %w", err)
	}
	if err := b.registerCleatCallHeartbeat(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_call_heartbeat: %w", err)
	}
	if err := b.registerCleatPluginCall(linker); err != nil {
		return nil, fmt.Errorf("host: register plugin_call: %w", err)
	}
	if err := b.registerCleatPluginCallStreaming(linker); err != nil {
		return nil, fmt.Errorf("host: register plugin_call_streaming: %w", err)
	}
	if err := b.registerCleatRegisterUpdateHandler(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_register_update_handler: %w", err)
	}
	if err := b.registerCleatCreatePromise(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_create_promise: %w", err)
	}
	if err := b.registerCleatAwaitPromise(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_await_promise: %w", err)
	}
	if err := b.registerCleatSendSignalAndWait(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_send_signal_and_wait: %w", err)
	}
	if err := b.registerCleatReplyToSignal(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_reply_to_signal: %w", err)
	}
	if err := b.registerCleatSignalWorkflow(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_signal_workflow: %w", err)
	}
	if err := b.registerCleatSetScope(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_set_scope: %w", err)
	}
	if err := b.registerCleatGetScope(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_get_scope: %w", err)
	}
	if err := b.registerCleatUUID(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_uuid: %w", err)
	}
	if err := b.registerCleatAcquireLock(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_acquire_lock: %w", err)
	}
	if err := b.registerCleatReleaseLock(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_release_lock: %w", err)
	}
	if err := b.registerCleatSideEffect(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_side_effect: %w", err)
	}
	if err := b.registerCleatWorkflowID(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_workflow_id: %w", err)
	}
	if err := b.registerCleatRunID(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_run_id: %w", err)
	}
	if err := b.registerCleatResolvePromise(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_resolve_promise: %w", err)
	}
	if err := b.registerCleatRejectPromise(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_reject_promise: %w", err)
	}
	if err := b.registerCleatSend(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_send: %w", err)
	}
	if err := b.registerCleatScheduleInvoke(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_schedule_invoke: %w", err)
	}
	if err := b.registerCleatRegisterQueryHandler(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_register_query_handler: %w", err)
	}
	if err := b.registerCleatRunDetached(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_run_detached: %w", err)
	}
	if err := b.registerCleatSetState(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_set_state: %w", err)
	}
	if err := b.registerCleatGetState(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_get_state: %w", err)
	}
	if err := b.registerCleatDeleteState(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_delete_state: %w", err)
	}
	if err := b.registerCleatIncrState(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_incr_state: %w", err)
	}
	if err := b.registerCleatHasState(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_has_state: %w", err)
	}
	if err := b.registerCleatListState(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_list_state: %w", err)
	}
	if err := b.registerCleatFetch(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_fetch: %w", err)
	}
	if err := b.registerCleatComplete(linker, &completeResult, &completeErr); err != nil {
		return nil, fmt.Errorf("host: register cleat_complete: %w", err)
	}

	// Instantiate the module.
	instance, err := linker.Instantiate(store, module)
	if err != nil {
		return nil, fmt.Errorf("host: instantiate: %w", err)
	}

	// Get exported memory.
	memory := instance.GetExport(store, "memory")
	if memory == nil {
		return nil, fmt.Errorf("host: module has no exported memory")
	}
	mem := memory.Memory()
	if mem == nil {
		return nil, fmt.Errorf("host: memory export is not a memory")
	}

	// Place scratch buffers at the end of current WASM memory to avoid
	// collision with the module's heap, but never below the legacy 10 MiB
	// offset. Some WASM SDKs (Java/TeaVM, AssemblyScript) hardcode the
	// 10 MiB convention and will break if the buffer moves lower.
	outBufSz := OutBufSize // 1 MB default, configurable
	const legacyOffset = uint32(10 * 1024 * 1024) // 10 MiB

	currentSize := uint64(mem.DataSize(store))
	scratchBase := uint32(currentSize + wasmPageSize) // one guard page after current heap
	if scratchBase < legacyOffset {
		scratchBase = legacyOffset
	}
	inputOffset := scratchBase
	outputOffset := scratchBase + outBufSz

	// Grow memory if needed to fit the scratch region.
	needed := uint64(outputOffset + outBufSz)
	if currentSize < needed {
		pagesNeeded := (needed - currentSize + wasmPageSize - 1) / wasmPageSize
		if _, err := mem.Grow(store, pagesNeeded); err != nil {
			return nil, fmt.Errorf("host: grow memory: %w", err)
		}
	}

	// Write input JSON into WASM linear memory.
	inputBytes := []byte(input)
	if len(inputBytes) > 0 {
		data := mem.UnsafeData(store)
		if uint64(inputOffset)+uint64(len(inputBytes)) > uint64(len(data)) {
			return nil, fmt.Errorf("host: input exceeds memory bounds")
		}
		copy(data[inputOffset:], inputBytes)
	}

	// If the module exports _start (Go wasip1), call it in the background
	// to initialize the Go runtime.
	if startFn := instance.GetFunc(store, "_start"); startFn != nil {
		started := make(chan struct{})
		go func() {
			close(started)
			startFn.Call(store)
		}()
		// Wait for the goroutine to begin executing.
		<-started

		// Wait for memory to become accessible (signals runtime init complete).
		delay := 100 * time.Microsecond
		const maxDelay = 10 * time.Millisecond
		for {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
			if mem.DataSize(store) > 0 {
				break
			}
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}

	// Get the entry point export.
	fn := instance.GetFunc(store, entryPoint)
	if fn == nil {
		return nil, fmt.Errorf("host: export %q not found", entryPoint)
	}

	// Call the entry point. The return value is a single i64 encoding
	// the error code (low 32 bits) and output length (high 32 bits).
	results, callErr := fn.Call(store, int32(inputOffset), int32(len(inputBytes)), int32(outputOffset), int32(outBufSz))

	// Check for a result delivered via cleat_complete before treating
	// a trap/proc_exit as an error.
	if completeErr != "" {
		return nil, fmt.Errorf("host: export %q failed: %s", entryPoint, completeErr)
	}
	if completeResult != "" {
		return &ExecResult{Result: completeResult, Suspended: false}, nil
	}

	if callErr != nil {
		return nil, fmt.Errorf("host: export %q: %w", entryPoint, callErr)
	}

	if results == nil {
		return nil, fmt.Errorf("host: export %q returned no results", entryPoint)
	}

	// Decode the packed int64 result.
	raw, ok := results.(int64)
	if !ok {
		return nil, fmt.Errorf("host: export %q returned non-int64 result", entryPoint)
	}

	// Check for the suspend sentinel: (1 << 62).
	if raw == (1 << 62) {
		return &ExecResult{Suspended: true}, nil
	}

	errCode, actualLen := decodeExportResult(uint64(raw))

	// Read output from linear memory.
	data := mem.UnsafeData(store)
	if actualLen > outBufSz {
		return nil, fmt.Errorf("host: export %q: output overflow: wrote %d bytes, buffer is %d bytes", entryPoint, actualLen, outBufSz)
	}
	outputStr := string(data[outputOffset : outputOffset+actualLen])

	if errCode != 0 {
		return nil, fmt.Errorf("host: export %q: %s", entryPoint, outputStr)
	}

	return &ExecResult{Result: outputStr, Suspended: false}, nil
}

// ---------------------------------------------------------------------------
// Helper: raw memory read/write on a []byte slice
// ---------------------------------------------------------------------------

// wasmtimeReadString reads a string from a raw WASM linear memory buffer.
func wasmtimeReadString(buf []byte, ptr, length uint32) string {
	if length == 0 {
		return ""
	}
	if uint64(ptr)+uint64(length) > uint64(len(buf)) {
		return ""
	}
	return string(buf[ptr : ptr+length])
}

// wasmtimeReadStringValidated reads and validates a string from a raw buffer.
func wasmtimeReadStringValidated(buf []byte, ptr, length, maxLen uint32) (string, bool) {
	if length == 0 || length > maxLen {
		return "", false
	}
	if uint64(ptr)+uint64(length) > uint64(len(buf)) {
		return "", false
	}
	return string(buf[ptr : ptr+length]), true
}

// wasmtimeReadServiceName reads and validates a service/operation name.
func wasmtimeReadServiceName(buf []byte, ptr, length uint32) (string, bool) {
	s, ok := wasmtimeReadStringValidated(buf, ptr, length, MaxWasmStringLen)
	if !ok {
		return "", false
	}
	if !validServiceName(s) {
		return "", false
	}
	return s, true
}

// wasmtimeWriteString writes a string into a raw WASM linear memory buffer.
func wasmtimeWriteString(buf []byte, ptr uint32, s string, maxLen uint32) (uint32, error) {
	data := []byte(s)
	if uint32(len(data)) > maxLen {
		data = data[:maxLen]
	}
	if len(data) > 0 {
		if uint64(ptr)+uint64(len(data)) > uint64(len(buf)) {
			return 0, fmt.Errorf("wasmtimeWriteString: write %d bytes at ptr %d exceeds buffer", len(data), ptr)
		}
		copy(buf[ptr:], data)
	}
	return uint32(len(data)), nil
}

// ---------------------------------------------------------------------------
// Stub registrations
// ---------------------------------------------------------------------------

// registerWasiStubs registers WASI preview1 stubs needed by core modules.
func (b *wasmtimeBackend) registerWasiStubs(linker *wasmtime.Linker) error {
	// reset_adapter_state is required by core modules extracted from Component
	// Model binaries produced by componentize-py. It is a no-op.
	if err := linker.FuncWrap("wasi_snapshot_preview1", "reset_adapter_state",
		func(ctx context.Context) {},
	); err != nil {
		return err
	}
	return nil
}

// registerEnvStubs registers no-op stubs for optional "env" imports.
func (b *wasmtimeBackend) registerEnvStubs(linker *wasmtime.Linker) error {
	// abort is required by AssemblyScript-compiled WASM modules.
	if err := linker.FuncWrap("env", "abort",
		func(ctx context.Context, msg, file, line, col uint32) {},
	); err != nil {
		// Some modules may not import abort; this is not fatal.
		if isWasmtimeLinkerError(err) {
			return err
		}
	}
	return nil
}

// registerTeavmStubs registers no-op stubs for TeaVM-compiled Java modules.
func (b *wasmtimeBackend) registerTeavmStubs(linker *wasmtime.Linker) error {
	// putwcharsOut
	if err := linker.FuncWrap("teavm", "putwcharsOut",
		func(ctx context.Context, chars, count uint32) {},
	); err != nil {
		if isWasmtimeLinkerError(err) {
			return err
		}
	}
	// currentTimeMillis
	if err := linker.FuncWrap("teavm", "currentTimeMillis",
		func(ctx context.Context) float64 { return 0 },
	); err != nil {
		if isWasmtimeLinkerError(err) {
			return err
		}
	}
	// logString
	if err := linker.FuncWrap("teavm", "logString",
		func(ctx context.Context, ptr uint32) {},
	); err != nil {
		if isWasmtimeLinkerError(err) {
			return err
		}
	}
	// logInt
	if err := linker.FuncWrap("teavm", "logInt",
		func(ctx context.Context, ptr uint32) {},
	); err != nil {
		if isWasmtimeLinkerError(err) {
			return err
		}
	}
	// logOutOfMemory
	if err := linker.FuncWrap("teavm", "logOutOfMemory",
		func(ctx context.Context) {},
	); err != nil {
		if isWasmtimeLinkerError(err) {
			return err
		}
	}
	return nil
}

// isWasmtimeLinkerError returns true if the error indicates a linker failure
// (as opposed to "duplicate definition" which is benign for stubs).
func isWasmtimeLinkerError(err error) bool {
	return err != nil && !isDuplicateDefinition(err)
}

func isDuplicateDefinition(err error) bool {
	// wasmtime returns errors containing "duplicate" when a function is
	// already defined. Since stubs are best-effort, this is not fatal.
	return err != nil && strings.Contains(err.Error(), "duplicate")
}

// ---------------------------------------------------------------------------
// Host function helpers
// ---------------------------------------------------------------------------

// helper to get the raw memory buffer from the wasmtime caller.
// Returns an error if the module has no "memory" export or the export is not a memory.
func callerMemBuf(caller *wasmtime.Caller) ([]byte, *wasmtime.Memory, error) {
	export := caller.GetExport("memory")
	if export == nil {
		return nil, nil, fmt.Errorf("host: module has no exported memory")
	}
	mem := export.Memory()
	if mem == nil {
		return nil, nil, fmt.Errorf("host: memory export is not a memory")
	}
	buf := mem.UnsafeData(caller)
	return buf, mem, nil
}

// helper to create a context with raw memory override for writeResult.
func ctxWithMem(ctx context.Context, buf []byte) context.Context {
	return contextWithRawMemBuf(ctx, buf)
}

// ---------------------------------------------------------------------------
// cleat_call: (i32,i32, i32,i32, i32,i32, i32,i32) -> i64
// service(ptr,len), operation(ptr,len), request(ptr,len), response(ptr,maxLen)
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatCall(linker *wasmtime.Linker, completeResult, completeErr *string) error {
	return linker.FuncWrap("env", "cleat_call", func(ctx context.Context, caller *wasmtime.Caller,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen, respPtr, respMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		service, ok := wasmtimeReadServiceName(buf, svcPtr, svcLen)
		if !ok {
			return errBadParam, nil
		}
		op, ok := wasmtimeReadServiceName(buf, opPtr, opLen)
		if !ok {
			return errBadParam, nil
		}
		req, ok := wasmtimeReadStringValidated(buf, reqPtr, reqLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.DurableCall(callCtx, nil, service, op, req, respPtr, respMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_sleep: (i64) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatSleep(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_sleep", func(ctx context.Context, durationMs int64) (uint64, error) {
		return uint64(handlerFromContext(ctx).DurableSleep(ctx, nil, durationMs)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_now: () -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatNow(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_now", func(ctx context.Context) (uint64, error) {
		return uint64(handlerFromContext(ctx).Now(ctx)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_random: () -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatRandom(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_random", func(ctx context.Context) (uint64, error) {
		return uint64(handlerFromContext(ctx).Random(ctx)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_log: (i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatLog(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_log", func(ctx context.Context, caller *wasmtime.Caller,
		msgPtr, msgLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		msg, ok := wasmtimeReadStringValidated(buf, msgPtr, msgLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		return uint64(h.DurableLog(ctx, nil, msg)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_version: () -> i64
// cleat_min_version: () -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatVersion(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_version", func(ctx context.Context) (uint64, error) {
		return uint64(handlerFromContext(ctx).Version(ctx)), nil
	})
}

func (b *wasmtimeBackend) registerCleatMinVersion(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_min_version", func(ctx context.Context) (uint64, error) {
		return uint64(handlerFromContext(ctx).MinVersion(ctx)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_defer: (i32,i32 x2) -> i64
// desc(ptr,len), deferID(ptr,maxLen)
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatDefer(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_defer", func(ctx context.Context, caller *wasmtime.Caller,
		descPtr, descLen, deferIDPtr, deferIDMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		desc, ok := wasmtimeReadStringValidated(buf, descPtr, descLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.DurableDefer(callCtx, nil, desc, deferIDPtr, deferIDMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_poll_cancellation: (i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatPollCancellation(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_poll_cancellation", func(ctx context.Context, caller *wasmtime.Caller,
		reasonPtr, reasonMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.PollCancellation(callCtx, nil, reasonPtr, reasonMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_poll_signal: (i32,i32, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatPollSignal(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_poll_signal", func(ctx context.Context, caller *wasmtime.Caller,
		namePtr, nameLen, payloadPtr, payloadMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		name, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParam, nil
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.PollSignal(callCtx, nil, name, payloadPtr, payloadMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_continue_as_new: (i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatContinueAsNew(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_continue_as_new", func(ctx context.Context, caller *wasmtime.Caller,
		inputPtr, inputLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		newInput, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		return uint64(h.ContinueAsNew(ctx, nil, newInput)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_continue_as_new_versioned: (i32,i32, i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatContinueAsNewVersioned(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_continue_as_new_versioned", func(ctx context.Context, caller *wasmtime.Caller,
		inputPtr, inputLen uint32, newVersion int32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		newInput, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		return uint64(h.ContinueAsNewWithVersion(ctx, nil, newInput, int(newVersion))), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_child_workflow: (i32,i32 x3, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatChildWorkflow(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_child_workflow", func(ctx context.Context, caller *wasmtime.Caller,
		namePtr, nameLen, inputPtr, inputLen, runIDPtr, runIDMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		wfName, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParam, nil
		}
		wfInput, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.ChildWorkflow(callCtx, nil, wfName, wfInput, runIDPtr, runIDMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_child_workflow_with_options: (i32,i32 x3, i64, i64, i32,i32, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatChildWorkflowWithOptions(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_child_workflow_with_options", func(ctx context.Context, caller *wasmtime.Caller,
		namePtr, nameLen, inputPtr, inputLen uint32, version int64, priority int64,
		policyPtr, policyLen, runIDPtr, runIDMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		wfName, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParam, nil
		}
		wfInput, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		parentClosePolicy, ok := wasmtimeReadServiceName(buf, policyPtr, policyLen)
		if !ok {
			return errBadParam, nil
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.ChildWorkflowWithOptions(callCtx, nil, wfName, wfInput, version, priority, parentClosePolicy, runIDPtr, runIDMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_child_workflow_in_schema: (i32,i32 x4, i64, i64, i32,i32, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatChildWorkflowInSchema(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_child_workflow_in_schema", func(ctx context.Context, caller *wasmtime.Caller,
		schemaPtr, schemaLen, namePtr, nameLen, inputPtr, inputLen uint32, version int64, priority int64,
		policyPtr, policyLen, runIDPtr, runIDMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		targetSchema, ok := wasmtimeReadServiceName(buf, schemaPtr, schemaLen)
		if !ok {
			return errBadParam, nil
		}
		wfName, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParam, nil
		}
		wfInput, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		parentClosePolicy, ok := wasmtimeReadServiceName(buf, policyPtr, policyLen)
		if !ok {
			return errBadParam, nil
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.ChildWorkflowInSchema(callCtx, nil, targetSchema, wfName, wfInput, version, priority, parentClosePolicy, runIDPtr, runIDMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_await_child: (i32,i32, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatAwaitChild(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_await_child", func(ctx context.Context, caller *wasmtime.Caller,
		runIDPtr, runIDLen, resultPtr, resultMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		runID, ok := wasmtimeReadServiceName(buf, runIDPtr, runIDLen)
		if !ok {
			return errBadParam, nil
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.AwaitChild(callCtx, nil, runID, resultPtr, resultMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_await_all_children: (i32,i32, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatAwaitAllChildren(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_await_all_children", func(ctx context.Context, caller *wasmtime.Caller,
		idsPtr, idsLen, resultsPtr, resultsMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		runIDsJSON, ok := wasmtimeReadStringValidated(buf, idsPtr, idsLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.AwaitAllChildren(callCtx, nil, runIDsJSON, resultsPtr, resultsMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_call_retry: (i32,i32 x3, i64 x4, i32,i32, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatCallRetry(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_call_retry", func(ctx context.Context, caller *wasmtime.Caller,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen uint32,
		maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs int64,
		nonRetryPtr, nonRetryLen uint32,
		respPtr, respMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		service, ok := wasmtimeReadServiceName(buf, svcPtr, svcLen)
		if !ok {
			return errBadParam, nil
		}
		op, ok := wasmtimeReadServiceName(buf, opPtr, opLen)
		if !ok {
			return errBadParam, nil
		}
		req, ok := wasmtimeReadStringValidated(buf, reqPtr, reqLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		nonRetryableErrorsJSON, ok := wasmtimeReadStringValidated(buf, nonRetryPtr, nonRetryLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.DurableCallWithRetry(callCtx, nil, service, op, req,
			maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs,
			nonRetryableErrorsJSON, respPtr, respMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_await_signals: (i32,i32, i64, i32,i32 x2) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatAwaitSignals(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_await_signals", func(ctx context.Context, caller *wasmtime.Caller,
		namesPtr, namesLen uint32, timeoutMs int64,
		sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		names, ok := wasmtimeReadStringValidated(buf, namesPtr, namesLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.DurableAwaitSignals(callCtx, nil, names, timeoutMs,
			sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// set_query_state: (i32,i32 x2) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatSetQueryState(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "set_query_state", func(ctx context.Context, caller *wasmtime.Caller,
		keyPtr, keyLen, valPtr, valLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		key, ok := wasmtimeReadServiceName(buf, keyPtr, keyLen)
		if !ok {
			return errBadParam, nil
		}
		val, ok := wasmtimeReadStringValidated(buf, valPtr, valLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		return uint64(h.SetQueryState(ctx, nil, key, val)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_call_heartbeat: (i32,i32 x3, i64, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatCallHeartbeat(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_call_heartbeat", func(ctx context.Context, caller *wasmtime.Caller,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen uint32,
		heartbeatIntervalMs int64,
		respPtr, respMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		service, ok := wasmtimeReadServiceName(buf, svcPtr, svcLen)
		if !ok {
			return errBadParam, nil
		}
		op, ok := wasmtimeReadServiceName(buf, opPtr, opLen)
		if !ok {
			return errBadParam, nil
		}
		req, ok := wasmtimeReadStringValidated(buf, reqPtr, reqLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.DurableCallWithHeartbeat(callCtx, nil, service, op, req, heartbeatIntervalMs, respPtr, respMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// plugin_call: (i32,i32 x4, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatPluginCall(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "plugin_call", func(ctx context.Context, caller *wasmtime.Caller,
		pluginNamePtr, pluginNameLen,
		funcNamePtr, funcNameLen,
		inputPtr, inputLen,
		responsePtr, responseMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		pluginName, ok := wasmtimeReadServiceName(buf, pluginNamePtr, pluginNameLen)
		if !ok {
			return errBadParam, nil
		}
		funcName, ok := wasmtimeReadServiceName(buf, funcNamePtr, funcNameLen)
		if !ok {
			return errBadParam, nil
		}
		inputJSON, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.PluginCall(callCtx, nil, pluginName, funcName, inputJSON, responsePtr, responseMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// plugin_call_streaming: (i32,i32 x4, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatPluginCallStreaming(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "plugin_call_streaming", func(ctx context.Context, caller *wasmtime.Caller,
		pluginNamePtr, pluginNameLen,
		funcNamePtr, funcNameLen,
		inputPtr, inputLen,
		responsePtr, responseMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		pluginName, ok := wasmtimeReadServiceName(buf, pluginNamePtr, pluginNameLen)
		if !ok {
			return errBadParam, nil
		}
		funcName, ok := wasmtimeReadServiceName(buf, funcNamePtr, funcNameLen)
		if !ok {
			return errBadParam, nil
		}
		inputJSON, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.PluginCallStreaming(callCtx, nil, pluginName, funcName, inputJSON, responsePtr, responseMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_register_update_handler: (i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatRegisterUpdateHandler(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_register_update_handler", func(ctx context.Context, caller *wasmtime.Caller,
		namePtr, nameLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		name, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParam, nil
		}
		return uint64(h.RegisterUpdateHandler(ctx, nil, name)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_create_promise: (i32,i32, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatCreatePromise(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_create_promise", func(ctx context.Context, caller *wasmtime.Caller,
		namePtr, nameLen, promiseIDPtr, promiseIDMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		name, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParam, nil
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.CreatePromise(callCtx, nil, name, promiseIDPtr, promiseIDMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_await_promise: (i32,i32, i64, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatAwaitPromise(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_await_promise", func(ctx context.Context, caller *wasmtime.Caller,
		promiseIDPtr, promiseIDLen uint32, timeoutMs int64,
		resultPtr, resultMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		promiseID, ok := wasmtimeReadServiceName(buf, promiseIDPtr, promiseIDLen)
		if !ok {
			return errBadParam, nil
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.AwaitPromise(callCtx, nil, promiseID, timeoutMs, resultPtr, resultMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_send_signal_and_wait: (i32,i32 x3, i64, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatSendSignalAndWait(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_send_signal_and_wait", func(ctx context.Context, caller *wasmtime.Caller,
		targetPtr, targetLen, sigPtr, sigLen, payloadPtr, payloadLen uint32,
		timeoutMs int64,
		respPtr, respMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		targetRunID, ok := wasmtimeReadServiceName(buf, targetPtr, targetLen)
		if !ok {
			return errBadParam, nil
		}
		signalName, ok := wasmtimeReadServiceName(buf, sigPtr, sigLen)
		if !ok {
			return errBadParam, nil
		}
		payload, ok := wasmtimeReadStringValidated(buf, payloadPtr, payloadLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.SendSignalAndWait(callCtx, nil, targetRunID, signalName, payload, timeoutMs, respPtr, respMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_reply_to_signal: (i32,i32 x2) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatReplyToSignal(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_reply_to_signal", func(ctx context.Context, caller *wasmtime.Caller,
		correlationPtr, correlationLen, respPtr, respLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		correlationID, ok := wasmtimeReadServiceName(buf, correlationPtr, correlationLen)
		if !ok {
			return errBadParam, nil
		}
		response, ok := wasmtimeReadStringValidated(buf, respPtr, respLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		return uint64(h.ReplyToSignal(ctx, nil, correlationID, response)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_signal_workflow: (i32,i32 x3) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatSignalWorkflow(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_signal_workflow", func(ctx context.Context, caller *wasmtime.Caller,
		targetPtr, targetLen, sigPtr, sigLen, payloadPtr, payloadLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		targetRunID, ok := wasmtimeReadServiceName(buf, targetPtr, targetLen)
		if !ok {
			return errBadParam, nil
		}
		signalName, ok := wasmtimeReadServiceName(buf, sigPtr, sigLen)
		if !ok {
			return errBadParam, nil
		}
		payload, ok := wasmtimeReadStringValidated(buf, payloadPtr, payloadLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		return uint64(h.SignalWorkflow(ctx, nil, targetRunID, signalName, payload)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_set_scope: (i32,i32 x2, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatSetScope(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_set_scope", func(ctx context.Context, caller *wasmtime.Caller,
		objTypePtr, objTypeLen, instKeyPtr, instKeyLen uint32,
		prevScopePtr, prevScopeMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		objType, ok := wasmtimeReadServiceName(buf, objTypePtr, objTypeLen)
		if !ok {
			return errBadParam, nil
		}
		instKey, ok := wasmtimeReadServiceName(buf, instKeyPtr, instKeyLen)
		if !ok {
			return errBadParam, nil
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.SetScope(callCtx, nil, objType, instKey, prevScopePtr, prevScopeMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_get_scope: (i32,i32, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatGetScope(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_get_scope", func(ctx context.Context, caller *wasmtime.Caller,
		objTypePtr, objTypeMaxLen, instKeyPtr, instKeyMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.GetScope(callCtx, nil, objTypePtr, objTypeMaxLen, instKeyPtr, instKeyMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_uuid: (i32,i32, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatUUID(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_uuid", func(ctx context.Context, caller *wasmtime.Caller,
		seedPtr, seedLen, uuidPtr, uuidMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		seed, ok := wasmtimeReadStringValidated(buf, seedPtr, seedLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.UUID(callCtx, nil, seed, uuidPtr, uuidMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_acquire_lock: (i32,i32, i64) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatAcquireLock(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_acquire_lock", func(ctx context.Context, caller *wasmtime.Caller,
		keyPtr, keyLen uint32, ttlMs int64) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		key, ok := wasmtimeReadServiceName(buf, keyPtr, keyLen)
		if !ok {
			return errBadParam, nil
		}
		return uint64(h.AcquireLock(ctx, nil, key, ttlMs)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_release_lock: (i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatReleaseLock(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_release_lock", func(ctx context.Context, caller *wasmtime.Caller,
		keyPtr, keyLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		key, ok := wasmtimeReadServiceName(buf, keyPtr, keyLen)
		if !ok {
			return errBadParam, nil
		}
		return uint64(h.ReleaseLock(ctx, nil, key)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_side_effect: (i32,i32, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatSideEffect(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_side_effect", func(ctx context.Context, caller *wasmtime.Caller,
		resultPtr, resultLen, outPtr, outMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		result, ok := wasmtimeReadStringValidated(buf, resultPtr, resultLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.SideEffect(callCtx, nil, result, outPtr, outMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_workflow_id: (i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatWorkflowID(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_workflow_id", func(ctx context.Context, caller *wasmtime.Caller,
		idPtr, idMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.WorkflowID(callCtx, nil, idPtr, idMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_run_id: (i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatRunID(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_run_id", func(ctx context.Context, caller *wasmtime.Caller,
		idPtr, idMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.RunID(callCtx, nil, idPtr, idMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_resolve_promise: (i32,i32 x2) -> i64
// cleat_reject_promise: (i32,i32 x2) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatResolvePromise(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_resolve_promise", func(ctx context.Context, caller *wasmtime.Caller,
		idPtr, idLen, valPtr, valLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		promiseID, ok := wasmtimeReadServiceName(buf, idPtr, idLen)
		if !ok {
			return errBadParam, nil
		}
		value, ok := wasmtimeReadStringValidated(buf, valPtr, valLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		return uint64(h.ResolvePromise(ctx, nil, promiseID, value)), nil
	})
}

func (b *wasmtimeBackend) registerCleatRejectPromise(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_reject_promise", func(ctx context.Context, caller *wasmtime.Caller,
		idPtr, idLen, errPtr, errLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		promiseID, ok := wasmtimeReadServiceName(buf, idPtr, idLen)
		if !ok {
			return errBadParam, nil
		}
		errMsg, ok := wasmtimeReadStringValidated(buf, errPtr, errLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		return uint64(h.RejectPromise(ctx, nil, promiseID, errMsg)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_send: (i32,i32 x3) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatSend(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_send", func(ctx context.Context, caller *wasmtime.Caller,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		service, ok := wasmtimeReadServiceName(buf, svcPtr, svcLen)
		if !ok {
			return errBadParam, nil
		}
		op, ok := wasmtimeReadServiceName(buf, opPtr, opLen)
		if !ok {
			return errBadParam, nil
		}
		req, ok := wasmtimeReadStringValidated(buf, reqPtr, reqLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		return uint64(h.DurableSend(ctx, nil, service, op, req)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_schedule_invoke: (i32,i32 x3, i64) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatScheduleInvoke(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_schedule_invoke", func(ctx context.Context, caller *wasmtime.Caller,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen uint32, delayMs int64) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		service, ok := wasmtimeReadServiceName(buf, svcPtr, svcLen)
		if !ok {
			return errBadParam, nil
		}
		op, ok := wasmtimeReadServiceName(buf, opPtr, opLen)
		if !ok {
			return errBadParam, nil
		}
		req, ok := wasmtimeReadStringValidated(buf, reqPtr, reqLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		return uint64(h.DurableScheduleInvoke(ctx, nil, service, op, req, delayMs)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_register_query_handler: (i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatRegisterQueryHandler(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_register_query_handler", func(ctx context.Context, caller *wasmtime.Caller,
		namePtr, nameLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		name, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParam, nil
		}
		return uint64(h.RegisterQueryHandler(ctx, nil, name)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_run_detached: (i32,i32 x2) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatRunDetached(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_run_detached", func(ctx context.Context, caller *wasmtime.Caller,
		namePtr, nameLen, inputPtr, inputLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		name, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParam, nil
		}
		inputJSON, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		return uint64(h.RunDetached(ctx, nil, name, inputJSON)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_set_state: (i32,i32 x2) -> i64
// cleat_delete_state: (i32,i32) -> i64
// cleat_has_state: (i32,i32) -> i64
// cleat_incr_state: (i32,i32, i64) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatSetState(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_set_state", func(ctx context.Context, caller *wasmtime.Caller,
		keyPtr, keyLen, valPtr, valLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		key, ok := wasmtimeReadServiceName(buf, keyPtr, keyLen)
		if !ok {
			return errBadParam, nil
		}
		value, ok := wasmtimeReadStringValidated(buf, valPtr, valLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		return uint64(h.SetState(ctx, nil, key, value)), nil
	})
}

func (b *wasmtimeBackend) registerCleatGetState(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_get_state", func(ctx context.Context, caller *wasmtime.Caller,
		keyPtr, keyLen, valuePtr, valueMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		key, ok := wasmtimeReadServiceName(buf, keyPtr, keyLen)
		if !ok {
			return errBadParam, nil
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.GetState(callCtx, nil, key, valuePtr, valueMaxLen)), nil
	})
}

func (b *wasmtimeBackend) registerCleatDeleteState(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_delete_state", func(ctx context.Context, caller *wasmtime.Caller,
		keyPtr, keyLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		key, ok := wasmtimeReadServiceName(buf, keyPtr, keyLen)
		if !ok {
			return errBadParam, nil
		}
		return uint64(h.DeleteState(ctx, nil, key)), nil
	})
}

func (b *wasmtimeBackend) registerCleatIncrState(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_incr_state", func(ctx context.Context, caller *wasmtime.Caller,
		keyPtr, keyLen uint32, delta int64) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		key, ok := wasmtimeReadServiceName(buf, keyPtr, keyLen)
		if !ok {
			return errBadParam, nil
		}
		return uint64(h.IncrState(ctx, nil, key, delta)), nil
	})
}

func (b *wasmtimeBackend) registerCleatHasState(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_has_state", func(ctx context.Context, caller *wasmtime.Caller,
		keyPtr, keyLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		key, ok := wasmtimeReadServiceName(buf, keyPtr, keyLen)
		if !ok {
			return errBadParam, nil
		}
		return uint64(h.HasState(ctx, nil, key)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_list_state: (i32,i32, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatListState(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_list_state", func(ctx context.Context, caller *wasmtime.Caller,
		prefixPtr, prefixLen, keysPtr, keysMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		prefix, ok := wasmtimeReadStringValidated(buf, prefixPtr, prefixLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.ListState(callCtx, nil, prefix, keysPtr, keysMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_fetch: (i32,i32 x4, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatFetch(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_fetch", func(ctx context.Context, caller *wasmtime.Caller,
		methodPtr, methodLen, urlPtr, urlLen, headersPtr, headersLen, bodyPtr, bodyLen uint32,
		responsePtr, responseMaxLen uint32) (uint64, error) {
		h := handlerFromContext(ctx)
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		method, ok := wasmtimeReadServiceName(buf, methodPtr, methodLen)
		if !ok {
			return errBadParam, nil
		}
		url, ok := wasmtimeReadStringValidated(buf, urlPtr, urlLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		headersJSON, ok := wasmtimeReadStringValidated(buf, headersPtr, headersLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		body, ok := wasmtimeReadStringValidated(buf, bodyPtr, bodyLen, MaxWasmStringLen)
		if !ok {
			return errBadParam, nil
		}
		callCtx := ctxWithMem(ctx, buf)
		return uint64(h.Fetch(callCtx, nil, method, url, headersJSON, body, responsePtr, responseMaxLen)), nil
	})
}

// ---------------------------------------------------------------------------
// cleat_complete: (i32, i32,i32) -> i64
// Signals workflow completion. status=0 means success, status=1 means error.
// The result string is stored via closure variables so the Execute method
// can retrieve it even if the module subsequently traps (e.g. via proc_exit).
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatComplete(linker *wasmtime.Linker, completeResult, completeErr *string) error {
	return linker.FuncWrap("env", "cleat_complete", func(ctx context.Context, caller *wasmtime.Caller,
		status uint32, resultPtr uint32, resultLen uint32) (uint64, error) {
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return 0, err
		}
		if resultLen > 0 && uint64(resultPtr)+uint64(resultLen) <= uint64(len(buf)) {
			result := string(buf[resultPtr : resultPtr+resultLen])
			if status == 0 {
				*completeResult = result
			} else {
				*completeErr = result
			}
		}
		return 0, nil
	})
}
