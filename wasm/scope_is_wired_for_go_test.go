package wasm

import (
	"strings"
	"testing"
)

// TestGoGuestEmitsTheScopeImports is the converse of the evidence that opened
// cleat#984, and it is deliberately the same KIND of evidence.
//
// IMPROVEMENT-PLAN 3.223 did not settle that gap by reading tables. It compiled
// a Go workflow whose entire body was h.SetScope(obj, key) and read the produced
// binary:
//
//	imports wired:   cleat_complete, cleat_log, cleat_poll_work
//	adapter fields:  DurableLog
//
// cleat_set_scope was not in the binary at all. HostCallsImpl.SetScope set three
// local fields against a host call that was never generated.
//
// A table-only check cannot see that, and the tables are exactly what a fix
// touches -- so a test asserting "wasm/usage.go has a row" would pass on a tree
// where the generator still emitted nothing. This asserts the generator's
// OUTPUT instead, which is one step closer to the binary and the nearest thing
// to that compilation this package can run cheaply.
func TestGoGuestEmitsTheScopeImports(t *testing.T) {
	want := map[string]string{
		"SetScope": "cleat_set_scope",
		"GetScope": "cleat_get_scope",
	}

	var funcs []HostFunction
	used := map[string]bool{}
	for _, h := range hostFunctions {
		if _, ok := want[h.FieldName]; ok {
			funcs = append(funcs, h)
			used[h.ImportName] = true
		}
	}
	// Vacuity guard: if the rows vanish, `funcs` is empty and every assertion
	// below becomes trivially satisfiable. That is the state this test exists
	// to detect, so it must fail rather than pass quietly.
	if len(funcs) != len(want) {
		t.Fatalf("wasm/usage.go declares %d of the %d scope methods -- the Go SDK "+
			"cannot reach a host call it does not declare, which is cleat#984",
			len(funcs), len(want))
	}

	imports := string(GenerateImports("main", &UsageInfo{Used: used, Funcs: funcs}))
	for field, imp := range want {
		decl := "//go:wasmimport env " + imp
		if !strings.Contains(imports, decl) {
			t.Errorf("the generated imports for %s do not declare %q.\n"+
				"A Go guest cannot call a host function it never imports; this is "+
				"the exact shape of cleat#984, where SetScope set local fields "+
				"against a call that was never generated.", field, decl)
		}
	}

	// And the adapter must actually CALL the stub -- declaring an import that
	// nothing invokes is the same no-op wearing a different hat.
	for _, hf := range funcs {
		src := string(GenerateHostAdapter("main", &UsageInfo{
			Used: map[string]bool{hf.ImportName: true}, Funcs: []HostFunction{hf},
		}, "go"))
		stub := goName(hf.ImportName) + "Import("
		if !strings.Contains(src, stub) {
			t.Errorf("the generated adapter for %s never calls %s", hf.FieldName, stub)
		}
	}
}
