//go:build cgo

package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

// ---------------------------------------------------------------------------
// Section 1: Pure Go helper tests (always run with CGO)
// ---------------------------------------------------------------------------

func TestPutU32LE(t *testing.T) {
	// putU32LE (line 2554) is the same implementation as putUint32LE.
	// Exercise it separately to ensure both are covered.
	tests := []uint32{0, 1, 42, 0xFFFFFFFF, 0x80000000, 0x12345678}
	for _, v := range tests {
		var buf [4]byte
		putU32LE(buf[:], v)
		got := getUint32LE(buf[:])
		if got != v {
			t.Errorf("putU32LE round-trip %d: got %d", v, got)
		}
	}
}

func TestPutUint32LE_GetUint32LE_RoundTrip(t *testing.T) {
	tests := []uint32{0, 1, 42, 0xFFFFFFFF, 0x80000000, 0x12345678}
	for _, v := range tests {
		var buf [4]byte
		putUint32LE(buf[:], v)
		got := getUint32LE(buf[:])
		if got != v {
			t.Errorf("round-trip %d: got %d", v, got)
		}
	}
}

func TestPutUint32LE_Sequential(t *testing.T) {
	buf := make([]byte, 8)
	putUint32LE(buf[0:4], 0xDEADBEEF)
	putUint32LE(buf[4:8], 0xCAFEBABE)
	if got := getUint32LE(buf[0:4]); got != 0xDEADBEEF {
		t.Errorf("first: got 0x%X, want 0xDEADBEEF", got)
	}
	if got := getUint32LE(buf[4:8]); got != 0xCAFEBABE {
		t.Errorf("second: got 0x%X, want 0xCAFEBABE", got)
	}
}

func TestWasmtimeReadString(t *testing.T) {
	buf := []byte("hello world!!")
	tests := []struct {
		name   string
		ptr    int32
		length int32
		want   string
	}{
		{"normal", 0, 5, "hello"},
		{"full", 0, 13, "hello world!!"},
		{"offset", 6, 5, "world"},
		{"zero length", 0, 0, ""},
		{"negative length", 0, -1, ""},
		{"ptr overflow", int32(len(buf)), 5, ""},
		{"length overflow", 0, int32(len(buf)) + 10, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wasmtimeReadString(buf, tt.ptr, tt.length)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWasmtimeReadStringValidated(t *testing.T) {
	// Use a buffer large enough for max-length boundary test.
	buf := make([]byte, 200)
	copy(buf, "hello world!!")
	maxLen := int32(100)

	tests := []struct {
		name    string
		ptr     int32
		length  int32
		maxLen  int32
		want    string
		wantOK  bool
	}{
		{"normal", 0, 5, maxLen, "hello", true},
		{"full", 0, 13, maxLen, "hello world!!", true},
		{"exactly maxLen", 0, 50, 50, string(buf[:50]), true},
		{"zero length", 0, 0, maxLen, "", false},
		{"negative length", 0, -1, maxLen, "", false},
		{"exceeds maxLen", 0, 51, 50, "", false},
		{"ptr overflow", 150, 51, maxLen, "", false},
		{"length overflow", 0, 250, 300, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := wasmtimeReadStringValidated(buf, tt.ptr, tt.length, tt.maxLen)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWasmtimeReadServiceName(t *testing.T) {
	// Buffer with "valid.name_01" at offset 0, then "!!bad" at offset 14.
	buf := []byte("valid.name_01!!bad")

	tests := []struct {
		name   string
		ptr    int32
		length int32
		want   string
		wantOK bool
	}{
		{"valid with dots", 0, 13, "valid.name_01", true},
		{"empty", 0, 0, "", false},
		{"invalid char at ptr", 13, 3, "", false},
		{"ptr overflow", int32(len(buf)), 5, "", false},
		{"too long", 0, int32(MaxWasmStringLen) + 1, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := wasmtimeReadServiceName(buf, tt.ptr, tt.length)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWasmtimeReadServiceName_AllValidChars(t *testing.T) {
	valid := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._-"
	buf := []byte(valid)
	got, ok := wasmtimeReadServiceName(buf, 0, int32(len(valid)))
	if !ok {
		t.Error("all valid chars should pass")
	}
	if got != valid {
		t.Errorf("got %q, want %q", got, valid)
	}
}

func TestWasmtimeWriteString(t *testing.T) {
	t.Run("normal write", func(t *testing.T) {
		buf := make([]byte, 100)
		n, err := wasmtimeWriteString(buf, 10, "hello", 50)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 5 {
			t.Errorf("n = %d, want 5", n)
		}
		if got := string(buf[10:15]); got != "hello" {
			t.Errorf("got %q, want %q", got, "hello")
		}
	})

	t.Run("truncation at maxLen", func(t *testing.T) {
		buf := make([]byte, 100)
		n, err := wasmtimeWriteString(buf, 0, "hello world", 5)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 5 {
			t.Errorf("n = %d, want 5", n)
		}
		if got := string(buf[0:5]); got != "hello" {
			t.Errorf("got %q, want %q", got, "hello")
		}
	})

	t.Run("empty string", func(t *testing.T) {
		buf := make([]byte, 10)
		n, err := wasmtimeWriteString(buf, 0, "", 10)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 0 {
			t.Errorf("n = %d, want 0", n)
		}
	})

	t.Run("ptr overflow", func(t *testing.T) {
		buf := make([]byte, 10)
		_, err := wasmtimeWriteString(buf, 9, "ab", 10)
		if err == nil {
			t.Error("expected error for ptr overflow")
		}
	})
}

func TestIsWasmtimeLinkerError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"duplicate definition (benign)", errors.New("duplicate definition"), false},
		{"unknown import (linker error)", errors.New("unknown import"), true},
		{"has not been defined (linker error)", errors.New("has not been defined"), true},
		{"generic error", errors.New("something went wrong"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isWasmtimeLinkerError(tt.err)
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsDuplicateDefinition(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"contains duplicate", errors.New("duplicate definition for import"), true},
		{"no duplicate", errors.New("something went wrong"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isDuplicateDefinition(tt.err)
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Section 2: Struct-level tests (require functional wasmtime)
// ---------------------------------------------------------------------------

func TestWasmtimeBackend_Name(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	if got := b.Name(); got != "wasmtime" {
		t.Errorf("Name() = %q, want %q", got, "wasmtime")
	}
}

func TestWasmtimeBackend_PerExecution(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	pe := b.PerExecution()
	if pe == nil {
		t.Fatal("PerExecution() returned nil")
	}
	defer pe.Close(ctx)

	if pe.Name() != "wasmtime" {
		t.Errorf("PerExecution().Name() = %q, want %q", pe.Name(), "wasmtime")
	}

	wt, ok := pe.(*wasmtimeBackend)
	if !ok {
		t.Fatalf("PerExecution() returned %T, want *wasmtimeBackend", pe)
	}

	// Per-execution shares the engine but has fresh (nil) per-execution state.
	if wt.engine == nil {
		t.Error("PerExecution() engine is nil — should share the parent engine")
	}
	if wt.handler != nil {
		t.Error("PerExecution() handler should be nil (fresh state)")
	}
}

func TestWasmtimeBackend_Close(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}

	if err := b.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}

	// Close is idempotent — second close should not panic.
	if err := b.Close(ctx); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestWasmtimeBackend_ImplementsInterface(t *testing.T) {
	// Compile-time check that wasmtimeBackend satisfies WasmBackend.
	// This is already done in the source, but test guards against regression.
	var _ WasmBackend = (*wasmtimeBackend)(nil)
}

// ---------------------------------------------------------------------------
// Section 3: Execute integration tests (require functional wasmtime + cleat build)
// ---------------------------------------------------------------------------

func TestWasmtimeBackend_Execute_CompileError(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	// Invalid WASM bytes (not a valid module).
	// Nil session is safe here — compile happens before any handler call.
	_, err = b.Execute(ctx, []byte("not-valid-wasm"), "main", nil, nil)
	if err == nil {
		t.Fatal("expected compile error for invalid WASM bytes")
	}
	if err.Error() == "" {
		t.Error("error should have a message")
	}
}

func TestWasmtimeBackend_Execute_MinimalModule(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	// minimalWasm() produces a bare WASM module with no sections.
	// This exercises the non-component, non-WASI compile+instantiate path
	// up to the "no memory export" error at line 137-139.
	wasmBytes := minimalWasm()
	_, err = b.Execute(ctx, wasmBytes, "main", nil, nil)
	if err == nil {
		t.Log("minimal module executed (unexpected but OK)")
	} else {
		t.Logf("expected error from minimal module: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Section 4: WASM binary builder helpers
// ---------------------------------------------------------------------------

// leb128EncodeU32 encodes v as unsigned LEB128.
func leb128EncodeU32(v uint32) []byte {
	var buf []byte
	for {
		b := byte(v & 0x7F)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		buf = append(buf, b)
		if v == 0 {
			break
		}
	}
	return buf
}

// leb128EncodeS32 encodes v as signed LEB128 (32-bit).
func leb128EncodeS32(v int32) []byte {
	var buf []byte
	more := true
	for more {
		b := byte(v & 0x7F)
		v >>= 7
		if (v == 0 && (b&0x40) == 0) || (v == -1 && (b&0x40) != 0) {
			more = false
		} else {
			b |= 0x80
		}
		buf = append(buf, b)
	}
	return buf
}

// leb128EncodeS64 encodes v as signed LEB128 (64-bit).
func leb128EncodeS64(v int64) []byte {
	var buf []byte
	more := true
	for more {
		b := byte(v & 0x7F)
		v >>= 7
		if (v == 0 && (b&0x40) == 0) || (v == -1 && (b&0x40) != 0) {
			more = false
		} else {
			b |= 0x80
		}
		buf = append(buf, b)
	}
	return buf
}

// wasmValType constants for building WASM modules by hand.
const (
	wasmValTypeI32 = 0x7F
	wasmValTypeI64 = 0x7E
	wasmValTypeF32 = 0x7D
	wasmValTypeF64 = 0x7C
)

// writeLebU32 writes v as unsigned LEB128 to buf.
func writeLebU32(buf *bytes.Buffer, v uint32) {
	buf.Write(leb128EncodeU32(v))
}

// writeLebS64 writes v as signed LEB128 (64-bit) to buf.
func writeLebS64(buf *bytes.Buffer, v int64) {
	buf.Write(leb128EncodeS64(v))
}

// writeString writes a length-prefixed string to buf (WASM name format).
func writeString(buf *bytes.Buffer, s string) {
	writeLebU32(buf, uint32(len(s)))
	buf.WriteString(s)
}

// writeSection writes a WASM section header + content to buf.
func writeSection(buf *bytes.Buffer, id byte, content []byte) {
	buf.WriteByte(id)
	writeLebU32(buf, uint32(len(content)))
	buf.Write(content)
}

// buildImportWasm builds a minimal WASM module with one import and one export.
// The import is from moduleName/funcName with the given paramTypes and resultTypes
// (WASM valtype bytes). The module exports a "test" function that calls the import
// with zero-valued parameters, and exports "memory" (1 page).
func buildImportWasm(moduleName, funcName string, paramTypes, resultTypes []byte) []byte {
	var mod bytes.Buffer
	// Magic + version
	mod.Write([]byte{0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00})

	// --- Type section (id=1) ---
	var ts bytes.Buffer
	writeLebU32(&ts, 2) // 2 types
	// Type[0]: import signature
	ts.WriteByte(0x60) // functype
	writeLebU32(&ts, uint32(len(paramTypes)))
	ts.Write(paramTypes)
	writeLebU32(&ts, uint32(len(resultTypes)))
	ts.Write(resultTypes)
	// Type[1]: export signature (same as import)
	ts.WriteByte(0x60)
	writeLebU32(&ts, uint32(len(paramTypes)))
	ts.Write(paramTypes)
	writeLebU32(&ts, uint32(len(resultTypes)))
	ts.Write(resultTypes)
	writeSection(&mod, 1, ts.Bytes())

	// --- Import section (id=2) ---
	var imps bytes.Buffer
	writeLebU32(&imps, 1) // 1 import
	writeString(&imps, moduleName)
	writeString(&imps, funcName)
	imps.WriteByte(0x00) // import kind: func
	writeLebU32(&imps, 0) // type index 0 (the import's type)
	writeSection(&mod, 2, imps.Bytes())

	// --- Function section (id=3) ---
	var funcs bytes.Buffer
	writeLebU32(&funcs, 1) // 1 local function
	writeLebU32(&funcs, 1) // type index 1 (the export's type)
	writeSection(&mod, 3, funcs.Bytes())

	// --- Memory section (id=5) ---
	var mem bytes.Buffer
	writeLebU32(&mem, 1) // 1 memory
	mem.WriteByte(0x00)  // limits: no max
	writeLebU32(&mem, 1) // min 1 page (64KB)
	writeSection(&mod, 5, mem.Bytes())

	// --- Export section (id=7) ---
	var exports bytes.Buffer
	writeLebU32(&exports, 2) // 2 exports
	writeString(&exports, "memory")
	exports.WriteByte(0x02) // export kind: memory
	writeLebU32(&exports, 0) // mem index 0
	writeString(&exports, "test")
	exports.WriteByte(0x00) // export kind: func
	writeLebU32(&exports, 1) // func index 1 (import is 0, our func is 1)
	writeSection(&mod, 7, exports.Bytes())

	// --- Code section (id=10) ---
	var code bytes.Buffer
	writeLebU32(&code, 1) // 1 function body
	var body bytes.Buffer
	writeLebU32(&body, 0) // 0 local declarations
	// Push zero values for each param type
	for _, pt := range paramTypes {
		switch pt {
		case wasmValTypeI32:
			body.WriteByte(0x41) // i32.const
			writeLebU32(&body, 0)
		case wasmValTypeI64:
			body.WriteByte(0x42) // i64.const
			writeLebS64(&body, 0)
		}
	}
	body.WriteByte(0x10) // call
	writeLebU32(&body, 0) // func index 0 (the import)
	body.WriteByte(0x0B) // end
	writeLebU32(&code, uint32(body.Len()))
	code.Write(body.Bytes())
	writeSection(&mod, 10, code.Bytes())

	return mod.Bytes()
}

// buildImportWasmWithData is like buildImportWasm but appends a data section
// containing the given bytes at memory offset 0.
func buildImportWasmWithData(moduleName, funcName string, paramTypes, resultTypes []byte, data []byte) []byte {
	mod := buildImportWasm(moduleName, funcName, paramTypes, resultTypes)
	if len(data) == 0 {
		return mod
	}
	var buf bytes.Buffer
	buf.Write(mod)
	// --- Data section (id=11) ---
	var ds bytes.Buffer
	writeLebU32(&ds, 1) // 1 data segment
	ds.WriteByte(0x00)  // mode: active, memory 0
	ds.WriteByte(0x41)  // i32.const
	writeLebU32(&ds, 0) // offset 0
	ds.WriteByte(0x0B)  // end
	writeLebU32(&ds, uint32(len(data)))
	ds.Write(data)
	writeSection(&buf, 11, ds.Bytes())
	return buf.Bytes()
}

// buildMultiImportWasm builds a WASM module with multiple imports from "env",
// all with type () -> i64. Each import is specified by its function name.
// The module exports a "test" function that calls each import in order,
// dropping all results except the last.
func buildMultiImportWasm(funcNames []string) []byte {
	var mod bytes.Buffer
	mod.Write([]byte{0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00})

	n := len(funcNames)
	if n == 0 {
		n = 1
	}

	// --- Type section ---
	var ts bytes.Buffer
	writeLebU32(&ts, 2) // 2 types
	// Type[0]: () -> i64 for all imports
	ts.WriteByte(0x60) // functype
	writeLebU32(&ts, 0)
	writeLebU32(&ts, 1)
	ts.WriteByte(wasmValTypeI64)
	// Type[1]: () -> i64 for export
	ts.WriteByte(0x60)
	writeLebU32(&ts, 0)
	writeLebU32(&ts, 1)
	ts.WriteByte(wasmValTypeI64)
	writeSection(&mod, 1, ts.Bytes())

	// --- Import section ---
	var imps bytes.Buffer
	writeLebU32(&imps, uint32(len(funcNames)))
	for _, name := range funcNames {
		writeString(&imps, "env")
		writeString(&imps, name)
		imps.WriteByte(0x00) // func import
		writeLebU32(&imps, 0) // type index 0
	}
	if len(funcNames) == 0 {
		writeString(&imps, "env")
		writeString(&imps, "void")
		imps.WriteByte(0x00)
		writeLebU32(&imps, 0)
	}
	writeSection(&mod, 2, imps.Bytes())

	// --- Function section ---
	var funcs bytes.Buffer
	writeLebU32(&funcs, 1)
	writeLebU32(&funcs, 1) // type index 1
	writeSection(&mod, 3, funcs.Bytes())

	// --- Memory section ---
	var mem bytes.Buffer
	writeLebU32(&mem, 1)
	mem.WriteByte(0x00) // no max
	writeLebU32(&mem, 1)
	writeSection(&mod, 5, mem.Bytes())

	// --- Export section ---
	var exports bytes.Buffer
	writeLebU32(&exports, 2)
	writeString(&exports, "memory")
	exports.WriteByte(0x02)
	writeLebU32(&exports, 0)
	writeString(&exports, "test")
	exports.WriteByte(0x00)
	writeLebU32(&exports, 1) // func index 1 (regardless of # of imports)
	writeSection(&mod, 7, exports.Bytes())

	// --- Code section ---
	var code bytes.Buffer
	writeLebU32(&code, 1)
	var body bytes.Buffer
	writeLebU32(&body, 0) // 0 locals
	for i := 0; i < len(funcNames); i++ {
		body.WriteByte(0x10) // call
		writeLebU32(&body, uint32(i)) // import at index i
		if i < len(funcNames)-1 {
			body.WriteByte(0x1A) // drop (discard result, keep last)
		}
	}
	body.WriteByte(0x0B) // end
	writeLebU32(&code, uint32(body.Len()))
	code.Write(body.Bytes())
	writeSection(&mod, 10, code.Bytes())

	return mod.Bytes()
}

// ---------------------------------------------------------------------------
// Section 5: Test infrastructure helpers
// ---------------------------------------------------------------------------

// closeTestEnv manages the lifecycle of a wasmtime backend for testing.
type closeTestEnv struct {
	t      *testing.T
	ctx    context.Context
	cancel context.CancelFunc
	b      *wasmtimeBackend
}

// newCloseTestEnv creates a new wasmtime backend test environment.
// Skips the test if wasmtime is not available.
func newCloseTestEnv(t *testing.T) *closeTestEnv {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		cancel()
		t.Skipf("wasmtime backend not available: %v", err)
	}
	t.Cleanup(func() {
		b.Close(ctx)
		cancel()
	})
	return &closeTestEnv{t: t, ctx: ctx, cancel: cancel, b: b}
}

// Backend returns the wasmtime backend.
func (e *closeTestEnv) Backend() *wasmtimeBackend { return e.b }

// Context returns the test context.
func (e *closeTestEnv) Context() context.Context { return e.ctx }

// Close releases the backend immediately (in addition to t.Cleanup).
func (e *closeTestEnv) Close() {
	e.b.Close(e.ctx)
	e.cancel()
}

// runWasm compiles, links, instantiates, and calls an exported function
// in a WASM module using the given backend. The handler is set on the
// backend before the call so closures can dispatch to it.
func runWasm(t *testing.T, b *wasmtimeBackend, wasmBytes []byte, fnName string, h HostHandler, args ...interface{}) (interface{}, error) {
	t.Helper()

	store := wasmtime.NewStore(b.engine)
	defer store.Close()

	module, err := wasmtime.NewModule(b.engine, wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("compile: %w", err)
	}
	defer module.Close()

	linker := wasmtime.NewLinker(b.engine)
	var completeResult, completeErr string
	if err := b.registerAllImports(linker, &completeResult, &completeErr, false); err != nil {
		return nil, fmt.Errorf("register imports: %w", err)
	}

	instance, err := linker.Instantiate(store, module)
	if err != nil {
		return nil, fmt.Errorf("instantiate: %w", err)
	}

	fn := instance.GetFunc(store, fnName)
	if fn == nil {
		return nil, fmt.Errorf("export %q not found", fnName)
	}

	// Set handler on the backend so host function closures can dispatch to it.
	b.handler = h

	// Wrap fn.Call in recover to capture panics from closures (e.g., nil handler).
	var callResult interface{}
	var callErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				callErr = fmt.Errorf("call panicked: %v", r)
			}
		}()
		callResult, callErr = fn.Call(store, args...)
	}()
	if callErr != nil {
		return nil, fmt.Errorf("call: %w", callErr)
	}

	return callResult, nil
}

// ---------------------------------------------------------------------------
// Section 6: LEB128 encoding tests
// ---------------------------------------------------------------------------

func TestLeb128EncodeU32_Zero(t *testing.T) {
	b := leb128EncodeU32(0)
	if len(b) != 1 || b[0] != 0 {
		t.Fatalf("encode 0: got %x, want [00]", b)
	}
}

func TestLeb128EncodeU32_MultiByte(t *testing.T) {
	tests := []struct {
		v    uint32
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{42, []byte{0x2A}},
		{127, []byte{0x7F}},
		{128, []byte{0x80, 0x01}},
		{255, []byte{0xFF, 0x01}},
		{256, []byte{0x80, 0x02}},
		{16383, []byte{0xFF, 0x7F}},
		{16384, []byte{0x80, 0x80, 0x01}},
	}
	for _, tt := range tests {
		got := leb128EncodeU32(tt.v)
		if !bytes.Equal(got, tt.want) {
			t.Errorf("encode(%d) = %x, want %x", tt.v, got, tt.want)
		}
		// Verify round-trip via manual decode.
		decoded := uint32(0)
		for i, b := range got {
			decoded |= uint32(b&0x7F) << (7 * i)
			if b&0x80 == 0 {
				break
			}
		}
		if decoded != tt.v {
			t.Errorf("round-trip %d: got %d", tt.v, decoded)
		}
	}
}

func TestLeb128EncodeU32_Large(t *testing.T) {
	tests := []uint32{
		0xFFFFFFFF,
		0x80000000,
		0xDEADBEEF,
		0x0FFFFFFF,
		1 << 28,
	}
	for _, v := range tests {
		got := leb128EncodeU32(v)
		if len(got) == 0 || len(got) > 5 {
			t.Errorf("encode(%d) length = %d, want 1-5", v, len(got))
		}
		// Manual decode verify
		decoded := uint32(0)
		for i, b := range got {
			decoded |= uint32(b&0x7F) << (7 * i)
			if b&0x80 == 0 {
				break
			}
		}
		if decoded != v {
			t.Errorf("round-trip %d: got %d", v, decoded)
		}
	}
}

func TestLeb128EncodeS32_Various(t *testing.T) {
	tests := []struct {
		v    int32
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{42, []byte{0x2A}},
		{63, []byte{0x3F}},
		{64, []byte{0xC0, 0x00}},
		{-1, []byte{0x7F}},
		{-2, []byte{0x7E}},
		{-64, []byte{0x40}},
		{-65, []byte{0xBF, 0x7F}},
	}
	for _, tt := range tests {
		got := leb128EncodeS32(tt.v)
		if !bytes.Equal(got, tt.want) {
			t.Errorf("encode(%d) = %x, want %x", tt.v, got, tt.want)
		}
	}
}

func TestLeb128EncodeS32_Negative(t *testing.T) {
	tests := []int32{
		-1, -42, -127, -128, -129, -16383, -16384,
		-1 << 30,
		-0x7FFFFFFF - 1, // math.MinInt32
	}
	for _, v := range tests {
		got := leb128EncodeS32(v)
		if len(got) == 0 || len(got) > 5 {
			t.Errorf("encode(%d) length = %d, want 1-5", v, len(got))
		}
		// Manual signed decode verify
		decoded := int32(0)
		for i, b := range got {
			decoded |= int32(b&0x7F) << (7 * i)
			if b&0x80 == 0 {
				if b&0x40 != 0 {
					decoded |= ^((1 << (7 * (i + 1))) - 1) // sign extend
				}
				break
			}
		}
		if decoded != v {
			t.Errorf("round-trip %d: got %d", v, decoded)
		}
	}
}

func TestLeb128EncodeS64_Various(t *testing.T) {
	tests := []struct {
		v    int64
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		// Signed LEB128: 127 has bit6=1 so needs 2 bytes (continuation + zero sign)
		{127, []byte{0xFF, 0x00}},
		{128, []byte{0x80, 0x01}},
		{-1, []byte{0x7F}},
		{-128, []byte{0x80, 0x7F}},
		{1 << 40, nil},  // just check length
		{-1 << 40, nil},
		{0x7FFFFFFFFFFFFFFF, nil},  // max int64
		{-0x8000000000000000, nil}, // min int64
	}
	for _, tt := range tests {
		got := leb128EncodeS64(tt.v)
		if tt.want != nil {
			if !bytes.Equal(got, tt.want) {
				t.Errorf("encode(%d) = %x, want %x", tt.v, got, tt.want)
			}
		}
		if len(got) == 0 || len(got) > 10 {
			t.Errorf("encode(%d) length = %d, want 1-10", tt.v, len(got))
		}
		// Manual signed decode verify
		decoded := int64(0)
		for i, b := range got {
			decoded |= int64(b&0x7F) << (7 * i)
			if b&0x80 == 0 {
				if b&0x40 != 0 {
					decoded |= ^((int64(1) << (7 * (i + 1))) - 1)
				}
				break
			}
		}
		if decoded != tt.v {
			t.Errorf("round-trip %d: got %d", tt.v, decoded)
		}
	}
}

// ---------------------------------------------------------------------------
// Section 7: WASM builder tests
// ---------------------------------------------------------------------------

func TestBuildImportWasm_Valid(t *testing.T) {
	env := newCloseTestEnv(t)

	// Build a module importing cleat_now: () -> i64
	wasmBytes := buildImportWasm("env", "cleat_now", nil, []byte{wasmValTypeI64})

	// Should compile without error
	module, err := wasmtime.NewModule(env.b.engine, wasmBytes)
	if err != nil {
		t.Fatalf("NewModule: %v", err)
	}
	defer module.Close()

	// Verify exports
	exports := module.Exports()
	hasTest := false
	hasMemory := false
	for _, e := range exports {
		if e.Name() == "test" {
			hasTest = true
		}
		if e.Name() == "memory" {
			hasMemory = true
		}
	}
	if !hasTest {
		t.Error("module missing 'test' export")
	}
	if !hasMemory {
		t.Error("module missing 'memory' export")
	}
}

func TestBuildImportWasm_TypeSection(t *testing.T) {
	// Build a module with i32 params
	wasmBytes := buildImportWasm("env", "cleat_log",
		[]byte{wasmValTypeI32, wasmValTypeI32},
		[]byte{wasmValTypeI64})

	if len(wasmBytes) < 8 {
		t.Fatal("module too short")
	}
	// Verify magic + version
	if string(wasmBytes[:4]) != "\x00asm" {
		t.Error("bad magic number")
	}
	if wasmBytes[4] != 1 || wasmBytes[5] != 0 || wasmBytes[6] != 0 || wasmBytes[7] != 0 {
		t.Error("bad version")
	}
}

func TestBuildImportWasmWithData_Content(t *testing.T) {
	env := newCloseTestEnv(t)

	data := []byte("hello\x00world")
	wasmBytes := buildImportWasmWithData("env", "cleat_now", nil, []byte{wasmValTypeI64}, data)

	// Should compile (module is valid even with unreferenced data)
	module, err := wasmtime.NewModule(env.b.engine, wasmBytes)
	if err != nil {
		t.Fatalf("NewModule: %v", err)
	}
	defer module.Close()
}

func TestBuildImportWasmWithData_NilData(t *testing.T) {
	// Passing nil data should produce same module as buildImportWasm
	withData := buildImportWasmWithData("env", "cleat_now", nil, []byte{wasmValTypeI64}, nil)
	withoutData := buildImportWasm("env", "cleat_now", nil, []byte{wasmValTypeI64})

	if !bytes.Equal(withData, withoutData) {
		t.Error("buildImportWasmWithData with nil data should equal buildImportWasm")
	}
}

// ---------------------------------------------------------------------------
// Section 8: Test infrastructure tests
// ---------------------------------------------------------------------------

func TestNewCloseTestEnv_CreatesBackend(t *testing.T) {
	env := newCloseTestEnv(t)
	if env.b == nil {
		t.Fatal("backend is nil")
	}
	if env.b.engine == nil {
		t.Fatal("engine is nil")
	}
	if env.b.Name() != "wasmtime" {
		t.Errorf("Name() = %q, want %q", env.b.Name(), "wasmtime")
	}
}

func TestRunWasm_NoImports(t *testing.T) {
	env := newCloseTestEnv(t)

	// Build the simplest possible module: just an exported function that returns 42.
	var mod bytes.Buffer
	mod.Write([]byte{0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00})
	// Type section: (func (result i32))
	var ts bytes.Buffer
	writeLebU32(&ts, 1)
	ts.WriteByte(0x60) // functype
	writeLebU32(&ts, 0)
	writeLebU32(&ts, 1)
	ts.WriteByte(wasmValTypeI32)
	writeSection(&mod, 1, ts.Bytes())
	// Function section
	var funcs bytes.Buffer
	writeLebU32(&funcs, 1)
	writeLebU32(&funcs, 0) // type index 0
	writeSection(&mod, 3, funcs.Bytes())
	// Memory section
	var mem bytes.Buffer
	writeLebU32(&mem, 1)
	mem.WriteByte(0x00)
	writeLebU32(&mem, 1)
	writeSection(&mod, 5, mem.Bytes())
	// Export section
	var exports bytes.Buffer
	writeLebU32(&exports, 2)
	writeString(&exports, "memory")
	exports.WriteByte(0x02)
	writeLebU32(&exports, 0)
	writeString(&exports, "test")
	exports.WriteByte(0x00)
	writeLebU32(&exports, 0) // func index 0
	writeSection(&mod, 7, exports.Bytes())
	// Code section
	var code bytes.Buffer
	writeLebU32(&code, 1)
	var body bytes.Buffer
	writeLebU32(&body, 0)
	body.WriteByte(0x41) // i32.const
	writeLebU32(&body, 42)
	body.WriteByte(0x0B) // end
	writeLebU32(&code, uint32(body.Len()))
	code.Write(body.Bytes())
	writeSection(&mod, 10, code.Bytes())

	result, err := runWasm(t, env.b, mod.Bytes(), "test", &mockHostHandler{ret: 0})
	if err != nil {
		t.Fatalf("runWasm: %v", err)
	}
	if result == nil {
		t.Fatal("runWasm returned nil result")
	}
}

func TestRunWasm_NoExports(t *testing.T) {
	env := newCloseTestEnv(t)

	// Module with only memory, no exported function.
	wasmBytes := minimalWasm() // empty module, no exports

	_, err := runWasm(t, env.b, wasmBytes, "test", &mockHostHandler{ret: 0})
	if err == nil {
		t.Fatal("expected error for missing export 'test'")
	}
}

// ---------------------------------------------------------------------------
// Section 9: Closure tests
// ---------------------------------------------------------------------------

func TestClosure_ImportCleatNow(t *testing.T) {
	env := newCloseTestEnv(t)
	wasmBytes := buildImportWasm("env", "cleat_now", nil, []byte{wasmValTypeI64})

	result, err := runWasm(t, env.b, wasmBytes, "test", &mockHostHandler{ret: 42})
	if err != nil {
		t.Fatalf("runWasm: %v", err)
	}
	val, ok := result.(int64)
	if !ok {
		t.Fatalf("result type = %T, want int64", result)
	}
	if val != 42 {
		t.Errorf("cleat_now returned %d, want 42", val)
	}
}

func TestClosure_ImportCleatVersion(t *testing.T) {
	env := newCloseTestEnv(t)
	wasmBytes := buildImportWasm("env", "cleat_version", nil, []byte{wasmValTypeI64})

	result, err := runWasm(t, env.b, wasmBytes, "test", &mockHostHandler{ret: 99})
	if err != nil {
		t.Fatalf("runWasm: %v", err)
	}
	val, ok := result.(int64)
	if !ok {
		t.Fatalf("result type = %T, want int64", result)
	}
	if val != 99 {
		t.Errorf("cleat_version returned %d, want 99", val)
	}
}

func TestClosure_ImportCleatRandom(t *testing.T) {
	env := newCloseTestEnv(t)
	wasmBytes := buildImportWasm("env", "cleat_random", nil, []byte{wasmValTypeI64})

	result, err := runWasm(t, env.b, wasmBytes, "test", &mockHostHandler{ret: 77})
	if err != nil {
		t.Fatalf("runWasm: %v", err)
	}
	val, ok := result.(int64)
	if !ok {
		t.Fatalf("result type = %T, want int64", result)
	}
	if val != 77 {
		t.Errorf("cleat_random returned %d, want 77", val)
	}
}

func TestClosure_ImportCleatMinVersion(t *testing.T) {
	env := newCloseTestEnv(t)
	wasmBytes := buildImportWasm("env", "cleat_min_version", nil, []byte{wasmValTypeI64})

	result, err := runWasm(t, env.b, wasmBytes, "test", &mockHostHandler{ret: 5})
	if err != nil {
		t.Fatalf("runWasm: %v", err)
	}
	val, ok := result.(int64)
	if !ok {
		t.Fatalf("result type = %T, want int64", result)
	}
	if val != 5 {
		t.Errorf("cleat_min_version returned %d, want 5", val)
	}
}

func TestClosure_ImportCleatSleep(t *testing.T) {
	env := newCloseTestEnv(t)

	// cleat_sleep: (i64) -> i64
	wasmBytes := buildImportWasm("env", "cleat_sleep",
		[]byte{wasmValTypeI64}, []byte{wasmValTypeI64})

	result, err := runWasm(t, env.b, wasmBytes, "test",
		&mockHostHandler{ret: 0}, int64(0))
	if err != nil {
		t.Fatalf("runWasm: %v", err)
	}
	val, ok := result.(int64)
	if !ok {
		t.Fatalf("result type = %T, want int64", result)
	}
	if val != 0 {
		t.Errorf("cleat_sleep returned %d, want 0", val)
	}
}

func TestClosure_FiveImports(t *testing.T) {
	env := newCloseTestEnv(t)

	wasmBytes := buildMultiImportWasm([]string{
		"cleat_now", "cleat_version", "cleat_random",
		"cleat_min_version", "cleat_now",
	})

	result, err := runWasm(t, env.b, wasmBytes, "test", &mockHostHandler{ret: 55})
	if err != nil {
		t.Fatalf("runWasm: %v", err)
	}
	val, ok := result.(int64)
	if !ok {
		t.Fatalf("result type = %T, want int64", result)
	}
	if val != 55 {
		t.Errorf("five imports returned %d, want 55", val)
	}
}

func TestClosure_TwoImports(t *testing.T) {
	env := newCloseTestEnv(t)

	wasmBytes := buildMultiImportWasm([]string{"cleat_now", "cleat_version"})

	result, err := runWasm(t, env.b, wasmBytes, "test", &mockHostHandler{ret: 42})
	if err != nil {
		t.Fatalf("runWasm: %v", err)
	}
	val, ok := result.(int64)
	if !ok {
		t.Fatalf("result type = %T, want int64", result)
	}
	// Last imported function (cleat_version) returns mock ret
	if val != 42 {
		t.Errorf("two imports returned %d, want 42", val)
	}
}

func TestClosure_ThreeImports(t *testing.T) {
	env := newCloseTestEnv(t)

	wasmBytes := buildMultiImportWasm([]string{"cleat_now", "cleat_version", "cleat_random"})

	result, err := runWasm(t, env.b, wasmBytes, "test", &mockHostHandler{ret: 100})
	if err != nil {
		t.Fatalf("runWasm: %v", err)
	}
	val, ok := result.(int64)
	if !ok {
		t.Fatalf("result type = %T, want int64", result)
	}
	if val != 100 {
		t.Errorf("three imports returned %d, want 100", val)
	}
}

func TestClosure_ImportUnknownFunction(t *testing.T) {
	env := newCloseTestEnv(t)

	// Import a function that doesn't exist in the host
	wasmBytes := buildImportWasm("env", "nonexistent_func", nil, []byte{wasmValTypeI64})

	_, err := runWasm(t, env.b, wasmBytes, "test", &mockHostHandler{ret: 0})
	if err == nil {
		t.Fatal("expected error for unknown import")
	}
}

func TestClosure_ImportUnknownModule(t *testing.T) {
	env := newCloseTestEnv(t)

	// Import from a module that's not registered
	wasmBytes := buildImportWasm("unknown_module", "some_func", nil, []byte{wasmValTypeI64})

	_, err := runWasm(t, env.b, wasmBytes, "test", &mockHostHandler{ret: 0})
	if err == nil {
		t.Fatal("expected error for unknown module")
	}
}

func TestClosure_HandlerWired(t *testing.T) {
	env := newCloseTestEnv(t)

	// Create a handler that returns a specific non-zero value
	h := &mockHostHandler{ret: 12345}
	wasmBytes := buildImportWasm("env", "cleat_now", nil, []byte{wasmValTypeI64})

	result, err := runWasm(t, env.b, wasmBytes, "test", h)
	if err != nil {
		t.Fatalf("runWasm: %v", err)
	}
	val, ok := result.(int64)
	if !ok {
		t.Fatalf("result type = %T, want int64", result)
	}
	// The closure dispatches to h.Now(), which returns h.ret = 12345
	if val != 12345 {
		t.Errorf("handler returned %d, want 12345", val)
	}
}

func TestClosure_PerExecutionIsolated(t *testing.T) {
	env := newCloseTestEnv(t)

	wasmBytes := buildImportWasm("env", "cleat_now", nil, []byte{wasmValTypeI64})

	// Create two per-execution backends with different handler values
	pe1 := env.b.PerExecution().(*wasmtimeBackend)
	pe2 := env.b.PerExecution().(*wasmtimeBackend)

	h1 := &mockHostHandler{ret: 111}
	h2 := &mockHostHandler{ret: 222}

	result1, err := runWasm(t, pe1, wasmBytes, "test", h1)
	if err != nil {
		t.Fatalf("pe1: %v", err)
	}
	result2, err := runWasm(t, pe2, wasmBytes, "test", h2)
	if err != nil {
		t.Fatalf("pe2: %v", err)
	}

	val1 := result1.(int64)
	val2 := result2.(int64)

	if val1 != 111 {
		t.Errorf("pe1 returned %d, want 111", val1)
	}
	if val2 != 222 {
		t.Errorf("pe2 returned %d, want 222", val2)
	}
	if val1 == val2 && val1 == 111 {
		t.Error("both executions returned same value; isolation may be broken")
	}
}

func TestClosure_NilHandlerRecovery(t *testing.T) {
	env := newCloseTestEnv(t)

	wasmBytes := buildImportWasm("env", "cleat_now", nil, []byte{wasmValTypeI64})

	// Passing nil handler: the closure will try to call b.handler.Now()
	// which will panic. runWasm should propagate this as an error.
	_, err := runWasm(t, env.b, wasmBytes, "test", nil)
	if err == nil {
		t.Fatal("expected error/panic from nil handler, got nil")
	}
}

func TestClosure_MultipleCallsToImport(t *testing.T) {
	env := newCloseTestEnv(t)

	// Build module that calls cleat_now twice (via two imports of cleat_now)
	wasmBytes := buildMultiImportWasm([]string{"cleat_now", "cleat_now"})

	result, err := runWasm(t, env.b, wasmBytes, "test", &mockHostHandler{ret: 777})
	if err != nil {
		t.Fatalf("runWasm: %v", err)
	}
	val, ok := result.(int64)
	if !ok {
		t.Fatalf("result type = %T, want int64", result)
	}
	// The last import call returns 777
	if val != 777 {
		t.Errorf("multiple calls returned %d, want 777", val)
	}
}

func TestClosure_RepeatedInstantiation(t *testing.T) {
	env := newCloseTestEnv(t)

	wasmBytes := buildImportWasm("env", "cleat_now", nil, []byte{wasmValTypeI64})

	// Run the same module twice with the same handler
	h := &mockHostHandler{ret: 42}
	for i := 0; i < 3; i++ {
		result, err := runWasm(t, env.b, wasmBytes, "test", h)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		val, ok := result.(int64)
		if !ok {
			t.Fatalf("iteration %d: result type = %T, want int64", i, result)
		}
		if val != 42 {
			t.Errorf("iteration %d: got %d, want 42", i, val)
		}
	}
}

func TestClosure_MemorySectionBinary(t *testing.T) {
	env := newCloseTestEnv(t)

	// Verify the module builder includes a valid memory section.
	wasmBytes := buildImportWasm("env", "cleat_now", nil, []byte{wasmValTypeI64})

	// Compile the module and verify it has a memory export.
	module, err := wasmtime.NewModule(env.b.engine, wasmBytes)
	if err != nil {
		t.Fatalf("NewModule: %v", err)
	}
	defer module.Close()

	// Check for memory export through module metadata.
	hasMemory := false
	for _, e := range module.Exports() {
		if e.Name() == "memory" {
			hasMemory = true
			break
		}
	}
	if !hasMemory {
		t.Fatal("module should have a 'memory' export")
	}
}

func TestClosure_ModuleTypeMatching(t *testing.T) {
	env := newCloseTestEnv(t)

	// Build a module with wrong import type: import cleat_now (needs () -> i64)
	// as (i32) -> i64, which should cause a link error.
	var mod bytes.Buffer
	mod.Write([]byte{0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00})

	// Type section: one type (i32) -> i64
	var ts bytes.Buffer
	writeLebU32(&ts, 1)
	ts.WriteByte(0x60) // functype
	writeLebU32(&ts, 1)
	ts.WriteByte(wasmValTypeI32)
	writeLebU32(&ts, 1)
	ts.WriteByte(wasmValTypeI64)
	writeSection(&mod, 1, ts.Bytes())

	// Import section: import cleat_now with wrong type
	var imps bytes.Buffer
	writeLebU32(&imps, 1)
	writeString(&imps, "env")
	writeString(&imps, "cleat_now")
	imps.WriteByte(0x00)
	writeLebU32(&imps, 0)
	writeSection(&mod, 2, imps.Bytes())

	// No function/export/code - instantiation should fail at link time
	// because cleat_now is registered as () -> i64 but module says (i32) -> i64
	_, err := runWasm(t, env.b, mod.Bytes(), "test", &mockHostHandler{ret: 0})
	if err == nil {
		t.Fatal("expected type mismatch error from linker")
	}
}

func TestClosure_FourImports(t *testing.T) {
	env := newCloseTestEnv(t)

	wasmBytes := buildMultiImportWasm([]string{
		"cleat_now", "cleat_version", "cleat_random", "cleat_min_version",
	})

	result, err := runWasm(t, env.b, wasmBytes, "test", &mockHostHandler{ret: 2024})
	if err != nil {
		t.Fatalf("runWasm: %v", err)
	}
	val, ok := result.(int64)
	if !ok {
		t.Fatalf("result type = %T, want int64", result)
	}
	if val != 2024 {
		t.Errorf("four imports returned %d, want 2024", val)
	}
}

func TestClosure_ImportPreservesOrder(t *testing.T) {
	env := newCloseTestEnv(t)

	// Import in different orders should still resolve correctly.
	// Version 1: now, version
	wasm1 := buildMultiImportWasm([]string{"cleat_now", "cleat_version"})
	// Version 2: version, now
	wasm2 := buildMultiImportWasm([]string{"cleat_version", "cleat_now"})

	h := &mockHostHandler{ret: 42}

	result1, err := runWasm(t, env.b, wasm1, "test", h)
	if err != nil {
		t.Fatalf("order1: %v", err)
	}
	result2, err := runWasm(t, env.b, wasm2, "test", h)
	if err != nil {
		t.Fatalf("order2: %v", err)
	}
	val1 := result1.(int64)
	val2 := result2.(int64)
	if val1 != 42 || val2 != 42 {
		t.Errorf("order: got %d, %d, want 42, 42", val1, val2)
	}
}

func TestClosure_HandlerChangeBetweenCalls(t *testing.T) {
	env := newCloseTestEnv(t)

	wasmBytes := buildImportWasm("env", "cleat_now", nil, []byte{wasmValTypeI64})

	// First call with handler returning 10
	h1 := &mockHostHandler{ret: 10}
	result1, err := runWasm(t, env.b, wasmBytes, "test", h1)
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	// Second call with handler returning 20
	h2 := &mockHostHandler{ret: 20}
	result2, err := runWasm(t, env.b, wasmBytes, "test", h2)
	if err != nil {
		t.Fatalf("second: %v", err)
	}

	val1 := result1.(int64)
	val2 := result2.(int64)
	if val1 != 10 {
		t.Errorf("first call: got %d, want 10", val1)
	}
	if val2 != 20 {
		t.Errorf("second call: got %d, want 20", val2)
	}
}

func TestClosure_DifferentReturnValues(t *testing.T) {
	env := newCloseTestEnv(t)

	wasmBytes := buildImportWasm("env", "cleat_now", nil, []byte{wasmValTypeI64})

	// Test with different handler return values.
	for _, expected := range []int64{0, 1, -1, 1000, 1 << 40, -1 << 40} {
		h := &mockHostHandler{ret: expected}
		result, err := runWasm(t, env.b, wasmBytes, "test", h)
		if err != nil {
			t.Fatalf("runWasm(ret=%d): %v", expected, err)
		}
		val, ok := result.(int64)
		if !ok {
			t.Fatalf("ret=%d: result type = %T, want int64", expected, result)
		}
		if val != expected {
			t.Errorf("ret=%d: got %d", expected, val)
		}
	}
}

func TestClosure_AllS32Variants(t *testing.T) {
	// This is a pure-Go test (no WASM) that verifies leb128EncodeS32
	// produces encodings that the module builder can consume for closure types.
	// It tests the boundary between LEB128 encoding and WASM module construction.
	tests := []int32{0, 1, -1, 63, -64, 64, -65, 127, -128, 128, -129, 1 << 20, -(1 << 20)}
	for _, v := range tests {
		enc := leb128EncodeS32(v)
		if len(enc) == 0 || len(enc) > 5 {
			t.Errorf("leb128EncodeS32(%d) length %d out of range", v, len(enc))
		}
		// Decode and verify
		decoded := int32(0)
		for i, b := range enc {
			decoded |= int32(b&0x7F) << (7 * i)
			if b&0x80 == 0 {
				if b&0x40 != 0 {
					decoded |= ^((1 << (7 * (i + 1))) - 1)
				}
				break
			}
		}
		if decoded != v {
			t.Errorf("leb128EncodeS32(%d) round-trip = %d", v, decoded)
		}
	}
}

func TestClosure_ImportThenExecuteNonGo(t *testing.T) {
	env := newCloseTestEnv(t)

	// Build a module that imports cleat_now and conforms to Execute's
	// expected export signature: (i32, i32, i32, i32) -> i64.
	// The export function ignores the 4 input params, calls cleat_now,
	// and returns the result packed as an error code.
	wasmBytes := buildImportWasm("env", "cleat_now", []byte{
		wasmValTypeI32, wasmValTypeI32, wasmValTypeI32, wasmValTypeI32,
	}, []byte{wasmValTypeI64})

	// Execute requires a non-nil session. The handler on the backend
	// is set by Execute's session parameter.
	// We pass a nil session to verify the Execute path handles it.
	_, err := env.b.Execute(env.ctx, wasmBytes, "test", nil, &mockHostHandler{ret: 99})
	if err != nil {
		t.Logf("Execute with non-Go module: %v (expected if session/handler mismatch)", err)
	}
}

func TestClosure_PerExecutionBackendUsesSeparateHandler(t *testing.T) {
	env := newCloseTestEnv(t)

	wasmBytes := buildImportWasm("env", "cleat_now", nil, []byte{wasmValTypeI64})

	// Set a handler on the base backend
	env.b.handler = &mockHostHandler{ret: 1}

	// Create per-execution backend - should copy handler state
	pe := env.b.PerExecution().(*wasmtimeBackend)

	// pe starts with nil handler (fresh per-execution state)
	if pe.handler != nil {
		t.Error("PerExecution() handler should be nil initially")
	}

	// Set a different handler on pe
	result, err := runWasm(t, pe, wasmBytes, "test", &mockHostHandler{ret: 999})
	if err != nil {
		t.Fatalf("pe run: %v", err)
	}
	val := result.(int64)
	if val != 999 {
		t.Errorf("pe returned %d, want 999", val)
	}

	// Base backend handler should still be 1
	if env.b.handler.(*mockHostHandler).ret != 1 {
		t.Error("base backend handler was modified by per-execution")
	}
}

func TestClosure_CleatCompleteResultCapture(t *testing.T) {
	env := newCloseTestEnv(t)

	// cleat_complete: (i32, i32, i32) -> i64
	// params: status, resultPtr, resultLen
	wasmBytes := buildImportWasm("env", "cleat_complete",
		[]byte{wasmValTypeI32, wasmValTypeI32, wasmValTypeI32},
		[]byte{wasmValTypeI64})

	// Call with status=0, ptr=0, len=0
	result, err := runWasm(t, env.b, wasmBytes, "test",
		&mockHostHandler{ret: 0}, int32(0), int32(0), int32(0))
	if err != nil {
		t.Fatalf("runWasm: %v", err)
	}
	val, ok := result.(int64)
	if !ok {
		t.Fatalf("result type = %T, want int64", result)
	}
	if val != 0 {
		t.Errorf("cleat_complete returned %d, want 0", val)
	}
}

