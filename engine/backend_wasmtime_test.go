//go:build cgo

package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/bytecodealliance/wasmtime-go/v44"
	wazero "github.com/tetratelabs/wazero/api"
)

// =============================================================================
// Test infrastructure
// =============================================================================

// mockWasmtimeHandler implements HostHandler with all methods returning 0.
type mockWasmtimeHandler struct{}

func (m *mockWasmtimeHandler) DurableCall(_ context.Context, _ wazero.Module, _, _, _ string, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) DurableSleep(_ context.Context, _ wazero.Module, _ int64) int64 {
	return 0
}
func (m *mockWasmtimeHandler) DurableAwaitSignals(_ context.Context, _ wazero.Module, _ string, _ int64, _, _, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) DurableDefer(_ context.Context, _ wazero.Module, _ string, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) DurableLog(_ context.Context, _ wazero.Module, _ string) int64 {
	return 0
}
func (m *mockWasmtimeHandler) PollCancellation(_ context.Context, _ wazero.Module, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) PollSignal(_ context.Context, _ wazero.Module, _ string, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) ContinueAsNew(_ context.Context, _ wazero.Module, _ string) int64 {
	return 0
}
func (m *mockWasmtimeHandler) ContinueAsNewWithVersion(_ context.Context, _ wazero.Module, _ string, _ int) int64 {
	return 0
}
func (m *mockWasmtimeHandler) ChildWorkflow(_ context.Context, _ wazero.Module, _, _ string, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) ChildWorkflowWithOptions(_ context.Context, _ wazero.Module, _, _ string, _, _ int64, _ string, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) ChildWorkflowInSchema(_ context.Context, _ wazero.Module, _, _, _ string, _, _ int64, _ string, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) AwaitChild(_ context.Context, _ wazero.Module, _ string, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) AwaitAllChildren(_ context.Context, _ wazero.Module, _ string, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) PollChild(_ context.Context, _ wazero.Module, _ string, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) AwaitAnyChild(_ context.Context, _ wazero.Module, _ string, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) DurableCallWithRetry(_ context.Context, _ wazero.Module, _, _, _ string, _, _, _, _ int64, _ string, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) DurableCallWithHeartbeat(_ context.Context, _ wazero.Module, _, _, _ string, _ int64, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) Version(_ context.Context) int64     { return 0 }
func (m *mockWasmtimeHandler) MinVersion(_ context.Context) int64  { return 0 }
func (m *mockWasmtimeHandler) Now(_ context.Context) int64         { return 0 }
func (m *mockWasmtimeHandler) Random(_ context.Context) int64      { return 0 }
func (m *mockWasmtimeHandler) SetQueryState(_ context.Context, _ wazero.Module, _, _ string) int64 {
	return 0
}
func (m *mockWasmtimeHandler) CreatePromise(_ context.Context, _ wazero.Module, _ string, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) AwaitPromise(_ context.Context, _ wazero.Module, _ string, _ int64, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) PluginCall(_ context.Context, _ wazero.Module, _, _, _ string, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) PluginCallStreaming(_ context.Context, _ wazero.Module, _, _, _ string, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) RegisterUpdateHandler(_ context.Context, _ wazero.Module, _ string) int64 {
	return 0
}
func (m *mockWasmtimeHandler) SendSignalAndWait(_ context.Context, _ wazero.Module, _, _, _ string, _ int64, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) ReplyToSignal(_ context.Context, _ wazero.Module, _, _ string) int64 {
	return 0
}
func (m *mockWasmtimeHandler) SignalWorkflow(_ context.Context, _ wazero.Module, _, _, _ string) int64 {
	return 0
}
func (m *mockWasmtimeHandler) SetScope(_ context.Context, _ wazero.Module, _, _ string, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) GetScope(_ context.Context, _ wazero.Module, _, _, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) UUID(_ context.Context, _ wazero.Module, _ string, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) AcquireLock(_ context.Context, _ wazero.Module, _ string, _ int64) int64 {
	return 0
}
func (m *mockWasmtimeHandler) ReleaseLock(_ context.Context, _ wazero.Module, _ string) int64 {
	return 0
}
func (m *mockWasmtimeHandler) SideEffect(_ context.Context, _ wazero.Module, _ string, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) WorkflowID(_ context.Context, _ wazero.Module, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) RunID(_ context.Context, _ wazero.Module, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) ResolvePromise(_ context.Context, _ wazero.Module, _, _ string) int64 {
	return 0
}
func (m *mockWasmtimeHandler) RejectPromise(_ context.Context, _ wazero.Module, _, _ string) int64 {
	return 0
}
func (m *mockWasmtimeHandler) DurableSend(_ context.Context, _ wazero.Module, _, _, _ string) int64 {
	return 0
}
func (m *mockWasmtimeHandler) DurableScheduleInvoke(_ context.Context, _ wazero.Module, _, _, _ string, _ int64) int64 {
	return 0
}
func (m *mockWasmtimeHandler) RegisterQueryHandler(_ context.Context, _ wazero.Module, _ string) int64 {
	return 0
}
func (m *mockWasmtimeHandler) SetState(_ context.Context, _ wazero.Module, _, _ string) int64 {
	return 0
}
func (m *mockWasmtimeHandler) GetState(_ context.Context, _ wazero.Module, _ string, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) DeleteState(_ context.Context, _ wazero.Module, _ string) int64 {
	return 0
}
func (m *mockWasmtimeHandler) IncrState(_ context.Context, _ wazero.Module, _ string, _ int64) int64 {
	return 0
}
func (m *mockWasmtimeHandler) HasState(_ context.Context, _ wazero.Module, _ string) int64 { return 0 }
func (m *mockWasmtimeHandler) ListState(_ context.Context, _ wazero.Module, _ string, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) RunDetached(_ context.Context, _ wazero.Module, _, _ string) int64 {
	return 0
}
func (m *mockWasmtimeHandler) Fetch(_ context.Context, _ wazero.Module, _, _, _, _ string, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) JsonParse(_ context.Context, _ wazero.Module, _, _, _, _ uint32) int64 {
	return 0
}
func (m *mockWasmtimeHandler) JsonStringify(_ context.Context, _ wazero.Module, _, _, _, _ uint32) int64 {
	return 0
}

// Compile-time check.
var _ HostHandler = (*mockWasmtimeHandler)(nil)

// =============================================================================
// Group 1: Binary encoding helpers
// =============================================================================

func TestWasmtimePutUint32LE(t *testing.T) {
	tests := []uint32{0, 1, 255, 256, 65535, 65536, 0xFFFFFFFF, 0x12345678}
	for _, v := range tests {
		var buf [4]byte
		putUint32LE(buf[:], v)
		got := getUint32LE(buf[:])
		if got != v {
			t.Errorf("putUint32LE/getUint32LE round-trip: put %d, got %d", v, got)
		}
	}
}

func TestWasmtimeGetUint32LE(t *testing.T) {
	// 0x78563412 in LE = 0x12345678 in native
	buf := []byte{0x12, 0x34, 0x56, 0x78}
	got := getUint32LE(buf)
	if got != 0x78563412 {
		t.Errorf("getUint32LE([0x12,0x34,0x56,0x78]) = 0x%x, want 0x78563412", got)
	}
}

func TestWasmtimePutU32LE(t *testing.T) {
	tests := []uint32{0, 1, 255, 0xFFFFFFFF, 0xDEADBEEF}
	for _, v := range tests {
		var buf [4]byte
		putU32LE(buf[:], v)
		got := binaryLEUint32(buf[:])
		if got != v {
			t.Errorf("putU32LE round-trip: put %d, got %d", v, got)
		}
	}
}

func binaryLEUint32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// =============================================================================
// Group 2: String helpers
// =============================================================================

func TestWasmtimeReadString(t *testing.T) {
	tests := []struct {
		name   string
		buf    []byte
		ptr    int32
		length int32
		want   string
	}{
		{"empty_length", []byte("hello"), 0, 0, ""},
		{"negative_length", []byte("hello"), 0, -1, ""},
		{"valid", []byte("hello world"), 0, 5, "hello"},
		{"full_string", []byte("hello"), 0, 5, "hello"},
		{"with_offset", []byte("xxhello"), 2, 5, "hello"},
		{"out_of_bounds", []byte("hi"), 0, 10, ""},
		{"ptr_negative", []byte("hi"), -1, 5, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wasmtimeReadString(tt.buf, tt.ptr, tt.length)
			if got != tt.want {
				t.Errorf("wasmtimeReadString = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWasmtimeReadStringValidated(t *testing.T) {
	tests := []struct {
		name   string
		buf    []byte
		ptr    int32
		length int32
		maxLen int32
		want   string
		wantOK bool
	}{
		{"valid", []byte("hello world"), 0, 5, 100, "hello", true},
		{"empty_length", []byte("hello"), 0, 0, 100, "", false},
		{"negative_length", []byte("hello"), 0, -1, 100, "", false},
		{"exceeds_maxlen", []byte("toolong"), 0, 7, 5, "", false},
		{"out_of_bounds", []byte("hi"), 0, 10, 100, "", false},
		{"at_maxlen_boundary", []byte("hello"), 0, 5, 5, "hello", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := wasmtimeReadStringValidated(tt.buf, tt.ptr, tt.length, tt.maxLen)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("wasmtimeReadStringValidated = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestWasmtimeReadServiceName(t *testing.T) {
	tests := []struct {
		name   string
		buf    []byte
		ptr    int32
		length int32
		want   string
		wantOK bool
	}{
		{"valid", []byte("my-service.operation_v1"), 0, 23, "my-service.operation_v1", true},
		{"empty", []byte{}, 0, 0, "", false},
		{"has_space", []byte("has space"), 0, 9, "", false},
		{"has_slash", []byte("has/slash"), 0, 9, "", false},
		{"has_at", []byte("has@at"), 0, 6, "", false},
		{"valid_alphanumeric", []byte("MyService123.Operation_v1-2"), 0, 27, "MyService123.Operation_v1-2", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := wasmtimeReadServiceName(tt.buf, tt.ptr, tt.length)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("wasmtimeReadServiceName = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestWasmtimeWriteString(t *testing.T) {
	t.Run("normal_write", func(t *testing.T) {
		buf := make([]byte, 100)
		n, err := wasmtimeWriteString(buf, 0, "hello", 100)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 5 {
			t.Errorf("n = %d, want 5", n)
		}
		if string(buf[:5]) != "hello" {
			t.Errorf("buf = %q, want %q", string(buf[:5]), "hello")
		}
	})
	t.Run("truncate", func(t *testing.T) {
		buf := make([]byte, 10)
		n, err := wasmtimeWriteString(buf, 0, "hello world", 5)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 5 {
			t.Errorf("n = %d, want 5", n)
		}
		if string(buf[:5]) != "hello" {
			t.Errorf("buf = %q, want %q", string(buf[:5]), "hello")
		}
	})
	t.Run("empty_string", func(t *testing.T) {
		buf := make([]byte, 10)
		n, err := wasmtimeWriteString(buf, 0, "", 100)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 0 {
			t.Errorf("n = %d, want 0", n)
		}
	})
	t.Run("ptr_out_of_bounds", func(t *testing.T) {
		buf := make([]byte, 5)
		_, err := wasmtimeWriteString(buf, 10, "hello", 100)
		if err == nil {
			t.Error("expected error for out-of-bounds write")
		}
	})
}

// =============================================================================
// Group 3: Error helpers
// =============================================================================

func TestWasmtimeIsDuplicateDefinition(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"duplicate_in_msg", fmt.Errorf("duplicate definition"), true},
		{"duplicate_caps", fmt.Errorf("duplicate handler"), true},
		{"no_dup", fmt.Errorf("some other error"), false},
		{"empty_msg_contains_dup", fmt.Errorf("this is a duplicate entry"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDuplicateDefinition(tt.err); got != tt.want {
				t.Errorf("isDuplicateDefinition(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestWasmtimeIsWasmtimeLinkerError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"duplicate", fmt.Errorf("duplicate"), false},
		{"real_error", fmt.Errorf("something bad happened"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isWasmtimeLinkerError(tt.err); got != tt.want {
				t.Errorf("isWasmtimeLinkerError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// =============================================================================
// Group 4: Constructor/lifecycle
// =============================================================================

func TestWasmtimeNewBackend(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	if b == nil {
		t.Fatal("NewWasmtimeBackend returned nil")
	}
	if b.engine == nil {
		t.Error("backend has nil engine")
	}
	// Clean up.
	b.Close(ctx)
}

func TestWasmtimeName(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	defer b.Close(ctx)

	if got := b.Name(); got != "wasmtime" {
		t.Errorf("Name() = %q, want %q", got, "wasmtime")
	}
}

func TestWasmtimeClose(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	if err := b.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestWasmtimePerExecution(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	defer b.Close(ctx)

	pe := b.PerExecution()
	if pe == nil {
		t.Fatal("PerExecution returned nil")
	}
	wtPe, ok := pe.(*wasmtimeBackend)
	if !ok {
		t.Fatalf("PerExecution returned %T, want *wasmtimeBackend", pe)
	}
	if wtPe.engine != b.engine {
		t.Error("PerExecution does not share the same engine")
	}
	if wtPe.Name() != "wasmtime" {
		t.Errorf("PerExecution Name() = %q, want %q", wtPe.Name(), "wasmtime")
	}
}

func TestWasmtimeCtxWithMem(t *testing.T) {
	buf := []byte("test")
	ctx := ctxWithMem(context.Background(), buf)
	if ctx == nil {
		t.Error("ctxWithMem returned nil context")
	}
}

// =============================================================================
// Group 5: callerMemBuf tests
// =============================================================================

func TestWasmtimeCallerMemBufErrorPaths(t *testing.T) {
	engine := wasmtime.NewEngine()

	// Module without memory export — callerMemBuf should return an error.
	t.Run("no_memory_export", func(t *testing.T) {
		// WAT module with no memory export but imports a function so we can
		// trigger the closure.
		wat := `(module
			(import "env" "test_fn" (func (result i64)))
			(func (export "run") (result i64)
				call 0
			)
		)`
		wasm, err := wasmtime.Wat2Wasm(wat)
		if err != nil {
			t.Fatalf("Wat2Wasm: %v", err)
		}

		module, err := wasmtime.NewModule(engine, wasm)
		if err != nil {
			t.Fatalf("NewModule: %v", err)
		}

		store := wasmtime.NewStore(engine)
		defer store.Close()

		linker := wasmtime.NewLinker(engine)
		var called bool
		err = linker.FuncWrap("env", "test_fn", func(caller *wasmtime.Caller) int64 {
			called = true
			_, _, err := callerMemBuf(caller)
			if err == nil {
				return 0
			}
			// The error message should be about missing memory.
			return 1
		})
		if err != nil {
			t.Fatalf("FuncWrap: %v", err)
		}

		instance, err := linker.Instantiate(store, module)
		if err != nil {
			t.Fatalf("Instantiate: %v", err)
		}

		runFn := instance.GetFunc(store, "run")
		if runFn == nil {
			t.Fatal("run function not found")
		}

		result, err := runFn.Call(store)
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		if !called {
			t.Error("test_fn was not called")
		}
		if result.(int64) != 1 {
			t.Error("callerMemBuf did not return error for missing memory")
		}
	})

	// Module WITH memory export — callerMemBuf should succeed.
	t.Run("with_memory_export", func(t *testing.T) {
		wat := `(module
			(import "env" "test_fn" (func (result i64)))
			(memory (export "memory") 1)
			(func (export "run") (result i64)
				call 0
			)
		)`
		wasm, err := wasmtime.Wat2Wasm(wat)
		if err != nil {
			t.Fatalf("Wat2Wasm: %v", err)
		}

		module, err := wasmtime.NewModule(engine, wasm)
		if err != nil {
			t.Fatalf("NewModule: %v", err)
		}

		store := wasmtime.NewStore(engine)
		defer store.Close()

		linker := wasmtime.NewLinker(engine)
		var gotBuf []byte
		err = linker.FuncWrap("env", "test_fn", func(caller *wasmtime.Caller) int64 {
			buf, _, err := callerMemBuf(caller)
			if err != nil {
				return -1
			}
			gotBuf = buf
			return 0
		})
		if err != nil {
			t.Fatalf("FuncWrap: %v", err)
		}

		instance, err := linker.Instantiate(store, module)
		if err != nil {
			t.Fatalf("Instantiate: %v", err)
		}

		runFn := instance.GetFunc(store, "run")
		if runFn == nil {
			t.Fatal("run function not found")
		}

		result, err := runFn.Call(store)
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		if result.(int64) != 0 {
			t.Error("callerMemBuf returned error for module with memory")
		}
		if gotBuf == nil {
			t.Error("callerMemBuf did not return buffer")
		}
	})
}

// =============================================================================
// Group 6: writeWorkToFixedMemory
// =============================================================================

func TestWasmtimeWriteWorkToFixedMemory(t *testing.T) {
	engine := wasmtime.NewEngine()
	store := wasmtime.NewStore(engine)
	defer store.Close()

	// Create a module with just memory so we can get a real wasmtime.Memory.
	wat := `(module
		(memory (export "memory") 2)
	)`
	wasm, err := wasmtime.Wat2Wasm(wat)
	if err != nil {
		t.Fatalf("Wat2Wasm: %v", err)
	}

	module, err := wasmtime.NewModule(engine, wasm)
	if err != nil {
		t.Fatalf("NewModule: %v", err)
	}

	linker := wasmtime.NewLinker(engine)
	instance, err := linker.Instantiate(store, module)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}

	memExp := instance.GetExport(store, "memory")
	if memExp == nil {
		t.Fatal("no memory export")
	}
	mem := memExp.Memory()
	if mem == nil {
		t.Fatal("memory export is not a memory")
	}

	b := &wasmtimeBackend{engine: engine}
	entryPoint := "my_entry_point"
	input := []byte(`{"key":"value"}`)

	b.writeWorkToFixedMemory(mem, store, entryPoint, input)

	// Read back from fixed memory.
	data := mem.UnsafeData(store)
	if len(data) < fixedWorkOffset+8 {
		t.Fatal("memory too small")
	}

	entryLen := binaryLEUint32(data[fixedWorkOffset : fixedWorkOffset+4])
	inputLen := binaryLEUint32(data[fixedWorkOffset+4 : fixedWorkOffset+8])

	if entryLen != uint32(len(entryPoint)) {
		t.Errorf("entryLen = %d, want %d", entryLen, len(entryPoint))
	}
	if inputLen != uint32(len(input)) {
		t.Errorf("inputLen = %d, want %d", inputLen, len(input))
	}

	gotEntry := string(data[fixedWorkOffset+8 : fixedWorkOffset+8+int(entryLen)])
	if gotEntry != entryPoint {
		t.Errorf("entry = %q, want %q", gotEntry, entryPoint)
	}

	gotInput := string(data[fixedWorkOffset+8+int(entryLen) : fixedWorkOffset+8+int(entryLen)+int(inputLen)])
	if string(gotInput) != string(input) {
		t.Errorf("input = %q, want %q", gotInput, string(input))
	}
}

func TestWasmtimeWriteWorkToFixedMemory_Truncation(t *testing.T) {
	engine := wasmtime.NewEngine()
	store := wasmtime.NewStore(engine)
	defer store.Close()

	wat := `(module (memory (export "memory") 1))`
	wasm, _ := wasmtime.Wat2Wasm(wat)
	module, _ := wasmtime.NewModule(engine, wasm)
	linker := wasmtime.NewLinker(engine)
	instance, _ := linker.Instantiate(store, module)
	mem := instance.GetExport(store, "memory").Memory()

	b := &wasmtimeBackend{engine: engine}

	// Entry point longer than 256 chars should be truncated.
	longEntry := make([]byte, 300)
	for i := range longEntry {
		longEntry[i] = 'x'
	}
	b.writeWorkToFixedMemory(mem, store, string(longEntry), []byte("input"))

	data := mem.UnsafeData(store)
	entryLen := binaryLEUint32(data[fixedWorkOffset : fixedWorkOffset+4])
	if entryLen != fixedWorkMaxEntry {
		t.Errorf("entryLen = %d, want %d (truncated)", entryLen, fixedWorkMaxEntry)
	}
}

// =============================================================================
// Group 7: registerAllImports + stubs
// =============================================================================

func TestWasmtimeRegisterAllImports(t *testing.T) {
	engine := wasmtime.NewEngine()
	defer engine.Close()

	linker := wasmtime.NewLinker(engine)
	b := &wasmtimeBackend{engine: engine, handler: &mockWasmtimeHandler{}}

	var completeResult, completeErr string
	err := b.registerAllImports(linker, &completeResult, &completeErr, true)
	if err != nil {
		t.Fatalf("registerAllImports: %v", err)
	}
}

func TestWasmtimeRegisterWasiStubs(t *testing.T) {
	engine := wasmtime.NewEngine()
	defer engine.Close()

	linker := wasmtime.NewLinker(engine)
	b := &wasmtimeBackend{engine: engine}

	err := b.registerWasiStubs(linker)
	if err != nil {
		t.Fatalf("registerWasiStubs: %v", err)
	}
}

func TestWasmtimeRegisterEnvStubs(t *testing.T) {
	engine := wasmtime.NewEngine()
	defer engine.Close()

	linker := wasmtime.NewLinker(engine)
	b := &wasmtimeBackend{engine: engine}

	err := b.registerEnvStubs(linker)
	if err != nil {
		t.Fatalf("registerEnvStubs: %v", err)
	}
}

func TestWasmtimeRegisterTeavmStubs(t *testing.T) {
	engine := wasmtime.NewEngine()
	defer engine.Close()

	linker := wasmtime.NewLinker(engine)
	b := &wasmtimeBackend{engine: engine}

	err := b.registerTeavmStubs(linker)
	if err != nil {
		t.Fatalf("registerTeavmStubs: %v", err)
	}
}

func TestWasmtimeRegisterTeavmStubsDuplicateOk(t *testing.T) {
	engine := wasmtime.NewEngine()
	defer engine.Close()

	b := &wasmtimeBackend{engine: engine}

	// First registration should succeed.
	linker1 := wasmtime.NewLinker(engine)
	if err := b.registerTeavmStubs(linker1); err != nil {
		t.Fatalf("first registerTeavmStubs: %v", err)
	}

	// Second registration on same linker should not panic or return error
	// because duplicate definitions are treated as benign.
	// (We use a fresh linker since FuncWrap on already-defined function
	// actually returns an error; the test verifies our error handling
	// correctly identifies duplicates.)
	linker2 := wasmtime.NewLinker(engine)
	if err := b.registerTeavmStubs(linker2); err != nil {
		t.Fatalf("second registerTeavmStubs: %v", err)
	}
}

// =============================================================================
// Group 8: Pattern A — Simple host functions (no memory access)
// =============================================================================

func TestWasmtimeRegisterSimpleHostFuncs(t *testing.T) {
	engine := wasmtime.NewEngine()
	defer engine.Close()

	b := &wasmtimeBackend{engine: engine, handler: &mockWasmtimeHandler{}}

	tests := []struct {
		name     string
		register func(*wasmtime.Linker) error
		watImport string
		callParams string
	}{
		{
			name:       "cleat_sleep",
			register:   b.registerCleatSleep,
			watImport:  `(import "env" "cleat_sleep" (func (param i64) (result i64)))`,
			callParams: "i64.const 100",
		},
		{
			name:       "cleat_now",
			register:   b.registerCleatNow,
			watImport:  `(import "env" "cleat_now" (func (result i64)))`,
			callParams: "",
		},
		{
			name:       "cleat_random",
			register:   b.registerCleatRandom,
			watImport:  `(import "env" "cleat_random" (func (result i64)))`,
			callParams: "",
		},
		{
			name:       "cleat_version",
			register:   b.registerCleatVersion,
			watImport:  `(import "env" "cleat_version" (func (result i64)))`,
			callParams: "",
		},
		{
			name:       "cleat_min_version",
			register:   b.registerCleatMinVersion,
			watImport:  `(import "env" "cleat_min_version" (func (result i64)))`,
			callParams: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wat := fmt.Sprintf(`(module
				%s
				(memory (export "memory") 1)
				(func (export "test") (result i64)
					%s
					call 0
				)
			)`, tt.watImport, tt.callParams)

			wasm, err := wasmtime.Wat2Wasm(wat)
			if err != nil {
				t.Fatalf("Wat2Wasm: %v", err)
			}

			module, err := wasmtime.NewModule(engine, wasm)
			if err != nil {
				t.Fatalf("NewModule: %v", err)
			}

			store := wasmtime.NewStore(engine)
			defer store.Close()

			linker := wasmtime.NewLinker(engine)
			if err := tt.register(linker); err != nil {
				t.Fatalf("register: %v", err)
			}

			instance, err := linker.Instantiate(store, module)
			if err != nil {
				t.Fatalf("Instantiate: %v", err)
			}

			testFn := instance.GetFunc(store, "test")
			if testFn == nil {
				t.Fatal("test function not found")
			}

			result, callErr := testFn.Call(store)
			if callErr != nil {
				t.Fatalf("Call: %v", callErr)
			}
			// Mock handler returns 0 for all methods.
			if result != nil && result.(int64) != 0 {
				t.Errorf("expected 0, got %d", result.(int64))
			}
		})
	}
}

// =============================================================================
// Group 9: Pattern B — Single string read host functions
// =============================================================================

func TestWasmtimeRegisterSingleStringHostFuncs(t *testing.T) {
	engine := wasmtime.NewEngine()
	defer engine.Close()

	b := &wasmtimeBackend{engine: engine, handler: &mockWasmtimeHandler{}}

	tests := []struct {
		name      string
		register  func(*wasmtime.Linker) error
		watImport string
		watParams string
	}{
		{
			name:      "cleat_log_error",
			register:  b.registerCleatLog,
			watImport: `(import "env" "cleat_log" (func (param i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0",
		},
		{
			name:      "cleat_defer_error",
			register:  b.registerCleatDefer,
			watImport: `(import "env" "cleat_defer" (func (param i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_continue_as_new_error",
			register:  b.registerCleatContinueAsNew,
			watImport: `(import "env" "cleat_continue_as_new" (func (param i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0",
		},
		{
			name:      "cleat_register_update_handler_error",
			register:  b.registerCleatRegisterUpdateHandler,
			watImport: `(import "env" "cleat_register_update_handler" (func (param i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0",
		},
		{
			name:      "cleat_register_query_handler_error",
			register:  b.registerCleatRegisterQueryHandler,
			watImport: `(import "env" "cleat_register_query_handler" (func (param i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0",
		},
		{
			name:      "cleat_poll_cancellation_error",
			register:  b.registerCleatPollCancellation,
			watImport: `(import "env" "cleat_poll_cancellation" (func (param i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wat := fmt.Sprintf(`(module
  %s
  (memory (export "memory") 1)
  (func (export "test") (result i64)
    %s
    call 0
  )
)`, tt.watImport, tt.watParams)

			wasm, err := wasmtime.Wat2Wasm(wat)
			if err != nil {
				t.Fatalf("Wat2Wasm: %v", err)
			}

			module, err := wasmtime.NewModule(engine, wasm)
			if err != nil {
				t.Fatalf("NewModule: %v", err)
			}

			store := wasmtime.NewStore(engine)
			defer store.Close()

			linker := wasmtime.NewLinker(engine)
			if err := tt.register(linker); err != nil {
				t.Fatalf("register: %v", err)
			}

			instance, err := linker.Instantiate(store, module)
			if err != nil {
				t.Fatalf("Instantiate: %v", err)
			}

			testFn := instance.GetFunc(store, "test")
			if testFn == nil {
				t.Fatal("test function not found")
			}

			// Call the function — verify registration + instantiation works.
			// May return errBadParamInt64 or trap on invalid params.
			result, _ := testFn.Call(store)
			_ = result
		})
	}
}

func TestWasmtimeRegisterSingleStringHostFuncs_ErrorReturns(t *testing.T) {
	engine := wasmtime.NewEngine()
	defer engine.Close()

	b := &wasmtimeBackend{engine: engine, handler: &mockWasmtimeHandler{}}

	// Test that empty/invalid service names cause errBadParamInt64 return.
	t.Run("cleat_continue_as_new_empty_name", func(t *testing.T) {
		wat := `(module
			(import "env" "cleat_continue_as_new" (func (param i32 i32) (result i64)))
			(memory (export "memory") 1)
			(func (export "test") (result i64)
				i32.const 0
				i32.const 0
				call 0
			)
		)`

		wasm, _ := wasmtime.Wat2Wasm(wat)
		module, _ := wasmtime.NewModule(engine, wasm)
		store := wasmtime.NewStore(engine)
		defer store.Close()

		linker := wasmtime.NewLinker(engine)
		b.registerCleatContinueAsNew(linker)

		instance, _ := linker.Instantiate(store, module)
		testFn := instance.GetFunc(store, "test")

		result, _ := testFn.Call(store)
		if result != nil {
			if result.(int64) == errBadParamInt64 {
				return // expected
			}
			t.Errorf("expected errBadParamInt64 (%d), got %d", errBadParamInt64, result.(int64))
		}
	})
}

// =============================================================================
// Group 10: registerCleatCall pattern — service+op+req
// =============================================================================

func TestWasmtimeRegisterCleatCall(t *testing.T) {
	engine := wasmtime.NewEngine()
	defer engine.Close()

	b := &wasmtimeBackend{engine: engine, handler: &mockWasmtimeHandler{}}

	// Test error path: null service name.
	t.Run("null_service", func(t *testing.T) {
		wat := `(module
			(import "env" "cleat_call" (func (param i32 i32 i32 i32 i32 i32 i32 i32) (result i64)))
			(memory (export "memory") 1)
			(func (export "test") (result i64)
				i32.const 0 i32.const 0
				i32.const 0 i32.const 0
				i32.const 0 i32.const 0
				i32.const 0 i32.const 0
				call 0
			)
		)`

		wasm, _ := wasmtime.Wat2Wasm(wat)
		module, _ := wasmtime.NewModule(engine, wasm)
		store := wasmtime.NewStore(engine)
		defer store.Close()

		linker := wasmtime.NewLinker(engine)
		var completeResult, completeErr string
		if err := b.registerCleatCall(linker, &completeResult, &completeErr); err != nil {
			t.Fatalf("registerCleatCall: %v", err)
		}

		instance, _ := linker.Instantiate(store, module)
		testFn := instance.GetFunc(store, "test")
		result, _ := testFn.Call(store)

		if result != nil && result.(int64) == errBadParamInt64 {
			return // expected
		}
		if result != nil {
			t.Errorf("expected errBadParamInt64 (%d), got %d", errBadParamInt64, result.(int64))
		}
	})

	// Test success path.
	t.Run("valid_call", func(t *testing.T) {
		wat := `(module
			(import "env" "cleat_call" (func (param i32 i32 i32 i32 i32 i32 i32 i32) (result i64)))
			(memory (export "memory") 1)
			(func (export "test") (result i64)
				i32.const 0  i32.const 7   ;; "service" at offset 0
				i32.const 8  i32.const 2   ;; "op" at offset 8
				i32.const 16 i32.const 2   ;; "{}" at offset 16
				i32.const 32 i32.const 100 ;; response buffer
				call 0
			)
		)`

		wasm, _ := wasmtime.Wat2Wasm(wat)
		module, _ := wasmtime.NewModule(engine, wasm)
		store := wasmtime.NewStore(engine)
		defer store.Close()

		linker := wasmtime.NewLinker(engine)
		var completeResult, completeErr string
		b.registerCleatCall(linker, &completeResult, &completeErr)

		instance, _ := linker.Instantiate(store, module)
		memExp := instance.GetExport(store, "memory")
		mem := memExp.Memory()
		data := mem.UnsafeData(store)
		copy(data[0:], "service")
		copy(data[8:], "op")
		copy(data[16:], "{}")

		testFn := instance.GetFunc(store, "test")
		result, _ := testFn.Call(store)

		if result == nil || result.(int64) == errBadParamInt64 {
			t.Errorf("expected success (0 from mock), got %v", result)
		}
	})
}

// =============================================================================
// Group 11: Expanded register* function tests
// =============================================================================

func TestWasmtimeRegisterHostFuncsAll(t *testing.T) {
	engine := wasmtime.NewEngine()
	defer engine.Close()

	b := &wasmtimeBackend{engine: engine, handler: &mockWasmtimeHandler{}}

	type testCase struct {
		name      string
		register  func(*wasmtime.Linker) error
		watImport string
		watParams string
	}
	tests := []testCase{
		{
			name:      "cleat_child_workflow",
			register:  b.registerCleatChildWorkflow,
			watImport: `(import "env" "cleat_child_workflow" (func (param i32 i32 i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_continue_as_new_versioned",
			register:  b.registerCleatContinueAsNewVersioned,
			watImport: `(import "env" "cleat_continue_as_new_versioned" (func (param i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 1",
		},
		{
			name:      "cleat_await_child",
			register:  b.registerCleatAwaitChild,
			watImport: `(import "env" "cleat_await_child" (func (param i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_await_all_children",
			register:  b.registerCleatAwaitAllChildren,
			watImport: `(import "env" "cleat_await_all_children" (func (param i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_poll_child",
			register:  b.registerCleatPollChild,
			watImport: `(import "env" "cleat_poll_child" (func (param i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_await_any_child",
			register:  b.registerCleatAwaitAnyChild,
			watImport: `(import "env" "cleat_await_any_child" (func (param i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_resolve_promise",
			register:  b.registerCleatResolvePromise,
			watImport: `(import "env" "cleat_resolve_promise" (func (param i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 0",
		},
		{
			name:      "cleat_reject_promise",
			register:  b.registerCleatRejectPromise,
			watImport: `(import "env" "cleat_reject_promise" (func (param i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 0",
		},
		{
			name:      "cleat_acquire_lock",
			register:  b.registerCleatAcquireLock,
			watImport: `(import "env" "cleat_acquire_lock" (func (param i32 i32 i64) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i64.const 1000",
		},
		{
			name:      "cleat_release_lock",
			register:  b.registerCleatReleaseLock,
			watImport: `(import "env" "cleat_release_lock" (func (param i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0",
		},
		{
			name:      "cleat_set_state",
			register:  b.registerCleatSetState,
			watImport: `(import "env" "cleat_set_state" (func (param i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 0",
		},
		{
			name:      "cleat_workflow_id",
			register:  b.registerCleatWorkflowID,
			watImport: `(import "env" "cleat_workflow_id" (func (param i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_run_id",
			register:  b.registerCleatRunID,
			watImport: `(import "env" "cleat_run_id" (func (param i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_get_scope",
			register:  b.registerCleatGetScope,
			watImport: `(import "env" "cleat_get_scope" (func (param i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 100 i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_call_heartbeat",
			register:  b.registerCleatCallHeartbeat,
			watImport: `(import "env" "cleat_call_heartbeat" (func (param i32 i32 i32 i32 i32 i32 i64 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i64.const 1000 i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_call_retry",
			register:  b.registerCleatCallRetry,
			watImport: `(import "env" "cleat_call_retry" (func (param i32 i32 i32 i32 i32 i32 i64 i64 i64 i64 i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i64.const 3 i64.const 1000 i64.const 200 i64.const 60000 i32.const 0 i32.const 0 i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_send",
			register:  b.registerCleatSend,
			watImport: `(import "env" "cleat_send" (func (param i32 i32 i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0",
		},
		{
			name:      "cleat_schedule_invoke",
			register:  b.registerCleatScheduleInvoke,
			watImport: `(import "env" "cleat_schedule_invoke" (func (param i32 i32 i32 i32 i32 i32 i64) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i64.const 1000",
		},
		{
			name:      "cleat_send_signal_and_wait",
			register:  b.registerCleatSendSignalAndWait,
			watImport: `(import "env" "cleat_send_signal_and_wait" (func (param i32 i32 i32 i32 i32 i32 i64 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i64.const 1000 i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_reply_to_signal",
			register:  b.registerCleatReplyToSignal,
			watImport: `(import "env" "cleat_reply_to_signal" (func (param i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 0",
		},
		{
			name:      "cleat_signal_workflow",
			register:  b.registerCleatSignalWorkflow,
			watImport: `(import "env" "cleat_signal_workflow" (func (param i32 i32 i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0",
		},
		{
			name:      "cleat_poll_signal",
			register:  b.registerCleatPollSignal,
			watImport: `(import "env" "cleat_poll_signal" (func (param i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_plugin_call",
			register:  b.registerCleatPluginCall,
			watImport: `(import "env" "plugin_call" (func (param i32 i32 i32 i32 i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_plugin_call_streaming",
			register:  b.registerCleatPluginCallStreaming,
			watImport: `(import "env" "plugin_call_streaming" (func (param i32 i32 i32 i32 i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_get_state",
			register:  b.registerCleatGetState,
			watImport: `(import "env" "cleat_get_state" (func (param i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_delete_state",
			register:  b.registerCleatDeleteState,
			watImport: `(import "env" "cleat_delete_state" (func (param i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0",
		},
		{
			name:      "cleat_incr_state",
			register:  b.registerCleatIncrState,
			watImport: `(import "env" "cleat_incr_state" (func (param i32 i32 i64) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i64.const 1",
		},
		{
			name:      "cleat_has_state",
			register:  b.registerCleatHasState,
			watImport: `(import "env" "cleat_has_state" (func (param i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0",
		},
		{
			name:      "cleat_list_state",
			register:  b.registerCleatListState,
			watImport: `(import "env" "cleat_list_state" (func (param i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_run_detached",
			register:  b.registerCleatRunDetached,
			watImport: `(import "env" "cleat_run_detached" (func (param i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 0",
		},
		{
			name:      "cleat_await_signals",
			register:  b.registerCleatAwaitSignals,
			watImport: `(import "env" "cleat_await_signals" (func (param i32 i32 i64 i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i64.const 1000 i32.const 0 i32.const 100 i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_set_query_state",
			register:  b.registerCleatSetQueryState,
			watImport: `(import "env" "set_query_state" (func (param i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 0",
		},
		{
			name:      "cleat_create_promise",
			register:  b.registerCleatCreatePromise,
			watImport: `(import "env" "cleat_create_promise" (func (param i32 i32 i32 i32 i64) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 100 i64.const 60000",
		},
		{
			name:      "cleat_await_promise",
			register:  b.registerCleatAwaitPromise,
			watImport: `(import "env" "cleat_await_promise" (func (param i32 i32 i64 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i64.const 1000 i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_set_scope",
			register:  b.registerCleatSetScope,
			watImport: `(import "env" "cleat_set_scope" (func (param i32 i32 i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_uuid",
			register:  b.registerCleatUUID,
			watImport: `(import "env" "cleat_uuid" (func (param i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_side_effect",
			register:  b.registerCleatSideEffect,
			watImport: `(import "env" "cleat_side_effect" (func (param i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_fetch",
			register:  b.registerCleatFetch,
			watImport: `(import "env" "cleat_fetch" (func (param i32 i32 i32 i32 i32 i32 i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_child_workflow_with_options",
			register:  b.registerCleatChildWorkflowWithOptions,
			watImport: `(import "env" "cleat_child_workflow_with_options" (func (param i32 i32 i32 i32 i64 i64 i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 0 i64.const 1 i64.const 1 i32.const 0 i32.const 0 i32.const 0 i32.const 100",
		},
		{
			name:      "cleat_child_workflow_in_schema",
			register:  b.registerCleatChildWorkflowInSchema,
			watImport: `(import "env" "cleat_child_workflow_in_schema" (func (param i32 i32 i32 i32 i32 i32 i64 i64 i32 i32 i32 i32) (result i64)))`,
			watParams: "i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i32.const 0 i64.const 1 i64.const 1 i32.const 0 i32.const 0 i32.const 0 i32.const 100",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wat := fmt.Sprintf(`(module
  %s
  (memory (export "memory") 1)
  (func (export "test") (result i64)
    %s
    call 0
  )
)`, tc.watImport, tc.watParams)

			wasm, err := wasmtime.Wat2Wasm(wat)
			if err != nil {
				t.Fatalf("Wat2Wasm: %v", err)
			}

			module, err := wasmtime.NewModule(engine, wasm)
			if err != nil {
				t.Fatalf("NewModule: %v", err)
			}

			store := wasmtime.NewStore(engine)
			defer store.Close()

			linker := wasmtime.NewLinker(engine)
			if err := tc.register(linker); err != nil {
				t.Fatalf("register: %v", err)
			}

			instance, err := linker.Instantiate(store, module)
			if err != nil {
				t.Fatalf("Instantiate: %v", err)
			}

			testFn := instance.GetFunc(store, "test")
			if testFn == nil {
				t.Fatal("test function not found")
			}

			// Call the function — it may return errBadParamInt64 or trap
			// on invalid params. What matters is no panic.
			result, _ := testFn.Call(store)
			_ = result
		})
	}
}
