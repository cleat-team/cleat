package wasm

import (
	"testing"
)

// buildTestModule assembles a minimal WASM module: two imported functions, then
// `bodies` locally-defined functions with the given payload lengths, and a name
// section naming them.
//
// The imports are NOT decoration. Function indices in the name section count
// imports first, so a size report that ignores them attributes every function
// to its neighbour two slots away -- a shift that produces a plausible report
// rather than an error. This module exists to make that shift fail.
func buildTestModule(t *testing.T, bodies []int, names map[uint32]string, withNames bool) []byte {
	t.Helper()
	var out []byte
	out = append(out, 0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00)

	section := func(id byte, payload []byte) {
		out = append(out, id)
		out = append(out, encodeULEB128(uint32(len(payload)))...)
		out = append(out, payload...)
	}
	name := func(s string) []byte {
		b := encodeULEB128(uint32(len(s)))
		return append(b, []byte(s)...)
	}

	// Import section: two functions, one memory. Only the functions occupy the
	// function index space; the memory is here so that skipping non-function
	// imports is exercised rather than assumed.
	var imports []byte
	imports = append(imports, encodeULEB128(3)...)
	for _, fn := range []string{"host_a", "host_b"} {
		imports = append(imports, name("env")...)
		imports = append(imports, name(fn)...)
		imports = append(imports, 0x00)                // kind: func
		imports = append(imports, encodeULEB128(0)...) // typeidx
	}
	imports = append(imports, name("env")...)
	imports = append(imports, name("memory")...)
	imports = append(imports, 0x02)                // kind: memory
	imports = append(imports, 0x00)                // limits: min only
	imports = append(imports, encodeULEB128(1)...) // min
	section(2, imports)

	// Code section.
	var code []byte
	code = append(code, encodeULEB128(uint32(len(bodies)))...)
	for _, n := range bodies {
		code = append(code, encodeULEB128(uint32(n))...)
		code = append(code, make([]byte, n)...)
	}
	section(10, code)

	if withNames {
		var nameMap []byte
		nameMap = append(nameMap, encodeULEB128(uint32(len(names)))...)
		// The name map must be emitted in ascending index order.
		for idx := uint32(0); idx < 100; idx++ {
			nm, ok := names[idx]
			if !ok {
				continue
			}
			nameMap = append(nameMap, encodeULEB128(idx)...)
			nameMap = append(nameMap, name(nm)...)
		}
		var sub []byte
		sub = append(sub, 0x01) // subsection 1: function names
		sub = append(sub, encodeULEB128(uint32(len(nameMap)))...)
		sub = append(sub, nameMap...)

		var custom []byte
		custom = append(custom, name("name")...)
		custom = append(custom, sub...)
		section(0, custom)
	}
	return out
}

// TestASizeReportIsMeasuredFromTheBinary is cleat#1314.
//
// The report it replaces multiplied the file's length by hardcoded constants,
// so every binary produced the same shape of answer. The property that
// distinguishes a measurement from a model is that DIFFERENT INPUTS PRODUCE
// DIFFERENT OUTPUTS, and it is asserted here with exact byte counts rather than
// with "the report mentions reflect", which the old implementation satisfied.
func TestASizeReportIsMeasuredFromTheBinary(t *testing.T) {
	// Indices 0 and 1 are the imports; the three local functions are 2, 3, 4.
	mod := buildTestModule(t, []int{100, 40, 10}, map[uint32]string{
		2: "runtime.mapassign",
		3: "encoding_json.Marshal",
		4: "runtime.newobject",
	}, true)

	br, err := AnalyzeSize(mod)
	if err != nil {
		t.Fatalf("AnalyzeSize: %v", err)
	}
	if !br.HaveNames {
		t.Fatal("HaveNames is false for a module carrying a name section")
	}

	// Each body costs its payload plus its own one-byte length prefix.
	want := map[string]int64{"runtime": 100 + 1 + 10 + 1, "encoding_json": 40 + 1}
	got := map[string]int64{}
	for _, p := range br.Packages {
		got[p.Package] = p.Size
	}
	for pkg, w := range want {
		if got[pkg] != w {
			t.Errorf("package %q measured %d bytes, want %d", pkg, got[pkg], w)
		}
	}
	if br.Unattributed != 0 {
		t.Errorf("Unattributed = %d, want 0: every function here has a name", br.Unattributed)
	}
	if br.Packages[0].Package != "runtime" {
		t.Errorf("packages are not sorted by size: first is %q", br.Packages[0].Package)
	}

	// The same module with one function's bytes moved to another package must
	// report differently. This is the assertion the old implementation could
	// not pass at any input.
	other := buildTestModule(t, []int{100, 40, 10}, map[uint32]string{
		2: "reflect.Value.Call",
		3: "encoding_json.Marshal",
		4: "runtime.newobject",
	}, true)
	br2, err := AnalyzeSize(other)
	if err != nil {
		t.Fatalf("AnalyzeSize: %v", err)
	}
	got2 := map[string]int64{}
	for _, p := range br2.Packages {
		got2[p.Package] = p.Size
	}
	if got2["runtime"] == got["runtime"] {
		t.Errorf("two modules with different contents reported the same size for "+
			"runtime (%d) -- the report is not reading the binary", got2["runtime"])
	}
	if got2["reflect"] != 101 {
		t.Errorf("reflect measured %d, want 101", got2["reflect"])
	}
}

// TestFunctionIndicesAccountForImports pins the off-by-imports shift, which is
// the failure this parser is most likely to have and the one that would look
// like a working report.
func TestFunctionIndicesAccountForImports(t *testing.T) {
	// If imports were ignored, index 0 would be read as the first local
	// function and everything would be attributed to the wrong names.
	mod := buildTestModule(t, []int{50, 20}, map[uint32]string{
		0: "wrong.ImportA", // an import's own name, present in real modules
		1: "wrong.ImportB",
		2: "right.First",
		3: "right.Second",
	}, true)
	br, err := AnalyzeSize(mod)
	if err != nil {
		t.Fatalf("AnalyzeSize: %v", err)
	}
	for _, p := range br.Packages {
		if p.Package == "wrong" {
			t.Errorf("attributed %d bytes to %q: function indices were read without "+
				"offsetting by the import count", p.Size, p.Package)
		}
	}
	var right int64
	for _, p := range br.Packages {
		if p.Package == "right" {
			right = p.Size
		}
	}
	if want := int64(50 + 1 + 20 + 1); right != want {
		t.Errorf("right measured %d bytes, want %d", right, want)
	}
}

// TestAStrippedBinaryReportsNoBreakdownRatherThanAModel is the honest-fallback
// half, and it is the case the replaced implementation got wrong everywhere.
//
// A binary with no name section yields no attribution. The report must say so.
// Substituting constants here -- the one path nobody exercises -- would put the
// defect back in the only place it would not be noticed.
func TestAStrippedBinaryReportsNoBreakdownRatherThanAModel(t *testing.T) {
	mod := buildTestModule(t, []int{100, 40}, nil, false)
	br, err := AnalyzeSize(mod)
	if err != nil {
		t.Fatalf("AnalyzeSize: %v", err)
	}
	if br.HaveNames {
		t.Fatal("HaveNames is true for a module with no name section")
	}
	if len(br.Packages) != 0 {
		t.Errorf("reported %d package(s) for a stripped binary: %v", len(br.Packages), br.Packages)
	}
	if want := int64(100 + 1 + 40 + 1); br.Unattributed != want {
		t.Errorf("Unattributed = %d, want %d -- the bytes still exist and must be "+
			"reported as unattributed rather than dropped", br.Unattributed, want)
	}
	if br.CodeSize == 0 {
		t.Error("CodeSize is 0: a stripped binary can still report what it does know")
	}
}

func TestPackageOf(t *testing.T) {
	for _, c := range []struct{ sym, want string }{
		{"runtime.mapaccess1", "runtime"},
		{"internal_abi.NoEscape", "internal_abi"},
		{"internal_abi.__Type_.Len", "internal_abi"},
		{"encoding_json_v2.Marshal", "encoding_json_v2"},
		{"github.com_cleat-team_cleat_engine.Run", "github"}, // mangled: documented, not guessed at
		// No package: compiler-generated families and bare symbols.
		{"go_buildid", ""},
		{"type_.eq.[3]string", ""},
		{"go_string.foo", ""},
		{"gcbits_1", ""},
		{"", ""},
	} {
		if got := packageOf(c.sym); got != c.want {
			t.Errorf("packageOf(%q) = %q, want %q", c.sym, got, c.want)
		}
	}
}
