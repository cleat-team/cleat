package engine

import (
	"context"
	"testing"
)

// ---------------------------------------------------------------------------
// mockWasmMem implements api.Memory with an in-memory byte slice.
// ---------------------------------------------------------------------------

type mockWasmMem struct {
	buf []byte
}

func (m *mockWasmMem) Read(addr, size uint32) ([]byte, bool) {
	if addr+size > uint32(len(m.buf)) {
		return nil, false
	}
	out := make([]byte, size)
	copy(out, m.buf[addr:addr+size])
	return out, true
}

func (m *mockWasmMem) Write(addr uint32, val []byte) bool {
	end := addr + uint32(len(val))
	if end > uint32(len(m.buf)) {
		newBuf := make([]byte, end)
		copy(newBuf, m.buf)
		m.buf = newBuf
	}
	copy(m.buf[addr:], val)
	return true
}

func (m *mockWasmMem) Size() uint32                       { return uint32(len(m.buf)) }
func (m *mockWasmMem) Length() uint32                     { return uint32(len(m.buf)) }
func (m *mockWasmMem) ReadByte(uint32) (byte, bool)       { return 0, false }
func (m *mockWasmMem) ReadUint32Le(uint32) (uint32, bool) { return 0, false }
func (m *mockWasmMem) ReadUint64Le(uint32) (uint64, bool) { return 0, false }
func (m *mockWasmMem) WriteByte(uint32, byte) bool        { return false }
func (m *mockWasmMem) WriteUint32Le(uint32, uint32) bool  { return false }
func (m *mockWasmMem) WriteUint64Le(uint32, uint64) bool  { return false }

// failWriteMem is like mockWasmMem but Write always fails.
type failWriteMem struct {
	mockWasmMem
}

func (m *failWriteMem) Write(uint32, []byte) bool { return false }

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
	mem := &mockWasmMem{buf: []byte("hello world")}

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
	// Out of bounds.
	if got := readWasmString(mem, 0, 100); got != "" {
		t.Errorf("readWasmString(0,100) = %q, want %q", got, "")
	}
}

// ---------------------------------------------------------------------------
// readWasmStringValidated tests
// ---------------------------------------------------------------------------

func TestReadWasmStringValidated(t *testing.T) {
	mem := &mockWasmMem{buf: []byte("hello world")}

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
	// Out of bounds read.
	s, ok = readWasmStringValidated(mem, 0, 100, 200)
	if s != "" || ok {
		t.Errorf("readWasmStringValidated OOB = %q, %v, want %q, false", s, ok, "")
	}
}

// ---------------------------------------------------------------------------
// readServiceName tests
// ---------------------------------------------------------------------------

func TestReadServiceName(t *testing.T) {
	mem := &mockWasmMem{buf: []byte("valid-name")}
	name, ok := readServiceName(mem, 0, 10)
	if name != "valid-name" || !ok {
		t.Errorf("readServiceName = %q, %v, want %q, true", name, ok, "valid-name")
	}

	// Invalid characters.
	mem2 := &mockWasmMem{buf: []byte("invalid!name")}
	_, ok = readServiceName(mem2, 0, 12)
	if ok {
		t.Error("readServiceName with invalid chars should return false")
	}

	// Empty / out of bounds.
	mem3 := &mockWasmMem{buf: []byte("")}
	_, ok = readServiceName(mem3, 0, 1)
	if ok {
		t.Error("readServiceName on empty should return false")
	}
}

// ---------------------------------------------------------------------------
// writeWasmString tests
// ---------------------------------------------------------------------------

func TestWriteWasmString(t *testing.T) {
	mem := &mockWasmMem{buf: make([]byte, 100)}

	n, err := writeWasmString(mem, 10, "hello", 50)
	if n != 5 || err != nil {
		t.Fatalf("writeWasmString(10, hello, 50) = %d, %v, want 5, nil", n, err)
	}
	if string(mem.buf[10:15]) != "hello" {
		t.Errorf("buf[10:15] = %q, want %q", string(mem.buf[10:15]), "hello")
	}

	// Truncated to maxLen.
	n, err = writeWasmString(mem, 20, "hello world", 5)
	if n != 5 || err != nil {
		t.Fatalf("writeWasmString truncated = %d, %v, want 5, nil", n, err)
	}
	if string(mem.buf[20:25]) != "hello" {
		t.Errorf("truncated = %q, want %q", string(mem.buf[20:25]), "hello")
	}

	// Empty string.
	n, err = writeWasmString(mem, 0, "", 50)
	if n != 0 || err != nil {
		t.Errorf("writeWasmString empty = %d, %v, want 0, nil", n, err)
	}
}

func TestWriteWasmString_Failure(t *testing.T) {
	mem := &failWriteMem{}
	_, err := writeWasmString(mem, 0, "data", 100)
	if err == nil {
		t.Error("expected error from failed write")
	}
}

func TestWriteWasmStringOrTrap(t *testing.T) {
	mem := &mockWasmMem{buf: make([]byte, 100)}
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
	// responseLen=100 (bits 40-63), callErrorCode=2 (bits 8-39), errCode=1 (bits 0-7)
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
	// Without extra.
	result := packSimpleResult(5)
	if result != 5 {
		t.Errorf("packSimpleResult(5) = %d, want 5", result)
	}
	// With extra.
	result = packSimpleResult(3, 42)
	expected := int64(uint64(42)<<32 | 3)
	if result != expected {
		t.Errorf("packSimpleResult(3,42) = %d, want %d", result, expected)
	}
	// Zero.
	result = packSimpleResult(0)
	if result != 0 {
		t.Errorf("packSimpleResult(0) = %d, want 0", result)
	}
}

func TestDecodeExportResult(t *testing.T) {
	// errCode=7, actualLen=10.
	val := uint64(10)<<32 | 7
	errCode, actualLen := decodeExportResult(val)
	if errCode != 7 || actualLen != 10 {
		t.Errorf("decodeExportResult = (%d, %d), want (7, 10)", errCode, actualLen)
	}
	// Zero.
	errCode, actualLen = decodeExportResult(0)
	if errCode != 0 || actualLen != 0 {
		t.Errorf("decodeExportResult(0) = (%d, %d), want (0, 0)", errCode, actualLen)
	}
	// Max values.
	val = uint64(0xFFFFFFFF_FFFFFFFF)
	errCode, actualLen = decodeExportResult(val)
	if errCode != 0xFFFFFFFF || actualLen != 0xFFFFFFFF {
		t.Errorf("decodeExportResult(max) = (%d, %d), want (0xFFFFFFFF, 0xFFFFFFFF)", errCode, actualLen)
	}
}

// ---------------------------------------------------------------------------
// minU32 tests
// ---------------------------------------------------------------------------

func TestMinU32(t *testing.T) {
	tests := []struct {
		a, b, want uint32
	}{
		{1, 2, 1},
		{5, 3, 3},
		{4, 4, 4},
		{0, 100, 0},
		{100, 0, 0},
	}
	for _, tc := range tests {
		got := minU32(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("minU32(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
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
	// Verify the value is stored and retrievable via the unexported key.
	val := ctx.Value(wasmMemBufKey{})
	if val == nil {
		t.Fatal("wasmMemBufKey not found in context")
	}
	got, ok := val.([]byte)
	if !ok || string(got) != "test-data" {
		t.Errorf("context value = %v, want %q", val, "test-data")
	}
}
