package main

import (
	"fmt"
	"strings"
)

// verifyEntryPointsAreExports fails the build if any name in entryPoints is
// not actually a function export of the .wasm the compiler just produced.
//
// It is the second half of the guarantee every SDK's build path now relies
// on (cleat#2113 for Rust, cleat#2145 for AssemblyScript and Java): the
// FIRST half is that entryPoints itself comes from the SDK's own codegen --
// a linker-merged WASM section for Rust (wasm.ReadEntryPointsSection), a
// sidecar manifest the AS transform or the Java annotation processor wrote
// during compilation for the other two -- not a source-level guess assembled
// separately. That closes the "the extractor's prediction missed a real
// entry" gap this repo used to have (cleat#2109); it says nothing about
// whether the compiler's OWN linker/dead-code-elimination pass then dropped
// or renamed what codegen asked for. This check is what closes that second,
// narrower gap: a name that came from the SDK's own accounting but the
// compiled binary does not actually export fails HERE, loudly, rather than
// surfacing later as a live "cannot determine entry point" with no path back
// to the build that caused it.
//
// Deliberately one-directional: it does not require every wasm export to be
// a declared entry point (a build may export other things -- allocator
// hooks, helper functions marked pub -- that were never meant to be entry
// points), only that every declared entry point is a real export.
func verifyEntryPointsAreExports(sdk string, wasmBytes []byte, entryPoints []string) error {
	if len(entryPoints) == 0 {
		return nil
	}
	exports := wasmFuncExportNames(wasmBytes)
	var missing []string
	for _, name := range entryPoints {
		if !exports[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%s build: cleat.metadata would declare %d entry point(s) not actually exported "+
		"by the compiled .wasm: %s -- the SDK's own codegen named an entry the compiler did not produce "+
		"an export for. This is refused rather than deployed with metadata that lies about the binary's "+
		"own exports; see build_entry_points.go's doc comment for why this check exists",
		sdk, len(missing), strings.Join(missing, ", "))
}

// wasmFuncExportNames returns the names of every function export (export
// kind 0) in a compiled WASM binary's export section. Same binary-format
// parse cmd/cleat-worker/setup.go's firstHandleExport already uses in
// production (magic+version header, then walk sections for id 7), just
// collecting every func export instead of the first "handle_"-prefixed one.
func wasmFuncExportNames(wasmBytes []byte) map[string]bool {
	names := map[string]bool{}
	if len(wasmBytes) < 8 {
		return names
	}
	pos := 8 // skip magic + version
	for pos < len(wasmBytes) {
		sectionID := wasmBytes[pos]
		pos++
		sectionLen, n := decodeULEB128AtOffset(wasmBytes, pos)
		pos = n
		sectionEnd := pos + int(sectionLen)
		if sectionID != 7 { // not the export section
			pos = sectionEnd
			continue
		}
		count, n := decodeULEB128AtOffset(wasmBytes, pos)
		pos = n
		for i := uint32(0); i < count; i++ {
			nameLen, n := decodeULEB128AtOffset(wasmBytes, pos)
			pos = n
			if pos+int(nameLen) > len(wasmBytes) {
				return names // malformed; report what was parsed so far
			}
			name := string(wasmBytes[pos : pos+int(nameLen)])
			pos += int(nameLen)
			if pos >= len(wasmBytes) {
				return names
			}
			kind := wasmBytes[pos]
			pos++
			_, n = decodeULEB128AtOffset(wasmBytes, pos) // index
			pos = n
			if kind == 0 {
				names[name] = true
			}
		}
		return names
	}
	return names
}

// decodeULEB128AtOffset reads an unsigned LEB128 value from buf at offset
// pos and returns the value and the new offset.
func decodeULEB128AtOffset(buf []byte, pos int) (uint32, int) {
	var result uint32
	var shift uint
	for pos < len(buf) {
		b := buf[pos]
		pos++
		result |= uint32(b&0x7F) << shift
		if b&0x80 == 0 {
			break
		}
		shift += 7
	}
	return result, pos
}
