package wasm

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// ScannedImport represents a single import entry in a WASM binary.
type ScannedImport struct {
	Module string // import module name (e.g. "env")
	Name   string // import field name (e.g. "cleat_call")
	Kind   byte   // 0 = func, 1 = table, 2 = mem, 3 = global
}

// ScanWasmImports reads a WASM binary and returns all import entries.
// This is used post-compilation to verify that the set of host functions
// actually imported matches what the closure analysis predicted.
//
// The WASM binary format is:
//   - magic: \0asm (4 bytes)
//   - version: 1 (4 bytes, little-endian u32)
//   - sections: each with id (1 byte) + size (LEB128 u32) + content
// Section 2 = Import section.
func ScanWasmImports(wasmBytes []byte) ([]ScannedImport, error) {
	if len(wasmBytes) < 8 {
		return nil, fmt.Errorf("WASM binary too short: %d bytes", len(wasmBytes))
	}

	// Validate magic: \0asm
	if wasmBytes[0] != 0x00 || wasmBytes[1] != 0x61 ||
		wasmBytes[2] != 0x73 || wasmBytes[3] != 0x6d {
		return nil, fmt.Errorf("invalid WASM magic number")
	}

	// Validate version (u32 little-endian).
	version := binary.LittleEndian.Uint32(wasmBytes[4:8])
	if version != 1 {
		return nil, fmt.Errorf("unsupported WASM version: %d", version)
	}

	offset := 8
	var imports []ScannedImport

	for offset < len(wasmBytes) {
		if offset >= len(wasmBytes) {
			break
		}
		sectionID := wasmBytes[offset]
		offset++

		// Read section size (LEB128 u32).
		sectionSize, n := readLEB128U32(wasmBytes[offset:])
		if n == 0 {
			return nil, fmt.Errorf("invalid section size at offset %d", offset)
		}
		offset += n

		sectionEnd := offset + int(sectionSize)
		if sectionEnd > len(wasmBytes) {
			sectionEnd = len(wasmBytes)
		}

		if sectionID == 2 {
			// Import section.
			if offset >= sectionEnd {
				return imports, nil
			}
			importCount, n := readLEB128U32(wasmBytes[offset:])
			if n == 0 {
				return imports, nil
			}
			offset += n

			for i := uint32(0); i < importCount && offset < sectionEnd; i++ {
				// Read module string.
				moduleStr, n := readName(wasmBytes[offset:])
				if n == 0 {
					return imports, fmt.Errorf("failed to read import module name at import %d", i)
				}
				offset += n

				// Read field name (import name).
				fieldStr, n := readName(wasmBytes[offset:])
				if n == 0 {
					return imports, fmt.Errorf("failed to read import field name at import %d", i)
				}
				offset += n

				if offset >= len(wasmBytes) {
					return imports, fmt.Errorf("truncated import entry %d", i)
				}

				imp := ScannedImport{
					Module: moduleStr,
					Name:   fieldStr,
					Kind:   wasmBytes[offset],
				}
				offset++

				// Skip type index for function imports.
				if imp.Kind == 0 {
					_, n := readLEB128U32(wasmBytes[offset:])
					offset += n
				}
				// Skip limits for table/memory imports.
				if imp.Kind == 1 || imp.Kind == 2 {
					_ = wasmBytes[offset] // limits type (0x00 or 0x01)
					offset++
					_, n := readLEB128U32(wasmBytes[offset:])
					offset += n
					if wasmBytes[offset-1] == 0x01 {
						_, n = readLEB128U32(wasmBytes[offset:])
						offset += n
					}
				}
				// Skip global type for global imports.
				if imp.Kind == 3 {
					offset += 2 // content type + mutability
				}

				imports = append(imports, imp)
			}
		}

		offset = sectionEnd
		// Align to next section boundary.
		if offset < len(wasmBytes) && wasmBytes[offset] == 0 {
			offset++
		}
	}

	return imports, nil
}

// FindCleatOrphanedImports scans a WASM binary and detects imported host
// functions (from the "env" module) that reference cleat operations but
// were not predicted by the closure analysis. Returns descriptions of
// each orphaned import found.
//
// expectedImports is the set of import names that the closure analysis
// predicted (e.g. "cleat_call", "cleat_sleep").
func FindCleatOrphanedImports(wasmBytes []byte, expectedImports map[string]bool) []string {
	imports, err := ScanWasmImports(wasmBytes)
	if err != nil {
		return []string{fmt.Sprintf("error scanning WASM imports: %v", err)}
	}

	var orphans []string
	cleatPrefixes := []string{"cleat_", "set_", "plugin_", "schedule_"}

	for _, imp := range imports {
		if imp.Module != "env" {
			continue
		}
		if imp.Kind != 0 {
			continue
		}
		// Check if this looks like a cleat host function.
		isCleatImport := false
		for _, prefix := range cleatPrefixes {
			if strings.HasPrefix(imp.Name, prefix) {
				isCleatImport = true
				break
			}
		}
		if !isCleatImport {
			continue
		}
		// Map import names to their closure-analysis keys.
		// Both "cleat_call" and "cleat_call_retry" are tracked under "cleat_call"
		// in the usage map. Normalize by taking the base name.
		baseName := normalizeImportName(imp.Name)
		if !expectedImports[baseName] && !expectedImports[imp.Name] {
			orphans = append(orphans, fmt.Sprintf(
				"host function %q imported from WASM env but not in computed closure; "+
					"either the closure analysis missed a call path or the WASM binary includes unused imports",
				imp.Name,
			))
		}
	}
	return orphans
}

// normalizeImportName maps a WASM import name to the key used in UsageInfo.Used.
func normalizeImportName(name string) string {
	// Map variants to their base import names for orphan detection.
	// cleat_fetch is mapped to cleat_call so we don't flag it as an orphan
	// when the closure analysis only tracks cleat_call. The double-check
	// !expectedImports[baseName] && !expectedImports[imp.Name] in
	// FindCleatOrphanedImports prevents false positives, but the mapping
	// makes the intent clearer.
	switch name {
	case "cleat_call_retry", "cleat_call_heartbeat", "cleat_fetch":
		return "cleat_call"
	case "cleat_child_workflow_with_options", "cleat_child_workflow_in_schema":
		return "cleat_child_workflow"
	case "cleat_continue_as_new_versioned":
		return "cleat_continue_as_new"
	case "cleat_send_signal_and_wait", "cleat_reply_to_signal", "cleat_signal_workflow":
		return "cleat_await_signals"
	case "cleat_acquire_lock", "cleat_release_lock":
		return "cleat_acquire_lock"
	case "cleat_set_state", "cleat_get_state", "cleat_delete_state",
		"cleat_incr_state", "cleat_has_state", "cleat_list_state":
		return "set_query_state"
	case "schedule_invoke":
		return "cleat_sleep"
	}
	return name
}

// readLEB128U32 reads a LEB128-encoded unsigned 32-bit integer.
// Returns (value, bytesRead). Returns (0, 0) on error.
func readLEB128U32(data []byte) (uint32, int) {
	var result uint32
	var shift uint
	bytesRead := 0
	for {
		if bytesRead >= len(data) || bytesRead >= 5 {
			return 0, 0
		}
		b := data[bytesRead]
		bytesRead++
		result |= uint32(b&0x7f) << shift
		if b&0x80 == 0 {
			return result, bytesRead
		}
		shift += 7
	}
}

// readName reads a WASM name (length-prefixed UTF-8 string).
// Returns (string, bytesRead). Returns ("", 0) on error.
func readName(data []byte) (string, int) {
	length, n := readLEB128U32(data)
	if n == 0 {
		return "", 0
	}
	offset := n
	if uint32(len(data))-uint32(offset) < length {
		return "", 0
	}
	name := string(data[offset : offset+int(length)])
	return name, n + int(length)
}

// IsCleatHostFunction returns true if the import name corresponds to a
// cleat host function import.
var IsCleatHostFunction = func(name string) bool {
	return strings.HasPrefix(name, "cleat_") ||
		strings.HasPrefix(name, "set_") ||
		strings.HasPrefix(name, "plugin_") ||
		strings.HasPrefix(name, "schedule_")
}

