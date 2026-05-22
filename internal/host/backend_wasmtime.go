//go:build cgo

package host

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v44"

	"github.com/cleat-team/cleat/internal/wasm"
)

// Compile-time check: wasmtimeBackend implements WasmBackend.
var _ WasmBackend = (*wasmtimeBackend)(nil)

// errBadParamInt64 is the int64 equivalent of errBadParam for return values
// from FuncWrap closures (which must return int64, not uint64).
const errBadParamInt64 int64 = -4294967295

// wasmtimeBackend implements WasmBackend using the wasmtime WASM runtime.
// It loads core WASM modules (post-decompose Component Model binaries).
//
// Build constraint: this file requires CGO because wasmtime-go wraps the
// wasmtime Rust runtime via CGo.
type wasmtimeBackend struct {
	engine  *wasmtime.Engine
	handler HostHandler // current execution session

	// Work data for the Go dispatcher (cleat_poll_work).
	workEntryPoint string
	workInput      []byte
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
	b.handler = session

	// Detect Component Model binaries and extract the first core module.
	// Component Model binaries (.wasm from componentize-py) contain nested
	// core WASM modules. We extract the first one and execute it directly.
	if isComponentWasm(wasmBytes) {
		bundle, bundleErr := wasm.ParseComponentBundle(wasmBytes)
		if bundleErr != nil {
			return nil, fmt.Errorf("host: parse component bundle: %w", bundleErr)
		}
		if len(bundle.Modules) == 0 {
			return nil, fmt.Errorf("host: component bundle has no core modules")
		}
		wasmBytes = wasm.PatchEmptyImportModuleName(bundle.Modules[0], "__component_adapter__")
	}

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
	if err := b.registerCleatPollChild(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_poll_child: %w", err)
	}
	if err := b.registerCleatAwaitAnyChild(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_await_any_child: %w", err)
	}
	if err := b.registerCleatJsonParse(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_json_parse: %w", err)
	}
	if err := b.registerCleatJsonStringify(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_json_stringify: %w", err)
	}
	if err := b.registerCleatComplete(linker, &completeResult, &completeErr); err != nil {
		return nil, fmt.Errorf("host: register cleat_complete: %w", err)
	}
	if err := b.registerCleatPollWork(linker); err != nil {
		return nil, fmt.Errorf("host: register cleat_poll_work: %w", err)
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

	// If the module exports _start (Go wasip1), use the cleat_poll_work
	// dispatcher protocol. We store the entry point + input on the backend,
	// then call _start synchronously. main() calls cleat_poll_work (which
	// returns the work), dispatches to the entry point, and calls
	// cleat_complete with the result. All WASM execution stays on one
	// goroutine, avoiding the Go wasip1 reentrancy issue.
	if startFn := instance.GetFunc(store, "_start"); startFn != nil {
		b.workEntryPoint = entryPoint
		b.workInput = []byte(input)

		// Call _start synchronously. main() processes the work and returns.
		func() {
				defer func() { recover() }()
				startFn.Call(store)
			}()

		// Result is delivered via cleat_complete hook.
		if completeErr != "" {
			return nil, fmt.Errorf("host: export %q failed: %s", entryPoint, completeErr)
		}
		if completeResult != "" {
			return &ExecResult{Result: completeResult, Suspended: false}, nil
		}
		return &ExecResult{Result: `"ok"`, Suspended: false}, nil
	}

	// Non-Go module: call the export directly.
	fn := instance.GetFunc(store, entryPoint)
	if fn == nil {
		return nil, fmt.Errorf("host: export %q not found", entryPoint)
	}

	// Call the entry point. The return value is a single i64 encoding
	// the error code (low 32 bits) and output length (high 32 bits).
	// Wrap in recover to handle wasmtime-go internal panics (e.g., from
	// modules with unexpected import/export configurations).
	var results interface{}
	var callErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				callErr = fmt.Errorf("host: wasmtime panic in %q: %v", entryPoint, r)
			}
		}()
		results, callErr = fn.Call(store, int32(inputOffset), int32(len(inputBytes)), int32(outputOffset), int32(outBufSz))
	}()

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
// Shared-memory dispatcher protocol for Go WASM modules
// ---------------------------------------------------------------------------

// dispatcher memory layout (matches gen_main_stub.go for --target go):
//
//	Offset  Size   Field
//	0       1      command byte (0=idle, 1=execute, 2=done, 3=error)
//	1       4      entry point name length (uint32 LE)
//	5       4      input JSON length (uint32 LE)
//	9       4      output JSON length (uint32 LE)
//	13      256    entry point name buffer
//	269     65536  input JSON buffer
//	65837   65536  output JSON buffer
const (
	_dispatcherBase       = 10 * 1024 * 1024 // 10 MiB
	_dispatcherCmd        = _dispatcherBase + 0
	_dispatcherNameLen    = _dispatcherBase + 1
	_dispatcherInputLen   = _dispatcherBase + 5
	_dispatcherOutputLen  = _dispatcherBase + 9
	_dispatcherNameBuf    = _dispatcherBase + 13
	_dispatcherInputBuf   = _dispatcherBase + 269
	_dispatcherOutputBuf  = _dispatcherBase + 65837
	_dispatcherNameMax    = 256
	_dispatcherInputMax   = 65536
	_dispatcherOutputMax  = 65536
	_dispatcherInterval   = 10 * time.Millisecond
	_dispatcherTimeout    = 30 * time.Second
)

// executeViaDispatcher communicates with main()'s dispatch loop via shared
// memory. The _start function is already running in a background goroutine
// and main() is polling the command byte. We write work, wait for the
// result, and return.
func (b *wasmtimeBackend) executeViaDispatcher(
	store wasmtime.Storelike,
	mem *wasmtime.Memory,
	entryPoint string,
	input json.RawMessage,
	completeResult, completeErr string,
) (*ExecResult, error) {
	data := mem.UnsafeData(store)

	// Grow memory to cover the dispatcher region if needed.
	needed := _dispatcherOutputBuf + _dispatcherOutputMax
	if uint64(len(data)) < uint64(needed) {
		pagesNeeded := (needed - len(data) + wasmPageSize - 1) / wasmPageSize
		if _, err := mem.Grow(store, uint64(pagesNeeded)); err != nil {
			return nil, fmt.Errorf("host: grow memory for dispatcher: %w", err)
		}
		data = mem.UnsafeData(store)
	}

	// Zero the command byte.
	data[_dispatcherCmd] = 0

	// Write entry point name (without NUL terminator, main reads length).
	entryBytes := []byte(entryPoint)
	if len(entryBytes) > _dispatcherNameMax {
		entryBytes = entryBytes[:_dispatcherNameMax]
	}
	putUint32LE(data[_dispatcherNameLen:], uint32(len(entryBytes)))
	copy(data[_dispatcherNameBuf:], entryBytes)

	// Write input JSON.
	inputBytes := []byte(input)
	if len(inputBytes) > _dispatcherInputMax {
		inputBytes = inputBytes[:_dispatcherInputMax]
	}
	putUint32LE(data[_dispatcherInputLen:], uint32(len(inputBytes)))
	copy(data[_dispatcherInputBuf:], inputBytes)

	// Signal work: set command byte to 1 (execute).
	data[_dispatcherCmd] = 1

	// Poll for completion.
	deadline := time.Now().Add(_dispatcherTimeout)
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("host: dispatcher timeout waiting for %q", entryPoint)
		}
		cmd := data[_dispatcherCmd]
		if cmd == 2 {
			// Done: read the result.
			outLen := getUint32LE(data[_dispatcherOutputLen:])
			if outLen > _dispatcherOutputMax {
				outLen = _dispatcherOutputMax
			}
			result := string(data[_dispatcherOutputBuf : _dispatcherOutputBuf+outLen])
			return &ExecResult{Result: result, Suspended: false}, nil
		}
		if cmd == 3 {
			// Error: read the error message from output buffer.
			outLen := getUint32LE(data[_dispatcherOutputLen:])
			if outLen > _dispatcherOutputMax {
				outLen = _dispatcherOutputMax
			}
			errMsg := string(data[_dispatcherOutputBuf : _dispatcherOutputBuf+outLen])
			return nil, fmt.Errorf("host: %q: %s", entryPoint, errMsg)
		}
		// Also check cleat_complete for suspend/results.
		if completeErr != "" {
			return nil, fmt.Errorf("host: %q failed: %s", entryPoint, completeErr)
		}
		if completeResult != "" {
			return &ExecResult{Result: completeResult, Suspended: false}, nil
		}
		time.Sleep(_dispatcherInterval)
	}
}

func putUint32LE(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

func getUint32LE(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// ---------------------------------------------------------------------------
// Helper: raw memory read/write on a []byte slice
// ---------------------------------------------------------------------------

// wasmtimeReadString reads a string from a raw WASM linear memory buffer.
func wasmtimeReadString(buf []byte, ptr, length int32) string {
	if length <= 0 {
		return ""
	}
	pu, lu := uint32(ptr), uint32(length)
	if uint64(pu)+uint64(lu) > uint64(len(buf)) {
		return ""
	}
	return string(buf[pu : pu+lu])
}

// wasmtimeReadStringValidated reads and validates a string from a raw buffer.
func wasmtimeReadStringValidated(buf []byte, ptr, length, maxLen int32) (string, bool) {
	if length <= 0 || length > maxLen {
		return "", false
	}
	pu, lu := uint32(ptr), uint32(length)
	if uint64(pu)+uint64(lu) > uint64(len(buf)) {
		return "", false
	}
	return string(buf[pu : pu+lu]), true
}

// wasmtimeReadServiceName reads and validates a service/operation name.
func wasmtimeReadServiceName(buf []byte, ptr, length int32) (string, bool) {
	s, ok := wasmtimeReadStringValidated(buf, ptr, length, int32(MaxWasmStringLen))
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
	// DefineWasi provides the full WASI preview1 implementation (fd_write,
	// random_get, clock_time_get, environ_get, proc_exit, sched_yield, etc.).
	// Required by Go wasip1, Rust wasm32-wasip1, and other WASI-compiled modules.
	if err := linker.DefineWasi(); err != nil {
		return err
	}

	// reset_adapter_state is required by core modules extracted from Component
	// Model binaries produced by componentize-py. It is a no-op.
	if err := linker.FuncWrap("wasi_snapshot_preview1", "reset_adapter_state",
		func() {},
	); err != nil {
		return err
	}

	return nil
}

// registerEnvStubs registers no-op stubs for optional "env" imports.
func (b *wasmtimeBackend) registerEnvStubs(linker *wasmtime.Linker) error {
	// abort is required by AssemblyScript-compiled WASM modules.
	if err := linker.FuncWrap("env", "abort",
		func(msg, file, line, col int32) {},
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
		func(chars, count int32) {},
	); err != nil {
		if isWasmtimeLinkerError(err) {
			return err
		}
	}
	// currentTimeMillis
	if err := linker.FuncWrap("teavm", "currentTimeMillis",
		func() float64 { return 0 },
	); err != nil {
		if isWasmtimeLinkerError(err) {
			return err
		}
	}
	// logString
	if err := linker.FuncWrap("teavm", "logString",
		func(ptr int32) {},
	); err != nil {
		if isWasmtimeLinkerError(err) {
			return err
		}
	}
	// logInt
	if err := linker.FuncWrap("teavm", "logInt",
		func(ptr int32) {},
	); err != nil {
		if isWasmtimeLinkerError(err) {
			return err
		}
	}
	// logOutOfMemory
	if err := linker.FuncWrap("teavm", "logOutOfMemory",
		func() {},
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
	return linker.FuncWrap("env", "cleat_call", func(caller *wasmtime.Caller,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen, respPtr, respMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		service, ok := wasmtimeReadServiceName(buf, svcPtr, svcLen)
		if !ok {
			return errBadParamInt64
		}
		op, ok := wasmtimeReadServiceName(buf, opPtr, opLen)
		if !ok {
			return errBadParamInt64
		}
		req, ok := wasmtimeReadStringValidated(buf, reqPtr, reqLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		callCtx := ctxWithMem(context.Background(), buf)
	return h.DurableCall(callCtx, nil, service, op, req, uint32(respPtr), uint32(respMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_sleep: (i64) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatSleep(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_sleep", func(durationMs int64) int64 {
		return b.handler.DurableSleep(context.Background(), nil, durationMs)
	})
}

// ---------------------------------------------------------------------------
// cleat_now: () -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatNow(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_now", func() int64 {
		return b.handler.Now(context.Background())
	})
}

// ---------------------------------------------------------------------------
// cleat_random: () -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatRandom(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_random", func() int64 {
		return b.handler.Random(context.Background())
	})
}

// ---------------------------------------------------------------------------
// cleat_log: (i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatLog(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_log", func(caller *wasmtime.Caller,
		msgPtr, msgLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		msg, ok := wasmtimeReadStringValidated(buf, msgPtr, msgLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.DurableLog(context.Background(), nil, msg)
	})
}

// ---------------------------------------------------------------------------
// cleat_version: () -> i64
// cleat_min_version: () -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatVersion(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_version", func() int64 {
		return b.handler.Version(context.Background())
	})
}

func (b *wasmtimeBackend) registerCleatMinVersion(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_min_version", func() int64 {
		return b.handler.MinVersion(context.Background())
	})
}

// ---------------------------------------------------------------------------
// cleat_defer: (i32,i32 x2) -> i64
// desc(ptr,len), deferID(ptr,maxLen)
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatDefer(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_defer", func(caller *wasmtime.Caller,
		descPtr, descLen, deferIDPtr, deferIDMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		desc, ok := wasmtimeReadStringValidated(buf, descPtr, descLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.DurableDefer(context.Background(), nil, desc, uint32(deferIDPtr), uint32(deferIDMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_poll_cancellation: (i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatPollCancellation(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_poll_cancellation", func(caller *wasmtime.Caller,
		reasonPtr, reasonMaxLen int32) int64 {
		h := b.handler
		_, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		return h.PollCancellation(context.Background(), nil, uint32(reasonPtr), uint32(reasonMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_poll_signal: (i32,i32, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatPollSignal(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_poll_signal", func(caller *wasmtime.Caller,
		namePtr, nameLen, payloadPtr, payloadMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		name, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParamInt64
		}
		return h.PollSignal(context.Background(), nil, name, uint32(payloadPtr), uint32(payloadMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_continue_as_new: (i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatContinueAsNew(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_continue_as_new", func(caller *wasmtime.Caller,
		inputPtr, inputLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		newInput, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.ContinueAsNew(context.Background(), nil, newInput)
	})
}

// ---------------------------------------------------------------------------
// cleat_continue_as_new_versioned: (i32,i32, i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatContinueAsNewVersioned(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_continue_as_new_versioned", func(caller *wasmtime.Caller,
		inputPtr, inputLen int32, newVersion int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		newInput, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.ContinueAsNewWithVersion(context.Background(), nil, newInput, int(newVersion))
	})
}

// ---------------------------------------------------------------------------
// cleat_child_workflow: (i32,i32 x3, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatChildWorkflow(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_child_workflow", func(caller *wasmtime.Caller,
		namePtr, nameLen, inputPtr, inputLen, runIDPtr, runIDMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		wfName, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParamInt64
		}
		wfInput, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.ChildWorkflow(context.Background(), nil, wfName, wfInput, uint32(runIDPtr), uint32(runIDMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_child_workflow_with_options: (i32,i32 x3, i64, i64, i32,i32, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatChildWorkflowWithOptions(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_child_workflow_with_options", func(caller *wasmtime.Caller,
		namePtr, nameLen, inputPtr, inputLen int32, version int64, priority int64,
		policyPtr, policyLen, runIDPtr, runIDMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		wfName, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParamInt64
		}
		wfInput, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		parentClosePolicy, ok := wasmtimeReadServiceName(buf, policyPtr, policyLen)
		if !ok {
			return errBadParamInt64
		}
		return h.ChildWorkflowWithOptions(context.Background(), nil, wfName, wfInput, version, priority, parentClosePolicy, uint32(runIDPtr), uint32(runIDMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_child_workflow_in_schema: (i32,i32 x4, i64, i64, i32,i32, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatChildWorkflowInSchema(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_child_workflow_in_schema", func(caller *wasmtime.Caller,
		schemaPtr, schemaLen, namePtr, nameLen, inputPtr, inputLen int32, version int64, priority int64,
		policyPtr, policyLen, runIDPtr, runIDMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		targetSchema, ok := wasmtimeReadServiceName(buf, schemaPtr, schemaLen)
		if !ok {
			return errBadParamInt64
		}
		wfName, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParamInt64
		}
		wfInput, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		parentClosePolicy, ok := wasmtimeReadServiceName(buf, policyPtr, policyLen)
		if !ok {
			return errBadParamInt64
		}
		return h.ChildWorkflowInSchema(context.Background(), nil, targetSchema, wfName, wfInput, version, priority, parentClosePolicy, uint32(runIDPtr), uint32(runIDMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_await_child: (i32,i32, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatAwaitChild(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_await_child", func(caller *wasmtime.Caller,
		runIDPtr, runIDLen, resultPtr, resultMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		runID, ok := wasmtimeReadServiceName(buf, runIDPtr, runIDLen)
		if !ok {
			return errBadParamInt64
		}
		return h.AwaitChild(context.Background(), nil, runID, uint32(resultPtr), uint32(resultMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_await_all_children: (i32,i32, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatAwaitAllChildren(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_await_all_children", func(caller *wasmtime.Caller,
		idsPtr, idsLen, resultsPtr, resultsMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		runIDsJSON, ok := wasmtimeReadStringValidated(buf, idsPtr, idsLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.AwaitAllChildren(context.Background(), nil, runIDsJSON, uint32(resultsPtr), uint32(resultsMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_call_retry: (i32,i32 x3, i64 x4, i32,i32, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatCallRetry(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_call_retry", func(caller *wasmtime.Caller,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen int32,
		maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs int64,
		nonRetryPtr, nonRetryLen int32,
		respPtr, respMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		service, ok := wasmtimeReadServiceName(buf, svcPtr, svcLen)
		if !ok {
			return errBadParamInt64
		}
		op, ok := wasmtimeReadServiceName(buf, opPtr, opLen)
		if !ok {
			return errBadParamInt64
		}
		req, ok := wasmtimeReadStringValidated(buf, reqPtr, reqLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		nonRetryableErrorsJSON, ok := wasmtimeReadStringValidated(buf, nonRetryPtr, nonRetryLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.DurableCallWithRetry(context.Background(), nil, service, op, req,
			maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs,
			nonRetryableErrorsJSON, uint32(respPtr), uint32(respMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_await_signals: (i32,i32, i64, i32,i32 x2) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatAwaitSignals(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_await_signals", func(caller *wasmtime.Caller,
		namesPtr, namesLen int32, timeoutMs int64,
		sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		names, ok := wasmtimeReadStringValidated(buf, namesPtr, namesLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.DurableAwaitSignals(context.Background(), nil, names, timeoutMs,
			uint32(sigNamePtr), uint32(sigNameMaxLen), uint32(payloadPtr), uint32(payloadMaxLen))
	})
}

// ---------------------------------------------------------------------------
// set_query_state: (i32,i32 x2) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatSetQueryState(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "set_query_state", func(caller *wasmtime.Caller,
		keyPtr, keyLen, valPtr, valLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		key, ok := wasmtimeReadServiceName(buf, keyPtr, keyLen)
		if !ok {
			return errBadParamInt64
		}
		val, ok := wasmtimeReadStringValidated(buf, valPtr, valLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.SetQueryState(context.Background(), nil, key, val)
	})
}

// ---------------------------------------------------------------------------
// cleat_call_heartbeat: (i32,i32 x3, i64, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatCallHeartbeat(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_call_heartbeat", func(caller *wasmtime.Caller,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen int32,
		heartbeatIntervalMs int64,
		respPtr, respMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		service, ok := wasmtimeReadServiceName(buf, svcPtr, svcLen)
		if !ok {
			return errBadParamInt64
		}
		op, ok := wasmtimeReadServiceName(buf, opPtr, opLen)
		if !ok {
			return errBadParamInt64
		}
		req, ok := wasmtimeReadStringValidated(buf, reqPtr, reqLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.DurableCallWithHeartbeat(context.Background(), nil, service, op, req, heartbeatIntervalMs, uint32(respPtr), uint32(respMaxLen))
	})
}

// ---------------------------------------------------------------------------
// plugin_call: (i32,i32 x4, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatPluginCall(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "plugin_call", func(caller *wasmtime.Caller,
		pluginNamePtr, pluginNameLen,
		funcNamePtr, funcNameLen,
		inputPtr, inputLen,
		responsePtr, responseMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		pluginName, ok := wasmtimeReadServiceName(buf, pluginNamePtr, pluginNameLen)
		if !ok {
			return errBadParamInt64
		}
		funcName, ok := wasmtimeReadServiceName(buf, funcNamePtr, funcNameLen)
		if !ok {
			return errBadParamInt64
		}
		inputJSON, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.PluginCall(context.Background(), nil, pluginName, funcName, inputJSON, uint32(responsePtr), uint32(responseMaxLen))
	})
}

// ---------------------------------------------------------------------------
// plugin_call_streaming: (i32,i32 x4, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatPluginCallStreaming(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "plugin_call_streaming", func(caller *wasmtime.Caller,
		pluginNamePtr, pluginNameLen,
		funcNamePtr, funcNameLen,
		inputPtr, inputLen,
		responsePtr, responseMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		pluginName, ok := wasmtimeReadServiceName(buf, pluginNamePtr, pluginNameLen)
		if !ok {
			return errBadParamInt64
		}
		funcName, ok := wasmtimeReadServiceName(buf, funcNamePtr, funcNameLen)
		if !ok {
			return errBadParamInt64
		}
		inputJSON, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.PluginCallStreaming(context.Background(), nil, pluginName, funcName, inputJSON, uint32(responsePtr), uint32(responseMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_register_update_handler: (i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatRegisterUpdateHandler(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_register_update_handler", func(caller *wasmtime.Caller,
		namePtr, nameLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		name, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParamInt64
		}
		return h.RegisterUpdateHandler(context.Background(), nil, name)
	})
}

// ---------------------------------------------------------------------------
// cleat_create_promise: (i32,i32, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatCreatePromise(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_create_promise", func(caller *wasmtime.Caller,
		namePtr, nameLen, promiseIDPtr, promiseIDMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		name, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParamInt64
		}
		return h.CreatePromise(context.Background(), nil, name, uint32(promiseIDPtr), uint32(promiseIDMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_await_promise: (i32,i32, i64, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatAwaitPromise(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_await_promise", func(caller *wasmtime.Caller,
		promiseIDPtr, promiseIDLen int32, timeoutMs int64,
		resultPtr, resultMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		promiseID, ok := wasmtimeReadServiceName(buf, promiseIDPtr, promiseIDLen)
		if !ok {
			return errBadParamInt64
		}
		return h.AwaitPromise(context.Background(), nil, promiseID, timeoutMs, uint32(resultPtr), uint32(resultMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_send_signal_and_wait: (i32,i32 x3, i64, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatSendSignalAndWait(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_send_signal_and_wait", func(caller *wasmtime.Caller,
		targetPtr, targetLen, sigPtr, sigLen, payloadPtr, payloadLen int32,
		timeoutMs int64,
		respPtr, respMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		targetRunID, ok := wasmtimeReadServiceName(buf, targetPtr, targetLen)
		if !ok {
			return errBadParamInt64
		}
		signalName, ok := wasmtimeReadServiceName(buf, sigPtr, sigLen)
		if !ok {
			return errBadParamInt64
		}
		payload, ok := wasmtimeReadStringValidated(buf, payloadPtr, payloadLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.SendSignalAndWait(context.Background(), nil, targetRunID, signalName, payload, timeoutMs, uint32(respPtr), uint32(respMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_reply_to_signal: (i32,i32 x2) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatReplyToSignal(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_reply_to_signal", func(caller *wasmtime.Caller,
		correlationPtr, correlationLen, respPtr, respLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		correlationID, ok := wasmtimeReadServiceName(buf, correlationPtr, correlationLen)
		if !ok {
			return errBadParamInt64
		}
		response, ok := wasmtimeReadStringValidated(buf, respPtr, respLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.ReplyToSignal(context.Background(), nil, correlationID, response)
	})
}

// ---------------------------------------------------------------------------
// cleat_signal_workflow: (i32,i32 x3) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatSignalWorkflow(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_signal_workflow", func(caller *wasmtime.Caller,
		targetPtr, targetLen, sigPtr, sigLen, payloadPtr, payloadLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		targetRunID, ok := wasmtimeReadServiceName(buf, targetPtr, targetLen)
		if !ok {
			return errBadParamInt64
		}
		signalName, ok := wasmtimeReadServiceName(buf, sigPtr, sigLen)
		if !ok {
			return errBadParamInt64
		}
		payload, ok := wasmtimeReadStringValidated(buf, payloadPtr, payloadLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.SignalWorkflow(context.Background(), nil, targetRunID, signalName, payload)
	})
}

// ---------------------------------------------------------------------------
// cleat_set_scope: (i32,i32 x2, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatSetScope(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_set_scope", func(caller *wasmtime.Caller,
		objTypePtr, objTypeLen, instKeyPtr, instKeyLen int32,
		prevScopePtr, prevScopeMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		objType, ok := wasmtimeReadServiceName(buf, objTypePtr, objTypeLen)
		if !ok {
			return errBadParamInt64
		}
		instKey, ok := wasmtimeReadServiceName(buf, instKeyPtr, instKeyLen)
		if !ok {
			return errBadParamInt64
		}
		return h.SetScope(context.Background(), nil, objType, instKey, uint32(prevScopePtr), uint32(prevScopeMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_get_scope: (i32,i32, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatGetScope(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_get_scope", func(caller *wasmtime.Caller,
		objTypePtr, objTypeMaxLen, instKeyPtr, instKeyMaxLen int32) int64 {
		h := b.handler
		_, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		return h.GetScope(context.Background(), nil, uint32(objTypePtr), uint32(objTypeMaxLen), uint32(instKeyPtr), uint32(instKeyMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_uuid: (i32,i32, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatUUID(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_uuid", func(caller *wasmtime.Caller,
		seedPtr, seedLen, uuidPtr, uuidMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		seed, ok := wasmtimeReadStringValidated(buf, seedPtr, seedLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.UUID(context.Background(), nil, seed, uint32(uuidPtr), uint32(uuidMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_acquire_lock: (i32,i32, i64) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatAcquireLock(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_acquire_lock", func(caller *wasmtime.Caller,
		keyPtr, keyLen int32, ttlMs int64) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		key, ok := wasmtimeReadServiceName(buf, keyPtr, keyLen)
		if !ok {
			return errBadParamInt64
		}
		return h.AcquireLock(context.Background(), nil, key, ttlMs)
	})
}

// ---------------------------------------------------------------------------
// cleat_release_lock: (i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatReleaseLock(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_release_lock", func(caller *wasmtime.Caller,
		keyPtr, keyLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		key, ok := wasmtimeReadServiceName(buf, keyPtr, keyLen)
		if !ok {
			return errBadParamInt64
		}
		return h.ReleaseLock(context.Background(), nil, key)
	})
}

// ---------------------------------------------------------------------------
// cleat_side_effect: (i32,i32, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatSideEffect(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_side_effect", func(caller *wasmtime.Caller,
		resultPtr, resultLen, outPtr, outMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		result, ok := wasmtimeReadStringValidated(buf, resultPtr, resultLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.SideEffect(context.Background(), nil, result, uint32(outPtr), uint32(outMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_workflow_id: (i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatWorkflowID(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_workflow_id", func(caller *wasmtime.Caller,
		idPtr, idMaxLen int32) int64 {
		h := b.handler
		_, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		return h.WorkflowID(context.Background(), nil, uint32(idPtr), uint32(idMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_run_id: (i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatRunID(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_run_id", func(caller *wasmtime.Caller,
		idPtr, idMaxLen int32) int64 {
		h := b.handler
		_, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		return h.RunID(context.Background(), nil, uint32(idPtr), uint32(idMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_resolve_promise: (i32,i32 x2) -> i64
// cleat_reject_promise: (i32,i32 x2) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatResolvePromise(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_resolve_promise", func(caller *wasmtime.Caller,
		idPtr, idLen, valPtr, valLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		promiseID, ok := wasmtimeReadServiceName(buf, idPtr, idLen)
		if !ok {
			return errBadParamInt64
		}
		value, ok := wasmtimeReadStringValidated(buf, valPtr, valLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.ResolvePromise(context.Background(), nil, promiseID, value)
	})
}

func (b *wasmtimeBackend) registerCleatRejectPromise(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_reject_promise", func(caller *wasmtime.Caller,
		idPtr, idLen, errPtr, errLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		promiseID, ok := wasmtimeReadServiceName(buf, idPtr, idLen)
		if !ok {
			return errBadParamInt64
		}
		errMsg, ok := wasmtimeReadStringValidated(buf, errPtr, errLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.RejectPromise(context.Background(), nil, promiseID, errMsg)
	})
}

// ---------------------------------------------------------------------------
// cleat_send: (i32,i32 x3) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatSend(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_send", func(caller *wasmtime.Caller,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		service, ok := wasmtimeReadServiceName(buf, svcPtr, svcLen)
		if !ok {
			return errBadParamInt64
		}
		op, ok := wasmtimeReadServiceName(buf, opPtr, opLen)
		if !ok {
			return errBadParamInt64
		}
		req, ok := wasmtimeReadStringValidated(buf, reqPtr, reqLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.DurableSend(context.Background(), nil, service, op, req)
	})
}

// ---------------------------------------------------------------------------
// cleat_schedule_invoke: (i32,i32 x3, i64) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatScheduleInvoke(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_schedule_invoke", func(caller *wasmtime.Caller,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen int32, delayMs int64) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		service, ok := wasmtimeReadServiceName(buf, svcPtr, svcLen)
		if !ok {
			return errBadParamInt64
		}
		op, ok := wasmtimeReadServiceName(buf, opPtr, opLen)
		if !ok {
			return errBadParamInt64
		}
		req, ok := wasmtimeReadStringValidated(buf, reqPtr, reqLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.DurableScheduleInvoke(context.Background(), nil, service, op, req, delayMs)
	})
}

// ---------------------------------------------------------------------------
// cleat_register_query_handler: (i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatRegisterQueryHandler(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_register_query_handler", func(caller *wasmtime.Caller,
		namePtr, nameLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		name, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParamInt64
		}
		return h.RegisterQueryHandler(context.Background(), nil, name)
	})
}

// ---------------------------------------------------------------------------
// cleat_run_detached: (i32,i32 x2) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatRunDetached(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_run_detached", func(caller *wasmtime.Caller,
		namePtr, nameLen, inputPtr, inputLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		name, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParamInt64
		}
		inputJSON, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.RunDetached(context.Background(), nil, name, inputJSON)
	})
}

// ---------------------------------------------------------------------------
// cleat_set_state: (i32,i32 x2) -> i64
// cleat_delete_state: (i32,i32) -> i64
// cleat_has_state: (i32,i32) -> i64
// cleat_incr_state: (i32,i32, i64) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatSetState(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_set_state", func(caller *wasmtime.Caller,
		keyPtr, keyLen, valPtr, valLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		key, ok := wasmtimeReadServiceName(buf, keyPtr, keyLen)
		if !ok {
			return errBadParamInt64
		}
		value, ok := wasmtimeReadStringValidated(buf, valPtr, valLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.SetState(context.Background(), nil, key, value)
	})
}

func (b *wasmtimeBackend) registerCleatGetState(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_get_state", func(caller *wasmtime.Caller,
		keyPtr, keyLen, valuePtr, valueMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		key, ok := wasmtimeReadServiceName(buf, keyPtr, keyLen)
		if !ok {
			return errBadParamInt64
		}
		return h.GetState(context.Background(), nil, key, uint32(valuePtr), uint32(valueMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatDeleteState(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_delete_state", func(caller *wasmtime.Caller,
		keyPtr, keyLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		key, ok := wasmtimeReadServiceName(buf, keyPtr, keyLen)
		if !ok {
			return errBadParamInt64
		}
		return h.DeleteState(context.Background(), nil, key)
	})
}

func (b *wasmtimeBackend) registerCleatIncrState(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_incr_state", func(caller *wasmtime.Caller,
		keyPtr, keyLen int32, delta int64) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		key, ok := wasmtimeReadServiceName(buf, keyPtr, keyLen)
		if !ok {
			return errBadParamInt64
		}
		return h.IncrState(context.Background(), nil, key, delta)
	})
}

func (b *wasmtimeBackend) registerCleatHasState(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_has_state", func(caller *wasmtime.Caller,
		keyPtr, keyLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		key, ok := wasmtimeReadServiceName(buf, keyPtr, keyLen)
		if !ok {
			return errBadParamInt64
		}
		return h.HasState(context.Background(), nil, key)
	})
}

// ---------------------------------------------------------------------------
// cleat_list_state: (i32,i32, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatListState(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_list_state", func(caller *wasmtime.Caller,
		prefixPtr, prefixLen, keysPtr, keysMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		prefix, ok := wasmtimeReadStringValidated(buf, prefixPtr, prefixLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.ListState(context.Background(), nil, prefix, uint32(keysPtr), uint32(keysMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_fetch: (i32,i32 x4, i32,i32) -> i64
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatFetch(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_fetch", func(caller *wasmtime.Caller,
		methodPtr, methodLen, urlPtr, urlLen, headersPtr, headersLen, bodyPtr, bodyLen int32,
		responsePtr, responseMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		method, ok := wasmtimeReadServiceName(buf, methodPtr, methodLen)
		if !ok {
			return errBadParamInt64
		}
		url, ok := wasmtimeReadStringValidated(buf, urlPtr, urlLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		headersJSON, ok := wasmtimeReadStringValidated(buf, headersPtr, headersLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		body, ok := wasmtimeReadStringValidated(buf, bodyPtr, bodyLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.Fetch(context.Background(), nil, method, url, headersJSON, body, uint32(responsePtr), uint32(responseMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_poll_child: (i32,i32, i32,i32) -> i64
// Polls for a child workflow result without blocking.
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatPollChild(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_poll_child", func(caller *wasmtime.Caller,
		runIDPtr, runIDLen, resultPtr, resultMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		runID, ok := wasmtimeReadServiceName(buf, runIDPtr, runIDLen)
		if !ok {
			return errBadParamInt64
		}
		return h.PollChild(context.Background(), nil, runID, uint32(resultPtr), uint32(resultMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_await_any_child: (i32,i32, i32,i32) -> i64
// Awaits any one of the given child workflows.
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatAwaitAnyChild(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_await_any_child", func(caller *wasmtime.Caller,
		idsPtr, idsLen, resultPtr, resultMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		runIDsJSON, ok := wasmtimeReadStringValidated(buf, idsPtr, idsLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.AwaitAnyChild(context.Background(), nil, runIDsJSON, uint32(resultPtr), uint32(resultMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_json_parse: (i32,i32, i32,i32) -> i64
// Validates and normalizes a JSON string via the host's encoding/json.
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatJsonParse(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_json_parse", func(caller *wasmtime.Caller,
		jsonPtr, jsonLen, outPtr, outMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		callCtx := ctxWithMem(context.Background(), buf)
		return h.JsonParse(callCtx, nil, uint32(jsonPtr), uint32(jsonLen), uint32(outPtr), uint32(outMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_json_stringify: (i32,i32, i32,i32) -> i64
// Validates and re-serializes a JSON string via the host's encoding/json.
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatJsonStringify(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_json_stringify", func(caller *wasmtime.Caller,
		ptr, len, outPtr, outMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		callCtx := ctxWithMem(context.Background(), buf)
		return h.JsonStringify(callCtx, nil, uint32(ptr), uint32(len), uint32(outPtr), uint32(outMaxLen))
	})
}

// ---------------------------------------------------------------------------
// cleat_complete: (i32, i32,i32) -> i64
// Signals workflow completion. status=0 means success, status=1 means error.
// The result string is stored via closure variables so the Execute method
// can retrieve it even if the module subsequently traps (e.g. via proc_exit).
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatComplete(linker *wasmtime.Linker, completeResult, completeErr *string) error {
	return linker.FuncWrap("env", "cleat_complete", func(caller *wasmtime.Caller,
		status int32, resultPtr int32, resultLen int32) int64 {
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		if resultLen > 0 && uint64(resultPtr)+uint64(resultLen) <= uint64(len(buf)) {
			result := string(buf[resultPtr : resultPtr+resultLen])
			if status == 0 {
				*completeResult = result
			} else {
				*completeErr = result
			}
		}
		return 0
	})
}

// ---------------------------------------------------------------------------
// cleat_poll_work: (i32,i32, i32,i32) -> i64
// entryName(ptr,maxLen), argsJSON(ptr,maxLen)
// Returns the work data set by Execute() before calling _start.
// ---------------------------------------------------------------------------

func (b *wasmtimeBackend) registerCleatPollWork(linker *wasmtime.Linker) error {
	return linker.FuncWrap("env", "cleat_poll_work", func(caller *wasmtime.Caller,
		entryNamePtr int32, entryNameMaxLen int32,
		argsPtr int32, argsMaxLen int32) int64 {
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}

		// Write entry point name.
		entryBytes := []byte(b.workEntryPoint)
		entryLen := len(entryBytes)
		if entryLen > int(entryNameMaxLen) {
			entryLen = int(entryNameMaxLen)
		}
		if entryLen > 0 {
			copy(buf[entryNamePtr:entryNamePtr+int32(entryLen)], entryBytes[:entryLen])
		}

		// Write input JSON.
		argsLen := len(b.workInput)
		if argsLen > int(argsMaxLen) {
			argsLen = int(argsMaxLen)
		}
		if argsLen > 0 {
			copy(buf[argsPtr:argsPtr+int32(argsLen)], b.workInput[:argsLen])
		}

		return (int64(entryLen) << 32) | int64(argsLen)
	})
}

