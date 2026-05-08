package wasm

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// hasWasmHeader — WASM magic + version validation
// ---------------------------------------------------------------------------

func TestHasWasmHeaderValid(t *testing.T) {
	wasmBytes := []byte{
		0x00, 0x61, 0x73, 0x6d, // magic: \0asm
		0x01, 0x00, 0x00, 0x00, // version: 1
	}
	if !hasWasmHeader(wasmBytes) {
		t.Error("expected valid header to be recognized")
	}
}

func TestHasWasmHeaderInvalid(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"too short (3 bytes)", []byte{0x00, 0x61, 0x73}},
		{"header without version (4 bytes)", []byte{0x00, 0x61, 0x73, 0x6d}},
		{"bad magic", []byte{0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00}},
		{"bad version", []byte{0x00, 0x61, 0x73, 0x6d, 0x02, 0x00, 0x00, 0x00}},
		{"all zeros", []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}},
		{"all ones", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if hasWasmHeader(tt.data) {
				t.Error("expected invalid header to be rejected")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ULEB128 encoding / decoding roundtrip and edge cases
// ---------------------------------------------------------------------------

func TestULEB128Roundtrip(t *testing.T) {
	values := []uint32{
		0, 1, 2, 63, 64, 127, 128, 255, 256, 16383, 16384,
		65535, 65536, 1<<20 - 1, 1 << 20, 1<<28 - 1, 1 << 28,
		1<<31 - 1, 1 << 31, 0xFFFFFFFF,
	}
	for _, v := range values {
		t.Run(fmt.Sprintf("0x%x", v), func(t *testing.T) {
			encoded := encodeULEB128(v)
			if len(encoded) == 0 {
				t.Fatal("encode returned empty slice")
			}
			decoded, n := decodeULEB128(encoded)
			if n != len(encoded) {
				t.Errorf("consumed %d bytes, want encoded length %d", n, len(encoded))
			}
			if decoded != v {
				t.Errorf("roundtrip: got %d, want %d", decoded, v)
			}
		})
	}
}

func TestDecodeULEB128EdgeCases(t *testing.T) {
	t.Run("nil input", func(t *testing.T) {
		val, n := decodeULEB128(nil)
		if val != 0 || n != 0 {
			t.Errorf("nil: got (%d, %d), want (0, 0)", val, n)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		val, n := decodeULEB128([]byte{})
		if val != 0 || n != 0 {
			t.Errorf("empty: got (%d, %d), want (0, 0)", val, n)
		}
	})

	t.Run("single byte zero", func(t *testing.T) {
		val, n := decodeULEB128([]byte{0x00})
		if val != 0 || n != 1 {
			t.Errorf("0x00: got (%d, %d), want (0, 1)", val, n)
		}
	})

	t.Run("max single byte", func(t *testing.T) {
		val, n := decodeULEB128([]byte{0x7f})
		if val != 127 || n != 1 {
			t.Errorf("0x7f: got (%d, %d), want (127, 1)", val, n)
		}
	})

	t.Run("truncated continuation", func(t *testing.T) {
		// A byte with high bit set but no continuation byte.
		val, n := decodeULEB128([]byte{0x80})
		if val != 0 || n != 0 {
			t.Errorf("truncated 0x80: got (%d, %d), want (0, 0)", val, n)
		}
		val, n = decodeULEB128([]byte{0xc0})
		if val != 0 || n != 0 {
			t.Errorf("truncated 0xc0: got (%d, %d), want (0, 0)", val, n)
		}
	})

	t.Run("overflow after 5 bytes", func(t *testing.T) {
		// 5 bytes all with high bit set — the 5th byte pushes shift past 35.
		val, n := decodeULEB128([]byte{0x80, 0x80, 0x80, 0x80, 0x80})
		if val != 0 || n != 0 {
			t.Errorf("5 continuation bytes: got (%d, %d), want (0, 0)", val, n)
		}
	})

	t.Run("exact max 5-byte value", func(t *testing.T) {
		// 0xFFFFFFFF encodes as 0xff 0xff 0xff 0xff 0x0f.
		val, n := decodeULEB128([]byte{0xff, 0xff, 0xff, 0xff, 0x0f})
		if val != 0xFFFFFFFF || n != 5 {
			t.Errorf("max 5-byte: got (%d, %d), want (%d, 5)", val, n, 0xFFFFFFFF)
		}
	})

	t.Run("multi-byte values", func(t *testing.T) {
		tests := []struct {
			input []byte
			want  uint32
		}{
			{[]byte{0x80, 0x01}, 128},
			{[]byte{0xff, 0x01}, 255},
			{[]byte{0x80, 0x02}, 256},
			{[]byte{0xff, 0x7f}, 16383},
			{[]byte{0x80, 0x80, 0x01}, 16384},
		}
		for _, tt := range tests {
			t.Run(fmt.Sprintf("%x", tt.input), func(t *testing.T) {
				val, n := decodeULEB128(tt.input)
				if val != tt.want || n != len(tt.input) {
					t.Errorf("got (%d, %d), want (%d, %d)", val, n, tt.want, len(tt.input))
				}
			})
		}
	})
}

func TestEncodeULEB128EdgeCases(t *testing.T) {
	tests := []struct {
		value    uint32
		wantLen  int
		wantHex  string
	}{
		{0, 1, "00"},
		{1, 1, "01"},
		{127, 1, "7f"},
		{128, 2, "8001"},
		{16383, 2, "ff7f"},
		{16384, 3, "808001"},
		{1 << 28, 5, "8080808001"},
		{0xFFFFFFFF, 5, "ffffffff0f"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("0x%x", tt.value), func(t *testing.T) {
			encoded := encodeULEB128(tt.value)
			if len(encoded) != tt.wantLen {
				t.Errorf("got len %d, want %d (encoded: %x)", len(encoded), tt.wantLen, encoded)
			}
			decoded, n := decodeULEB128(encoded)
			if n != len(encoded) || decoded != tt.value {
				t.Errorf("roundtrip: got (%d, %d), want (%d, %d)", decoded, n, tt.value, len(encoded))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// WASM binary helpers — building minimal valid WASM for section tests
// ---------------------------------------------------------------------------

// wasmHeader returns the 8-byte WASM magic + version header.
func wasmHeader() []byte {
	return []byte{
		0x00, 0x61, 0x73, 0x6d, // magic: \0asm
		0x01, 0x00, 0x00, 0x00, // version: 1
	}
}

// buildWasmWithCustomSection constructs a valid WASM binary containing a
// single custom section (section ID 0) with the given name and payload.
func buildWasmWithCustomSection(name string, payload []byte) []byte {
	wasm := wasmHeader()

	// Body: name length (ULEB128) + name bytes + payload.
	encodedNameLen := encodeULEB128(uint32(len(name)))
	var body []byte
	body = append(body, encodedNameLen...)
	body = append(body, []byte(name)...)
	body = append(body, payload...)

	// Section: ID (0) + body size (ULEB128) + body.
	var section []byte
	section = append(section, 0) // custom section ID
	section = append(section, encodeULEB128(uint32(len(body)))...)
	section = append(section, body...)

	return append(wasm, section...)
}

// buildWasmWithSections constructs a WASM binary from a header and one or
// more raw section byte slices. This is useful for building modules with
// multiple sections including non-custom sections (type, function, etc.).
func buildWasmWithSections(sections ...[]byte) []byte {
	result := wasmHeader()
	for _, s := range sections {
		result = append(result, s...)
	}
	return result
}

// ---- WASM binary edge cases: empty module, exports, memory ----

func TestWasmEmptyModule(t *testing.T) {
	// A valid empty WASM module is just the 8-byte header with no sections.
	wasm := wasmHeader()
	if !hasWasmHeader(wasm) {
		t.Error("empty module should have valid header")
	}
	if len(wasm) != 8 {
		t.Errorf("expected 8 bytes, got %d", len(wasm))
	}
}

func TestWasmModuleWithCustomSectionOnly(t *testing.T) {
	name := "my-custom-section"
	payload := []byte("some metadata")
	wasm := buildWasmWithCustomSection(name, payload)

	if !hasWasmHeader(wasm) {
		t.Error("module with custom section should have valid header")
	}
	if len(wasm) <= 8 {
		t.Error("module should be larger than header alone")
	}
}

func TestWasmModuleWithTypeSection(t *testing.T) {
	// Build a WASM module with a type section (ID 1) — the simplest standard section.
	// Content: 1 function type with no params and no results.
	typeSection := []byte{
		0x01,             // section ID: type
		0x04,             // section size (ULEB128): 4 bytes
		0x01,             // number of types: 1
		0x60, 0x00, 0x00, // functype: no params, no results
	}
	wasm := buildWasmWithSections(typeSection)

	if !hasWasmHeader(wasm) {
		t.Error("module with type section should have valid header")
	}

	// Verify we can still read a custom section appended to a module with
	// non-custom sections (the custom section reader should skip section ID 1).
	customName := "after-types"
	wasmWithCustom, err := writeCustomSection(wasm, customName, []byte("data"))
	if err != nil {
		t.Fatalf("writeCustomSection after type section: %v", err)
	}

	data, err := readCustomSection(wasmWithCustom, customName)
	if err != nil {
		t.Fatalf("readCustomSection after type section: %v", err)
	}
	if string(data) != "data" {
		t.Errorf("got %q, want 'data'", string(data))
	}
}

func TestWasmModuleWithMemorySection(t *testing.T) {
	// Build a module with type section, function section, and memory section
	// to simulate a realistic WASM module that has memory exports.
	// Memory section (ID 5): 1 memory, initial pages=1, no max.
	memorySection := []byte{
		0x05,       // section ID: memory
		0x04,       // section size (ULEB128): 4 bytes
		0x01,       // number of memories: 1
		0x00, 0x01, // limits: 0x00 (no max), initial=1 page
	}
	wasm := buildWasmWithSections(memorySection)

	if !hasWasmHeader(wasm) {
		t.Error("module with memory section should have valid header")
	}
}

// ---------------------------------------------------------------------------
// readCustomSection — edge cases
// ---------------------------------------------------------------------------

func TestReadCustomSection(t *testing.T) {
	name := "test-section"
	payload := []byte("hello payload")
	wasm := buildWasmWithCustomSection(name, payload)

	t.Run("found", func(t *testing.T) {
		result, err := readCustomSection(wasm, name)
		if err != nil {
			t.Fatalf("readCustomSection: %v", err)
		}
		if string(result) != string(payload) {
			t.Errorf("got %q, want %q", string(result), string(payload))
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, err := readCustomSection(wasm, "other-section")
		if err == nil {
			t.Error("expected error for missing section")
		}
	})

	t.Run("header only (no sections)", func(t *testing.T) {
		_, err := readCustomSection(wasmHeader(), "anything")
		if err == nil {
			t.Error("expected error for header-only wasm")
		}
	})
}

func TestReadCustomSectionCorrupt(t *testing.T) {
	t.Run("too short", func(t *testing.T) {
		_, err := readCustomSection([]byte{0x00, 0x61}, "x")
		if err == nil {
			t.Error("expected error for too-short input")
		}
	})

	t.Run("bad header", func(t *testing.T) {
		_, err := readCustomSection([]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, "x")
		if err == nil || !strings.Contains(err.Error(), "bad magic") {
			t.Errorf("expected bad magic error, got: %v", err)
		}
	})

	t.Run("corrupt section size", func(t *testing.T) {
		// Valid header followed by a section with truncated size encoding.
		corrupt := wasmHeader()
		corrupt = append(corrupt, 0x00)    // section ID (custom)
		corrupt = append(corrupt, 0x80)    // ULEB128 size: truncated continuation
		_, err := readCustomSection(corrupt, "x")
		if err == nil {
			t.Error("expected error for corrupt section size")
		}
	})

	t.Run("name overflows section", func(t *testing.T) {
		// Build a section whose name length byte claims more bytes than the section body.
		corrupt := wasmHeader()
		corrupt = append(corrupt, 0x00)                                               // section ID (custom)
		corrupt = append(corrupt, 0x05)                                               // size: 5 bytes
		corrupt = append(corrupt, 0x0a)                                               // name length: 10 (but only 3 bytes remain)
		corrupt = append(corrupt, []byte("abc")...)                                   // only 3 bytes of name
		_, err := readCustomSection(corrupt, "x")
		if err == nil {
			t.Error("expected error for name overflow")
		}
	})
}

// ---------------------------------------------------------------------------
// writeCustomSection — edge cases
// ---------------------------------------------------------------------------

func TestWriteCustomSection(t *testing.T) {
	header := wasmHeader()

	t.Run("append to header-only", func(t *testing.T) {
		name := "new-section"
		payload := []byte("data")
		result, err := writeCustomSection(header, name, payload)
		if err != nil {
			t.Fatalf("writeCustomSection: %v", err)
		}
		read, err := readCustomSection(result, name)
		if err != nil {
			t.Fatalf("readCustomSection: %v", err)
		}
		if string(read) != string(payload) {
			t.Errorf("got %q, want %q", string(read), string(payload))
		}
	})

	t.Run("replace existing", func(t *testing.T) {
		name := "replace-section"
		wasm := buildWasmWithCustomSection(name, []byte("old payload"))

		result, err := writeCustomSection(wasm, name, []byte("new payload"))
		if err != nil {
			t.Fatalf("writeCustomSection: %v", err)
		}

		if !hasWasmHeader(result) {
			t.Error("result has invalid WASM header")
		}

		read, err := readCustomSection(result, name)
		if err != nil {
			t.Fatalf("readCustomSection after replace: %v", err)
		}
		if string(read) != "new payload" {
			t.Errorf("got %q, want 'new payload'", string(read))
		}
	})

	t.Run("empty payload", func(t *testing.T) {
		name := "empty-section"
		result, err := writeCustomSection(header, name, []byte{})
		if err != nil {
			t.Fatalf("writeCustomSection: %v", err)
		}
		read, err := readCustomSection(result, name)
		if err != nil {
			t.Fatalf("readCustomSection: %v", err)
		}
		if len(read) != 0 {
			t.Errorf("expected empty payload, got %d bytes", len(read))
		}
	})
}

// ---------------------------------------------------------------------------
// stripCustomSection — edge cases
// ---------------------------------------------------------------------------

func TestStripCustomSection(t *testing.T) {
	name := "section-to-strip"
	payload := []byte("strip me")
	wasm := buildWasmWithCustomSection(name, payload)

	t.Run("strip existing", func(t *testing.T) {
		result, err := stripCustomSection(wasm, name)
		if err != nil {
			t.Fatalf("stripCustomSection: %v", err)
		}
		if len(result) != 8 {
			t.Errorf("expected 8 bytes (header only), got %d", len(result))
		}
		if !hasWasmHeader(result) {
			t.Error("result has invalid WASM header")
		}
	})

	t.Run("strip non-existent", func(t *testing.T) {
		_, err := stripCustomSection(wasm, "non-existent")
		if err == nil {
			t.Error("expected error for non-existent section")
		}
	})

	t.Run("too short", func(t *testing.T) {
		_, err := stripCustomSection([]byte{0x00}, name)
		if err == nil {
			t.Error("expected error for too-short input")
		}
	})

	t.Run("bad header", func(t *testing.T) {
		_, err := stripCustomSection([]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, name)
		if err == nil || !strings.Contains(err.Error(), "bad magic") {
			t.Errorf("expected bad magic error, got: %v", err)
		}
	})
}

func TestStripCustomSectionWithOtherSections(t *testing.T) {
	// Build WASM with a type section (ID 1) and two custom sections,
	// then strip one of the custom sections.

	// Type section: 1 function type with no params/results.
	typeSection := []byte{
		0x01,             // section ID: type
		0x04,             // section size
		0x01,             // 1 type
		0x60, 0x00, 0x00, // func type: () -> ()
	}

	// Custom section "keep-me"
	keepPayload := []byte("keep data")
	keepNameLen := encodeULEB128(uint32(len("keep-me")))
	keepBody := append(keepNameLen, []byte("keep-me")...)
	keepBody = append(keepBody, keepPayload...)
	keepSection := []byte{0x00} // custom section ID
	keepSection = append(keepSection, encodeULEB128(uint32(len(keepBody)))...)
	keepSection = append(keepSection, keepBody...)

	// Custom section "strip-me"
	stripPayload := []byte("strip data")
	stripNameLen := encodeULEB128(uint32(len("strip-me")))
	stripBody := append(stripNameLen, []byte("strip-me")...)
	stripBody = append(stripBody, stripPayload...)
	stripSection := []byte{0x00} // custom section ID
	stripSection = append(stripSection, encodeULEB128(uint32(len(stripBody)))...)
	stripSection = append(stripSection, stripBody...)

	wasm := buildWasmWithSections(typeSection, keepSection, stripSection)

	result, err := stripCustomSection(wasm, "strip-me")
	if err != nil {
		t.Fatalf("stripCustomSection: %v", err)
	}

	if !hasWasmHeader(result) {
		t.Error("result has invalid header")
	}

	// strip-me should be gone.
	_, err = readCustomSection(result, "strip-me")
	if err == nil {
		t.Error("strip-me section should have been removed")
	}

	// keep-me should still be present.
	data, err := readCustomSection(result, "keep-me")
	if err != nil {
		t.Fatalf("keep-me section missing: %v", err)
	}
	if string(data) != "keep data" {
		t.Errorf("keep-me payload: got %q, want 'keep data'", string(data))
	}
}

// ---------------------------------------------------------------------------
// Metadata — Validate, ReadMetadata, WriteMetadata
// ---------------------------------------------------------------------------

func TestMetadataValidate(t *testing.T) {
	t.Run("valid metadata", func(t *testing.T) {
		m := &Metadata{
			WorkflowName:         "test-workflow",
			WorkflowVersion:      1,
			ABIVersion:           1,
			MinCompatibleVersion: 1,
		}
		if err := m.Validate(); err != nil {
			t.Errorf("expected no error, got: %v", err)
		}
	})

	t.Run("empty name", func(t *testing.T) {
		m := &Metadata{
			WorkflowName:         "",
			WorkflowVersion:      1,
			ABIVersion:           1,
			MinCompatibleVersion: 1,
		}
		if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "workflow_name") {
			t.Errorf("expected workflow_name error, got: %v", err)
		}
	})

	t.Run("zero workflow version", func(t *testing.T) {
		m := &Metadata{
			WorkflowName:         "test",
			WorkflowVersion:      0,
			ABIVersion:           1,
			MinCompatibleVersion: 1,
		}
		if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "workflow_version") {
			t.Errorf("expected workflow_version error, got: %v", err)
		}
	})

	t.Run("negative workflow version", func(t *testing.T) {
		m := &Metadata{
			WorkflowName:         "test",
			WorkflowVersion:      -1,
			ABIVersion:           1,
			MinCompatibleVersion: 1,
		}
		if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "workflow_version") {
			t.Errorf("expected workflow_version error, got: %v", err)
		}
	})

	t.Run("zero abi version", func(t *testing.T) {
		m := &Metadata{
			WorkflowName:         "test",
			WorkflowVersion:      1,
			ABIVersion:           0,
			MinCompatibleVersion: 1,
		}
		if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "abi_version") {
			t.Errorf("expected abi_version error, got: %v", err)
		}
	})

	t.Run("zero min compatible version", func(t *testing.T) {
		m := &Metadata{
			WorkflowName:         "test",
			WorkflowVersion:      1,
			ABIVersion:           1,
			MinCompatibleVersion: 0,
		}
		if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "min_compatible_version") {
			t.Errorf("expected min_compatible_version error, got: %v", err)
		}
	})

	t.Run("min exceeds abi version", func(t *testing.T) {
		m := &Metadata{
			WorkflowName:         "test",
			WorkflowVersion:      1,
			ABIVersion:           1,
			MinCompatibleVersion: 2,
		}
		if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "min_compatible_version") {
			t.Errorf("expected min_compatible_version exceeds error, got: %v", err)
		}
	})
}

func TestMetadataRoundtrip(t *testing.T) {
	header := wasmHeader()

	meta := &Metadata{
		WorkflowName:         "roundtrip-test",
		WorkflowVersion:      42,
		ABIVersion:           2,
		MinCompatibleVersion: 1,
		PluginDeps: map[string]string{
			"blobstore": "0.1.0",
			"scheduler": "0.2.0",
		},
	}

	t.Run("write then read", func(t *testing.T) {
		written, err := WriteMetadata(header, meta)
		if err != nil {
			t.Fatalf("WriteMetadata: %v", err)
		}
		if len(written) <= len(header) {
			t.Error("written bytes should include added section")
		}

		read, err := ReadMetadata(written)
		if err != nil {
			t.Fatalf("ReadMetadata: %v", err)
		}

		if read.WorkflowName != meta.WorkflowName {
			t.Errorf("name: got %q, want %q", read.WorkflowName, meta.WorkflowName)
		}
		if read.WorkflowVersion != meta.WorkflowVersion {
			t.Errorf("version: got %d, want %d", read.WorkflowVersion, meta.WorkflowVersion)
		}
		if read.ABIVersion != meta.ABIVersion {
			t.Errorf("abi: got %d, want %d", read.ABIVersion, meta.ABIVersion)
		}
		if read.MinCompatibleVersion != meta.MinCompatibleVersion {
			t.Errorf("min_compat: got %d, want %d", read.MinCompatibleVersion, meta.MinCompatibleVersion)
		}
		if len(read.PluginDeps) != 2 || read.PluginDeps["blobstore"] != "0.1.0" {
			t.Errorf("plugin_deps: got %v", read.PluginDeps)
		}
	})

	t.Run("write replaces existing section", func(t *testing.T) {
		v1, err := WriteMetadata(header, meta)
		if err != nil {
			t.Fatalf("WriteMetadata v1: %v", err)
		}

		meta2 := &Metadata{
			WorkflowName:         "updated",
			WorkflowVersion:      99,
			ABIVersion:           3,
			MinCompatibleVersion: 2,
		}
		v2, err := WriteMetadata(v1, meta2)
		if err != nil {
			t.Fatalf("WriteMetadata v2: %v", err)
		}

		read, err := ReadMetadata(v2)
		if err != nil {
			t.Fatalf("ReadMetadata: %v", err)
		}
		if read.WorkflowName != "updated" || read.WorkflowVersion != 99 {
			t.Errorf("v2 values not found after replace: got %+v", read)
		}
	})
}

func TestReadMetadataInvalid(t *testing.T) {
	t.Run("too short", func(t *testing.T) {
		_, err := ReadMetadata([]byte{0x00, 0x61})
		if err == nil {
			t.Error("expected error for too-short input")
		}
	})

	t.Run("bad header", func(t *testing.T) {
		_, err := ReadMetadata([]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
		if err == nil || !strings.Contains(err.Error(), "bad magic") {
			t.Errorf("expected bad magic error, got: %v", err)
		}
	})

	t.Run("no custom section", func(t *testing.T) {
		_, err := ReadMetadata(wasmHeader())
		if err == nil || !strings.Contains(err.Error(), "no cleat.metadata section") {
			t.Errorf("expected no section error, got: %v", err)
		}
	})

	t.Run("invalid JSON in section", func(t *testing.T) {
		wasm := buildWasmWithCustomSection("cleat.metadata", []byte("{invalid json}"))
		_, err := ReadMetadata(wasm)
		if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
			t.Errorf("expected invalid JSON error, got: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// rewritePackageToMain — edge cases
// ---------------------------------------------------------------------------

func TestRewritePackageToMain(t *testing.T) {
	t.Run("simple rewrite", func(t *testing.T) {
		input := []byte("package foo\n\nfunc main() {}\n")
		result := rewritePackageToMain(input)
		if !strings.Contains(string(result), "package main") {
			t.Errorf("result does not contain 'package main': %q", string(result))
		}
		if strings.Contains(string(result), "package foo") {
			t.Errorf("result still contains 'package foo': %q", string(result))
		}
	})

	t.Run("no package declaration", func(t *testing.T) {
		input := []byte("func main() {}\n")
		result := rewritePackageToMain(input)
		if string(result) != string(input) {
			t.Errorf("expected unchanged output, got %q", string(result))
		}
	})

	t.Run("empty input", func(t *testing.T) {
		result := rewritePackageToMain([]byte{})
		if len(result) != 0 {
			t.Errorf("expected empty output, got %d bytes", len(result))
		}
	})

	t.Run("nil input", func(t *testing.T) {
		result := rewritePackageToMain(nil)
		if result != nil {
			t.Errorf("expected nil output, got %v", result)
		}
	})

	t.Run("package in middle of file", func(t *testing.T) {
		input := []byte("//go:build wasip1\n\npackage foo\n\nfunc F() {}\n")
		result := rewritePackageToMain(input)
		if !strings.Contains(string(result), "package main") {
			t.Errorf("result does not contain 'package main': %q", string(result))
		}
		if strings.Contains(string(result), "package foo") {
			t.Errorf("result still contains 'package foo': %q", string(result))
		}
	})

	t.Run("no trailing newline after package", func(t *testing.T) {
		input := []byte("package foo")
		result := rewritePackageToMain(input)
		if string(result) != "package main" {
			t.Errorf("expected 'package main', got %q", string(result))
		}
	})

	t.Run("comment before package, no trailing newline", func(t *testing.T) {
		// The loop finds \n after "// comment", which is not a package line.
		// Then the fallback logic rewrites the last line.
		input := []byte("// comment\npackage foo")
		result := rewritePackageToMain(input)
		expected := "// comment\npackage main"
		if string(result) != expected {
			t.Errorf("expected %q, got %q", expected, string(result))
		}
	})

	t.Run("package with spaces before declaration", func(t *testing.T) {
		input := []byte("package\nfoo\n\nfunc main() {}\n")
		// "package" on its own is not "package <name>", so no rewrite.
		result := rewritePackageToMain(input)
		if string(result) != string(input) {
			t.Errorf("expected unchanged for 'package' without name, got %q", string(result))
		}
	})

	t.Run("one-line file with newline", func(t *testing.T) {
		input := []byte("package foo\n")
		result := rewritePackageToMain(input)
		if string(result) != "package main\n" {
			t.Errorf("expected 'package main\\n', got %q", string(result))
		}
	})
}

// ---------------------------------------------------------------------------
// needsTime — untested pure function coverage
// ---------------------------------------------------------------------------

func TestNeedsTime(t *testing.T) {
	t.Run("heartbeat uses time.Duration", func(t *testing.T) {
		usage := &UsageInfo{
			Funcs: []HostFunction{
				{ImportName: "cleat_call_heartbeat", FieldName: "DurableCallWithHeartbeat"},
			},
		}
		if !needsTime(usage) {
			t.Error("DurableCallWithHeartbeat should need time")
		}
	})

	t.Run("basic call does not need time", func(t *testing.T) {
		usage := &UsageInfo{
			Funcs: []HostFunction{
				{ImportName: "cleat_call", FieldName: "DurableCall"},
			},
		}
		if needsTime(usage) {
			t.Error("DurableCall should not need time")
		}
	})

	t.Run("empty usage does not need time", func(t *testing.T) {
		usage := &UsageInfo{Funcs: nil}
		if needsTime(usage) {
			t.Error("empty usage should not need time")
		}
	})
}

// ---------------------------------------------------------------------------
// numOutBufs — edge cases not covered in existing tests
// ---------------------------------------------------------------------------

func TestNumOutBufsEdgeCases(t *testing.T) {
	tests := []struct {
		name string
		want int
	}{
		{"cleat_call_retry", 1},
		{"cleat_await_child", 1},
		{"cleat_child_workflow_with_options", 1},
		{"cleat_continue_as_new", 0},
		{"cleat_continue_as_new_versioned", 0},
		{"cleat_now", 0},
		{"cleat_random", 0},
		{"set_query_state", 0},
		{"cleat_register_update_handler", 0},
		{"cleat_side_effect", 1},
		{"plugin_call_streaming", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := numOutBufs(tt.name); got != tt.want {
				t.Errorf("numOutBufs(%q)=%d, want %d", tt.name, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// goName — additional edge cases
// ---------------------------------------------------------------------------

func TestGoNameEdgeCases(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"plugin_call", "pluginCall"},
		{"plugin_call_streaming", "pluginCallStreaming"},
		{"set_query_state", "setQueryState"},
		{"cleat_side_effect", "cleatSideEffect"},
		{"cleat_acquire_lock", "cleatAcquireLock"},
		{"cleat_release_lock", "cleatReleaseLock"},
		{"cleat_create_promise", "cleatCreatePromise"},
		{"cleat_await_promise", "cleatAwaitPromise"},
		{"cleat_register_update_handler", "cleatRegisterUpdateHandler"},
		{"cleat_call_retry", "cleatCallRetry"},
		{"cleat_call_heartbeat", "cleatCallHeartbeat"},
		{"cleat_child_workflow_with_options", "cleatChildWorkflowWithOptions"},
		{"cleat_await_all_children", "cleatAwaitAllChildren"},
		{"cleat_continue_as_new_versioned", "cleatContinueAsNewVersioned"},
		{"cleat_min_version", "cleatMinVersion"},
		{"cleat_now", "cleatNow"},
		{"cleat_random", "cleatRandom"},
		{"single", "single"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := goName(tt.in); got != tt.want {
				t.Errorf("goName(%q)=%q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// JSON roundtrip for Metadata
// ---------------------------------------------------------------------------

func TestMetadataJSONRoundtrip(t *testing.T) {
	meta := &Metadata{
		WorkflowName:         "json-test",
		WorkflowVersion:      1,
		ABIVersion:           2,
		MinCompatibleVersion: 1,
		PluginDeps:           map[string]string{"plugin-a": "1.0.0"},
	}

	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded Metadata
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.WorkflowName != meta.WorkflowName {
		t.Errorf("name: got %q, want %q", decoded.WorkflowName, meta.WorkflowName)
	}
	if decoded.WorkflowVersion != meta.WorkflowVersion {
		t.Errorf("version: got %d, want %d", decoded.WorkflowVersion, meta.WorkflowVersion)
	}
	if decoded.ABIVersion != meta.ABIVersion {
		t.Errorf("abi: got %d, want %d", decoded.ABIVersion, meta.ABIVersion)
	}
	if decoded.PluginDeps["plugin-a"] != "1.0.0" {
		t.Errorf("plugin_deps: got %v", decoded.PluginDeps)
	}
}

func TestMetadataCurrentABIVersion(t *testing.T) {
	if CurrentABIVersion != 1 {
		t.Errorf("expected CurrentABIVersion=1, got %d", CurrentABIVersion)
	}
}
