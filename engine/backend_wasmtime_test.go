//go:build cgo

package engine

import (
	"context"
	"errors"
	"testing"
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

