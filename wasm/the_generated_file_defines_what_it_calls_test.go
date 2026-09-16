package wasm

import (
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/internal/analyzer"
)

// Whatever gen_wasm_exports.go CALLS, it must also DEFINE or IMPORT -- and
// whatever it does not call, it must not import. cleat#1697.
//
// WHY A TEST AND NOT A CAREFUL PREDICATE. The imports and the extractJSONRaw
// helper used to be gated on hasComplexParams(result): a second walk of the
// parameter types, predicting what the emitter downstream would do. The two
// drifted TWICE, in opposite directions, and each time the generated file
// compiled nowhere:
//
//	cleat#1036  ints moved onto json.Unmarshal; the predicate still called
//	            them simple            -> "undefined: json"
//	cleat#1065  strings moved onto extractJSONRaw; the predicate still called
//	            them simple            -> "undefined: extractJSONRaw"
//
// The gate is now derived from the emitted text, so the drift is structurally
// impossible rather than merely fixed. This test is the backstop for that, and
// it checks the property in BOTH directions, because the obvious repair to
// cleat#1697 -- calling a string "complex" -- swaps a missing import for an
// unused one, which is equally a compile error.
//
// WHY THE EXISTING SUITES MISSED IT, which is the part worth keeping.
// TestGenerateExportsSyntaxValid parses the generated file, and an undefined
// identifier is a TYPE error, not a syntax error -- so a file that can never
// compile parses perfectly. And the only fixtures anyone generated in anger
// had a complex parameter, which makes the gate true and hides the question.
// The breakage surfaced in cleat-ports, a different repository, as a
// merge-queue ejection.
func TestTheGeneratedFileDefinesWhatItCalls(t *testing.T) {
	entries, err := os.ReadDir("../testdata")
	if err != nil {
		t.Fatalf("listing ../testdata: %v", err)
	}

	checked, stringOnly := 0, 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pkg := e.Name()
		fset := token.NewFileSet()
		result, lerr := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/"+pkg, fset)
		if lerr != nil || result == nil || result.TargetPkg == nil {
			// testdata/errors and testdata/vet-checks hold deliberately
			// broken packages; they are not this test's subject.
			continue
		}
		if len(result.EntryPoints) == 0 {
			continue
		}
		checked++

		code := string(GenerateExports(pkg, result, "go"))
		callsRaw := strings.Contains(code, "extractJSONRaw(")
		definesRaw := strings.Contains(code, "func extractJSONRaw(")
		callsJSON := strings.Contains(code, "json.Unmarshal(")
		importsJSON := strings.Contains(code, `"encoding/json"`)

		// Track the shape that cleat#1697 broke, so this test reports when it
		// stops covering it rather than passing over an empty population.
		if callsRaw && !hasAnyComplexParam(result, pkg) {
			stringOnly++
		}

		if callsRaw && !definesRaw {
			t.Errorf("%s: calls extractJSONRaw and does not define it -- "+
				"\"undefined: extractJSONRaw\" (cleat#1697)", pkg)
		}
		if definesRaw && !callsRaw {
			t.Errorf("%s: defines extractJSONRaw and never calls it -- "+
				"\"declared and not used\"", pkg)
		}
		if callsJSON && !importsJSON {
			t.Errorf("%s: calls json.Unmarshal and does not import encoding/json -- "+
				"\"undefined: json\" (cleat#1036)", pkg)
		}
		if importsJSON && !callsJSON {
			t.Errorf("%s: imports encoding/json and never uses it -- "+
				"\"imported and not used\". This is the direction the obvious fix "+
				"to cleat#1697 breaks: a lone-string entry point takes the "+
				"whole-payload fast path and emits no call at all.", pkg)
		}
	}

	// THE THIRD OUTCOME. A rename of testdata/, a loader change, or an
	// analyzer that stops returning entry points would otherwise leave this
	// reporting green over nothing -- which is exactly how cleat#1697 reached
	// develop.
	if checked < 5 {
		t.Fatalf("only %d testdata packages produced entry points; this test "+
			"scanned almost nothing and its green means nothing", checked)
	}
	t.Logf("checked %d generated files; %d of them bind scalars with no complex "+
		"parameter (the shape cleat#1697 broke)", checked, stringOnly)
}

// hasAnyComplexParam reports whether any entry point takes a parameter that is
// not a plain scalar, which is the condition that used to make the old gate
// true for the wrong reason.
//
// Kept only to report coverage in the log line above -- nothing branches on it.
func hasAnyComplexParam(result *analyzer.AnalysisResult, _ string) bool {
	for _, epName := range result.EntryPoints {
		fd := result.Funcs[epName]
		if fd == nil || fd.Type == nil {
			continue
		}
		params := fd.Type.Params()
		if params == nil {
			continue
		}
		for i := 0; i < params.Len(); i++ {
			if i == 0 && analyzer.IsHostCallsType(params.At(i).Type()) {
				continue
			}
			switch params.At(i).Type().String() {
			case "string", "int", "int32", "int64":
			default:
				return true
			}
		}
	}
	return false
}
