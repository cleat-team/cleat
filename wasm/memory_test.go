package wasm

import (
	"testing"
)

// ---- WASM binary builders for memory tests ----

// memTestHeader returns a valid 8-byte WASM header.
func memTestHeader() []byte {
	return []byte{
		0x00, 0x61, 0x73, 0x6d, // magic
		0x01, 0x00, 0x00, 0x00, // version
	}
}

// memTestWasm wraps a header and sections into a full WASM binary.
func memTestWasm(sections ...[]byte) []byte {
	out := make([]byte, len(memTestHeader()))
	copy(out, memTestHeader())
	for _, s := range sections {
		out = append(out, s...)
	}
	return out
}

// memSection builds a memory section (ID 5) with one memory entry
// having the given initial pages and flags (0 = no max, 1 = max present).
func memSection(initialPages uint32, flags byte) []byte {
	// Content: count=1, flags, initial pages, [max pages if flags&1 != 0]
	content := encodeULEB128(1)                  // count
	content = append(content, encodeULEB128(uint32(flags))...)
	content = append(content, encodeULEB128(initialPages)...)
	if flags&1 != 0 {
		content = append(content, encodeULEB128(initialPages*2)...) // max
	}
	// Section: id=5, size, content.
	size := encodeULEB128(uint32(len(content)))
	out := []byte{5}
	out = append(out, size...)
	out = append(out, content...)
	return out
}

// importSectionWithMemory builds an import section (ID 2) containing a
// memory import with the given initial pages.
func importSectionWithMemory(initialPages uint32) []byte {
	// Content: count=1
	content := encodeULEB128(1)
	// Module name: "env"
	content = append(content, encodeULEB128(3)...)
	content = append(content, []byte("env")...)
	// Field name: "memory"
	content = append(content, encodeULEB128(6)...)
	content = append(content, []byte("memory")...)
	// Kind: 2 (memory)
	content = append(content, byte(2))
	// Descriptor: flags=0, initial pages
	content = append(content, encodeULEB128(0)...) // flags (no max)
	content = append(content, encodeULEB128(initialPages)...)
	// Section: id=2, size, content.
	size := encodeULEB128(uint32(len(content)))
	out := []byte{2}
	out = append(out, size...)
	out = append(out, content...)
	return out
}

// importSectionWithMemoryAndMax builds an import section with a memory
// import that has a max pages field (flags=0x01).
func importSectionWithMemoryAndMax(initialPages uint32, maxPages uint32) []byte {
	content := encodeULEB128(1)
	content = append(content, encodeULEB128(3)...)
	content = append(content, []byte("env")...)
	content = append(content, encodeULEB128(6)...)
	content = append(content, []byte("memory")...)
	content = append(content, byte(2))
	content = append(content, encodeULEB128(1)...) // flags=1 (has max)
	content = append(content, encodeULEB128(initialPages)...)
	content = append(content, encodeULEB128(maxPages)...)
	size := encodeULEB128(uint32(len(content)))
	out := []byte{2}
	out = append(out, size...)
	out = append(out, content...)
	return out
}

// importSectionWithoutMemory builds an import section with a function
// import (not memory), to test that the function skips non-memory imports.
func importSectionWithoutMemory() []byte {
	content := encodeULEB128(1)
	content = append(content, encodeULEB128(3)...)
	content = append(content, []byte("env")...)
	content = append(content, encodeULEB128(10)...)
	content = append(content, []byte("cleat_call")...)
	content = append(content, byte(0)) // kind=func
	content = append(content, encodeULEB128(0)...) // type index
	size := encodeULEB128(uint32(len(content)))
	out := []byte{2}
	out = append(out, size...)
	out = append(out, content...)
	return out
}

// ---- ReadMemoryInitialPages tests ----

func TestReadMemoryInitialPages_HeaderOnly(t *testing.T) {
	wasm := memTestWasm()
	if got := ReadMemoryInitialPages(wasm); got != 0 {
		t.Errorf("expected 0 pages for header-only wasm, got %d", got)
	}
}

func TestReadMemoryInitialPages_FromMemorySection(t *testing.T) {
	wasm := memTestWasm(memSection(1, 0))
	if got := ReadMemoryInitialPages(wasm); got != 1 {
		t.Errorf("expected 1 page, got %d", got)
	}
}

func TestReadMemoryInitialPages_MemorySection256Pages(t *testing.T) {
	wasm := memTestWasm(memSection(256, 0))
	if got := ReadMemoryInitialPages(wasm); got != 256 {
		t.Errorf("expected 256 pages, got %d", got)
	}
}

func TestReadMemoryInitialPages_MemorySectionWithMax(t *testing.T) {
	// Memory section with flags=1 (max present). The function reads initial
	// regardless of flags. It returns immediately without consuming the max
	// value, which is fine since the function exits after read.
	wasm := memTestWasm(memSection(10, 1))
	if got := ReadMemoryInitialPages(wasm); got != 10 {
		t.Errorf("expected 10 pages, got %d", got)
	}
}

func TestReadMemoryInitialPages_FromImportedMemory(t *testing.T) {
	wasm := memTestWasm(importSectionWithMemory(3))
	if got := ReadMemoryInitialPages(wasm); got != 3 {
		t.Errorf("expected 3 pages from imported memory, got %d", got)
	}
}

func TestReadMemoryInitialPages_ImportedWithMax(t *testing.T) {
	wasm := memTestWasm(importSectionWithMemoryAndMax(5, 20))
	if got := ReadMemoryInitialPages(wasm); got != 5 {
		t.Errorf("expected 5 pages from imported memory with max, got %d", got)
	}
}

func TestReadMemoryInitialPages_MemorySectionWinsOverImport(t *testing.T) {
	wasm := memTestWasm(
		memSection(7, 0),
		importSectionWithMemory(3),
	)
	if got := ReadMemoryInitialPages(wasm); got != 7 {
		t.Errorf("expected 7 (memory section wins), got %d", got)
	}
}

func TestReadMemoryInitialPages_ImportSectionNoMemory(t *testing.T) {
	// Import section with a function import only — should return 0.
	wasm := memTestWasm(importSectionWithoutMemory())
	if got := ReadMemoryInitialPages(wasm); got != 0 {
		t.Errorf("expected 0 for import section without memory, got %d", got)
	}
}

func TestReadMemoryInitialPages_TooShort(t *testing.T) {
	short := []byte{0x00, 0x61}
	if got := ReadMemoryInitialPages(short); got != 0 {
		t.Errorf("expected 0 for short binary, got %d", got)
	}
}

func TestReadMemoryInitialPages_BadMagic(t *testing.T) {
	bad := []byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x00, 0x00, 0x00}
	if got := ReadMemoryInitialPages(bad); got != 0 {
		t.Errorf("expected 0 for bad magic, got %d", got)
	}
}

func TestReadMemoryInitialPages_MemorySectionZeroCount(t *testing.T) {
	// Memory section with count=0 — should return 0.
	content := encodeULEB128(0) // count = 0
	size := encodeULEB128(uint32(len(content)))
	out := make([]byte, len(memTestHeader()), len(memTestHeader())+1+len(size)+len(content))
	copy(out, memTestHeader())
	out = append(out, 5) // section ID
	out = append(out, size...)
	out = append(out, content...)
	if got := ReadMemoryInitialPages(out); got != 0 {
		t.Errorf("expected 0 for zero-count memory section, got %d", got)
	}
}
