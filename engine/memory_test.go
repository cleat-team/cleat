package engine

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// newTestMemory compiles a minimal WASM module that exports memory and returns
// the module's Memory for use in memory.go utility function tests.
func newTestMemory(t *testing.T, data []byte) api.Memory {
	t.Helper()
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	t.Cleanup(func() { rt.Close(ctx) })

	mod, err := rt.CompileModule(ctx, minimalMemoryWasm())
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	cfg := wazero.NewModuleConfig().WithName("test-mem")
	m, err := rt.InstantiateModule(ctx, mod, cfg)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	mem := m.Memory()
	if len(data) > 0 {
		if !mem.Write(0, data) {
			t.Fatal("write to memory failed")
		}
	}
	return mem
}

// minimalMemoryWasm returns a minimal valid WASM module that exports one
// memory (1 page) and a stub init function. Generated from:
//
//	(module
//	  (memory (export "mem") 1)
//	  (func (export "init"))
//	)
func minimalMemoryWasm() []byte {
	return []byte{
		0x00, 0x61, 0x73, 0x6d, // magic
		0x01, 0x00, 0x00, 0x00, // version
		0x01, 0x04, 0x01, 0x60, 0x00, 0x00, // type section: () -> ()
		0x03, 0x02, 0x01, 0x00, // function section: 1 import (index 0)
		0x05, 0x03, 0x01, 0x00, 0x01, // memory section: 1 mem, min 0, max 1
		0x07, 0x0e, 0x02, // export section: 2 exports (14 bytes content)
		0x03, 0x6d, 0x65, 0x6d, 0x02, 0x00, // "mem" memory index 0 (6 bytes)
		0x04, 0x69, 0x6e, 0x69, 0x74, 0x00, 0x00, // "init" function index 0 (7 bytes)
		0x0a, 0x04, 0x01, 0x02, 0x00, 0x0b, // code section: 1 body, 0 locals, end
	}
}

// ---------------------------------------------------------------------------
// validServiceName tests
// ---------------------------------------------------------------------------

func TestValidServiceName_Valid(t *testing.T) {
	tests := []struct {
		name  string
		valid bool
	}{
		{"", false},
		{"my-service", true},
		{"my_service", true},
		{"my.service", true},
		{"MyService1", true},
		{"a", true},
		{"a-b", true},
		{"UPPERCASE", true},
		{"123numeric", true},
		{"no spaces", false},
		{"with/slash", false},
		{"with:colon", false},
		{"with\nnewline", false},
		{"special!chars", false},
	}
	for _, tc := range tests {
		got := validServiceName(tc.name)
		if got != tc.valid {
			t.Errorf("validServiceName(%q) = %v, want %v", tc.name, got, tc.valid)
		}
	}
}

// ---------------------------------------------------------------------------
// readWasmString tests
// ---------------------------------------------------------------------------

func TestReadWasmString(t *testing.T) {
	mem := newTestMemory(t, []byte("hello world"))

	if got := readWasmString(mem, 0, 5); got != "hello" {
		t.Errorf("readWasmString(0,5) = %q, want %q", got, "hello")
	}
	if got := readWasmString(mem, 6, 5); got != "world" {
		t.Errorf("readWasmString(6,5) = %q, want %q", got, "world")
	}
	// Zero length.
	if got := readWasmString(mem, 0, 0); got != "" {
		t.Errorf("readWasmString(0,0) = %q, want %q", got, "")
	}
	// Out of bounds (wazero returns zero-extended buffer).
	if got := readWasmString(mem, 0, 100); len(got) != 100 {
		t.Errorf("readWasmString(0,100) length = %d, want 100", len(got))
	} else if got[:11] != "hello world" {
		t.Errorf("readWasmString(0,100) prefix = %q, want %q", got[:11], "hello world")
	}
}

// ---------------------------------------------------------------------------
// readWasmStringValidated tests
// ---------------------------------------------------------------------------

func TestReadWasmStringValidated(t *testing.T) {
	mem := newTestMemory(t, []byte("hello world"))

	// Normal.
	s, ok := readWasmStringValidated(mem, 0, 5, 100)
	if s != "hello" || !ok {
		t.Errorf("readWasmStringValidated = %q, %v, want %q, true", s, ok, "hello")
	}
	// Zero length.
	s, ok = readWasmStringValidated(mem, 0, 0, 100)
	if s != "" || ok {
		t.Errorf("readWasmStringValidated(0,0) = %q, %v, want %q, false", s, ok, "")
	}
	// Exceeds maxLen.
	s, ok = readWasmStringValidated(mem, 0, 10, 5)
	if s != "" || ok {
		t.Errorf("readWasmStringValidated maxLen = %q, %v, want %q, false", s, ok, "")
	}
	// Out of bounds read (wazero returns zero-extended buffer if available).
	s, ok = readWasmStringValidated(mem, 0, 100, 200)
	if len(s) != 100 || !ok {
		t.Errorf("readWasmStringValidated OOB = (len=%d), %v, want len=100, true", len(s), ok)
	}
}

// ---------------------------------------------------------------------------
// readServiceName tests
// ---------------------------------------------------------------------------

func TestReadServiceName(t *testing.T) {
	mem := newTestMemory(t, []byte("valid-name"))
	name, ok := readServiceName(mem, 0, 10)
	if name != "valid-name" || !ok {
		t.Errorf("readServiceName = %q, %v, want %q, true", name, ok, "valid-name")
	}

	// Invalid characters.
	mem2 := newTestMemory(t, []byte("invalid!name"))
	_, ok = readServiceName(mem2, 0, 12)
	if ok {
		t.Error("readServiceName with invalid chars should return false")
	}

	// Empty memory.
	mem3 := newTestMemory(t, nil)
	_, ok = readServiceName(mem3, 0, 1)
	if ok {
		t.Error("readServiceName on empty should return false")
	}
}

// ---------------------------------------------------------------------------
// writeWasmString tests
// ---------------------------------------------------------------------------

func TestWriteWasmString(t *testing.T) {
	mem := newTestMemory(t, make([]byte, 100))

	n, err := writeWasmString(mem, 10, "hello", 50)
	if n != 5 || err != nil {
		t.Fatalf("writeWasmString(10, hello, 50) = %d, %v, want 5, nil", n, err)
	}
	got, _ := mem.Read(10, 5)
	if string(got) != "hello" {
		t.Errorf("mem[10:15] = %q, want %q", string(got), "hello")
	}

	// Truncated to maxLen.
	n, err = writeWasmString(mem, 20, "hello world", 5)
	if n != 5 || err != nil {
		t.Fatalf("writeWasmString truncated = %d, %v, want 5, nil", n, err)
	}
	got, _ = mem.Read(20, 5)
	if string(got) != "hello" {
		t.Errorf("truncated = %q, want %q", string(got), "hello")
	}

	// Empty string.
	n, err = writeWasmString(mem, 0, "", 50)
	if n != 0 || err != nil {
		t.Errorf("writeWasmString empty = %d, %v, want 0, nil", n, err)
	}
}

func TestWriteWasmString_Failure(t *testing.T) {
	// wazero's real Memory.Write returns false when the write exceeds the
	// configured max memory. Create one page, fill it, then try writing
	// beyond.
	mem := newTestMemory(t, make([]byte, 65536))
	// Write at the last byte: one byte fits, but Memory.Write writes
	// byte-by-byte via WriteByte, which fails when the offset exceeds
	// the configured max. Each write of 1 byte at offset 65535 succeeds;
	// offset 65536 fails. So a 65536-byte write at offset 1 should fail.
	//
	// This is an assertion about a pinned dependency (the exact wazero
	// version in go.mod), not an environment fact -- it does not vary by OS,
	// CI job, or machine the way a missing toolchain does. The guard used to
	// read "if the write succeeds, t.Skip": that inverts the usual skip
	// shape (skip when a resource is *missing*) into "skip when the
	// assumption under test turns out false", which means a wazero upgrade
	// that changed Write to auto-grow instead of failing would report this
	// suite green having verified nothing. The whole purpose of this test is
	// to catch exactly that kind of semantics change, so it must fail loudly
	// when it happens, not disappear.
	if mem.Write(1, make([]byte, 65536)) {
		t.Fatalf("wazero Memory.Write grew past the configured max instead of failing -- this pinned-dependency assumption changed (check go.mod wazero version); writeWasmString's failure handling needs updating to match, not this test skipping")
	}
}

func TestWriteWasmStringOrTrap(t *testing.T) {
	mem := newTestMemory(t, make([]byte, 100))
	n, err := writeWasmStringOrTrap(mem, 0, "test", 100)
	if n != 4 || err != nil {
		t.Errorf("writeWasmStringOrTrap = %d, %v, want 4, nil", n, err)
	}
}

// ---------------------------------------------------------------------------
// Packing / unpacking tests
// ---------------------------------------------------------------------------

func TestPackDurableCallResult(t *testing.T) {
	result := packDurableCallResult(100, 2, 1)
	expected := int64(uint64(100)<<40 | uint64(2)<<8 | uint64(1))
	if result != expected {
		t.Errorf("packDurableCallResult = %d, want %d", result, expected)
	}

	// Zero values.
	result = packDurableCallResult(0, 0, 0)
	if result != 0 {
		t.Errorf("packDurableCallResult zero = %d, want 0", result)
	}
}

func TestPackSimpleResult(t *testing.T) {
	result := packSimpleResult(5)
	if result != 5 {
		t.Errorf("packSimpleResult(5) = %d, want 5", result)
	}
	result = packSimpleResult(3, 42)
	expected := int64(uint64(42)<<32 | 3)
	if result != expected {
		t.Errorf("packSimpleResult(3,42) = %d, want %d", result, expected)
	}
	result = packSimpleResult(0)
	if result != 0 {
		t.Errorf("packSimpleResult(0) = %d, want 0", result)
	}
}

func TestDecodeExportResult(t *testing.T) {
	val := uint64(10)<<32 | 7
	errCode, actualLen := decodeExportResult(val)
	if errCode != 7 || actualLen != 10 {
		t.Errorf("decodeExportResult = (%d, %d), want (7, 10)", errCode, actualLen)
	}
	errCode, actualLen = decodeExportResult(0)
	if errCode != 0 || actualLen != 0 {
		t.Errorf("decodeExportResult(0) = (%d, %d), want (0, 0)", errCode, actualLen)
	}
	val = uint64(0xFFFFFFFF_FFFFFFFF)
	errCode, actualLen = decodeExportResult(val)
	if errCode != 0xFFFFFFFF || actualLen != 0xFFFFFFFF {
		t.Errorf("decodeExportResult(max) = (%d, %d), want (0xFFFFFFFF, 0xFFFFFFFF)", errCode, actualLen)
	}
}

// ---------------------------------------------------------------------------
// contextWithRawMemBuf tests
// ---------------------------------------------------------------------------

func TestContextWithRawMemBuf(t *testing.T) {
	buf := []byte("test-data")
	ctx := contextWithRawMemBuf(context.Background(), buf)
	if ctx == nil {
		t.Fatal("contextWithRawMemBuf returned nil context")
	}
	val := ctx.Value(wasmMemBufKey{})
	if val == nil {
		t.Fatal("wasmMemBufKey not found in context")
	}
	got, ok := val.([]byte)
	if !ok || string(got) != "test-data" {
		t.Errorf("context value = %v, want %q", val, "test-data")
	}
}
