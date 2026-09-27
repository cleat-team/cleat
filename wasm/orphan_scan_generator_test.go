package wasm

import (
	"regexp"
	"strings"
	"testing"
)

// The orphan scan must know what the generator emits.
//
// cleat#1125: every WASM build warned that cleat_complete and cleat_poll_work
// were orphaned imports -- including builds of a workflow the toolchain itself
// reported as using ZERO host functions. Two halves, each correct about what it
// owns, and nothing reconciling them:
//
//   - generator.go writes both imports into every shim unconditionally, because
//     the EXPORT WRAPPER calls them. They are the wrapper's, not the workflow's.
//   - FindCleatOrphanedImports compares every cleat_-prefixed import against the
//     WORKFLOW's computed closure, which cannot contain them by construction.
//
// THE HARM IS NOT THE NOISE. A true W003 -- "your single string parameter
// receives the ENTIRE input JSON" -- was emitted correctly, predicted the exact
// failure that surfaced two layers later as a result stored as `{}`, and went
// unread because it arrived third in a list whose first two entries are always
// wrong. A channel whose first two entries are always wrong trains its readers
// to skip it.

// TestAGeneratorEmittedImportIsNotAnOrphan is the case the bug produced. It
// fails on the pre-fix tree with two warnings.
func TestAGeneratorEmittedImportIsNotAnOrphan(t *testing.T) {
	// A workflow that uses NO host functions: the closure is empty, and the
	// only cleat imports in the binary are the ones the generator always
	// writes. This is the build cleat#1125 was reported against.
	var entries []importEntry
	for _, name := range generatorEmittedImports {
		entries = append(entries, importEntry{[]byte("env"), []byte(name), 0, encodeULEB128(0)})
	}
	binary := makeTestWasmBinary(makeImportSection(entries...))

	orphans := FindCleatOrphanedImports(binary, map[string]bool{})
	if len(orphans) != 0 {
		t.Errorf("a workflow using no host functions produced %d orphan warnings: %v\n\n"+
			"These imports are written by wasm/generator.go into every shim because the "+
			"export wrapper calls them. They are never in the workflow's closure, so "+
			"comparing them against it can only ever warn. Every build of every workflow "+
			"emitted these, and a true W003 warning went unread behind them (cleat#1125).",
			len(orphans), orphans)
	}
}

// TestARealOrphanIsStillReported is the control, and it is what stops the fix
// above from being indistinguishable from deleting the check.
//
// Suppressing two warnings and suppressing the scan produce the same output on
// the case above. Only an import that SHOULD warn separates them.
func TestARealOrphanIsStillReported(t *testing.T) {
	entries := []importEntry{{[]byte("env"), []byte("cleat_sleep"), 0, encodeULEB128(0)}}
	for _, name := range generatorEmittedImports {
		entries = append(entries, importEntry{[]byte("env"), []byte(name), 0, encodeULEB128(0)})
	}
	binary := makeTestWasmBinary(makeImportSection(entries...))

	orphans := FindCleatOrphanedImports(binary, map[string]bool{})
	if len(orphans) != 1 {
		t.Fatalf("expected exactly the one real orphan, got %d: %v -- if this is 0 the "+
			"generator exemption has swallowed the whole check", len(orphans), orphans)
	}
	if !strings.Contains(orphans[0], "cleat_sleep") {
		t.Errorf("the reported orphan is %q, want the one that is genuinely unaccounted for", orphans[0])
	}
}

// TestTheGeneratorEmitsExactlyTheImportsTheScanExempts is what stops this fix
// from becoming the same defect one level over.
//
// A hand-maintained skip-list in scan.go would leave the generator free to add a
// third unconditional import, with the scan not knowing and the warning
// returning with nobody having touched it. That is precisely the shape being
// fixed -- two halves, each correct, nothing reconciling them. The existing
// normalizeImportName carries a variant map with the same weakness.
//
// So the names live in one place and this asserts the generator's emitted block
// declares exactly that set. Adding a third to generator.go without adding it
// here fails at authoring time rather than arriving as noise in every build.
func TestTheGeneratorEmitsExactlyTheImportsTheScanExempts(t *testing.T) {
	// Read the names out of the generator's own output rather than its source:
	// what matters is what lands in the shim.
	shim := string(GenerateImports("wf", &UsageInfo{Used: map[string]bool{}}))

	declared := map[string]bool{}
	re := regexp.MustCompile(`//go:wasmimport env (\w+)`)
	for _, m := range re.FindAllStringSubmatch(shim, -1) {
		declared[m[1]] = true
	}
	if len(declared) == 0 {
		t.Fatal("no //go:wasmimport directives found in the generated shim -- the parse " +
			"is broken and this test would pass for any generator at all")
	}

	exempt := map[string]bool{}
	for _, n := range generatorEmittedImports {
		exempt[n] = true
	}

	for name := range declared {
		if !exempt[name] {
			t.Errorf("generator.go emits %q into every shim but generatorEmittedImports "+
				"does not list it, so FindCleatOrphanedImports will warn about it on every "+
				"build of every workflow. Add it to the list -- and if it is NOT "+
				"unconditional, this test is calling it so because GenerateImports emitted "+
				"it for an EMPTY closure, which is the condition that matters.", name)
		}
	}
	for name := range exempt {
		if !declared[name] {
			t.Errorf("generatorEmittedImports lists %q but the generator does not emit it "+
				"for an empty closure. An exemption covering nothing is a grant waiting to "+
				"cover something else -- delete it.", name)
		}
	}
}
