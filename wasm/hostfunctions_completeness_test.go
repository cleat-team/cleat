package wasm

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// notDeclaredForGo names every host call the engine registers that
// wasm/usage.go's hostFunctions deliberately does not declare, with the reason.
//
// An entry here is a CLAIM, and the test fails if it stops being true in either
// direction: an export that gains a hostFunctions row, or a name that is no
// longer registered at all. That is what stops this becoming the thing it
// guards against -- a hand-maintained list nobody re-checks.
var notDeclaredForGo = map[string]string{
	// Worker handshake, not guest-facing. CLAUDE.md names these two.
	"cleat_poll_work": "worker handshake; a guest never calls it",
	"cleat_complete":  "worker handshake; a guest never calls it",

	// Registered but deliberately unbindable. CLAUDE.md names this one.
	"cleat_register_query_handler": "deliberately unbindable from a guest",

	// Provided natively by Go, so no import is needed. These exist for guest
	// languages that cannot do them in-process.
	"cleat_uuid":           "HostCallsImpl.UUID derives it in Go from workflowID+seed via SHA-256, deterministic without a host call",
	"cleat_json_parse":     "Go uses encoding/json; the SDK exposes no JSONParse method",
	"cleat_json_stringify": "Go uses encoding/json; the SDK exposes no JSONStringify method",

	// Routed to a different import rather than unimplemented.
	"cleat_fetch": "the Go SDK's Fetch* methods map to cleat_call (wasm/usage.go, \"Fetch / HTTP methods\")",

	// THE OPEN GAP, machine-checked rather than described in prose.
	// IMPROVEMENT-PLAN 3.223 / cleat#984: HostCallsImpl.SetScope sets three
	// local fields and returns, so a Go guest takes no lock where Rust,
	// AssemblyScript and Python serialise. Delete these two entries when it is
	// fixed -- this test fails if they become stale, which is the point.
	"cleat_set_scope": "GAP: IMPROVEMENT-PLAN 3.223 / cleat#984 -- Go guest emits no call, takes no lock",
	"cleat_get_scope": "GAP: IMPROVEMENT-PLAN 3.223 / cleat#984 -- Go guest emits no call",
}

// TestHostFunctionsDeclaresEveryRegisteredHostCall closes the gap that let
// cleat#984 exist silently.
//
// TestEveryHostCallGeneratesACompilableAdapterAlone iterates hostFunctions and
// checks each row has an adapterDefs entry. Its denominator is the declaration
// list, so a host call with NO row is invisible to it -- and cleat_set_scope
// has no row. Every check built on hostFunctions inherits that blind spot.
//
// THIS IS THE THIRD INSTANCE OF ONE SHAPE, which is why it is worth a test
// rather than a note. CLAUDE.md records the first: rust_surface() matched
// `pub fn <name>(` and could not see a generic method, so ten of seventy-one
// were invisible and coverage read 61/61 = 100% when it was 63/71 = 88.7%.
// A generated-inventory table has the same limit -- --check verifies the README
// matches the generator and nothing verifies the generator against reality.
// Here the denominator is a hand-maintained list.
//
// All three fail in the FLATTERING direction: a smaller denominator raises the
// percentage, so nobody re-derives it.
//
// The precedent for the fix is in this repo already:
// TestProcedureMigrationListsAreComplete does exactly this for the
// hand-maintained list of migrations defining finalize_workflow_status, and it
// caught a real omission the same day it was written.
func TestHostFunctionsDeclaresEveryRegisteredHostCall(t *testing.T) {
	src, err := os.ReadFile("../engine/imports.go")
	if err != nil {
		t.Fatalf("reading the engine's import registrations: %v", err)
	}
	registered := map[string]bool{}
	for _, m := range regexp.MustCompile(`\.Export\("([^"]+)"\)`).FindAllStringSubmatch(string(src), -1) {
		registered[m[1]] = true
	}
	if len(registered) == 0 {
		t.Fatal("found no .Export(\"...\") calls in engine/imports.go -- the extraction is " +
			"broken, and a guard that reads an empty denominator passes vacuously")
	}

	declared := map[string]bool{}
	for _, hf := range hostFunctions {
		declared[hf.ImportName] = true
	}

	var undeclared []string
	for name := range registered {
		if declared[name] {
			continue
		}
		if _, ok := notDeclaredForGo[name]; ok {
			continue
		}
		undeclared = append(undeclared, name)
	}
	sort.Strings(undeclared)
	if len(undeclared) > 0 {
		t.Errorf("the engine registers these host calls and wasm/usage.go declares none of them, "+
			"so the Go adapter generator emits no import and every guard keyed on hostFunctions "+
			"is blind to them:\n  %s\n\n"+
			"A Go guest calling the corresponding SDK method gets whatever the local "+
			"implementation does -- for cleat#984 that was a plausible return value and no lock. "+
			"Either add a hostFunctions row, or add an entry to notDeclaredForGo saying why not.",
			strings.Join(undeclared, "\n  "))
	}

	// The allowlist is a claim in both directions.
	for name, why := range notDeclaredForGo {
		if !registered[name] {
			t.Errorf("notDeclaredForGo names %q, which the engine no longer registers.\n"+
				"  reason on file: %s\n"+
				"An entry that excuses something that does not exist is a grant covering nothing, "+
				"and it hides the next real omission behind a list nobody trusts.", name, why)
		}
		if declared[name] {
			t.Errorf("notDeclaredForGo says %q is deliberately not declared, but wasm/usage.go "+
				"now declares it.\n  reason on file: %s\n"+
				"If this is cleat#984 being fixed, delete the entry -- that deletion is the "+
				"signal this test exists to produce.", name, why)
		}
	}
}
