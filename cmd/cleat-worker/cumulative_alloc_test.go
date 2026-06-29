package main

import (
	"sync/atomic"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// makeWasmBinary builds a minimal valid WASM binary with the specified
// initial memory pages. The binary has only the 8-byte header and a
// memory section (section ID 5) with one memory entry.
func makeWasmBinary(initialPages uint32) []byte {
	header := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

	// Memory section content: count=1, flags=0, initial pages
	content := []byte{0x01}                     // count of memories
	content = append(content, 0x00)             // flags: no max
	content = append(content, byte(initialPages)) // initial pages (assumes < 128)

	// Section: id=5, size=len(content), content
	section := []byte{0x05}               // section ID 5 = memory
	section = append(section, byte(len(content))) // section size (assumes < 128)
	section = append(section, content...)

	return append(header, section...)
}

func TestCheckCumulativeAllocation_Unlimited(t *testing.T) {
	zero := 0
	counter := new(atomic.Int64)
	w := &Worker{
		wasmCumulativeAllocationMaxMB: &zero,
		wasmCumulativeAllocBytes:      counter,
	}

	wasmBytes := makeWasmBinary(16) // 1 MB
	allocBytes, err := w.checkCumulativeAllocation(wasmBytes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allocBytes != 0 {
		t.Fatalf("expected 0 allocBytes when limit is disabled, got %d", allocBytes)
	}
}

func TestCheckCumulativeAllocation_NilPointer(t *testing.T) {
	counter := new(atomic.Int64)
	w := &Worker{
		wasmCumulativeAllocationMaxMB: nil,
		wasmCumulativeAllocBytes:      counter,
	}

	wasmBytes := makeWasmBinary(16) // 1 MB
	allocBytes, err := w.checkCumulativeAllocation(wasmBytes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allocBytes != 0 {
		t.Fatalf("expected 0 allocBytes when pointer is nil, got %d", allocBytes)
	}
}

func TestCheckCumulativeAllocation_WithinLimit(t *testing.T) {
	limit := 10 // 10 MB
	counter := new(atomic.Int64)
	w := &Worker{
		wasmCumulativeAllocationMaxMB: &limit,
		wasmCumulativeAllocBytes:      counter,
	}

	wasmBytes := makeWasmBinary(16) // 16 pages = 1 MB
	allocBytes, err := w.checkCumulativeAllocation(wasmBytes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectedBytes := int64(16) * 65536 // 1 MB
	if allocBytes != expectedBytes {
		t.Fatalf("expected %d allocBytes, got %d", expectedBytes, allocBytes)
	}
	if counter.Load() != expectedBytes {
		t.Fatalf("expected counter to be %d, got %d", expectedBytes, counter.Load())
	}
}

func TestCheckCumulativeAllocation_Exceeded(t *testing.T) {
	limit := 1 // 1 MB
	counter := new(atomic.Int64)
	w := &Worker{
		wasmCumulativeAllocationMaxMB: &limit,
		wasmCumulativeAllocBytes:      counter,
	}

	wasmBytes := makeWasmBinary(32) // 32 pages = 2 MB
	_, err := w.checkCumulativeAllocation(wasmBytes)
	if err == nil {
		t.Fatal("expected error when limit is exceeded")
	}
	if counter.Load() != 0 {
		t.Fatalf("expected counter to be 0 after rejection, got %d", counter.Load())
	}
}

func TestCheckCumulativeAllocation_CumulativeExceeded(t *testing.T) {
	limit := 10 // 10 MB
	counter := new(atomic.Int64)
	w := &Worker{
		wasmCumulativeAllocationMaxMB: &limit,
		wasmCumulativeAllocBytes:      counter,
	}

	// First allocation: 96 pages = 6 MB — should succeed.
	wasm1 := makeWasmBinary(96)
	alloc1, err := w.checkCumulativeAllocation(wasm1)
	if err != nil {
		t.Fatalf("unexpected error on first allocation: %v", err)
	}

	// Second allocation: another 96 pages = 6 MB — total would be 12 MB > 10 MB.
	wasm2 := makeWasmBinary(96)
	_, err = w.checkCumulativeAllocation(wasm2)
	if err == nil {
		t.Fatal("expected error when cumulative limit is exceeded")
	}

	// Counter should still be at first allocation (rollback succeeded).
	expected := int64(96) * 65536
	if counter.Load() != expected {
		t.Fatalf("expected counter to be %d after rejection, got %d", expected, counter.Load())
	}

	// Release first allocation.
	w.wasmCumulativeAllocBytes.Add(-alloc1)
	if counter.Load() != 0 {
		t.Fatalf("expected counter to be 0 after release, got %d", counter.Load())
	}
}

func TestCheckCumulativeAllocation_ReleaseAndRetry(t *testing.T) {
	limit := 10 // 10 MB
	counter := new(atomic.Int64)
	w := &Worker{
		wasmCumulativeAllocationMaxMB: &limit,
		wasmCumulativeAllocBytes:      counter,
	}

	// Reserve 6 MB.
	wasm1 := makeWasmBinary(96)
	alloc1, err := w.checkCumulativeAllocation(wasm1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Release it.
	w.wasmCumulativeAllocBytes.Add(-alloc1)

	// Reserve 6 MB again — should succeed.
	wasm2 := makeWasmBinary(96)
	_, err = w.checkCumulativeAllocation(wasm2)
	if err != nil {
		t.Fatalf("unexpected error after release: %v", err)
	}
}

func TestCheckCumulativeAllocation_ZeroPages(t *testing.T) {
	// WASM binary with 0 memory pages (no memory section) — should allocate 0 bytes.
	header := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	limit := 10
	counter := new(atomic.Int64)
	w := &Worker{
		wasmCumulativeAllocationMaxMB: &limit,
		wasmCumulativeAllocBytes:      counter,
	}

	allocBytes, err := w.checkCumulativeAllocation(header)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allocBytes != 0 {
		t.Fatalf("expected 0 allocBytes for 0 pages, got %d", allocBytes)
	}
}

// Verify that our test helper produces a valid WASM binary that
// wasm.ReadMemoryInitialPages can parse correctly.
func TestMakeWasmBinary_PageCount(t *testing.T) {
	for _, pages := range []uint32{0, 1, 16, 32, 96, 127} {
		binary := makeWasmBinary(pages)
		got := wasm.ReadMemoryInitialPages(binary)
		if got != pages {
			t.Errorf("makeWasmBinary(%d): ReadMemoryInitialPages returned %d", pages, got)
		}
	}
}
