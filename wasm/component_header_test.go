package wasm

import (
	"os"
	"testing"
)

// ---------------------------------------------------------------------------
// hasComponentHeader -- cleat#2936
// ---------------------------------------------------------------------------

func TestHasComponentHeaderValid(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{
			// The exact header of the real componentize-py artifact used
			// below: version 13, layer 1.
			name: "real componentize-py header (version 13)",
			data: []byte{0x00, 0x61, 0x73, 0x6d, 0x0d, 0x00, 0x01, 0x00},
		},
		{
			// The version field is deliberately unconstrained: only the
			// layer matters.
			name: "a different version, same layer",
			data: []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x01, 0x00},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !hasComponentHeader(tt.data) {
				t.Error("expected a valid component header to be recognized")
			}
		})
	}
}

func TestHasComponentHeaderInvalid(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"too short (7 bytes)", []byte{0x00, 0x61, 0x73, 0x6d, 0x0d, 0x00, 0x01}},
		{"a real core module header (layer 0)", []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}},
		{"bad magic", []byte{0x00, 0x00, 0x00, 0x00, 0x0d, 0x00, 0x01, 0x00}},
		{"layer 2 (not a layer this code recognizes)", []byte{0x00, 0x61, 0x73, 0x6d, 0x0d, 0x00, 0x02, 0x00}},
		{"all zeros", make([]byte, 8)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if hasComponentHeader(tt.data) {
				t.Error("expected invalid component header to be rejected")
			}
		})
	}
}

// hasWasmHeader and hasComponentHeader must be mutually exclusive: nothing
// should ever match both, or a caller `OR`-ing them (readCustomSection,
// stripCustomSection) could silently accept a binary that is neither.
func TestWasmHeaderAndComponentHeaderAreMutuallyExclusive(t *testing.T) {
	cases := [][]byte{
		{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}, // core module
		{0x00, 0x61, 0x73, 0x6d, 0x0d, 0x00, 0x01, 0x00}, // component
		{0x00, 0x61, 0x73, 0x6d, 0x00, 0x00, 0x00, 0x00}, // neither
	}
	for _, b := range cases {
		if hasWasmHeader(b) && hasComponentHeader(b) {
			t.Errorf("%x matched both headers", b)
		}
	}
}

// ---------------------------------------------------------------------------
// Metadata round-trip against a REAL Component Model binary -- cleat#2936
// ---------------------------------------------------------------------------

// componentArtifact is the real, checked-in componentize-py output used
// throughout this fix's investigation: 19,300,914 bytes, header
// 00 61 73 6d 0d 00 01 00 (version 13, layer 1), 642 top-level sections,
// landing exactly at EOF when walked with the same id+ULEB128-size+content
// framing a core module uses. See wasm/metadata.go's hasComponentHeader
// doc comment.
const componentArtifact = "../tests/plugin-harness/testdata/pythonworkflow/call_all_plugins.wasm"

// TestReadWriteMetadataRoundTripsOnARealComponentBinary is the regression
// test for cleat#2936: before this fix, hasWasmHeader rejected every
// Component Model binary outright ("not a valid WASM binary (bad
// magic/version)"), so ReadMetadata/WriteMetadata never worked on Python's
// actual build output -- only on a synthetic core-module-shaped fixture no
// real Python deployment produces. This uses the real artifact rather than a
// hand-built one so the fix is proven against the format the fix is for.
func TestReadWriteMetadataRoundTripsOnARealComponentBinary(t *testing.T) {
	original, err := os.ReadFile(componentArtifact)
	if err != nil {
		t.Fatalf("fixture %s is missing; it is checked in and this test "+
			"is meaningless without it: %v", componentArtifact, err)
	}
	if !hasComponentHeader(original) {
		t.Fatalf("fixture %s does not have the component header this test exists to exercise; "+
			"has the artifact changed shape?", componentArtifact)
	}
	if hasWasmHeader(original) {
		t.Fatalf("fixture %s unexpectedly also matches the core-module header", componentArtifact)
	}

	want := &Metadata{
		WorkflowName:         "CallAllPlugins",
		WorkflowVersion:      1,
		ABIVersion:           1,
		MinCompatibleVersion: 1,
		Language:             "python",
		EntryPoints:          []string{"call_all_plugins"},
	}
	stamped, err := WriteMetadata(original, want)
	if err != nil {
		t.Fatalf("WriteMetadata on a real component binary: %v", err)
	}

	got, err := ReadMetadata(stamped)
	if err != nil {
		t.Fatalf("ReadMetadata on a real stamped component binary: %v", err)
	}
	if got.WorkflowName != want.WorkflowName || got.WorkflowVersion != want.WorkflowVersion ||
		len(got.EntryPoints) != 1 || got.EntryPoints[0] != "call_all_plugins" {
		t.Errorf("round trip mismatch: got %+v, want %+v", got, want)
	}

	// determineEntryPoint's actual motivating use case (cleat#2914/#2936):
	// a worker resolving a Python workflow's entry point from the stamped
	// EntryPoints field, with no __entry_point in the start request.
	if len(got.EntryPoints) != 1 {
		t.Fatalf("expected exactly one entry point to resolve unambiguously, got %v", got.EntryPoints)
	}

	if lang := DetectLanguage(stamped); lang != "python" {
		t.Errorf("DetectLanguage(stamped component binary) = %q, want %q", lang, "python")
	}
}

// TestReadMetadataStillRejectsGenuinelyInvalidHeaders is the negative control
// for the fix above: widening readCustomSection/stripCustomSection to accept
// hasComponentHeader must not make them accept everything.
func TestReadMetadataStillRejectsGenuinelyInvalidHeaders(t *testing.T) {
	original, err := os.ReadFile(componentArtifact)
	if err != nil {
		t.Fatalf("fixture %s is missing: %v", componentArtifact, err)
	}
	stamped, err := WriteMetadata(original, &Metadata{WorkflowName: "x", WorkflowVersion: 1, ABIVersion: 1, MinCompatibleVersion: 1})
	if err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}

	badMagic := append([]byte(nil), stamped...)
	badMagic[0] = 0xff
	if _, err := ReadMetadata(badMagic); err == nil {
		t.Error("ReadMetadata accepted a binary with corrupted magic")
	}

	badLayer := append([]byte(nil), stamped...)
	badLayer[6] = 0x02 // neither core-module layer (0) nor component layer (1)
	if _, err := ReadMetadata(badLayer); err == nil {
		t.Error("ReadMetadata accepted a binary with an unrecognized layer field")
	}
}

// TestImportSectionReadersStayCoreModuleOnly documents and pins the
// deliberate scope boundary of cleat#2936: readImportSection and
// readImportModuleNames read section ID 2 as "import", which is true only
// for a core module. At a component's top level, section ID 2 means "core
// instance" -- a different section entirely, which the real fixture here
// does carry -- so widening these the same way readCustomSection was widened
// would silently misparse a component's "core instance" bytes as import
// module/field name pairs rather than failing loudly.
//
// Asserting only err != nil is not enough here and was tried first: with the
// header check widened to also accept hasComponentHeader, readImportSection
// still returns an error on this fixture ("corrupt WASM import 0: field name
// overflows section") -- but that error comes from the downstream parse
// stumbling over the component's actual bytes, not from the header guard
// this test exists to pin. A component binary whose section-2 bytes happened
// to parse as syntactically valid (wrong but well-formed) imports would
// return no error at all under that same widening, and an err != nil
// assertion would not have noticed the difference. Asserting the exact
// header-rejection message is what actually distinguishes "the guard caught
// this" from "something downstream happened to".
func TestImportSectionReadersStayCoreModuleOnly(t *testing.T) {
	component, err := os.ReadFile(componentArtifact)
	if err != nil {
		t.Fatalf("fixture %s is missing: %v", componentArtifact, err)
	}
	if !hasComponentHeader(component) {
		t.Fatalf("fixture %s does not have the component header this test needs", componentArtifact)
	}

	if _, err := readImportSection(component); err == nil || err.Error() != "not a valid WASM binary" {
		t.Errorf("readImportSection(component) = %v, want the header-rejection error "+
			"\"not a valid WASM binary\" -- if this changed because the guard was "+
			"deliberately widened, verify it against the component's actual "+
			"section-ID semantics first (see this test's doc comment)", err)
	}
	if _, err := readImportModuleNames(component); err == nil || err.Error() != "not a valid WASM binary (bad magic/version)" {
		t.Errorf("readImportModuleNames(component) = %v, want the header-rejection error "+
			"\"not a valid WASM binary (bad magic/version)\" -- if this changed because "+
			"the guard was deliberately widened, verify it against the component's "+
			"actual section-ID semantics first (see this test's doc comment)", err)
	}
}
