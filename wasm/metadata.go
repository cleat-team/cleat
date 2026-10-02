// Package wasm — WASM binary metadata (custom section "cleat.metadata").
//
// Metadata is embedded as a JSON payload in a WASM custom section so that
// cleat deploy can extract workflow name, version, ABI info, and plugin
// dependencies without separate configuration.

package wasm

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
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
	ChildBindingPolicy   string            `json:"child_binding_policy,omitempty"` // deployment channel / binding policy
	Language             string            `json:"language,omitempty"`

	// EntryPoints names the WASM exports a caller may start this workflow at,
	// in source declaration order -- nothing about their parameters, types or
	// signature. cleat#2066: none of this repo's SDKs ever export a
	// "handle_"-prefixed function by convention, so a worker guessing from
	// export names alone cannot tell a workflow's entry point from a helper,
	// and cannot disambiguate a binary with more than one. Codegen already
	// computes this list to generate the exports in the first place; this
	// carries it to the host instead of discarding it.
	//
	// See wasm/metadata_carries_no_entry_point_parameters_test.go for why this
	// field is allowed to exist at all: it is named and structurally
	// constrained ([]string, names only) so it cannot become the parameter
	// list cleat#1065/#1705 documented the host as unable to validate against.
	EntryPoints []string `json:"entry_points,omitempty"`
}

// EffectivePolicy returns the effective child binding policy after applying
// defaults:
//   - "" (empty)    — backwards compatible: if ChildVersions populated -> "frozen", else -> "latest"
//   - "frozen"      — strictly use pinned ChildVersions, never resolve at runtime
//   - "stable"      — resolve to version with "stable" tag at child creation time
//   - "latest"      — always resolve to MAX(version) at runtime
//   - "tag:X"       — resolve to version with tag X (e.g. "tag:canary", "tag:experiment-b")
func (m *Metadata) EffectivePolicy() string {
	if m.ChildBindingPolicy != "" {
		return m.ChildBindingPolicy
	}
	if len(m.ChildVersions) > 0 {
		return "frozen"
	}
	return "latest"
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

// entryPointsSectionName is the WASM custom section an SDK's own compiled
// output carries its entry-point names in -- cleat#2113. Unlike
// "cleat.metadata" (sectionName, above), which cleat build writes itself
// after compilation from a source-level prediction, this section is written
// by the SDK's own build process (the Rust #[cleat_entry] proc macro, at
// present -- see crates/cleat-macro/src/entry.rs). Its presence is therefore
// proof the compiler itself saw each declared entry point, not a guess at
// what it would produce.
const entryPointsSectionName = "cleat_entry_points"

// ErrEntryPointsSectionMissing is returned by ReadEntryPointsSection when a
// WASM binary carries no "cleat_entry_points" custom section -- an SDK
// version built before it started emitting one. Callers should fail the
// build rather than fall back to a source-level guess: a name a regex
// predicts and the compiler never actually saw is exactly the silent-miss
// risk this section exists to remove (cleat#2113).
var ErrEntryPointsSectionMissing = errors.New("no cleat_entry_points section found in WASM binary")

// ReadEntryPointsSection extracts and parses the "cleat_entry_points" custom
// section: one export name per line, in the order the linker assembled them
// from however many #[cleat_entry]-style expansions contributed one. Returns
// ErrEntryPointsSectionMissing if the section is absent, and an error naming
// every name that appears more than once if any does.
func ReadEntryPointsSection(wasmBytes []byte) ([]string, error) {
	payload, err := readCustomSection(wasmBytes, entryPointsSectionName)
	if err != nil {
		return nil, ErrEntryPointsSectionMissing
	}
	var names []string
	seen := make(map[string]bool)
	var dups []string
	dupSeen := make(map[string]bool)
	for _, line := range strings.Split(string(payload), "\n") {
		if line == "" {
			continue
		}
		if seen[line] {
			if !dupSeen[line] {
				dups = append(dups, line)
				dupSeen[line] = true
			}
			continue
		}
		seen[line] = true
		names = append(names, line)
	}
	if len(dups) > 0 {
		return nil, fmt.Errorf("cleat_entry_points: duplicate entry point name(s): %s", strings.Join(dups, ", "))
	}
	return names, nil
}

// WriteEntryPointsSection embeds names as the "cleat_entry_points" custom
// section, replacing any existing one -- the same section
// ReadEntryPointsSection reads back, and in the same format: one name per
// line. cleat#2145: AssemblyScript and Java have no linker-level mechanism
// equivalent to Rust's `#[link_section]` statics (crates/cleat-macro), so
// their `cleat build` paths call this to embed, post-compilation, the list
// their own codegen (the AS transform's AST walk, the Java annotation
// processor) already computed at compile time -- not a source-level guess
// assembled separately, but the same computation that decided what to
// export, written down where the Rust path gets it for free from the
// linker. Callers should immediately re-read the result with
// ReadEntryPointsSection: that gets duplicate-name detection for free,
// rather than duplicating it here, and confirms the round-trip.
func WriteEntryPointsSection(wasmBytes []byte, names []string) ([]byte, error) {
	var payload strings.Builder
	for _, n := range names {
		payload.WriteString(n)
		payload.WriteByte('\n')
	}
	return writeCustomSection(wasmBytes, entryPointsSectionName, []byte(payload.String()))
}

// ErrNotAJSONObject is returned by SetMetadataField when the cleat.metadata
// payload is valid JSON but not an object: the literal `null`, an array, a bare
// scalar. There are no keys to patch, and json.Unmarshal leaves its map target
// nil for `null`, so assigning into that nil map would panic rather than fail.
//
// It is a sentinel rather than a message because callers differ on what it
// means. A caller that can leave the section as it found it should treat it as
// "nothing to patch" rather than a failure -- a binary carrying such a payload
// is one the calling command did not produce.
var ErrNotAJSONObject = errors.New("cleat.metadata: not a JSON object")

// SetMetadataField returns wasmBytes with the cleat.metadata key set to
// rawValue, leaving every OTHER key exactly as the build wrote it.
//
// It exists because ReadMetadata/WriteMetadata round-trip through the Metadata
// struct, and that struct models the keys the engine reads -- while a build may
// write keys it does not. stamp_metadata.py writes sdk_language, sdk_version and
// created_at, and the Rust, Java and AssemblyScript builds inject sdk_version
// too. Rebuilding the payload from the struct DROPS every one of them, so a
// caller that only meant to change one field silently rewrites the whole
// section and loses provenance (cleat#2944).
//
// Key ORDER is not preserved, and values are preserved SEMANTICALLY rather than
// byte-for-byte: the payload is decoded into a map and re-encoded, so
// encoding/json sorts the keys and compacts the whitespace inside a value
// ([1, 2] becomes [1,2]). HTML escaping is switched off, so <, > and & inside a
// value are left as the build wrote them rather than becoming backslash-u
// escape sequences. Every key and every value survives; only their order and interior
// whitespace change. Nothing reads the section positionally, and the field this
// was written for is not order sensitive.
//
// A payload that is valid JSON but not an object (the literal null, an array, a
// bare string) is an error, not a panic: it has no keys to patch.
func SetMetadataField(wasmBytes []byte, key string, rawValue json.RawMessage) ([]byte, error) {
	payload, err := readCustomSection(wasmBytes, sectionName)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		if json.Valid(payload) {
			// Valid JSON that is not an object: an array, a string, a number, a
			// boolean. Unmarshal fails on the target TYPE rather than on the
			// syntax, and "not a JSON object" is exactly the condition the
			// sentinel names, so reporting it here is what keeps the documented
			// contract true rather than narrowing the doc to `null`.
			return nil, ErrNotAJSONObject
		}
		// The same message ReadMetadata gives for the same input, so a caller
		// cannot tell the two readers apart by their error.
		return nil, fmt.Errorf("cleat.metadata: invalid JSON: %w", err)
	}
	if fields == nil {
		// Unmarshal leaves the map nil for a JSON `null` -- valid JSON, and the
		// only non-object shape that decodes cleanly enough to reach here rather
		// than the branch above.
		return nil, ErrNotAJSONObject
	}
	fields[key] = rawValue

	// An Encoder rather than json.Marshal, with HTML escaping off, so a value the
	// build wrote is not rewritten. Marshal escapes <, > and & inside a
	// RawMessage; the compaction below happens either way.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(fields); err != nil {
		return nil, fmt.Errorf("cleat.metadata: %w", err)
	}
	return writeCustomSection(wasmBytes, sectionName, bytes.TrimRight(buf.Bytes(), "\n"))
}

// --- low-level WASM custom section helpers ---

func readCustomSection(wasmBytes []byte, name string) ([]byte, error) {
	if len(wasmBytes) < 8 {
		return nil, fmt.Errorf("not a valid WASM binary (too short)")
	}
	// Check magic + version header. A Component Model binary's top-level
	// sections use the identical id/size/content framing a core module's do
	// for a custom section (id 0) -- verified against a real componentize-py
	// artifact, cleat#2936 -- so this walk is safe on either header. It is
	// NOT safe to widen readImportSection/readImportModuleNames the same way:
	// their section ID 2 means "import" only in a core module; at a
	// component's top level it means "core instance", a different section
	// entirely.
	if !hasWasmHeader(wasmBytes) && !hasComponentHeader(wasmBytes) {
		return nil, fmt.Errorf("not a valid WASM binary (bad magic/version)")
	}
	offset := 8 // skip magic (4) + version/layer (4)
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
	return nil, fmt.Errorf("no %s section found in WASM binary", name)
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
	// See readCustomSection's comment: this walk only ever inspects custom
	// sections (id 0), which both headers frame identically.
	if !hasWasmHeader(wasmBytes) && !hasComponentHeader(wasmBytes) {
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

// hasComponentHeader reports whether b opens with a WASM Component Model
// binary header -- cleat#2936. It shares hasWasmHeader's 4-byte magic, but
// what follows is not the same field read two ways: the Component Model
// spec splits the core module's single u32 LE version into a u16 LE version
// (bytes 4-5) and a u16 LE "layer" (bytes 6-7), fixed at 1 for a component
// and -- because a core module's version 1 fits entirely in the low u16 --
// always 0 for a core module. That is why hasWasmHeader's bytes 6-7 check
// reads 0x00 0x00: it is reading a core module's zero layer, not asserting
// anything about a byte the spec calls unused. See
// https://github.com/WebAssembly/component-model/blob/main/design/mvp/Binary.md.
//
// The version field itself is deliberately NOT constrained to one value here:
// it read 13 (0x0d 0x00) in a real componentize-py artifact
// (tests/plugin-harness/testdata/pythonworkflow/call_all_plugins.wasm), and
// the spec does not promise it will not move again.
func hasComponentHeader(b []byte) bool {
	if len(b) < 8 {
		return false
	}
	return b[0] == 0x00 && b[1] == 0x61 && b[2] == 0x73 && b[3] == 0x6d &&
		b[6] == 0x01 && b[7] == 0x00
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

	// 2. Check Component Model header. This used to hardcode the exact
	// version bytes one observed componentize-py artifact happened to carry
	// (0x0d 0x00, i.e. version 13) alongside the layer field, which is the
	// only part the spec actually fixes -- see hasComponentHeader's comment
	// (cleat#2936). A future componentize-py bumping its version would have
	// silently stopped matching here.
	if hasComponentHeader(wasmBytes) {
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

// NeededEnvImports parses the WASM import section and returns the set of
// function names imported from the "env" module. Used to skip registration
// of host functions the module doesn't need.
func NeededEnvImports(wasmBytes []byte) map[string]bool {
	imports, err := readImportSection(wasmBytes)
	if err != nil {
		return nil // nil means "register everything" (conservative fallback)
	}
	needed := make(map[string]bool)
	for _, imp := range imports {
		if imp.module == "env" {
			needed[imp.field] = true
		}
	}
	return needed
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

	// Rust cdylib modules embed /rustc/ paths from the standard library.
	// Detect these before import-based heuristics so Rust modules fall
	// through to the default runtime instead of crashing in wasmtime.
	if strings.Contains(string(wasmBytes), "/rustc/") {
		return "rust"
	}

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
// skipImportDesc advances past one import descriptor, which is a kind byte
// followed by a kind-specific payload, and returns the new offset.
//
// The four kinds are defined by the WebAssembly core spec (importdesc):
//
//	0x00 func    typeidx                        -- one LEB128
//	0x01 table   reftype + limits               -- one byte, then limits
//	0x02 mem     limits                         -- limits
//	0x03 global  valtype + mut                  -- two bytes
//
// limits is a flags byte, a minimum, and a maximum when flags has bit 0 set.
//
// Only 0x00 occurs in practice for the workflows cleat builds -- Go, Rust,
// AssemblyScript and TeaVM guests all import functions only -- but the other
// three are handled rather than assumed away, since guessing wrong here
// desynchronises the whole section rather than losing one entry.
func skipImportDesc(b []byte, offset, sectionEnd int) (int, error) {
	if offset >= sectionEnd {
		return 0, fmt.Errorf("descriptor truncated: no kind byte")
	}
	kind := b[offset]
	offset++

	readULEB := func(what string) error {
		v, n := decodeULEB128(b[offset:])
		_ = v
		if n <= 0 || offset+n > sectionEnd {
			return fmt.Errorf("descriptor truncated: %s", what)
		}
		offset += n
		return nil
	}
	skipLimits := func() error {
		if offset >= sectionEnd {
			return fmt.Errorf("descriptor truncated: limits flags")
		}
		flags := b[offset]
		offset++
		if err := readULEB("limits minimum"); err != nil {
			return err
		}
		if flags&0x01 != 0 {
			if err := readULEB("limits maximum"); err != nil {
				return err
			}
		}
		return nil
	}

	switch kind {
	case 0x00: // func: typeidx
		if err := readULEB("func typeidx"); err != nil {
			return 0, err
		}
	case 0x01: // table: reftype then limits
		if offset >= sectionEnd {
			return 0, fmt.Errorf("descriptor truncated: table reftype")
		}
		offset++
		if err := skipLimits(); err != nil {
			return 0, err
		}
	case 0x02: // mem: limits
		if err := skipLimits(); err != nil {
			return 0, err
		}
	case 0x03: // global: valtype then mutability
		if offset+2 > sectionEnd {
			return 0, fmt.Errorf("descriptor truncated: global valtype/mut")
		}
		offset += 2
	default:
		return 0, fmt.Errorf("unknown import kind 0x%02x", kind)
	}
	return offset, nil
}

// readImportSection reads core-module section ID 2 as "import" -- true only
// for a core module. At a Component Model binary's top level, section ID 2
// means "core instance" instead, so this deliberately stays on hasWasmHeader
// alone rather than also accepting hasComponentHeader (cleat#2936): widening
// it would silently parse a component's core-instance bytes as import
// module/field name pairs. Callers already treat a rejection here as "cannot
// tell" (see the comment on skipImportDesc below), so a component binary
// falls through safely without this function understanding it.
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

			// Skip the import descriptor: a kind byte followed by a
			// kind-specific payload.
			//
			// This used to advance by one byte -- the kind -- and stop. The
			// payload was left unread, so the next iteration started on it and
			// read a type index as the following import's module-name length.
			// The parser desynchronised after the *first* import and every
			// module with two or more imports returned "corrupt WASM import N".
			//
			// Nothing reported it, because both callers treat an error as
			// "cannot tell": NeededEnvImports falls back to registering every
			// host function, and detectLanguageFromImports returns "", which
			// DetectLanguage turns into its "go" default. So the AssemblyScript
			// and TeaVM branches below it had never once been reached.
			var derr error
			if offset, derr = skipImportDesc(wasmBytes, offset, sectionEnd); derr != nil {
				return nil, fmt.Errorf("corrupt WASM import %d: %w", i, derr)
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
//
// Deliberately core-module-only -- see readImportSection's comment above;
// the same section-ID-2 ambiguity applies here (cleat#2936).
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
