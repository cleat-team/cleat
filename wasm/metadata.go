// Package wasm — WASM binary metadata (custom section "cleat.metadata").
//
// Metadata is embedded as a JSON payload in a WASM custom section so that
// cleat deploy can extract workflow name, version, ABI info, and plugin
// dependencies without separate configuration.

package wasm

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
)

// Metadata is embedded in the "cleat.metadata" custom section of compiled
// WASM binaries. It carries the information needed for deployment.
type Metadata struct {
	WorkflowName         string            `json:"workflow_name"`
	WorkflowVersion      int               `json:"workflow_version"`
	ABIVersion           int               `json:"abi_version"`
	MinCompatibleVersion int               `json:"min_compatible_version"`
	PluginDeps           map[string]string `json:"plugin_deps,omitempty"`
	ChildVersions        map[string]int    `json:"child_versions,omitempty"`
	Language             string            `json:"language,omitempty"`
}

// CurrentABIVersion is the ABI version produced by this version of cleat.
const CurrentABIVersion = 1

// sectionName is the WASM custom section name used to store metadata.
const sectionName = "cleat.metadata"

// Validate checks that the metadata fields are within acceptable ranges.
func (m *Metadata) Validate() error {
	if m.WorkflowName == "" {
		return fmt.Errorf("metadata: workflow_name is empty")
	}
	if m.WorkflowVersion <= 0 {
		return fmt.Errorf("metadata: workflow_version must be positive, got %d", m.WorkflowVersion)
	}
	if m.ABIVersion <= 0 {
		return fmt.Errorf("metadata: abi_version must be positive, got %d", m.ABIVersion)
	}
	if m.MinCompatibleVersion <= 0 {
		return fmt.Errorf("metadata: min_compatible_version must be positive, got %d", m.MinCompatibleVersion)
	}
	if m.MinCompatibleVersion > m.ABIVersion {
		return fmt.Errorf("metadata: min_compatible_version (%d) exceeds abi_version (%d)",
			m.MinCompatibleVersion, m.ABIVersion)
	}
	return nil
}

// ReadMetadata extracts the "cleat.metadata" custom section from a WASM
// binary and unmarshals it into a Metadata struct.
func ReadMetadata(wasmBytes []byte) (*Metadata, error) {
	payload, err := readCustomSection(wasmBytes, sectionName)
	if err != nil {
		return nil, err
	}
	var meta Metadata
	if err := json.Unmarshal(payload, &meta); err != nil {
		return nil, fmt.Errorf("cleat.metadata: invalid JSON: %w", err)
	}
	return &meta, nil
}

// WriteMetadata appends (or replaces) the "cleat.metadata" custom section
// in a WASM binary and returns the modified bytes.
func WriteMetadata(wasmBytes []byte, meta *Metadata) ([]byte, error) {
	payload, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("marshaling metadata: %w", err)
	}
	result, err := writeCustomSection(wasmBytes, sectionName, payload)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// --- low-level WASM custom section helpers ---

func readCustomSection(wasmBytes []byte, name string) ([]byte, error) {
	if len(wasmBytes) < 8 {
		return nil, fmt.Errorf("not a valid WASM binary (too short)")
	}
	// Check magic + version header.
	if !hasWasmHeader(wasmBytes) {
		return nil, fmt.Errorf("not a valid WASM binary (bad magic/version)")
	}
	offset := 8 // skip magic (4) + version (4)
	for offset < len(wasmBytes) {
		sectionID := wasmBytes[offset]
		offset++
		size, n := decodeULEB128(wasmBytes[offset:])
		if n <= 0 {
			return nil, fmt.Errorf("corrupt WASM: failed to decode section size at offset %d", offset)
		}
		offset += n
		// Protect against malformed input declaring impossibly large sections.
		if int(size) > len(wasmBytes)-offset {
			return nil, fmt.Errorf("corrupt WASM at offset %d: section size %d overflows binary", offset, size)
		}
		if sectionID != 0 {
			// Not a custom section; skip.
			offset += int(size)
			continue
		}
		// Custom section: parse the name.
		sectionEnd := offset + int(size)
		nameLen, nn := decodeULEB128(wasmBytes[offset:])
		if nn <= 0 {
			return nil, fmt.Errorf("corrupt WASM at offset %d: failed to decode custom section name length", offset)
		}
		offset += nn
		if offset+int(nameLen) > sectionEnd {
			return nil, fmt.Errorf("corrupt WASM at offset %d: custom section name overflows section boundary", offset)
		}
		sectionName := string(wasmBytes[offset : offset+int(nameLen)])
		offset += int(nameLen)
		payloadLen := sectionEnd - offset
		if sectionName == name {
			payload := make([]byte, payloadLen)
			copy(payload, wasmBytes[offset:sectionEnd])
			return payload, nil
		}
		// Not the section we want; skip payload.
		offset = sectionEnd
	}
	return nil, fmt.Errorf("no cleat.metadata section found in WASM binary")
}

func writeCustomSection(wasmBytes []byte, name string, payload []byte) ([]byte, error) {
	// First, strip any existing section with this name.
	stripped, err := stripCustomSection(wasmBytes, name)
	if err != nil {
		// If there's no existing section, proceed with original bytes.
		stripped = wasmBytes
	}

	// Encode the custom section: section ID (0), then the section content.
	// Section content: name length (ULEB128) + name bytes + payload.
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

	return append(stripped, section...), nil
}

func stripCustomSection(wasmBytes []byte, name string) ([]byte, error) {
	if len(wasmBytes) < 8 {
		return nil, fmt.Errorf("not a valid WASM binary (too short)")
	}
	if !hasWasmHeader(wasmBytes) {
		return nil, fmt.Errorf("not a valid WASM binary (bad magic/version)")
	}

	var result []byte
	result = append(result, wasmBytes[0:8]...) // keep header
	offset := 8
	found := false
	for offset < len(wasmBytes) {
		sectionID := wasmBytes[offset]
		sectionStart := offset
		offset++
		size, n := decodeULEB128(wasmBytes[offset:])
		if n <= 0 {
			return nil, fmt.Errorf("corrupt WASM at offset %d: failed to decode section size", offset)
		}
		offset += n
		sectionLen := 1 + n + int(size) // ID byte + size-encoding + body
		if sectionStart+sectionLen > len(wasmBytes) {
			result = append(result, wasmBytes[sectionStart:]...)
			break
		}
		if sectionID != 0 {
			result = append(result, wasmBytes[sectionStart:sectionStart+sectionLen]...)
			offset = sectionStart + sectionLen
			continue
		}
		// Custom section — check its name.
		sectionEnd := sectionStart + sectionLen
		nameLen, nn := decodeULEB128(wasmBytes[offset:])
		if nn <= 0 {
			return nil, fmt.Errorf("corrupt WASM at offset %d: failed to decode custom section name", offset)
		}
		offset += nn
		if offset+int(nameLen) > sectionEnd {
			return nil, fmt.Errorf("corrupt WASM at offset %d: custom section name overflows section boundary", offset)
		}
		sectionName := string(wasmBytes[offset : offset+int(nameLen)])
		if sectionName == name {
			found = true
			offset = sectionEnd // skip this section entirely
		} else {
			result = append(result, wasmBytes[sectionStart:sectionEnd]...)
			offset = sectionEnd
		}
	}
	if !found {
		return nil, fmt.Errorf("section %q not found", name)
	}
	return result, nil
}

func hasWasmHeader(b []byte) bool {
	// WASM magic: 0x00 0x61 0x73 0x6d ("\0asm")
	// WASM version: 0x01 0x00 0x00 0x00
	if len(b) < 8 {
		return false
	}
	return b[0] == 0x00 && b[1] == 0x61 && b[2] == 0x73 && b[3] == 0x6d &&
		b[4] == 0x01 && b[5] == 0x00 && b[6] == 0x00 && b[7] == 0x00
}

// decodeULEB128 decodes an unsigned LEB128 value from b and returns the
// decoded value and the number of bytes consumed.
func decodeULEB128(b []byte) (uint32, int) {
	var result uint32
	var shift uint
	for i, bb := range b {
		result |= uint32(bb&0x7f) << shift
		if bb&0x80 == 0 {
			return result, i + 1
		}
		shift += 7
		if shift >= 35 {
			return 0, 0 // overflow
		}
	}
	return 0, 0 // truncated
}

// encodeULEB128 encodes v as unsigned LEB128.
func encodeULEB128(v uint32) []byte {
	// Use binary.PutUvarint with a pre-allocated buffer.
	var buf [binary.MaxVarintLen32]byte
	n := binary.PutUvarint(buf[:], uint64(v))
	return buf[:n]
}

// DetectLanguage attempts to determine the source language of a WASM binary.
// It first checks the "cleat.metadata" custom section for an explicit Language
// field. If absent, it scans the import section for Component Model import
// patterns (imports with module names starting with "cleat:"). If neither
// provides a result, "go" is returned as the default.
func DetectLanguage(wasmBytes []byte) string {
	// 1. Try cleat.metadata custom section.
	if meta, err := ReadMetadata(wasmBytes); err == nil && meta.Language != "" {
		return meta.Language
	}

	// 2. Check Component Model header (bytes 4-7 = 0x0d 0x00 0x01 0x00).
	if len(wasmBytes) > 7 &&
		wasmBytes[4] == 0x0d && wasmBytes[5] == 0x00 &&
		wasmBytes[6] == 0x01 && wasmBytes[7] == 0x00 {
		return "python"
	}

	// 3. Scan the import section for Component Model patterns.
	if hasComponentModelImports(wasmBytes) {
		return "python"
	}

	// 4. Scan import section for language-specific import patterns.
	if lang := detectLanguageFromImports(wasmBytes); lang != "" {
		return lang
	}

	// 5. Default to Go.
	return "go"
}

// HasWasiImports scans the WASM binary for wasi_snapshot_preview1 import module.
func HasWasiImports(wasmBytes []byte) bool {
	return strings.Contains(string(wasmBytes), "wasi_snapshot_preview1")
}

// HasImport checks whether a WASM binary imports a specific function
// from a specific module. This is used to detect features like the
// cleat_poll_work dispatch protocol.
func HasImport(wasmBytes []byte, module, name string) bool {
	return strings.Contains(string(wasmBytes), module) &&
		strings.Contains(string(wasmBytes), name)
}

// hasComponentModelImports scans the WASM import section for module names
// that contain "cleat:" — the prefix used by the Component Model toolchain
// (e.g., componentize-py).
func hasComponentModelImports(wasmBytes []byte) bool {
	imports, err := readImportModuleNames(wasmBytes)
	if err != nil {
		return false
	}
	for _, mod := range imports {
		if strings.Contains(mod, "cleat:") {
			return true
		}
	}
	return false
}

// detectLanguageFromImports scans the WASM import section for language-specific
// patterns that identify the source language when no metadata is present.
func detectLanguageFromImports(wasmBytes []byte) string {
	imports, err := readImportSection(wasmBytes)
	if err != nil {
		return ""
	}
	for _, imp := range imports {
		// TeaVM-compiled Java modules import from the "teavm" module.
		if imp.module == "teavm" {
			return "java"
		}
		// AssemblyScript modules import env.abort for runtime error handling.
		if imp.module == "env" && imp.field == "abort" {
			return "assemblyscript"
		}
	}
	return ""
}

// wasmImport represents a single WASM import entry.
type wasmImport struct {
	module string
	field  string
}

// readImportSection extracts all (module, field) pairs from the WASM import section.
func readImportSection(wasmBytes []byte) ([]wasmImport, error) {
	if len(wasmBytes) < 8 || !hasWasmHeader(wasmBytes) {
		return nil, fmt.Errorf("not a valid WASM binary")
	}

	var imports []wasmImport
	offset := 8

	for offset < len(wasmBytes) {
		sectionID := wasmBytes[offset]
		offset++
		size, n := decodeULEB128(wasmBytes[offset:])
		if n <= 0 {
			return nil, fmt.Errorf("corrupt WASM at offset %d", offset)
		}
		offset += n
		sectionEnd := offset + int(size)
		if int(size) > len(wasmBytes)-offset {
			return nil, fmt.Errorf("section size %d overflows", size)
		}

		if sectionID != 2 {
			offset = sectionEnd
			continue
		}

		count, nn := decodeULEB128(wasmBytes[offset:])
		if nn <= 0 {
			return nil, fmt.Errorf("failed to decode import count")
		}
		offset += nn

		for i := uint32(0); i < count; i++ {
			// Module name.
			nameLen, nn := decodeULEB128(wasmBytes[offset:])
			if nn <= 0 {
				return nil, fmt.Errorf("failed to decode module name len")
			}
			offset += nn
			if int(nameLen) > sectionEnd-offset {
				return nil, fmt.Errorf("corrupt WASM import %d: name overflows section", i)
			}
			moduleName := string(wasmBytes[offset : offset+int(nameLen)])
			offset += int(nameLen)

			// Field name.
			fieldLen, nn := decodeULEB128(wasmBytes[offset:])
			if nn <= 0 {
				return nil, fmt.Errorf("failed to decode field name len")
			}
			offset += nn
			if int(fieldLen) > sectionEnd-offset {
				return nil, fmt.Errorf("corrupt WASM import %d: field name overflows section", i)
			}
			fieldName := string(wasmBytes[offset : offset+int(fieldLen)])
			offset += int(fieldLen)

			// Skip kind byte.
			if offset < sectionEnd {
				offset++
			}

			imports = append(imports, wasmImport{module: moduleName, field: fieldName})
		}
		return imports, nil
	}
	return imports, nil
}

// readImportModuleNames extracts the module names from the import section
// (section ID 2) of a WASM binary. It returns only the module names, skipping
// the full descriptor parsing of each import.
func readImportModuleNames(wasmBytes []byte) ([]string, error) {
	if len(wasmBytes) < 8 {
		return nil, fmt.Errorf("not a valid WASM binary (too short)")
	}
	if !hasWasmHeader(wasmBytes) {
		return nil, fmt.Errorf("not a valid WASM binary (bad magic/version)")
	}

	var modules []string
	offset := 8 // skip magic (4) + version (4)

	for offset < len(wasmBytes) {
		sectionID := wasmBytes[offset]
		offset++
		size, n := decodeULEB128(wasmBytes[offset:])
		if n <= 0 {
			return nil, fmt.Errorf("corrupt WASM at offset %d: failed to decode section size", offset)
		}
		offset += n
		sectionEnd := offset + int(size)
		if int(size) > len(wasmBytes)-offset {
			return nil, fmt.Errorf("corrupt WASM at offset %d: section size %d overflows binary", offset, size)
		}

		if sectionID != 2 {
			// Not the import section; skip.
			offset = sectionEnd
			continue
		}

		// Import section.
		count, nn := decodeULEB128(wasmBytes[offset:])
		if nn <= 0 {
			return nil, fmt.Errorf("corrupt WASM import section: failed to decode count")
		}
		offset += nn

		for i := uint32(0); i < count; i++ {
			// Module name.
			nameLen, nn := decodeULEB128(wasmBytes[offset:])
			if nn <= 0 {
				return nil, fmt.Errorf("corrupt WASM import %d: failed to decode name length", i)
			}
			offset += nn
			if offset+int(nameLen) > sectionEnd {
				return nil, fmt.Errorf("corrupt WASM import %d: name overflows section", i)
			}
			moduleName := string(wasmBytes[offset : offset+int(nameLen)])
			offset += int(nameLen)
			modules = append(modules, moduleName)

			// Import name (field).
			fieldLen, nn := decodeULEB128(wasmBytes[offset:])
			if nn <= 0 {
				return nil, fmt.Errorf("corrupt WASM import %d: failed to decode field name length", i)
			}
			offset += nn
			if offset+int(fieldLen) > sectionEnd {
				return nil, fmt.Errorf("corrupt WASM import %d: field name overflows section", i)
			}
			offset += int(fieldLen)

			// Import kind and descriptor.
			if offset >= sectionEnd {
				return nil, fmt.Errorf("corrupt WASM import %d: truncated at kind byte", i)
			}
			kind := wasmBytes[offset]
			offset++

			switch kind {
			case 0: // func
				if _, nn := decodeULEB128(wasmBytes[offset:]); nn <= 0 {
					return nil, fmt.Errorf("corrupt WASM import %d: bad func type index", i)
				}
				offset += nn
			case 1: // table
				offset++ // elem type byte
				flags, nn := decodeULEB128(wasmBytes[offset:])
				if nn <= 0 {
					return nil, fmt.Errorf("corrupt WASM import %d: bad table limits", i)
				}
				offset += nn
				if flags&0x01 != 0 { // has max
					if _, nn := decodeULEB128(wasmBytes[offset:]); nn <= 0 {
						return nil, fmt.Errorf("corrupt WASM import %d: bad table max", i)
					}
					offset += nn
				}
			case 2: // memory
				flags, nn := decodeULEB128(wasmBytes[offset:])
				if nn <= 0 {
					return nil, fmt.Errorf("corrupt WASM import %d: bad memory limits", i)
				}
				offset += nn
				// skip min
				if _, nn := decodeULEB128(wasmBytes[offset:]); nn <= 0 {
					return nil, fmt.Errorf("corrupt WASM import %d: bad memory min", i)
				}
				offset += nn
				if flags&0x01 != 0 { // has max
					if _, nn := decodeULEB128(wasmBytes[offset:]); nn <= 0 {
						return nil, fmt.Errorf("corrupt WASM import %d: bad memory max", i)
					}
					offset += nn
				}
			case 3: // global
				if offset+2 > sectionEnd {
					return nil, fmt.Errorf("corrupt WASM import %d: truncated global import", i)
				}
				offset += 2 // content type + mutability
			default:
				return nil, fmt.Errorf("corrupt WASM import %d: unknown kind %d", i, kind)
			}
		}
		return modules, nil
	}
	return nil, fmt.Errorf("no import section found in WASM binary")
}
