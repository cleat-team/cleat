package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// cleat#4023: AssemblyScript was removed, and the owner decided (2026-11-18) that the engine REFUSES a module built with it rather
// than leaving the answer to whatever routing happens to do with an unknown label.
//
// What is pinned is the MESSAGE as well as the refusal, because "refused" has two very different shapes here. resolveBackend's generic
// answer for any unregistered language is "no WASM backend registered for guest language %q ... not a supported guest language", which
// reads as a typo in the module's metadata. For a language that used to be supported, that sends an operator looking for a typo in a
// module that is fine. So the refusal must name the language and say it was REMOVED, and a test that only checked for an error would pass
// on the generic one.

// anASModuleWithNoMetadata is a module that declares nothing about itself but imports env.abort, which is how DetectLanguage recognises
// an AssemblyScript build. A removed language must be refused by its shape too: an AssemblyScript module that carried no metadata
// would otherwise detect as the default, "go", and run as one.
func anASModuleWithNoMetadata(t *testing.T) []byte {
	t.Helper()
	return mustWat2Wasm(t, `(module (import "env" "abort" (func (param i32 i32 i32 i32))) (func (export "run")))`)
}

func TestAModuleBuiltWithARemovedLanguageIsRefusedAndTheMessageSaysSo(t *testing.T) {
	ctx := context.Background()

	modules := map[string][]byte{
		"declared in its metadata":  wasmWithLanguage("assemblyscript"),
		"recognised by its imports": anASModuleWithNoMetadata(t),
	}
	// A worker-shaped engine (a backend for every routable language, no wazero Runtime) and one with NO backends: the second is
	// cleatctl replay|debug, cleat run_embedded, cleat-bench and cleat/wasmtest, whose modules would otherwise run on wazero.
	engines := map[string]func() (*Engine, *mockBackend){
		"a worker-shaped engine": func() (*Engine, *mockBackend) {
			b := &mockBackend{name: "wasmtime-stub"}
			return NewEngine(nil, &mockCaller{}, WithBackends(WasmtimeLanguages, b)), b
		},
		"an engine with no backends": func() (*Engine, *mockBackend) {
			return NewEngine(nil, &mockCaller{}), nil
		},
	}

	for modName, mod := range modules {
		// Control: the module is what the test says it is. Without it a module that detected as something else would be refused
		// (or not) for the wrong reason and the message assertions below would be about the wrong thing.
		if got := wasm.DetectLanguage(mod); got != "assemblyscript" {
			t.Fatalf("%s: DetectLanguage = %q, want assemblyscript, so this case is not exercising the removed language", modName, got)
		}
		for engName, mk := range engines {
			t.Run(modName+" on "+engName, func(t *testing.T) {
				eng, backend := mk()

				check := func(entry string, err error) {
					t.Helper()
					if err == nil {
						t.Fatalf("%s accepted an AssemblyScript module", entry)
					}
					msg := err.Error()
					for _, want := range []string{"AssemblyScript", `"assemblyscript"`, "removed", "cleat#4023"} {
						if !strings.Contains(msg, want) {
							t.Errorf("%s refused it, but the message does not contain %q: %v", entry, want, err)
						}
					}
					if strings.Contains(msg, "no WASM backend registered") {
						t.Errorf("%s gave the GENERIC unroutable-language message, which reads as a typo in the metadata: %v", entry, err)
					}
				}

				_, _, _, _, _, err := eng.Execute(ctx, mod, "run", json.RawMessage(`{}`))
				check("Execute", err)
				_, _, _, _, _, err = eng.Replay(ctx, mod, "run", json.RawMessage(`{}`), nil)
				check("Replay", err)
				_, err = eng.RunDefer(ctx, mod, "defer-1", json.RawMessage(`{}`))
				check("RunDefer", err)

				if backend != nil && backend.executeCalled != 0 {
					t.Errorf("the backend was called %d times for a module that was refused", backend.executeCalled)
				}
			})
		}
	}
}

// The control the refusal needs, so it cannot be satisfied by an engine that refuses everything: a module of a language that is
// still routable still reaches its backend, and an unknown language that was NEVER supported keeps the generic message (the refusal is
// about languages that were removed, not a blanket on anything unrecognised).
func TestTheRemovedLanguageRefusalDoesNotCatchOtherLanguages(t *testing.T) {
	ctx := context.Background()

	backend := &mockBackend{name: "wasmtime-stub"}
	eng := NewEngine(nil, &mockCaller{}, WithBackends(WasmtimeLanguages, backend))
	if _, err := eng.RunDefer(ctx, wasmWithLanguage("java"), "defer-1", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("a java module was refused: %v", err)
	}
	if backend.executeCalled == 0 {
		t.Error("a java module returned nil without reaching its backend, so the refusal above could be a blanket refusal")
	}

	_, err := eng.RunDefer(ctx, wasmWithLanguage("cobol"), "defer-1", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "no WASM backend registered") {
		t.Errorf("an unknown language that was never supported should keep the generic message, got %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "removed") {
		t.Errorf("a language that was never supported was told it was removed: %v", err)
	}
}

// AssemblyScript is not a routable language any more, and nothing may list it as one.
func TestAssemblyScriptIsNotRoutable(t *testing.T) {
	for _, lang := range WasmtimeLanguages {
		if lang == "assemblyscript" {
			t.Errorf("WasmtimeLanguages still names assemblyscript: %v", WasmtimeLanguages)
		}
	}
	if RunsOnWasmtime("assemblyscript") {
		t.Error(`RunsOnWasmtime("assemblyscript") is true`)
	}
	if deferSegmentLanguages["assemblyscript"] {
		t.Error("deferSegmentLanguages still names assemblyscript")
	}
}
