package wasm

import "fmt"

// ReadMemoryInitialPages reads the initial (minimum) memory pages required by
// a WASM binary. It checks the memory section (section ID 5) for directly defined
// memories and the import section (section ID 2) for imported memories. Returns 0
// if no memory requirement is found.
//
// NOTE: This reads the initial/minimum pages from the WASM declaration, not the
// maximum. Runtime enforcement via wazero's WithMemoryLimitPages is the actual
// security boundary. The claim-time check here is an early-advisory filter only.
func ReadMemoryInitialPages(wasmBytes []byte) uint32 {
	minPages, _ := readMemorySectionMin(wasmBytes)
	if minPages > 0 {
		return minPages
	}
	importedMin, _ := readImportedMemoryMin(wasmBytes)
	return importedMin
}

// readMemorySectionMin reads the minimum memory pages from the WASM memory
// section (section ID 5). Returns 0 if there is no memory section.
func readMemorySectionMin(wasmBytes []byte) (uint32, error) {
	if len(wasmBytes) < 8 {
		return 0, fmt.Errorf("WASM binary too short")
	}
	if !hasWasmHeader(wasmBytes) {
		return 0, fmt.Errorf("not a valid WASM binary")
	}
	offset := 8
	for offset < len(wasmBytes) {
		sectionID := wasmBytes[offset]
		offset++
		if offset >= len(wasmBytes) {
			return 0, fmt.Errorf("truncated WASM: section size missing")
		}
		size, n := decodeULEB128(wasmBytes[offset:])
		if n <= 0 {
			return 0, fmt.Errorf("corrupt WASM: failed to decode section size at offset %d", offset)
		}
		offset += n
		if int(size) > len(wasmBytes)-offset {
			return 0, fmt.Errorf("corrupt WASM: section size overflows binary")
		}
		sectionEnd := offset + int(size)
		if sectionID != 5 {
			offset = sectionEnd
			continue
		}
		count, nn := decodeULEB128(wasmBytes[offset:])
		if nn <= 0 {
			return 0, fmt.Errorf("corrupt WASM memory section: failed to decode count")
		}
		offset += nn
		if count == 0 {
			return 0, nil
		}
		// Read the first memory entry's initial (minimum) pages.
		_, nn = decodeULEB128(wasmBytes[offset:]) // flags
		if nn <= 0 {
			return 0, fmt.Errorf("corrupt WASM memory section: bad flags")
		}
		offset += nn
		var initialPages uint32
		initialPages, nn = decodeULEB128(wasmBytes[offset:])
		if nn <= 0 {
			return 0, fmt.Errorf("corrupt WASM memory section: bad initial pages")
		}
		return initialPages, nil
	}
	return 0, nil
}

// readImportedMemoryMin checks the import section (section ID 2) for imported
// memory and returns its minimum pages. Returns 0 if no memory import is found.
func readImportedMemoryMin(wasmBytes []byte) (uint32, error) {
	if len(wasmBytes) < 8 {
		return 0, fmt.Errorf("WASM binary too short")
	}
	if !hasWasmHeader(wasmBytes) {
		return 0, fmt.Errorf("not a valid WASM binary")
	}
	offset := 8
	for offset < len(wasmBytes) {
		sectionID := wasmBytes[offset]
		offset++
		size, n := decodeULEB128(wasmBytes[offset:])
		if n <= 0 {
			return 0, fmt.Errorf("corrupt WASM: failed to decode section size")
		}
		offset += n
		if int(size) > len(wasmBytes)-offset {
			return 0, fmt.Errorf("corrupt WASM: import section size overflows")
		}
		sectionEnd := offset + int(size)
		if sectionID != 2 {
			offset = sectionEnd
			continue
		}
		count, nn := decodeULEB128(wasmBytes[offset:])
		if nn <= 0 {
			return 0, fmt.Errorf("corrupt WASM import section: bad count")
		}
		offset += nn
		for i := uint32(0); i < count; i++ {
			// Module name.
			nameLen, nn := decodeULEB128(wasmBytes[offset:])
			if nn <= 0 {
				return 0, fmt.Errorf("corrupt WASM import %d: bad module name length", i)
			}
			offset += nn
			if offset+int(nameLen) > sectionEnd {
				return 0, fmt.Errorf("corrupt WASM import %d: module name overflow", i)
			}
			offset += int(nameLen)
			// Field name.
			fieldLen, nn := decodeULEB128(wasmBytes[offset:])
			if nn <= 0 {
				return 0, fmt.Errorf("corrupt WASM import %d: bad field name length", i)
			}
			offset += nn
			if offset+int(fieldLen) > sectionEnd {
				return 0, fmt.Errorf("corrupt WASM import %d: field name overflow", i)
			}
			offset += int(fieldLen)
			if offset >= sectionEnd {
				return 0, fmt.Errorf("corrupt WASM import %d: missing kind", i)
			}
			kind := wasmBytes[offset]
			offset++
			if kind == 2 {
				// Memory import: read pages.
				_, nn := decodeULEB128(wasmBytes[offset:]) // flags
				if nn <= 0 {
					return 0, fmt.Errorf("corrupt WASM import %d: bad memory flags", i)
				}
				offset += nn
				var initialPages uint32
				initialPages, nn = decodeULEB128(wasmBytes[offset:]) // min
				if nn <= 0 {
					return 0, fmt.Errorf("corrupt WASM import %d: bad memory min", i)
				}
				return initialPages, nil
			}
			// Skip non-memory import descriptors.
			switch kind {
			case 0: // func
				if _, nn := decodeULEB128(wasmBytes[offset:]); nn <= 0 {
					return 0, fmt.Errorf("corrupt WASM import %d: bad func type index", i)
				}
				offset += nn
			case 1: // table
				offset++
				tFlags, nn := decodeULEB128(wasmBytes[offset:])
				if nn <= 0 {
					return 0, fmt.Errorf("corrupt WASM import %d: bad table flags", i)
				}
				offset += nn
				if _, nn := decodeULEB128(wasmBytes[offset:]); nn <= 0 {
					return 0, fmt.Errorf("corrupt WASM import %d: bad table min", i)
				}
				offset += nn
				if tFlags&0x01 != 0 {
					if _, nn := decodeULEB128(wasmBytes[offset:]); nn <= 0 {
						return 0, fmt.Errorf("corrupt WASM import %d: bad table max", i)
					}
					offset += nn
				}
			case 3: // global
				offset += 2
			}
		}
		return 0, nil
	}
	return 0, nil
}
