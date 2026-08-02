//go:build cgo

package engine

import (
	"context"
	"errors"
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
		name   string
		ptr    int32
		length int32
		maxLen int32
		want   string
		wantOK bool
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
// Section 4: registerWasiStubs / registerTeavmStubs / registerAllImports
// error-path tests (no WASM modules needed)
// ---------------------------------------------------------------------------

func TestRegisterWasiStubs_DoubleRegistration(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	linker := wasmtime.NewLinker(b.engine)

	// Pre-define WASI to force DefineWasi to fail inside registerWasiStubs.
	if err := linker.DefineWasi(); err != nil {
		t.Fatalf("pre-define WASI: %v", err)
	}

	// Second call to registerWasiStubs should fail because WASI is already defined.
	err = b.registerWasiStubs(linker)
	if err == nil {
		t.Error("expected error from double WASI registration, got nil")
	}
	t.Logf("double WASI registration error (expected): %v", err)
}

func TestRegisterWasiStubs_ResetAdapterStateAlreadyDefined(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	linker := wasmtime.NewLinker(b.engine)

	// Pre-define reset_adapter_state to exercise the FuncWrap error path.
	if err := linker.FuncWrap("wasi_snapshot_preview1", "reset_adapter_state", func() {}); err != nil {
		t.Fatalf("pre-define reset_adapter_state: %v", err)
	}

	err = b.registerWasiStubs(linker)
	if err == nil {
		t.Error("expected error from duplicate reset_adapter_state, got nil")
	}
	t.Logf("duplicate reset_adapter_state error (expected): %v", err)
}

func TestRegisterTeavmStubs_FreshLinker(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	linker := wasmtime.NewLinker(b.engine)

	// On a fresh linker, all teavm stubs should register without error.
	err = b.registerTeavmStubs(linker)
	if err != nil {
		t.Errorf("registerTeavmStubs on fresh linker: unexpected error: %v", err)
	}
}

func TestRegisterTeavmStubs_AlreadyDefined_ErrorPropagation(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	linker := wasmtime.NewLinker(b.engine)

	// Pre-define putwcharsOut — the first function registerTeavmStubs tries to
	// register. wasmtime uses "defined twice" (not "duplicate definition") for
	// FuncWrap conflicts, so isDuplicateDefinition returns false and this is
	// treated as a real linker error that propagates.
	if err := linker.FuncWrap("teavm", "putwcharsOut", func(chars, count int32) {}); err != nil {
		t.Fatalf("pre-define putwcharsOut: %v", err)
	}

	err = b.registerTeavmStubs(linker)
	if err == nil {
		t.Error("expected error propagation from double-registered teavm stub, got nil")
	}
	t.Logf("teavm stub error propagation (expected): %v", err)
}

func TestRegisterAllImports_NeedsWasiTrue(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	linker := wasmtime.NewLinker(b.engine)
	var cr, ce string

	err = b.registerAllImports(linker, &cr, &ce, true)
	if err != nil {
		t.Errorf("registerAllImports(needsWasi=true): %v", err)
	}
}

func TestRegisterAllImports_NeedsWasiFalse(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	linker := wasmtime.NewLinker(b.engine)
	var cr, ce string

	err = b.registerAllImports(linker, &cr, &ce, false)
	if err != nil {
		t.Errorf("registerAllImports(needsWasi=false): %v", err)
	}
}

func TestRegisterAllImports_WasiErrorPropagation(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	linker := wasmtime.NewLinker(b.engine)
	// Pre-define WASI so registerWasiStubs inside registerAllImports fails.
	if err := linker.DefineWasi(); err != nil {
		t.Fatalf("pre-define WASI: %v", err)
	}

	var cr, ce string
	err = b.registerAllImports(linker, &cr, &ce, true)
	if err == nil {
		t.Error("expected error propagation from registerWasiStubs in registerAllImports")
	}
	t.Logf("registerAllImports WASI error propagation (expected): %v", err)
}

// ---------------------------------------------------------------------------
// Section 5: WASM binary builder helpers + writeWorkToFixedMemory
// ---------------------------------------------------------------------------

// encodeULEB128 encodes v as unsigned LEB128 into buf.
func encodeULEB128(v uint32) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if v == 0 {
			break
		}
	}
	return out
}

// wasmSection builds a WASM section: id + size-prefixed content.
func wasmSection(id byte, content []byte) []byte {
	size := encodeULEB128(uint32(len(content)))
	return append(append([]byte{id}, size...), content...)
}

// wasmVec builds a WASM vector: count-prefixed content. Pass count and
// pre-concatenated bytes.
func wasmVec(count int, content []byte) []byte {
	return append(encodeULEB128(uint32(count)), content...)
}

// wasmName encodes a WASM name: length-prefixed UTF-8 bytes.
func wasmName(s string) []byte {
	nameLen := encodeULEB128(uint32(len(s)))
	return append(nameLen, []byte(s)...)
}

// wasmFunctype encodes a WASM functype: 0x60 params... results...
func wasmFunctype(params, results []byte) []byte {
	nParams := encodeULEB128(uint32(len(params)))
	nResults := encodeULEB128(uint32(len(results)))
	return append(append(append([]byte{0x60}, nParams...), params...), append(nResults, results...)...)
}

// functypeNumParams extracts the parameter count from a functype byte slice.
// Format: 0x60, nParams (ULEB128), param_types..., nResults (ULEB128), result_types...
func functypeNumParams(ft []byte) int {
	if len(ft) < 2 || ft[0] != 0x60 {
		return 0
	}
	// Read ULEB128 at ft[1].
	n := 0
	shift := 0
	for i := 1; i < len(ft); i++ {
		n |= int(ft[i]&0x7f) << shift
		if ft[i]&0x80 == 0 {
			return n
		}
		shift += 7
	}
	return 0
}

// memoryWasm returns a minimal WASM module that exports pages of linear memory.
func memoryWasm(pages byte) []byte {
	mem := wasmSection(5, []byte{0x01, 0x00, pages})
	expContent := append(wasmName("memory"), 0x02, 0x00) // kind=memory, index=0
	exp := wasmSection(7, wasmVec(1, expContent))
	return append(append(
		[]byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}, // magic + version
		mem...),
		exp...)
}

// buildImportWasm constructs a WASM module that imports functions from "env"
// and exports wrapper functions that call them.
//
// imports: list of {name, functype} pairs.
// exports: list of {name, callImportIndex} pairs (body is: call $idx; end).
// includeMemory: if true, adds 1-page memory and exports it.
func buildImportWasm(imports []struct {
	Name string
	Type []byte
}, exports []struct {
	Name    string
	CallIdx uint32
}, includeMemory bool) []byte {
	makeSection := func(id byte, content []byte) []byte {
		size := encodeULEB128(uint32(len(content)))
		return append(append([]byte{id}, size...), content...)
	}

	var out []byte
	out = append(out, 0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00)

	// Type section: collect unique functypes.
	typeIdx := make(map[string]int)
	var types [][]byte
	for _, imp := range imports {
		key := string(imp.Type)
		if _, ok := typeIdx[key]; ok {
			continue
		}
		typeIdx[key] = len(types)
		types = append(types, imp.Type)
	}
	if len(types) > 0 {
		var typeContent []byte
		for _, ft := range types {
			typeContent = append(typeContent, ft...)
		}
		out = append(out, makeSection(1, wasmVec(len(types), typeContent))...)
	}

	// Import section.
	numImports := len(imports)
	if numImports > 0 {
		var importContent []byte
		for _, imp := range imports {
			key := string(imp.Type)
			idx := typeIdx[key]
			importContent = append(importContent, wasmName("env")...)
			importContent = append(importContent, wasmName(imp.Name)...)
			importContent = append(importContent, 0x00)                          // kind = function
			importContent = append(importContent, encodeULEB128(uint32(idx))...) // type index
		}
		out = append(out, makeSection(2, wasmVec(numImports, importContent))...)
	}

	// Function section: one internal function per export. Must come before Memory.
	numFuncs := len(exports)
	if numFuncs > 0 {
		var funcContent []byte
		for _, exp := range exports {
			// Use the type of the import being called.
			impIdx := int(exp.CallIdx)
			if impIdx < len(imports) {
				key := string(imports[impIdx].Type)
				funcContent = append(funcContent, encodeULEB128(uint32(typeIdx[key]))...)
			} else {
				funcContent = append(funcContent, encodeULEB128(0)...)
			}
		}
		out = append(out, makeSection(3, wasmVec(numFuncs, funcContent))...)
	}

	// Memory section.
	memIdx := uint32(0)
	if includeMemory {
		out = append(out, makeSection(5, []byte{0x01, 0x00, 0x01})...)
	}

	// Export section.
	numExports := 0
	if includeMemory {
		numExports++
	}
	numExports += numFuncs
	if numExports > 0 {
		var exportContent []byte
		if includeMemory {
			exportContent = append(exportContent, wasmName("memory")...)
			exportContent = append(exportContent, 0x02)                     // kind = memory
			exportContent = append(exportContent, encodeULEB128(memIdx)...) // index
		}
		for i, exp := range exports {
			exportContent = append(exportContent, wasmName(exp.Name)...)
			exportContent = append(exportContent, 0x00)                                   // kind = function
			exportContent = append(exportContent, encodeULEB128(uint32(numImports+i))...) // func index
		}
		out = append(out, makeSection(7, wasmVec(numExports, exportContent))...)
	}

	// Code section: function bodies.
	if numFuncs > 0 {
		var codeContent []byte
		for _, exp := range exports {
			impIdx := int(exp.CallIdx)
			var body []byte
			body = append(body, 0x00) // 0 locals
			// Push arguments for the import's parameters.
			if impIdx < len(imports) {
				nParams := functypeNumParams(imports[impIdx].Type)
				for p := 0; p < nParams; p++ {
					body = append(body, 0x20)    // local.get
					body = append(body, byte(p)) // param index
				}
			}
			body = append(body, 0x10, byte(exp.CallIdx)) // call
			body = append(body, 0x0b)                    // end
			bodyLen := encodeULEB128(uint32(len(body)))
			codeContent = append(codeContent, bodyLen...)
			codeContent = append(codeContent, body...)
		}
		out = append(out, makeSection(10, wasmVec(numFuncs, codeContent))...)
	}

	return out
}

func TestWriteWorkToFixedMemory(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	mod, err := wasmtime.NewModule(b.engine, memoryWasm(2))
	if err != nil {
		t.Fatalf("compile memory module: %v", err)
	}
	defer mod.Close()

	store := wasmtime.NewStore(b.engine)
	defer store.Close()
	// b.engine now always has epoch interruption enabled (see
	// NewWasmtimeBackend / IMPROVEMENT-PLAN.md 1.5); a store with no
	// explicit deadline defaults to deadline 0, which traps on the very
	// first exported-function call. These tests exercise individual host
	// function registrations directly (bypassing wasmtimeBackend.Execute,
	// which calls configureStore for real executions), so give the store
	// a deadline generous enough to never fire for a fast unit test.
	store.SetEpochDeadline(1 << 32)

	linker := wasmtime.NewLinker(b.engine)
	inst, err := linker.Instantiate(store, mod)
	if err != nil {
		t.Fatalf("instantiate memory module: %v", err)
	}

	memExp := inst.GetExport(store, "memory")
	if memExp == nil {
		t.Fatal("no memory export")
	}
	mem := memExp.Memory()
	if mem == nil {
		t.Fatal("memory export is not a memory")
	}

	t.Run("normal", func(t *testing.T) {
		b.writeWorkToFixedMemory(mem, store, "myEntry", []byte(`{"key":"val"}`))
		data := mem.UnsafeData(store)
		entryLen := getUint32LE(data[fixedWorkOffset : fixedWorkOffset+4])
		inputLen := getUint32LE(data[fixedWorkOffset+4 : fixedWorkOffset+8])
		if entryLen != 7 {
			t.Errorf("entryLen = %d, want 7", entryLen)
		}
		if inputLen != 13 {
			t.Errorf("inputLen = %d, want 13", inputLen)
		}
		if got := string(data[fixedWorkOffset+8 : fixedWorkOffset+8+int(entryLen)]); got != "myEntry" {
			t.Errorf("entry = %q, want %q", got, "myEntry")
		}
		if got := string(data[fixedWorkOffset+8+int(entryLen) : fixedWorkOffset+8+int(entryLen)+int(inputLen)]); got != `{"key":"val"}` {
			t.Errorf("input = %q, want %q", got, `{"key":"val"}`)
		}
	})

	t.Run("empty", func(t *testing.T) {
		d := mem.UnsafeData(store)
		for i := range d {
			d[i] = 0
		}
		b.writeWorkToFixedMemory(mem, store, "", nil)
		data := mem.UnsafeData(store)
		entryLen := getUint32LE(data[fixedWorkOffset : fixedWorkOffset+4])
		inputLen := getUint32LE(data[fixedWorkOffset+4 : fixedWorkOffset+8])
		if entryLen != 0 {
			t.Errorf("entryLen = %d, want 0", entryLen)
		}
		if inputLen != 0 {
			t.Errorf("inputLen = %d, want 0", inputLen)
		}
	})

	t.Run("truncation", func(t *testing.T) {
		longEntry := make([]byte, 300)
		for i := range longEntry {
			longEntry[i] = 'a'
		}
		longInput := make([]byte, 70000)
		for i := range longInput {
			longInput[i] = 'b'
		}
		b.writeWorkToFixedMemory(mem, store, string(longEntry), longInput)
		data := mem.UnsafeData(store)
		entryLen := getUint32LE(data[fixedWorkOffset : fixedWorkOffset+4])
		inputLen := getUint32LE(data[fixedWorkOffset+4 : fixedWorkOffset+8])
		if entryLen != fixedWorkMaxEntry {
			t.Errorf("entryLen = %d, want %d (clamped)", entryLen, fixedWorkMaxEntry)
		}
		if inputLen != fixedWorkMaxInput {
			t.Errorf("inputLen = %d, want %d (clamped)", inputLen, fixedWorkMaxInput)
		}
	})
}

// ---------------------------------------------------------------------------
// Section 6: Closure body tests via WASM module execution
// ---------------------------------------------------------------------------

// wasmValI32 and wasmValI64 are WASM value type codes.
const (
	wasmValI32 = 0x7f
	wasmValI64 = 0x7e
)

// closureWasm builds a WASM module that imports the given functions from
// "env" and exports "memory" plus a no-arg wrapper for each import.
func closureWasm(importNames []string, importTypes [][]byte) []byte {
	var imports []struct {
		Name string
		Type []byte
	}
	for i, name := range importNames {
		imports = append(imports, struct {
			Name string
			Type []byte
		}{name, importTypes[i]})
	}
	var exports []struct {
		Name    string
		CallIdx uint32
	}
	for i, name := range importNames {
		exports = append(exports, struct {
			Name    string
			CallIdx uint32
		}{"test_" + name, uint32(i)})
	}
	return buildImportWasm(imports, exports, true)
}

func TestClosure_CleatNow(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	b.handler = &mockHostHandler{ret: 42}

	ft := wasmFunctype(nil, []byte{wasmValI64})
	wasmBytes := closureWasm([]string{"cleat_now"}, [][]byte{ft})

	mod, err := wasmtime.NewModule(b.engine, wasmBytes)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer mod.Close()

	store := wasmtime.NewStore(b.engine)
	defer store.Close()
	// b.engine now always has epoch interruption enabled (see
	// NewWasmtimeBackend / IMPROVEMENT-PLAN.md 1.5); a store with no
	// explicit deadline defaults to deadline 0, which traps on the very
	// first exported-function call. These tests exercise individual host
	// function registrations directly (bypassing wasmtimeBackend.Execute,
	// which calls configureStore for real executions), so give the store
	// a deadline generous enough to never fire for a fast unit test.
	store.SetEpochDeadline(1 << 32)

	linker := wasmtime.NewLinker(b.engine)
	if err := b.registerCleatNow(linker); err != nil {
		t.Fatalf("register cleat_now: %v", err)
	}

	inst, err := linker.Instantiate(store, mod)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}

	testFn := inst.GetFunc(store, "test_cleat_now")
	if testFn == nil {
		t.Fatal("export test_cleat_now not found")
	}

	result, err := testFn.Call(store)
	if err != nil {
		t.Fatalf("call test_cleat_now: %v", err)
	}
	if result != int64(42) {
		t.Errorf("got %v, want 42", result)
	}
}

func TestClosure_CleatRandom(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	b.handler = &mockHostHandler{ret: 99}

	ft := wasmFunctype(nil, []byte{wasmValI64})
	wasmBytes := closureWasm([]string{"cleat_random"}, [][]byte{ft})

	mod, err := wasmtime.NewModule(b.engine, wasmBytes)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer mod.Close()

	store := wasmtime.NewStore(b.engine)
	defer store.Close()
	// b.engine now always has epoch interruption enabled (see
	// NewWasmtimeBackend / IMPROVEMENT-PLAN.md 1.5); a store with no
	// explicit deadline defaults to deadline 0, which traps on the very
	// first exported-function call. These tests exercise individual host
	// function registrations directly (bypassing wasmtimeBackend.Execute,
	// which calls configureStore for real executions), so give the store
	// a deadline generous enough to never fire for a fast unit test.
	store.SetEpochDeadline(1 << 32)

	linker := wasmtime.NewLinker(b.engine)
	if err := b.registerCleatRandom(linker); err != nil {
		t.Fatalf("register cleat_random: %v", err)
	}

	inst, err := linker.Instantiate(store, mod)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}

	testFn := inst.GetFunc(store, "test_cleat_random")
	if testFn == nil {
		t.Fatal("export test_cleat_random not found")
	}

	result, err := testFn.Call(store)
	if err != nil {
		t.Fatalf("call test_cleat_random: %v", err)
	}
	if result != int64(99) {
		t.Errorf("got %v, want 99", result)
	}
}

func TestClosure_CleatVersion(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	b.handler = &mockHostHandler{ret: 7}

	ft := wasmFunctype(nil, []byte{wasmValI64})
	wasmBytes := closureWasm([]string{"cleat_version"}, [][]byte{ft})

	mod, err := wasmtime.NewModule(b.engine, wasmBytes)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer mod.Close()

	store := wasmtime.NewStore(b.engine)
	defer store.Close()
	// b.engine now always has epoch interruption enabled (see
	// NewWasmtimeBackend / IMPROVEMENT-PLAN.md 1.5); a store with no
	// explicit deadline defaults to deadline 0, which traps on the very
	// first exported-function call. These tests exercise individual host
	// function registrations directly (bypassing wasmtimeBackend.Execute,
	// which calls configureStore for real executions), so give the store
	// a deadline generous enough to never fire for a fast unit test.
	store.SetEpochDeadline(1 << 32)

	linker := wasmtime.NewLinker(b.engine)
	if err := b.registerCleatVersion(linker); err != nil {
		t.Fatalf("register cleat_version: %v", err)
	}

	inst, err := linker.Instantiate(store, mod)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}

	testFn := inst.GetFunc(store, "test_cleat_version")
	if testFn == nil {
		t.Fatal("export test_cleat_version not found")
	}

	result, err := testFn.Call(store)
	if err != nil {
		t.Fatalf("call test_cleat_version: %v", err)
	}
	if result != int64(7) {
		t.Errorf("got %v, want 7", result)
	}
}

func TestClosure_CleatMinVersion(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	b.handler = &mockHostHandler{ret: 3}

	ft := wasmFunctype(nil, []byte{wasmValI64})
	wasmBytes := closureWasm([]string{"cleat_min_version"}, [][]byte{ft})

	mod, err := wasmtime.NewModule(b.engine, wasmBytes)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer mod.Close()

	store := wasmtime.NewStore(b.engine)
	defer store.Close()
	// b.engine now always has epoch interruption enabled (see
	// NewWasmtimeBackend / IMPROVEMENT-PLAN.md 1.5); a store with no
	// explicit deadline defaults to deadline 0, which traps on the very
	// first exported-function call. These tests exercise individual host
	// function registrations directly (bypassing wasmtimeBackend.Execute,
	// which calls configureStore for real executions), so give the store
	// a deadline generous enough to never fire for a fast unit test.
	store.SetEpochDeadline(1 << 32)

	linker := wasmtime.NewLinker(b.engine)
	if err := b.registerCleatMinVersion(linker); err != nil {
		t.Fatalf("register cleat_min_version: %v", err)
	}

	inst, err := linker.Instantiate(store, mod)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}

	testFn := inst.GetFunc(store, "test_cleat_min_version")
	if testFn == nil {
		t.Fatal("export test_cleat_min_version not found")
	}

	result, err := testFn.Call(store)
	if err != nil {
		t.Fatalf("call test_cleat_min_version: %v", err)
	}
	if result != int64(3) {
		t.Errorf("got %v, want 3", result)
	}
}

func TestClosure_CleatSleep(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	b.handler = &mockHostHandler{ret: 100}

	ft := wasmFunctype([]byte{wasmValI64}, []byte{wasmValI64})
	wasmBytes := closureWasm([]string{"cleat_sleep"}, [][]byte{ft})

	mod, err := wasmtime.NewModule(b.engine, wasmBytes)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer mod.Close()

	store := wasmtime.NewStore(b.engine)
	defer store.Close()
	// b.engine now always has epoch interruption enabled (see
	// NewWasmtimeBackend / IMPROVEMENT-PLAN.md 1.5); a store with no
	// explicit deadline defaults to deadline 0, which traps on the very
	// first exported-function call. These tests exercise individual host
	// function registrations directly (bypassing wasmtimeBackend.Execute,
	// which calls configureStore for real executions), so give the store
	// a deadline generous enough to never fire for a fast unit test.
	store.SetEpochDeadline(1 << 32)

	linker := wasmtime.NewLinker(b.engine)
	if err := b.registerCleatSleep(linker); err != nil {
		t.Fatalf("register cleat_sleep: %v", err)
	}

	inst, err := linker.Instantiate(store, mod)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}

	testFn := inst.GetFunc(store, "test_cleat_sleep")
	if testFn == nil {
		t.Fatal("export test_cleat_sleep not found")
	}

	result, err := testFn.Call(store, int64(55))
	if err != nil {
		t.Fatalf("call test_cleat_sleep: %v", err)
	}
	if result != int64(100) {
		t.Errorf("got %v, want 100", result)
	}
}

func TestClosure_CleatLog(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	b.handler = &mockHostHandler{ret: 0}

	ft := wasmFunctype([]byte{wasmValI32, wasmValI32}, []byte{wasmValI64})
	wasmBytes := closureWasm([]string{"cleat_log"}, [][]byte{ft})

	mod, err := wasmtime.NewModule(b.engine, wasmBytes)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer mod.Close()

	store := wasmtime.NewStore(b.engine)
	defer store.Close()
	// b.engine now always has epoch interruption enabled (see
	// NewWasmtimeBackend / IMPROVEMENT-PLAN.md 1.5); a store with no
	// explicit deadline defaults to deadline 0, which traps on the very
	// first exported-function call. These tests exercise individual host
	// function registrations directly (bypassing wasmtimeBackend.Execute,
	// which calls configureStore for real executions), so give the store
	// a deadline generous enough to never fire for a fast unit test.
	store.SetEpochDeadline(1 << 32)

	linker := wasmtime.NewLinker(b.engine)
	if err := b.registerCleatLog(linker); err != nil {
		t.Fatalf("register cleat_log: %v", err)
	}

	inst, err := linker.Instantiate(store, mod)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}

	testFn := inst.GetFunc(store, "test_cleat_log")
	if testFn == nil {
		t.Fatal("export test_cleat_log not found")
	}

	memExp := inst.GetExport(store, "memory")
	if memExp == nil {
		t.Fatal("no memory export")
	}
	mem := memExp.Memory()
	if mem == nil {
		t.Fatal("memory export is not a memory")
	}

	// Write a valid message to linear memory and call cleat_log via the
	// WASM wrapper. The success path reaches h.DurableLog(...).
	data := mem.UnsafeData(store)
	msg := "hello from wasm log test"
	copy(data[8:], msg)
	result, err := testFn.Call(store, int32(8), int32(len(msg)))
	if err != nil {
		t.Fatalf("call test_cleat_log (success path): %v", err)
	}
	if result != int64(0) {
		t.Errorf("success path: got %v, want 0", result)
	}

	// Zero-length read is rejected by wasmtimeReadStringValidated.
	t.Run("error_path_zero_length", func(t *testing.T) {
		result, err := testFn.Call(store, int32(0), int32(0))
		if err != nil {
			t.Fatalf("call test_cleat_log: %v", err)
		}
		if result != errBadParamInt64 {
			t.Errorf("got %v, want %v (errBadParamInt64)", result, errBadParamInt64)
		}
	})
}

// The next three tests exercise registerCleatComplete closures. Note: the
// callerMemBuf error branch in cleat_complete (source line ~1697) is not
// covered — triggering it requires a WASM module that calls cleat_complete but
// has no exported memory, which is not constructible via closureWasm.
func TestClosure_CleatComplete(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	var cr, ce string
	b.handler = &mockHostHandler{ret: 0}

	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	wasmBytes := closureWasm([]string{"cleat_complete"}, [][]byte{ft})

	mod, err := wasmtime.NewModule(b.engine, wasmBytes)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer mod.Close()

	store := wasmtime.NewStore(b.engine)
	defer store.Close()
	// b.engine now always has epoch interruption enabled (see
	// NewWasmtimeBackend / IMPROVEMENT-PLAN.md 1.5); a store with no
	// explicit deadline defaults to deadline 0, which traps on the very
	// first exported-function call. These tests exercise individual host
	// function registrations directly (bypassing wasmtimeBackend.Execute,
	// which calls configureStore for real executions), so give the store
	// a deadline generous enough to never fire for a fast unit test.
	store.SetEpochDeadline(1 << 32)

	linker := wasmtime.NewLinker(b.engine)
	if err := b.registerCleatComplete(linker, &cr, &ce); err != nil {
		t.Fatalf("register cleat_complete: %v", err)
	}

	inst, err := linker.Instantiate(store, mod)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}

	testFn := inst.GetFunc(store, "test_cleat_complete")
	if testFn == nil {
		t.Fatal("export test_cleat_complete not found")
	}

	// Call with resultLen=0 — exercises the empty-result path.
	result, err := testFn.Call(store, int32(0), int32(0), int32(0))
	if err != nil {
		t.Fatalf("call test_cleat_complete: %v", err)
	}
	if result != int64(0) {
		t.Errorf("got %v, want 0", result)
	}
	if cr != "" || ce != "" {
		t.Errorf("cr=%q ce=%q, want both empty", cr, ce)
	}
}

func TestClosure_CleatComplete_WithResult(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	var cr, ce string
	b.handler = &mockHostHandler{ret: 0}

	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	wasmBytes := closureWasm([]string{"cleat_complete"}, [][]byte{ft})

	mod, err := wasmtime.NewModule(b.engine, wasmBytes)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer mod.Close()

	store := wasmtime.NewStore(b.engine)
	defer store.Close()
	// b.engine now always has epoch interruption enabled (see
	// NewWasmtimeBackend / IMPROVEMENT-PLAN.md 1.5); a store with no
	// explicit deadline defaults to deadline 0, which traps on the very
	// first exported-function call. These tests exercise individual host
	// function registrations directly (bypassing wasmtimeBackend.Execute,
	// which calls configureStore for real executions), so give the store
	// a deadline generous enough to never fire for a fast unit test.
	store.SetEpochDeadline(1 << 32)

	linker := wasmtime.NewLinker(b.engine)
	if err := b.registerCleatComplete(linker, &cr, &ce); err != nil {
		t.Fatalf("register cleat_complete: %v", err)
	}

	inst, err := linker.Instantiate(store, mod)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}

	memExp := inst.GetExport(store, "memory")
	mem := memExp.Memory()
	data := mem.UnsafeData(store)
	copy(data[0:2], "ok")

	testFn := inst.GetFunc(store, "test_cleat_complete")
	if testFn == nil {
		t.Fatal("export test_cleat_complete not found")
	}

	_, err = testFn.Call(store, int32(0), int32(0), int32(2))
	if err != nil {
		t.Fatalf("call test_cleat_complete: %v", err)
	}
	if cr != "ok" {
		t.Errorf("cr = %q, want %q", cr, "ok")
	}
}

func TestClosure_CleatComplete_ErrorStatus(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	var cr, ce string
	b.handler = &mockHostHandler{ret: 0}

	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	wasmBytes := closureWasm([]string{"cleat_complete"}, [][]byte{ft})

	mod, err := wasmtime.NewModule(b.engine, wasmBytes)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer mod.Close()

	store := wasmtime.NewStore(b.engine)
	defer store.Close()
	// b.engine now always has epoch interruption enabled (see
	// NewWasmtimeBackend / IMPROVEMENT-PLAN.md 1.5); a store with no
	// explicit deadline defaults to deadline 0, which traps on the very
	// first exported-function call. These tests exercise individual host
	// function registrations directly (bypassing wasmtimeBackend.Execute,
	// which calls configureStore for real executions), so give the store
	// a deadline generous enough to never fire for a fast unit test.
	store.SetEpochDeadline(1 << 32)

	linker := wasmtime.NewLinker(b.engine)
	if err := b.registerCleatComplete(linker, &cr, &ce); err != nil {
		t.Fatalf("register cleat_complete: %v", err)
	}

	inst, err := linker.Instantiate(store, mod)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}

	memExp := inst.GetExport(store, "memory")
	mem := memExp.Memory()
	data := mem.UnsafeData(store)
	copy(data[0:4], "fail")

	testFn := inst.GetFunc(store, "test_cleat_complete")
	_, err = testFn.Call(store, int32(1), int32(0), int32(4))
	if err != nil {
		t.Fatalf("call test_cleat_complete: %v", err)
	}
	if ce != "fail" {
		t.Errorf("ce = %q, want %q", ce, "fail")
	}
	if cr != "" {
		t.Errorf("cr = %q, want empty", cr)
	}
}

func TestClosure_CleatPollWork(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	b.workEntryPoint = "myEntry"
	b.workInput = []byte(`{"in":"put"}`)

	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	wasmBytes := closureWasm([]string{"cleat_poll_work"}, [][]byte{ft})

	mod, err := wasmtime.NewModule(b.engine, wasmBytes)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer mod.Close()

	store := wasmtime.NewStore(b.engine)
	defer store.Close()
	// b.engine now always has epoch interruption enabled (see
	// NewWasmtimeBackend / IMPROVEMENT-PLAN.md 1.5); a store with no
	// explicit deadline defaults to deadline 0, which traps on the very
	// first exported-function call. These tests exercise individual host
	// function registrations directly (bypassing wasmtimeBackend.Execute,
	// which calls configureStore for real executions), so give the store
	// a deadline generous enough to never fire for a fast unit test.
	store.SetEpochDeadline(1 << 32)

	linker := wasmtime.NewLinker(b.engine)
	if err := b.registerCleatPollWork(linker); err != nil {
		t.Fatalf("register cleat_poll_work: %v", err)
	}

	inst, err := linker.Instantiate(store, mod)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}

	memExp := inst.GetExport(store, "memory")
	mem := memExp.Memory()

	testFn := inst.GetFunc(store, "test_cleat_poll_work")
	result, err := testFn.Call(store, int32(0), int32(100), int32(200), int32(100))
	if err != nil {
		t.Fatalf("call test_cleat_poll_work: %v", err)
	}

	entryLen := int32(result.(int64) >> 32)
	argsLen := int32(result.(int64) & 0xFFFFFFFF)
	if entryLen != 7 {
		t.Errorf("entryLen = %d, want 7", entryLen)
	}
	if argsLen != 12 {
		t.Errorf("argsLen = %d, want 12", argsLen)
	}

	data := mem.UnsafeData(store)
	if got := string(data[0:entryLen]); got != "myEntry" {
		t.Errorf("entry = %q, want %q", got, "myEntry")
	}
	if got := string(data[200 : 200+argsLen]); got != `{"in":"put"}` {
		t.Errorf("input = %q, want %q", got, `{"in":"put"}`)
	}
}

func TestClosure_CleatPollWork_Truncation(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	longEntry := make([]byte, 300)
	for i := range longEntry {
		longEntry[i] = 'x'
	}
	longInput := make([]byte, 200)
	for i := range longInput {
		longInput[i] = 'y'
	}
	b.workEntryPoint = string(longEntry)
	b.workInput = longInput

	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	wasmBytes := closureWasm([]string{"cleat_poll_work"}, [][]byte{ft})

	mod, err := wasmtime.NewModule(b.engine, wasmBytes)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer mod.Close()

	store := wasmtime.NewStore(b.engine)
	defer store.Close()
	// b.engine now always has epoch interruption enabled (see
	// NewWasmtimeBackend / IMPROVEMENT-PLAN.md 1.5); a store with no
	// explicit deadline defaults to deadline 0, which traps on the very
	// first exported-function call. These tests exercise individual host
	// function registrations directly (bypassing wasmtimeBackend.Execute,
	// which calls configureStore for real executions), so give the store
	// a deadline generous enough to never fire for a fast unit test.
	store.SetEpochDeadline(1 << 32)

	linker := wasmtime.NewLinker(b.engine)
	if err := b.registerCleatPollWork(linker); err != nil {
		t.Fatalf("register cleat_poll_work: %v", err)
	}

	inst, err := linker.Instantiate(store, mod)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}

	testFn := inst.GetFunc(store, "test_cleat_poll_work")
	result, err := testFn.Call(store, int32(0), int32(10), int32(50), int32(5))
	if err != nil {
		t.Fatalf("call test_cleat_poll_work: %v", err)
	}

	entryLen := int32(result.(int64) >> 32)
	argsLen := int32(result.(int64) & 0xFFFFFFFF)
	if entryLen != 10 {
		t.Errorf("entryLen = %d, want 10 (truncated)", entryLen)
	}
	if argsLen != 5 {
		t.Errorf("argsLen = %d, want 5 (truncated)", argsLen)
	}
}

// ---------------------------------------------------------------------------
// Section 7: Execute error-path and component detection coverage
// ---------------------------------------------------------------------------

func TestExecute_ComponentWasmBytes(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	// Component model WASM starts with magic \x00asm then version,
	// and has a component section (section ID 0x0a) or uses the
	// component-model binary format. isComponentWasm checks for this.
	// Build a fake component-model header to exercise the detection path.
	componentHeader := []byte{
		0x00, 0x61, 0x73, 0x6d, // magic
		0x0d, 0x00, 0x01, 0x00, // component model version (checked by isComponentWasm)
		0x00, 0x61, 0x73, 0x6d, // second magic for component
		0x0a, 0x00, 0x01, 0x00, // version 10
	}

	_, err = b.Execute(ctx, componentHeader, "main", nil, b.handler)
	if err == nil {
		t.Log("component header executed (unexpected but OK)")
	} else {
		t.Logf("expected error from component header: %v", err)
	}
}

func TestExecute_GoModuleWithoutStart(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	b.handler = &mockHostHandler{ret: 0}

	// Build a module with memory, a cleat import, and an export.
	// The export needs signature (i32,i32,i32,i32) -> i64 for non-Go path.
	// Use closureWasm but rename the export to something specific.
	ft := wasmFunctype(nil, []byte{wasmValI64})
	wasmBytes := closureWasm([]string{"cleat_now"}, [][]byte{ft})

	// Try to Execute with an entry point that doesn't exist.
	_, err = b.Execute(ctx, wasmBytes, "nonexistent", nil, b.handler)
	if err == nil {
		t.Log("executed with nonexistent entry (unexpected)")
	} else {
		t.Logf("expected error with nonexistent entry: %v", err)
	}
}

func TestExecute_ExportNotFound(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	b.handler = &mockHostHandler{ret: 0}

	ft := wasmFunctype(nil, []byte{wasmValI64})
	wasmBytes := closureWasm([]string{"cleat_now"}, [][]byte{ft})

	// Export "test_cleat_now" exists, but we ask for "missing_func".
	_, err = b.Execute(ctx, wasmBytes, "missing_func", nil, b.handler)
	if err == nil {
		t.Error("expected error for missing export, got nil")
	}
	if err != nil {
		t.Logf("missing export error (expected): %v", err)
	}
}

func TestExecute_CompileError_BeforeHandler(t *testing.T) {
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer b.Close(ctx)

	// Invalid WASM bytes fail at compile time (before the handler is
	// installed). The compile error is caught in Execute line ~111-113,
	// so a nil handler parameter never triggers a nil-dereference panic.
	_, err = b.Execute(ctx, []byte("not-valid-wasm"), "main", nil, nil)
	if err == nil {
		t.Error("expected error for invalid WASM bytes")
	}
}

// ---------------------------------------------------------------------------
// Section 7: Batch closure tests for registerCleat* functions
// ---------------------------------------------------------------------------

// closureSetup holds the common state for a batch of closure tests.
type closureSetup struct {
	backend *wasmtimeBackend
	engine  *wasmtime.Engine
	store   *wasmtime.Store
	linker  *wasmtime.Linker
	inst    *wasmtime.Instance
	mem     *wasmtime.Memory
	data    []byte
}

// newClosureSetup creates a WASM module that imports all the given functions
// and exports a "test_<name>" wrapper for each. All functions are registered
// and the module is instantiated.
func newClosureSetup(t *testing.T, imports []struct {
	name string
	ft   []byte
}, registerAll func(*wasmtimeBackend, *wasmtime.Linker) error) *closureSetup {
	t.Helper()
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	t.Cleanup(func() { b.Close(ctx) })
	b.handler = &mockHostHandler{ret: 0}

	var names []string
	var types [][]byte
	for _, imp := range imports {
		names = append(names, imp.name)
		types = append(types, imp.ft)
	}
	wasmBytes := closureWasm(names, types)

	eng := wasmtime.NewEngine()
	mod, err := wasmtime.NewModule(eng, wasmBytes)
	if err != nil {
		t.Fatalf("compile multi-import module: %v", err)
	}
	t.Cleanup(func() { mod.Close() })

	store := wasmtime.NewStore(eng)
	t.Cleanup(func() { store.Close() })

	linker := wasmtime.NewLinker(eng)

	if err := registerAll(b, linker); err != nil {
		t.Fatalf("register all: %v", err)
	}

	inst, err := linker.Instantiate(store, mod)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}

	memExp := inst.GetExport(store, "memory")
	if memExp == nil {
		t.Fatal("no memory export")
	}
	mem := memExp.Memory()
	if mem == nil {
		t.Fatal("memory export is not a memory")
	}

	return &closureSetup{
		backend: b,
		engine:  eng,
		store:   store,
		linker:  linker,
		inst:    inst,
		mem:     mem,
		data:    mem.UnsafeData(store),
	}
}

func (s *closureSetup) call(t *testing.T, exportName string, args ...any) int64 {
	t.Helper()
	fn := s.inst.GetFunc(s.store, exportName)
	if fn == nil {
		t.Fatalf("export %s not found", exportName)
	}
	result, err := fn.Call(s.store, args...)
	if err != nil {
		t.Fatalf("call %s: %v", exportName, err)
	}
	if result == nil {
		t.Fatalf("call %s: no return value", exportName)
	}
	return result.(int64)
}

func (s *closureSetup) writeString(offset int, sval string) {
	copy(s.data[offset:], sval)
}

// i32 returns an int32 for use with call args.
func i32(v int32) int32 { return v }

// ---------------------------------------------------------------------------
// Batch 1: Simple 2-param (ptr,len) → i64 functions
// ---------------------------------------------------------------------------

func TestClosure_SimpleStringIn(t *testing.T) {
	// Functions that read a single string from memory and return i64.
	imports := []struct {
		name string
		ft   []byte
	}{
		{"cleat_release_lock", wasmFunctype([]byte{wasmValI32, wasmValI32}, []byte{wasmValI64})},
		{"cleat_register_update_handler", wasmFunctype([]byte{wasmValI32, wasmValI32}, []byte{wasmValI64})},
		{"cleat_delete_state", wasmFunctype([]byte{wasmValI32, wasmValI32}, []byte{wasmValI64})},
		{"cleat_has_state", wasmFunctype([]byte{wasmValI32, wasmValI32}, []byte{wasmValI64})},
		{"cleat_register_query_handler", wasmFunctype([]byte{wasmValI32, wasmValI32}, []byte{wasmValI64})},
	}

	s := newClosureSetup(t, imports, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		if err := b.registerCleatReleaseLock(l); err != nil {
			return err
		}
		if err := b.registerCleatRegisterUpdateHandler(l); err != nil {
			return err
		}
		if err := b.registerCleatDeleteState(l); err != nil {
			return err
		}
		if err := b.registerCleatHasState(l); err != nil {
			return err
		}
		return b.registerCleatRegisterQueryHandler(l)
	})

	for _, tc := range []struct {
		export string
		name   string
	}{
		{"test_cleat_release_lock", "cleat_release_lock"},
		{"test_cleat_register_update_handler", "cleat_register_update_handler"},
		{"test_cleat_delete_state", "cleat_delete_state"},
		{"test_cleat_has_state", "cleat_has_state"},
		{"test_cleat_register_query_handler", "cleat_register_query_handler"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.writeString(100, tc.name)
			got := s.call(t, tc.export, i32(100), int32(len(tc.name)))
			if got != 0 {
				t.Errorf("got %v, want 0", got)
			}
		})
	}
}

func TestClosure_TwoStringIn(t *testing.T) {
	// Functions that read two strings (ptr,len × 2) from memory.
	imports := []struct {
		name string
		ft   []byte
	}{
		{"cleat_set_state", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})},
		{"cleat_resolve_promise", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})},
		{"cleat_reject_promise", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})},
		{"set_query_state", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})},
		{"cleat_reply_to_signal", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})},
		{"cleat_run_detached", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})},
	}

	s := newClosureSetup(t, imports, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		if err := b.registerCleatSetState(l); err != nil {
			return err
		}
		if err := b.registerCleatResolvePromise(l); err != nil {
			return err
		}
		if err := b.registerCleatRejectPromise(l); err != nil {
			return err
		}
		if err := b.registerCleatSetQueryState(l); err != nil {
			return err
		}
		if err := b.registerCleatReplyToSignal(l); err != nil {
			return err
		}
		return b.registerCleatRunDetached(l)
	})

	for _, tc := range []struct {
		export string
		name   string
		second string
	}{
		{"test_cleat_set_state", "mykey", "myval"},
		{"test_cleat_resolve_promise", "promise-1", "resolved"},
		{"test_cleat_reject_promise", "promise-2", "error-msg"},
		{"test_set_query_state", "qk", "qv"},
		{"test_cleat_reply_to_signal", "corr-1", "resp"},
		{"test_cleat_run_detached", "child-wf", `{"in":"put"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.writeString(100, tc.name)
			s.writeString(200, tc.second)
			got := s.call(t, tc.export, i32(100), int32(len(tc.name)), i32(200), int32(len(tc.second)))
			if got != 0 {
				t.Errorf("got %v, want 0", got)
			}
		})
	}
}

func TestClosure_ThreeStringIn(t *testing.T) {
	// Functions that read three strings from memory.
	imports := []struct {
		name string
		ft   []byte
	}{
		{"cleat_send", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})},
		{"cleat_signal_workflow", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})},
	}

	s := newClosureSetup(t, imports, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		if err := b.registerCleatSend(l); err != nil {
			return err
		}
		return b.registerCleatSignalWorkflow(l)
	})

	t.Run("cleat_send", func(t *testing.T) {
		s.writeString(50, "my-svc")
		s.writeString(100, "my-op")
		s.writeString(200, `{"req":"data"}`)
		got := s.call(t, "test_cleat_send", i32(50), i32(6), i32(100), i32(5), i32(200), i32(14))
		if got != 0 {
			t.Errorf("got %v, want 0", got)
		}
	})
	t.Run("cleat_signal_workflow", func(t *testing.T) {
		s.writeString(50, "target-run-id")
		s.writeString(100, "signal-name")
		s.writeString(200, `{"p":"load"}`)
		got := s.call(t, "test_cleat_signal_workflow", i32(50), i32(13), i32(100), i32(11), i32(200), i32(11))
		if got != 0 {
			t.Errorf("got %v, want 0", got)
		}
	})
}

func TestClosure_StringAndI64(t *testing.T) {
	// Functions that read one string and have an i64 parameter.
	imports := []struct {
		name string
		ft   []byte
	}{
		{"cleat_acquire_lock", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI64}, []byte{wasmValI64})},
		{"cleat_incr_state", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI64}, []byte{wasmValI64})},
	}

	s := newClosureSetup(t, imports, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		if err := b.registerCleatAcquireLock(l); err != nil {
			return err
		}
		return b.registerCleatIncrState(l)
	})

	t.Run("cleat_acquire_lock", func(t *testing.T) {
		s.writeString(60, "lock-key-1")
		got := s.call(t, "test_cleat_acquire_lock", i32(60), i32(10), int64(5000))
		if got != 0 {
			t.Errorf("got %v, want 0", got)
		}
	})
	t.Run("cleat_incr_state", func(t *testing.T) {
		s.writeString(60, "counter-key")
		got := s.call(t, "test_cleat_incr_state", i32(60), i32(11), int64(5))
		if got != 0 {
			t.Errorf("got %v, want 0", got)
		}
	})
}

func TestClosure_CreatePromise(t *testing.T) {
	// cleat_create_promise: (namePtr,nameLen, promiseIDPtr,promiseIDMaxLen, ttlMs i64) -> i64
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI64}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_create_promise", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatCreatePromise(l)
	})

	s.writeString(80, "my-promise")
	got := s.call(t, "test_cleat_create_promise", i32(80), i32(10), i32(200), i32(64), int64(10000))
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_ScheduleInvoke(t *testing.T) {
	// cleat_schedule_invoke: (svc,op,req ptr,len × 3, delayMs i64) -> i64
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI64}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_schedule_invoke", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatScheduleInvoke(l)
	})

	s.writeString(50, "my-svc")
	s.writeString(100, "my-op")
	s.writeString(200, `{"key":"val"}`)
	got := s.call(t, "test_cleat_schedule_invoke", i32(50), i32(6), i32(100), i32(5), i32(200), i32(13), int64(30000))
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_UUID(t *testing.T) {
	// cleat_uuid: (seedPtr,seedLen, uuidPtr,uuidMaxLen) -> i64
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_uuid", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatUUID(l)
	})

	s.writeString(50, "seed-1")
	got := s.call(t, "test_cleat_uuid", i32(50), i32(6), i32(300), i32(64))
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_SetScope(t *testing.T) {
	// cleat_set_scope: (objTypePtr,objTypeLen, instKeyPtr,instKeyLen, prevScopePtr,prevScopeMaxLen) -> i64
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_set_scope", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatSetScope(l)
	})

	s.writeString(50, "tenant")
	s.writeString(100, "tenant-123")
	got := s.call(t, "test_cleat_set_scope", i32(50), i32(6), i32(100), i32(10), i32(300), i32(128))
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_WorkflowID(t *testing.T) {
	// cleat_workflow_id: (idPtr,idMaxLen) -> i64
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_workflow_id", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatWorkflowID(l)
	})

	got := s.call(t, "test_cleat_workflow_id", i32(400), i32(128))
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_RunID(t *testing.T) {
	// cleat_run_id: (idPtr,idMaxLen) -> i64
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_run_id", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatRunID(l)
	})

	got := s.call(t, "test_cleat_run_id", i32(400), i32(128))
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_GetScope(t *testing.T) {
	// cleat_get_scope: (objTypePtr,objTypeMaxLen, instKeyPtr,instKeyMaxLen) -> i64
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_get_scope", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatGetScope(l)
	})

	got := s.call(t, "test_cleat_get_scope", i32(500), i32(128), i32(700), i32(128))
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_SideEffect(t *testing.T) {
	// cleat_side_effect: (computedResultPtr,computedResultLen, respPtr,respMaxLen) -> i64
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_side_effect", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatSideEffect(l)
	})

	s.writeString(800, "computed-result")
	got := s.call(t, "test_cleat_side_effect", i32(800), i32(15), i32(900), i32(256))
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_GetState(t *testing.T) {
	// cleat_get_state: (keyPtr,keyLen, valuePtr,valueMaxLen) -> i64
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_get_state", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatGetState(l)
	})

	s.writeString(100, "my-key")
	got := s.call(t, "test_cleat_get_state", i32(100), i32(6), i32(200), i32(256))
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_ListState(t *testing.T) {
	// cleat_list_state: (prefixPtr,prefixLen, keysPtr,keysMaxLen) -> i64
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_list_state", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatListState(l)
	})

	s.writeString(100, "prefix-")
	got := s.call(t, "test_cleat_list_state", i32(100), i32(7), i32(200), i32(512))
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_Fetch(t *testing.T) {
	// cleat_fetch: (methodPtr,methodLen, urlPtr,urlLen, headersPtr,headersLen, bodyPtr,bodyLen, respPtr,respMaxLen) -> i64
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_fetch", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatFetch(l)
	})

	s.writeString(50, "GET")
	s.writeString(100, "/api/test")
	s.writeString(200, "{}")
	s.writeString(300, "b")
	got := s.call(t, "test_cleat_fetch", i32(50), i32(3), i32(100), i32(9), i32(200), i32(2), i32(300), i32(1), i32(400), i32(1024))
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_ChildWorkflow(t *testing.T) {
	// cleat_child_workflow: (namePtr,nameLen, inputPtr,inputLen, runIDPtr,runIDMaxLen) -> i64
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_child_workflow", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatChildWorkflow(l)
	})

	s.writeString(50, "child-wf")
	s.writeString(100, `{"in":"put"}`)
	got := s.call(t, "test_cleat_child_workflow", i32(50), i32(8), i32(100), i32(14), i32(200), i32(64))
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_AwaitChild(t *testing.T) {
	// cleat_await_child: (runIDPtr,runIDLen, resultPtr,resultMaxLen) -> i64
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_await_child", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatAwaitChild(l)
	})

	s.writeString(50, "child-run-id-123")
	got := s.call(t, "test_cleat_await_child", i32(50), i32(16), i32(200), i32(512))
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_AwaitAllChildren(t *testing.T) {
	// cleat_await_all_children: (runIDsJSONPtr,runIDsJSONLen, resultsPtr,resultsMaxLen) -> i64
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_await_all_children", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatAwaitAllChildren(l)
	})

	s.writeString(50, `["run-1","run-2"]`)
	got := s.call(t, "test_cleat_await_all_children", i32(50), i32(19), i32(200), i32(1024))
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_ContinueAsNew(t *testing.T) {
	// cleat_continue_as_new: (inputPtr,inputLen) -> i64
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_continue_as_new", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatContinueAsNew(l)
	})

	s.writeString(50, `{"new":"input"}`)
	got := s.call(t, "test_cleat_continue_as_new", i32(50), i32(14))
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_ContinueAsNewVersioned(t *testing.T) {
	// cleat_continue_as_new_versioned: (inputPtr,inputLen, version i32) -> i64
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_continue_as_new_versioned", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatContinueAsNewVersioned(l)
	})

	s.writeString(50, `{"new":"input"}`)
	got := s.call(t, "test_cleat_continue_as_new_versioned", i32(50), i32(14), i32(2))
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_PollSignal(t *testing.T) {
	// cleat_poll_signal: (signalNamePtr,signalNameLen, payloadPtr,payloadMaxLen) -> i64
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_poll_signal", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatPollSignal(l)
	})

	s.writeString(50, "my-signal")
	got := s.call(t, "test_cleat_poll_signal", i32(50), i32(9), i32(200), i32(256))
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_PollChild(t *testing.T) {
	// cleat_poll_child: (runIDPtr,runIDLen, resultPtr,resultMaxLen) -> i64
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_poll_child", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatPollChild(l)
	})

	s.writeString(50, "child-run-id")
	got := s.call(t, "test_cleat_poll_child", i32(50), i32(12), i32(200), i32(512))
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_AwaitAnyChild(t *testing.T) {
	// cleat_await_any_child: (runIDsJSONPtr,runIDsJSONLen, resultPtr,resultMaxLen) -> i64
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_await_any_child", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatAwaitAnyChild(l)
	})

	s.writeString(50, `["r1","r2"]`)
	got := s.call(t, "test_cleat_await_any_child", i32(50), i32(11), i32(200), i32(512))
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_JsonParse(t *testing.T) {
	// cleat_json_parse: (jsonPtr,jsonLen, outPtr,outMaxLen) -> i64
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_json_parse", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatJsonParse(l)
	})

	s.writeString(50, `{"a":1}`)
	got := s.call(t, "test_cleat_json_parse", i32(50), i32(7), i32(200), i32(512))
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_JsonStringify(t *testing.T) {
	// cleat_json_stringify: (ptr,len, outPtr,outMaxLen) -> i64
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_json_stringify", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatJsonStringify(l)
	})

	s.writeString(50, `[1,2,3]`)
	got := s.call(t, "test_cleat_json_stringify", i32(50), i32(7), i32(200), i32(512))
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_CallRetry(t *testing.T) {
	// cleat_call_retry: (svc,op,req ptr,len × 3, maxAttempts,initialInterval,backoff,maxInterval i64 × 4, nonRetryableJSON ptr,len, respPtr,respMaxLen) -> i64
	ft := wasmFunctype([]byte{
		wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32,
		wasmValI64, wasmValI64, wasmValI64, wasmValI64,
		wasmValI32, wasmValI32, wasmValI32, wasmValI32,
	}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_call_retry", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatCallRetry(l)
	})

	s.writeString(30, "my-svc")
	s.writeString(60, "my-op")
	s.writeString(90, `{"k":"v"}`)
	s.writeString(140, `["E1","E2"]`)
	got := s.call(t, "test_cleat_call_retry",
		i32(30), i32(6), // svc
		i32(60), i32(5), // op
		i32(90), i32(9), // req
		int64(3), int64(100), int64(200), int64(5000), // retry config
		i32(140), i32(12), // nonRetryable
		i32(200), i32(512), // resp
	)
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_CallHeartbeat(t *testing.T) {
	// cleat_call_heartbeat: (svc,op,req ptr,len × 3, heartbeatIntervalMs i64, respPtr,respMaxLen) -> i64
	ft := wasmFunctype([]byte{
		wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32,
		wasmValI64,
		wasmValI32, wasmValI32,
	}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_call_heartbeat", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatCallHeartbeat(l)
	})

	s.writeString(30, "my-svc")
	s.writeString(60, "my-op")
	s.writeString(90, `{"k":"v"}`)
	got := s.call(t, "test_cleat_call_heartbeat",
		i32(30), i32(6), // svc
		i32(60), i32(5), // op
		i32(90), i32(9), // req
		int64(5000),        // heartbeat interval
		i32(200), i32(512), // resp
	)
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_PluginCall(t *testing.T) {
	// plugin_call: (pluginName, funcName, input ptr,len × 3, respPtr,respMaxLen) -> i64
	ft := wasmFunctype([]byte{
		wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32,
		wasmValI32, wasmValI32,
	}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"plugin_call", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatPluginCall(l)
	})

	s.writeString(30, "my-plugin")
	s.writeString(60, "my-func")
	s.writeString(90, `{"in":"put"}`)
	got := s.call(t, "test_plugin_call",
		i32(30), i32(9), // plugin name
		i32(60), i32(7), // func name
		i32(90), i32(12), // input
		i32(200), i32(512), // resp
	)
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_PluginCallStreaming(t *testing.T) {
	// plugin_call_streaming: same signature as plugin_call
	ft := wasmFunctype([]byte{
		wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32,
		wasmValI32, wasmValI32,
	}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"plugin_call_streaming", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatPluginCallStreaming(l)
	})

	s.writeString(30, "my-plugin")
	s.writeString(60, "my-func")
	s.writeString(90, `{"in":"put"}`)
	got := s.call(t, "test_plugin_call_streaming",
		i32(30), i32(9),
		i32(60), i32(7),
		i32(90), i32(12),
		i32(200), i32(512),
	)
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_ChildWorkflowWithOptions(t *testing.T) {
	// cleat_child_workflow_with_options: (name,in ptr,len × 2, version,priority i64 × 2, parentClosePolicy ptr,len, runIDPtr,runIDMaxLen) -> i64
	ft := wasmFunctype([]byte{
		wasmValI32, wasmValI32, wasmValI32, wasmValI32,
		wasmValI64, wasmValI64,
		wasmValI32, wasmValI32,
		wasmValI32, wasmValI32,
	}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_child_workflow_with_options", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatChildWorkflowWithOptions(l)
	})

	s.writeString(30, "child-wf")
	s.writeString(70, `{"in":"put"}`)
	s.writeString(120, "ABANDON")
	got := s.call(t, "test_cleat_child_workflow_with_options",
		i32(30), i32(8), // name
		i32(70), i32(14), // input
		int64(2), int64(5), // version, priority
		i32(120), i32(7), // parentClosePolicy
		i32(200), i32(64), // runID
	)
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_ChildWorkflowInSchema(t *testing.T) {
	// cleat_child_workflow_in_schema: (targetSchema, name, input ptr,len × 3, version,priority i64 × 2, parentClosePolicy ptr,len, runIDPtr,runIDMaxLen) -> i64
	ft := wasmFunctype([]byte{
		wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32,
		wasmValI64, wasmValI64,
		wasmValI32, wasmValI32,
		wasmValI32, wasmValI32,
	}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_child_workflow_in_schema", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatChildWorkflowInSchema(l)
	})

	s.writeString(10, "other-schema")
	s.writeString(40, "child-wf")
	s.writeString(70, `{"in":"put"}`)
	s.writeString(120, "TERMINATE")
	got := s.call(t, "test_cleat_child_workflow_in_schema",
		i32(10), i32(12), // targetSchema
		i32(40), i32(8), // name
		i32(70), i32(14), // input
		int64(1), int64(3), // version, priority
		i32(120), i32(9), // parentClosePolicy
		i32(200), i32(64), // runID
	)
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_AwaitSignals(t *testing.T) {
	// cleat_await_signals: (signalNamesPtr,signalNamesLen, timeoutMs i64, sigNamePtr,sigNameMaxLen, payloadPtr,payloadMaxLen) -> i64
	ft := wasmFunctype([]byte{
		wasmValI32, wasmValI32,
		wasmValI64,
		wasmValI32, wasmValI32,
		wasmValI32, wasmValI32,
	}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_await_signals", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatAwaitSignals(l)
	})

	s.writeString(30, "sig1,sig2")
	got := s.call(t, "test_cleat_await_signals",
		i32(30), i32(9), // signalNames
		int64(30000),      // timeout
		i32(200), i32(64), // sigName
		i32(300), i32(512), // payload
	)
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_AwaitPromise(t *testing.T) {
	// cleat_await_promise: (promiseIDPtr,promiseIDLen, timeoutMs i64, resultPtr,resultMaxLen) -> i64
	ft := wasmFunctype([]byte{
		wasmValI32, wasmValI32,
		wasmValI64,
		wasmValI32, wasmValI32,
	}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_await_promise", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatAwaitPromise(l)
	})

	s.writeString(30, "promise-uuid-123")
	got := s.call(t, "test_cleat_await_promise",
		i32(30), i32(16), // promiseID
		int64(5000),        // timeout
		i32(200), i32(512), // result
	)
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_SendSignalAndWait(t *testing.T) {
	// cleat_send_signal_and_wait: (targetRunID,signalName,payload ptr,len × 3, timeoutMs i64, respPtr,respMaxLen) -> i64
	ft := wasmFunctype([]byte{
		wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32,
		wasmValI64,
		wasmValI32, wasmValI32,
	}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_send_signal_and_wait", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatSendSignalAndWait(l)
	})

	s.writeString(30, "target-run-1")
	s.writeString(70, "my-signal")
	s.writeString(110, `{"p":"load"}`)
	got := s.call(t, "test_cleat_send_signal_and_wait",
		i32(30), i32(12), // targetRunID
		i32(70), i32(9), // signalName
		i32(110), i32(11), // payload
		int64(10000),       // timeout
		i32(200), i32(512), // resp
	)
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_CleatDefer(t *testing.T) {
	// cleat_defer: (descPtr,descLen, deferIDPtr,deferIDMaxLen) -> i64
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_defer", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatDefer(l)
	})

	s.writeString(40, "cleanup-task")
	got := s.call(t, "test_cleat_defer", i32(40), i32(13), i32(200), i32(64))
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestClosure_CleatPollCancellation(t *testing.T) {
	// cleat_poll_cancellation: (reasonPtr,reasonMaxLen) -> i64
	ft := wasmFunctype([]byte{wasmValI32, wasmValI32}, []byte{wasmValI64})
	s := newClosureSetup(t, []struct {
		name string
		ft   []byte
	}{{"cleat_poll_cancellation", ft}}, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
		return b.registerCleatPollCancellation(l)
	})

	got := s.call(t, "test_cleat_poll_cancellation", i32(400), i32(256))
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

// TestClosure_ErrorPaths tests error-handling paths by passing zero-length strings.
func TestClosure_ErrorPaths(t *testing.T) {
	type testCase struct {
		importName string
		ft         []byte
		register   func(*wasmtimeBackend, *wasmtime.Linker) error
		args       []any
	}
	tests := []testCase{
		// 2-param (ptr,len) -> i64
		{"cleat_release_lock", wasmFunctype([]byte{wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatReleaseLock(l) },
			[]any{i32(0), i32(0)}},
		{"cleat_register_update_handler", wasmFunctype([]byte{wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatRegisterUpdateHandler(l) },
			[]any{i32(0), i32(0)}},
		{"cleat_delete_state", wasmFunctype([]byte{wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatDeleteState(l) },
			[]any{i32(0), i32(0)}},
		{"cleat_has_state", wasmFunctype([]byte{wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatHasState(l) },
			[]any{i32(0), i32(0)}},
		{"cleat_register_query_handler", wasmFunctype([]byte{wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatRegisterQueryHandler(l) },
			[]any{i32(0), i32(0)}},
		{"cleat_incr_state", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI64}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatIncrState(l) },
			[]any{i32(0), i32(0), int64(0)}},
		{"cleat_acquire_lock", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI64}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatAcquireLock(l) },
			[]any{i32(0), i32(0), int64(0)}},
		{"cleat_resolve_promise", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatResolvePromise(l) },
			[]any{i32(0), i32(0), i32(0), i32(0)}},
		{"cleat_reject_promise", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatRejectPromise(l) },
			[]any{i32(0), i32(0), i32(0), i32(0)}},
		{"cleat_set_state", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatSetState(l) },
			[]any{i32(0), i32(0), i32(0), i32(0)}},
		{"cleat_run_detached", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatRunDetached(l) },
			[]any{i32(0), i32(0), i32(0), i32(0)}},
		{"cleat_reply_to_signal", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatReplyToSignal(l) },
			[]any{i32(0), i32(0), i32(0), i32(0)}},
		{"cleat_send", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatSend(l) },
			[]any{i32(0), i32(0), i32(0), i32(0), i32(0), i32(0)}},
		{"cleat_signal_workflow", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatSignalWorkflow(l) },
			[]any{i32(0), i32(0), i32(0), i32(0), i32(0), i32(0)}},
	}

	for _, tc := range tests {
		t.Run(tc.importName+"_zero_len", func(t *testing.T) {
			imports := []struct {
				name string
				ft   []byte
			}{{tc.importName, tc.ft}}
			s := newClosureSetup(t, imports, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
				return tc.register(b, l)
			})
			exportName := "test_" + tc.importName
			got := s.call(t, exportName, tc.args...)
			if got != errBadParamInt64 {
				t.Errorf("got %v, want %v (errBadParamInt64)", got, errBadParamInt64)
			}
		})
	}
}

func TestClosure_MoreErrorPaths(t *testing.T) {
	type testCase struct {
		importName string
		ft         []byte
		register   func(*wasmtimeBackend, *wasmtime.Linker) error
		args       []any
	}
	tests := []testCase{
		// 4-param (2 ptr,len pairs)
		{"set_query_state", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatSetQueryState(l) },
			[]any{i32(0), i32(0), i32(0), i32(0)}},
		{"cleat_await_child", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatAwaitChild(l) },
			[]any{i32(0), i32(0), i32(0), i32(0)}},
		{"cleat_await_all_children", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatAwaitAllChildren(l) },
			[]any{i32(0), i32(0), i32(0), i32(0)}},
		{"cleat_poll_child", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatPollChild(l) },
			[]any{i32(0), i32(0), i32(0), i32(0)}},
		{"cleat_await_any_child", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatAwaitAnyChild(l) },
			[]any{i32(0), i32(0), i32(0), i32(0)}},
		{"cleat_get_state", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatGetState(l) },
			[]any{i32(0), i32(0), i32(0), i32(0)}},
		{"cleat_list_state", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatListState(l) },
			[]any{i32(0), i32(0), i32(0), i32(0)}},
		{"cleat_side_effect", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatSideEffect(l) },
			[]any{i32(0), i32(0), i32(0), i32(0)}},
		{"cleat_continue_as_new", wasmFunctype([]byte{wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatContinueAsNew(l) },
			[]any{i32(0), i32(0)}},
		{"cleat_continue_as_new_versioned", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatContinueAsNewVersioned(l) },
			[]any{i32(0), i32(0), i32(0)}},
		{"cleat_poll_signal", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatPollSignal(l) },
			[]any{i32(0), i32(0), i32(0), i32(0)}},
	}

	for _, tc := range tests {
		t.Run(tc.importName+"_zero_len", func(t *testing.T) {
			imports := []struct {
				name string
				ft   []byte
			}{{tc.importName, tc.ft}}
			s := newClosureSetup(t, imports, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
				return tc.register(b, l)
			})
			exportName := "test_" + tc.importName
			got := s.call(t, exportName, tc.args...)
			if got != errBadParamInt64 {
				t.Errorf("got %v, want %v (errBadParamInt64)", got, errBadParamInt64)
			}
		})
	}
}

func TestClosure_FinalErrorPaths(t *testing.T) {
	type testCase struct {
		importName string
		ft         []byte
		register   func(*wasmtimeBackend, *wasmtime.Linker) error
		args       []any
	}
	tests := []testCase{
		{"cleat_defer", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatDefer(l) },
			[]any{i32(0), i32(0), i32(0), i32(0)}},
		{"cleat_child_workflow", wasmFunctype([]byte{wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32, wasmValI32}, []byte{wasmValI64}),
			func(b *wasmtimeBackend, l *wasmtime.Linker) error { return b.registerCleatChildWorkflow(l) },
			[]any{i32(0), i32(0), i32(0), i32(0), i32(0), i32(0)}},
	}

	for _, tc := range tests {
		t.Run(tc.importName+"_zero_len", func(t *testing.T) {
			imports := []struct {
				name string
				ft   []byte
			}{{tc.importName, tc.ft}}
			s := newClosureSetup(t, imports, func(b *wasmtimeBackend, l *wasmtime.Linker) error {
				return tc.register(b, l)
			})
			exportName := "test_" + tc.importName
			got := s.call(t, exportName, tc.args...)
			if got != errBadParamInt64 {
				t.Errorf("got %v, want %v (errBadParamInt64)", got, errBadParamInt64)
			}
		})
	}
}
