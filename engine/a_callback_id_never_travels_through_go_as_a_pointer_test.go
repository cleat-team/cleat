package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cleat#4417. A callback-registry id is a small integer that wasmtime hands back to goComponentCallback as `void *env`. A C helper that took the id
// and RETURNED it as a `void *` brought it into Go as an unsafe.Pointer temporary, and while that was live in a pointer-typed stack slot holding a
// value below the smallest legal address (the id 0x215), any stack copy aborted the whole process: "fatal error: invalid pointer found on stack".
// It was rare, it killed every run on the worker, and it sat on every Python or C# component execution for months.
//
// The fix keeps the id an integer in Go and does the cast and the call in C (add_func_with_id). What this pins is the SHAPE, not the old name, so a
// helper rewritten under another name trips it too:
//
//  1. no C function in the package takes a uintptr_t and returns a `void *`, and
//  2. Go never calls C.wasmtime_component_linker_instance_add_func itself, because its `data` parameter is the one a Go caller would fill with such a
//     value; the C wrapper is the only caller.
//
// Both patterns are first run against the code that was removed, verbatim, so a pattern that stopped matching anything cannot let this pass.
var (
	// A function whose parameter is a uintptr_t and whose result is `void *`, in a cgo preamble (a Go comment, so it may follow `// `).
	intToVoidPointerHelper = regexp.MustCompile(`static\s+(?:inline\s+)?void\s*\*\s*\w+\s*\(\s*(?://\s*)?uintptr_t\b`)
	// A Go call of the C add_func, which takes `void *data`.
	goCallsAddFunc = regexp.MustCompile(`\bC\.wasmtime_component_linker_instance_add_func\s*\(`)
)

func TestACallbackIdNeverTravelsThroughGoAsAPointer(t *testing.T) {
	// Known positives: the removed helper and its two call forms, as they stood on develop 80d43f19.
	removedHelper := "// static void *cbid_as_env(uintptr_t id) { return (void *)id; }"
	removedCall := "err := C.wasmtime_component_linker_instance_add_func(sub, fnPtr, C.size_t(len(witFuncName)), C.wasmtime_component_func_callback_t(C.goComponentCallback), C.cbid_as_env(C.uintptr_t(cbID)), nil)"
	if !intToVoidPointerHelper.MatchString(removedHelper) {
		t.Fatalf("PRECONDITION FAILED: the helper pattern does not match the removed helper, so it guards nothing")
	}
	if !goCallsAddFunc.MatchString(removedCall) {
		t.Fatalf("PRECONDITION FAILED: the call pattern does not match the removed call, so it guards nothing")
	}
	// The negative controls: the replacement wrapper (returns an error pointer, not a void *) and a sentence about the hazard must NOT match.
	replacement := "// static wasmtime_error_t *add_func_with_id(wasmtime_component_linker_instance_t *inst, const char *name, size_t name_len, uintptr_t id) {"
	prose := "// a helper that takes a uintptr_t and returns a void * is the hazard"
	if intToVoidPointerHelper.MatchString(replacement) || intToVoidPointerHelper.MatchString(prose) {
		t.Fatalf("the helper pattern matches the replacement wrapper or a sentence about the hazard: it would refuse the fix")
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	cgoFiles := 0
	for _, f := range files {
		// A _test.go file cannot import "C", so no C helper can live in one, and this file holds the removed code as literals. cgo_test_helpers.go is
		// not named *_test.go for exactly that reason and IS scanned.
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		text := string(src)
		if strings.Contains(text, `import "C"`) {
			cgoFiles++
		}
		if loc := intToVoidPointerHelper.FindStringIndex(text); loc != nil {
			t.Errorf("%s: a C function takes a uintptr_t and returns a void *: %q. The id would come back into Go as an unsafe.Pointer holding a "+
				"small non-pointer value, and a stack copy while it is live aborts the process (cleat#4417). Do the cast and the call in C.",
				f, strings.TrimSpace(text[loc[0]:min(loc[1]+40, len(text))]))
		}
		if loc := goCallsAddFunc.FindStringIndex(text); loc != nil {
			t.Errorf("%s: Go calls C.wasmtime_component_linker_instance_add_func directly. Its data parameter is where a callback id was passed as a "+
				"Go pointer value (cleat#4417); call add_func_with_id, which takes the id as a uintptr_t.", f)
		}
	}
	// A scan that read nothing agrees with every tree.
	if cgoFiles < 3 {
		t.Errorf("the scan found %d cgo files in the package, want at least 3: it is not looking at the package", cgoFiles)
	}
	// And the wrapper has to be what registration uses, or the two checks above are satisfied by registering nothing.
	cb, err := os.ReadFile("component_callbacks.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(cb), "C.add_func_with_id("); n < 1 {
		t.Errorf("component_callbacks.go never calls C.add_func_with_id: nothing is registering callbacks through the wrapper")
	}
}
