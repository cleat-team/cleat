package engine

import (
	"reflect"
	"testing"
)

// TestWasmtimeLanguages pins the contents of the routing list.
//
// It is deliberately an exact-match assertion rather than a set of "contains"
// checks. Membership means a language was verified to load and execute on
// wasmtime; adding one should be a decision someone makes and records, not a
// line that appears during an unrelated change. Whichever direction this test
// fails in, the fix is to justify the edit in the comment on WasmtimeLanguages
// and update this list -- not to loosen the assertion.
func TestWasmtimeLanguages(t *testing.T) {
	want := []string{"go", "assemblyscript", "java", "rust"}
	if !reflect.DeepEqual(WasmtimeLanguages, want) {
		t.Errorf("WasmtimeLanguages = %v, want %v", WasmtimeLanguages, want)
	}
}

// TestRunsOnWasmtimeAgreesWithTheList guards the invariant that made two
// diverging copies dangerous: the worker uses RunsOnWasmtime to decide whether
// to build a wazero Runtime, while the Engine routes by looking the language up
// in the backend map built from WasmtimeLanguages. If those two disagree, a
// workflow either gets a redundant runtime or no backend at all.
func TestRunsOnWasmtimeAgreesWithTheList(t *testing.T) {
	for _, lang := range WasmtimeLanguages {
		if !RunsOnWasmtime(lang) {
			t.Errorf("RunsOnWasmtime(%q) = false but it is in WasmtimeLanguages", lang)
		}
	}
	for _, lang := range []string{"python", "ruby", "", "GO"} {
		if RunsOnWasmtime(lang) {
			t.Errorf("RunsOnWasmtime(%q) = true but it is not in WasmtimeLanguages", lang)
		}
	}
}

// TestWithBackendsRegistersEvery covers the option itself: registering N
// languages one call at a time is what made it easy to leave one out silently,
// so the replacement has to actually register all of them.
func TestWithBackendsRegistersEvery(t *testing.T) {
	e := &Engine{}
	WithBackends([]string{"go", "assemblyscript", "java"}, nil)(e)

	if len(e.backends) != 3 {
		t.Fatalf("registered %d backends, want 3: %v", len(e.backends), e.backends)
	}
	for _, lang := range []string{"go", "assemblyscript", "java"} {
		if _, ok := e.backends[lang]; !ok {
			t.Errorf("language %q was not registered", lang)
		}
	}
	if _, ok := e.backends["python"]; ok {
		t.Error("python was registered but was not asked for")
	}
}

// TestBackendForWasmFallsBackForUnregistered asserts the fallback direction.
// backendForWasm returning nil is what sends a guest to the wazero Runtime; if
// it ever returned a backend for an unregistered language, an unsupported guest
// would run somewhere nobody verified.
func TestBackendForWasmFallsBackForUnregistered(t *testing.T) {
	e := &Engine{}
	WithBackends(WasmtimeLanguages, nil)(e)

	// A component-model header detects as "python", which is not registered.
	pythonish := []byte{0x00, 0x61, 0x73, 0x6d, 0x0d, 0x00, 0x01, 0x00}
	if b := e.backendForWasm(pythonish); b != nil {
		t.Errorf("backendForWasm returned %v for python; unregistered languages must fall back", b)
	}
}
