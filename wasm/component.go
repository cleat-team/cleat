package wasm

import (
	"fmt"
)

// ComponentBundle represents a decomposed WASM Component Model binary into its
// constituent core WASM modules, the instance instantiation DAG, and the
// top-level exports that expose component entry points.
type ComponentBundle struct {
	// Modules are the raw core WASM binaries extracted from the component.
	Modules [][]byte

	// Instances describes how to instantiate each core module instance in the
	// component's instance DAG. The slice index IS the instance index.
	Instances []CoreInstance

	// Exports maps component-level export names (e.g. "handler") to the
	// specific instance and export that provides them.
	Exports map[string]ComponentExport

	// ImportModules lists the module names used in import sections across
	// all core modules (e.g. "env", "wasi_snapshot_preview1", "teavm").
	ImportModules []string
}

// CoreInstance describes how a single core module instance is created in the
// component's instance DAG.
type CoreInstance struct {
	// ModuleIndex is the index into ComponentBundle.Modules. -1 means this
	// instance has no module of its own (FromExports-only alias).
	ModuleIndex int

	// Args maps each import module name that the module's WASM import section
	// declares to the source instance that supplies it.
	Args []InstantiateArg

	// FromExports lists export aliases that re-export exports from other
	// instances under different names.
	FromExports []ExportSpec
}

// InstantiateArg maps an import module name (as used in the WASM import section)
// to the source instance that provides those imports.
type InstantiateArg struct {
	// Name is the import module name the WASM binary uses, e.g. "env",
	// "wasi_snapshot_preview1", or a cross-module name like "libpython3.14.so".
	Name string

	// InstanceIndex is the index of the source instance in
	// ComponentBundle.Instances.
	InstanceIndex int
}

// ExportSpec describes a single export alias that re-exports an export from
// another instance under a potentially different name.
type ExportSpec struct {
	// Name is the export name in THIS instance (the alias).
	Name string

	// Kind is the export kind: 0=func, 1=table, 2=memory, 3=global.
	Kind byte

	// Index is the position within this instance's module's exports
	// (unused for FromExports-only instances).
	Index int

	// SourceInstance is the index of the source instance.
	SourceInstance int

	// SourceName is the export name in the source instance.
	SourceName string
}

// ComponentExport maps a top-level component export to a specific instance export.
type ComponentExport struct {
	// Name is the component-level export name (e.g. "handler").
	Name string

	// Kind is the export kind: 0=func, 1=table, 2=memory, 3=global.
	Kind byte

	// InstanceIndex is the component instance index that provides this export.
	InstanceIndex int

	// ExportIndex is the index of the export within the instance's module.
	ExportIndex int
}

// Section IDs in component model binaries.
const (
	secCustom          byte = 0x00
	secCoreModule      byte = 0x01
	secCoreInstance    byte = 0x02
	secComponentImport byte = 0x0a
	secComponentExport byte = 0x0b
)

// Sort byte constants for component-level exports.
const (
	compSortFunc     byte = 0x01
	compSortType     byte = 0x03
	compSortInstance byte = 0x05
)

// Sort byte for "entire instance" in core instantiate args.
const instSortInstance byte = 0x12

// ParseComponentBundle parses a WASM Component Model binary and returns the
// decomposed ComponentBundle (core modules, instance DAG, and exports).
func ParseComponentBundle(wasmBytes []byte) (*ComponentBundle, error) {
	if len(wasmBytes) < 8 {
		return nil, fmt.Errorf("not a valid component binary (too short)")
	}

	// Check magic: \0asm
	if wasmBytes[0] != 0x00 || wasmBytes[1] != 0x61 || wasmBytes[2] != 0x73 || wasmBytes[3] != 0x6d {
		return nil, fmt.Errorf("not a valid WASM binary (bad magic)")
	}

	// Check component model layer: 0x0d 0x00 0x01 0x00
	if !hasComponentHeader(wasmBytes[4:8]) {
		return nil, fmt.Errorf("not a component model binary (bad layer)")
	}

	bundle := &ComponentBundle{
		Exports: make(map[string]ComponentExport),
	}

	offset := 8 // skip magic (4) + layer (4)
	for offset < len(wasmBytes) {
		sectionID := wasmBytes[offset]
		offset++
		size, n := decodeULEB128(wasmBytes[offset:])
		if n <= 0 {
			return nil, fmt.Errorf("corrupt component: failed to decode section size at offset %d", offset)
		}
		offset += n

		if int(size) > len(wasmBytes)-offset {
			return nil, fmt.Errorf("corrupt component at offset %d: section size %d overflows binary", offset, size)
		}

		payload := wasmBytes[offset : offset+int(size)]

		if err := parseSection(bundle, sectionID, payload); err != nil {
			return nil, fmt.Errorf("section %d at offset %d: %w", sectionID, offset, err)
		}

		offset += int(size)
	}

	return bundle, nil
}

func hasComponentHeader(layer []byte) bool {
	return len(layer) >= 4 && layer[0] == 0x0d && layer[1] == 0x00 && layer[2] == 0x01 && layer[3] == 0x00
}

func parseSection(bundle *ComponentBundle, sectionID byte, payload []byte) error {
	switch sectionID {
	case secCustom:
		return nil // skip custom sections
	case secCoreModule:
		return parseCoreModuleSection(bundle, payload)
	case secCoreInstance:
		return parseCoreInstanceSection(bundle, payload)
	case secComponentImport:
		return parseComponentImportSection(bundle, payload)
	case secComponentExport:
		return parseComponentExportSection(bundle, payload)
	default:
		return nil // skip unknown/unused sections
	}
}

// --- Core Module section (0x01) ---
//
// In the component model binary format, the section payload IS the raw
// core WASM module. The section size (from the outer section header)
// equals the module byte count — there is no separate size prefix inside
// the payload.
func parseCoreModuleSection(bundle *ComponentBundle, payload []byte) error {
	if len(payload) == 0 {
		return fmt.Errorf("empty core module section")
	}
	module := make([]byte, len(payload))
	copy(module, payload)
	bundle.Modules = append(bundle.Modules, module)
	return nil
}

// --- Core Instance section (0x02) ---

func parseCoreInstanceSection(bundle *ComponentBundle, payload []byte) error {
	pos := 0
	count, pos := readLEB128(payload, pos)

	for i := uint32(0); i < count; i++ {
		if pos >= len(payload) {
			return fmt.Errorf("truncated core instance %d", i)
		}
		disc := payload[pos]
		pos++

		switch disc {
		case 0x00: // Instantiate
			result, err := parseInstantiate(payload, pos)
			if err != nil {
				return fmt.Errorf("core instance %d (instantiate): %w", i, err)
			}
			bundle.Instances = append(bundle.Instances, result.CoreInstance)
			pos = result.endPos

		case 0x01: // FromExports
			result, err := parseFromExports(payload, pos)
			if err != nil {
				return fmt.Errorf("core instance %d (from-exports): %w", i, err)
			}
			bundle.Instances = append(bundle.Instances, result.CoreInstance)
			pos = result.endPos

		default:
			return fmt.Errorf("unknown core instance discriminator 0x%02x at index %d", disc, i)
		}
	}
	return nil
}

type parseResult struct {
	CoreInstance
	endPos int
}

// parseInstantiate parses a 0x00 instantiate entry.
func parseInstantiate(payload []byte, pos int) (parseResult, error) {
	modIdx, pos := readLEB128(payload, pos)
	argCount, pos := readLEB128(payload, pos)

	r := parseResult{
		CoreInstance: CoreInstance{
			ModuleIndex: int(modIdx),
		},
		endPos: pos,
	}

	for i := uint32(0); i < argCount; i++ {
		if r.endPos >= len(payload) {
			return r, fmt.Errorf("truncated args list at arg %d", i)
		}
		name, n := readCoreString(payload[r.endPos:])
		if n <= 0 {
			return r, fmt.Errorf("corrupt arg name at arg %d", i)
		}
		r.endPos += n

		if r.endPos >= len(payload) {
			return r, fmt.Errorf("truncated arg %d (sort byte missing)", i)
		}
		sortByte := payload[r.endPos]
		r.endPos++

		srcIdx, n2 := decodeULEB128(payload[r.endPos:])
		if n2 <= 0 {
			return r, fmt.Errorf("corrupt arg %d source instance index", i)
		}
		r.endPos += n2

		// Only track instance-kind args (sortByte == 0x12).
		// Other sorts (core func/table/mem/global) are internal exports.
		if sortByte == instSortInstance {
			r.Args = append(r.Args, InstantiateArg{
				Name:          name,
				InstanceIndex: int(srcIdx),
			})
		}
	}

	return r, nil
}

// parseFromExports parses a 0x01 from-exports entry.
func parseFromExports(payload []byte, pos int) (parseResult, error) {
	exportCount, pos := readLEB128(payload, pos)

	r := parseResult{
		CoreInstance: CoreInstance{
			ModuleIndex: -1,
		},
		endPos: pos,
	}

	for i := uint32(0); i < exportCount; i++ {
		if r.endPos >= len(payload) {
			return r, fmt.Errorf("truncated from-export at export %d", i)
		}
		name, n := readCoreString(payload[r.endPos:])
		if n <= 0 {
			return r, fmt.Errorf("corrupt export name at export %d", i)
		}
		r.endPos += n

		if r.endPos >= len(payload) {
			return r, fmt.Errorf("truncated export %d (sort byte missing)", i)
		}
		sortByte := payload[r.endPos]
		r.endPos++

		idx, n2 := decodeULEB128(payload[r.endPos:])
		if n2 <= 0 {
			return r, fmt.Errorf("corrupt export %d index", i)
		}
		r.endPos += n2

		r.FromExports = append(r.FromExports, ExportSpec{
			Name:  name,
			Kind:  sortByte,
			Index: int(idx),
		})
	}

	return r, nil
}

// --- Component Import section (0x0a) ---

func parseComponentImportSection(bundle *ComponentBundle, payload []byte) error {
	pos := 0
	count, pos := readLEB128(payload, pos)

	for i := uint32(0); i < count; i++ {
		name, n := readComponentString(payload, pos)
		if n <= 0 {
			return fmt.Errorf("corrupt component import %d name", i)
		}
		pos += n

		if pos >= len(payload) {
			return fmt.Errorf("truncated component import %d (sort byte)", i)
		}
		pos++ // skip sort byte

		if pos >= len(payload) {
			return fmt.Errorf("truncated component import %d (type index)", i)
		}
		_, n = decodeULEB128(payload[pos:])
		if n <= 0 {
			return fmt.Errorf("corrupt component import %d type index", i)
		}
		pos += n

		bundle.ImportModules = append(bundle.ImportModules, name)
	}

	return nil
}

// --- Component Export section (0x0b) ---

func parseComponentExportSection(bundle *ComponentBundle, payload []byte) error {
	pos := 0
	count, pos := readLEB128(payload, pos)

	for i := uint32(0); i < count; i++ {
		name, n := readComponentString(payload, pos)
		if n <= 0 {
			return fmt.Errorf("corrupt component export %d name", i)
		}
		pos += n

		if pos >= len(payload) {
			return fmt.Errorf("truncated component export %d (sort byte)", i)
		}
		sortByte := payload[pos]
		pos++

		idx, n := decodeULEB128(payload[pos:])
		if n <= 0 {
			return fmt.Errorf("corrupt component export %d index", i)
		}
		pos += n

		ce := ComponentExport{
			Name: name,
			Kind: sortByte,
		}

		switch sortByte {
		case compSortFunc:
			// For func exports, there's an optional type reference.
			// After the index: 0x00 = no type, 0x01 = type-sort + type-idx follows.
			if pos < len(payload) {
				hasType := payload[pos]
				pos++
				if hasType != 0 {
					// Skip type-sort byte and LEB128 type index.
					if pos < len(payload) {
						pos++ // type-sort
					}
					_, n = decodeULEB128(payload[pos:])
					if n > 0 {
						pos += n
					}
				}
			}
			ce.ExportIndex = int(idx)
			ce.InstanceIndex = -1

		case compSortType:
			ce.ExportIndex = int(idx)
			ce.InstanceIndex = -1

		case compSortInstance:
			ce.InstanceIndex = int(idx)
			ce.ExportIndex = -1

		default:
			ce.ExportIndex = int(idx)
			ce.InstanceIndex = -1
		}

		bundle.Exports[name] = ce
	}

	return nil
}

// --- String reading helpers ---

// readCoreString reads a core-level WASM string (LEB128 length prefix + UTF-8 bytes).
func readCoreString(buf []byte) (string, int) {
	length, n := decodeULEB128(buf)
	if n <= 0 {
		return "", 0
	}
	start := n
	end := start + int(length)
	if end > len(buf) {
		return "", 0
	}
	return string(buf[start:end]), n + int(length)
}

// readComponentString reads a component-level string (2-byte big-endian length + UTF-8 bytes).
func readComponentString(buf []byte, pos int) (string, int) {
	if pos+2 > len(buf) {
		return "", 0
	}
	length := int(buf[pos])<<8 | int(buf[pos+1])
	start := pos + 2
	end := start + length
	if end > len(buf) {
		return "", 0
	}
	return string(buf[start:end]), 2 + length
}

// --- LEB128 helpers ---

// readLEB128 is a convenience wrapper that decodes a LEB128 u32 from buf[pos:]
// and returns the value and the new position.
func readLEB128(buf []byte, pos int) (uint32, int) {
	val, n := decodeULEB128(buf[pos:])
	if n <= 0 {
		return 0, pos // caller must check via context
	}
	return val, pos + n
}

// PatchEmptyImportModuleName patches core WASM bytecode that has imports with
// empty ("") module names by replacing them with replacementName. This is needed
// because wazero rejects imports with empty module names, but component-model
// core modules (produced by componentize-py) use them for cross-module
// references that the component runtime resolves via the instance DAG.
//
// Returns the modified WASM bytes (a new allocation if changes were made, or
// the original slice if no empty module names were found).
func PatchEmptyImportModuleName(raw []byte, replacementName string) []byte {
	if len(raw) < 8 || string(raw[0:4]) != "\x00asm" {
		return raw
	}

	replaceBytes := []byte(replacementName)
	replaceLenLEB := encodeULEB128(uint32(len(replaceBytes)))

	// Parse sections.
	type section struct {
		id      byte
		payload []byte
	}
	var sections []section
	pos := 8 // skip magic + version
	for pos < len(raw) {
		id := raw[pos]
		pos++
		size, n := decodeULEB128(raw[pos:])
		if n <= 0 {
			return raw
		}
		pos += n
		if int(size) > len(raw)-pos {
			return raw
		}
		sections = append(sections, section{id: id, payload: raw[pos : pos+int(size)]})
		pos += int(size)
	}

	// Find the import section.
	importIdx := -1
	for i := range sections {
		if sections[i].id == 0x02 {
			importIdx = i
			break
		}
	}
	if importIdx < 0 {
		return raw
	}

	oldPayload := sections[importIdx].payload
	pos2 := 0
	importCount, n := decodeULEB128(oldPayload[pos2:])
	if n <= 0 {
		return raw
	}
	pos2 += n

	// Rebuild the import section payload.
	var newPayload []byte
	newPayload = append(newPayload, encodeULEB128(importCount)...)

	needsPatch := false
	for i := uint32(0); i < importCount; i++ {
		// Module name.
		modLen, n := decodeULEB128(oldPayload[pos2:])
		if n <= 0 {
			return raw
		}
		pos2 += n
		if modLen == 0 {
			needsPatch = true
			newPayload = append(newPayload, replaceLenLEB...)
			newPayload = append(newPayload, replaceBytes...)
		} else {
			newPayload = append(newPayload, encodeULEB128(modLen)...)
			newPayload = append(newPayload, oldPayload[pos2:pos2+int(modLen)]...)
		}
		pos2 += int(modLen)

		// Field name.
		fieldLen, n := decodeULEB128(oldPayload[pos2:])
		if n <= 0 {
			return raw
		}
		pos2 += n
		newPayload = append(newPayload, encodeULEB128(fieldLen)...)
		newPayload = append(newPayload, oldPayload[pos2:pos2+int(fieldLen)]...)
		pos2 += int(fieldLen)

		// Kind + kind-specific tail.
		kind := oldPayload[pos2]
		pos2++
		newPayload = append(newPayload, kind)

		switch kind {
		case 0x00: // func: type index (LEB128)
			ti, n := decodeULEB128(oldPayload[pos2:])
			if n <= 0 {
				return raw
			}
			pos2 += n
			newPayload = append(newPayload, encodeULEB128(ti)...)

		case 0x01: // table: reftype(1) + limits(flags:1, min:LEB128, [max:LEB128])
			newPayload = append(newPayload, oldPayload[pos2]) // reftype
			pos2++
			flags := oldPayload[pos2]
			pos2++
			newPayload = append(newPayload, flags)
			minVal, n := decodeULEB128(oldPayload[pos2:])
			if n <= 0 {
				return raw
			}
			pos2 += n
			newPayload = append(newPayload, encodeULEB128(minVal)...)
			if flags != 0 {
				maxVal, n := decodeULEB128(oldPayload[pos2:])
				if n <= 0 {
					return raw
				}
				pos2 += n
				newPayload = append(newPayload, encodeULEB128(maxVal)...)
			}

		case 0x02: // memory: limits(flags:1, min:LEB128, [max:LEB128])
			flags := oldPayload[pos2]
			pos2++
			newPayload = append(newPayload, flags)
			minVal, n := decodeULEB128(oldPayload[pos2:])
			if n <= 0 {
				return raw
			}
			pos2 += n
			newPayload = append(newPayload, encodeULEB128(minVal)...)
			if flags != 0 {
				maxVal, n := decodeULEB128(oldPayload[pos2:])
				if n <= 0 {
					return raw
				}
				pos2 += n
				newPayload = append(newPayload, encodeULEB128(maxVal)...)
			}

		case 0x03: // global: valtype(1) + mut(1)
			newPayload = append(newPayload, oldPayload[pos2], oldPayload[pos2+1])
			pos2 += 2
		}
	}

	if !needsPatch {
		return raw
	}

	// Rebuild the binary with the patched import section.
	sections[importIdx].payload = newPayload

	totalSize := 8 // magic + version
	for _, s := range sections {
		totalSize += 1
		totalSize += len(encodeULEB128(uint32(len(s.payload))))
		totalSize += len(s.payload)
	}

	newRaw := make([]byte, 0, totalSize)
	newRaw = append(newRaw, raw[0:8]...) // magic + version
	for _, s := range sections {
		newRaw = append(newRaw, s.id)
		newRaw = append(newRaw, encodeULEB128(uint32(len(s.payload)))...)
		newRaw = append(newRaw, s.payload...)
	}
	return newRaw
}
