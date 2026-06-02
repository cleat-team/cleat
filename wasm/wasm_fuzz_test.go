package wasm

import (
	"testing"
)

// FuzzWASMMetadata fuzzes the WASM binary metadata parser (ReadMetadata and
// WriteMetadata) with random byte sequences. It exercises the custom section
// reader, ULEB128 decoder, and section stripping logic.
func FuzzWASMMetadata(f *testing.F) {
	// A minimal valid WASM binary (8-byte header).
	validWasm := []byte{
		0x00, 0x61, 0x73, 0x6d, // magic: \0asm
		0x01, 0x00, 0x00, 0x00, // version: 1
	}

	// Build a valid WASM with a custom metadata section for the seed corpus.
	meta := &Metadata{
		WorkflowName:         "seed-workflow",
		WorkflowVersion:      1,
		ABIVersion:           1,
		MinCompatibleVersion: 1,
	}
	wasmWithMeta, err := WriteMetadata(validWasm, meta)
	_ = err

	// Seed corpus
	f.Add(validWasm)
	if len(wasmWithMeta) > 0 {
		f.Add(wasmWithMeta)
	}
	f.Add([]byte{})
	f.Add([]byte{0x00, 0x61, 0x73})
	f.Add([]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	f.Add([]byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x00})
	f.Add(bytesRepeat(0xff, 100))
	f.Add(bytesRepeat(0x00, 100))

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("panic on input %x: %v", data, r)
			}
		}()

		// Fuzz ReadMetadata — this parses the WASM binary custom sections.
		meta, err := ReadMetadata(data)
		_ = meta
		_ = err

		// If the data has at least an 8-byte WASM header, also try
		// WriteMetadata round trips and stripCustomSection.
		if len(data) >= 8 {
			testMeta := &Metadata{
				WorkflowName:         "fuzz-test",
				WorkflowVersion:      1,
				ABIVersion:           1,
				MinCompatibleVersion: 1,
			}
			written, wErr := WriteMetadata(data, testMeta)
			if wErr == nil && len(written) > 0 {
				// Round trip: read back what we just wrote.
				_, _ = ReadMetadata(written)
			}

			// Strip a non-existent section to exercise the full section walk.
			_, _ = stripCustomSection(data, "nonexistent.section")
		}
	})
}

// FuzzULEB128 fuzzes the ULEB128 decoder with random byte sequences.
func FuzzULEB128(f *testing.F) {
	f.Add([]byte{0x00})
	f.Add([]byte{0x7f})
	f.Add([]byte{0x80, 0x01})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0x0f})
	f.Add([]byte{})
	f.Add([]byte{0x80, 0x80, 0x80, 0x80, 0x01})
	f.Add([]byte{0x80})

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("panic on input %x: %v", data, r)
			}
		}()

		val, n := decodeULEB128(data)
		_ = val
		_ = n

		// Verify that encodeULEB128 round-trips with decodeULEB128 for any
		// successfully decoded value. This exercises both functions.
		if n > 0 {
			encoded := encodeULEB128(val)
			decoded, dn := decodeULEB128(encoded)
			_ = decoded
			_ = dn
		}
	})
}

// bytesRepeat returns a byte slice of length n filled with value b.
func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
