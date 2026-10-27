//go:build cgo

package engine

import (
	"fmt"
	"sort"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// missingFromWitTypeMap returns "module/function" for every WIT import the
// linker registers that witTypeMap has no entry for.
//
// registerCleatComponentImports loops over wasm.WitToEnvImport and looks each
// name up in witTypeMap, falling back to cbTypeDefault when there is no entry.
// That fallback writes a u64 0 into the result slot, whatever the WIT
// signature says the result is -- for a function that returns a string, a value
// of the wrong type.
//
// TestWitTypeMapNoDefaults cannot see this. It iterates over witTypeMap and
// rejects any ENTRY whose type is cbTypeDefault, and a name with no entry at
// all is never iterated: cleat#3390's durable-poll-child and
// durable-await-any-child linked to the fallback while that test passed.
func missingFromWitTypeMap(imports map[string]map[string]string, typeMap map[string]map[string]cbType) []string {
	var missing []string
	for module, funcs := range imports {
		for fn := range funcs {
			if _, ok := typeMap[module][fn]; !ok {
				missing = append(missing, fmt.Sprintf("%s/%s", module, fn))
			}
		}
	}
	sort.Strings(missing)
	return missing
}

// knownUndispatchedWitImports are imports that link to the fallback TODAY, with
// the issue that owns each. The list can only shrink: an entry whose import has
// a dispatcher fails the second half of the test below, so a fixed gap cannot
// keep its exemption. Adding an entry needs an issue and a sentence; it is not
// a way to make this test pass.
//
// It is EMPTY, and was not: it held durable-run-detached and durable-defer-phase
// until cleat#3428 gave them dispatchers. The mechanism stays so the next gap has
// somewhere honest to be recorded, and the stale-entry check below stays so it
// cannot linger.
var knownUndispatchedWitImports = map[string]string{}

func TestEveryRegisteredWitImportHasADispatcher(t *testing.T) {
	// Vacuity, per thing: a loop over an empty or renamed table passes whatever
	// the tree says. Both members are ones the check below cannot legitimately lose.
	if len(wasm.WitToEnvImport) < 8 {
		t.Fatalf("wasm.WitToEnvImport has %d modules; this check is reading the wrong thing", len(wasm.WitToEnvImport))
	}
	if _, ok := wasm.WitToEnvImport["cleat:host-calls/durable-children"]["durable-await-child"]; !ok {
		t.Fatal("durable-children/durable-await-child is missing from wasm.WitToEnvImport; " +
			"this check is reading the wrong thing")
	}

	missing := missingFromWitTypeMap(wasm.WitToEnvImport, witTypeMap)
	missingSet := map[string]bool{}
	var unexplained []string
	for _, m := range missing {
		missingSet[m] = true
		if _, known := knownUndispatchedWitImports[m]; !known {
			unexplained = append(unexplained, m)
		}
	}
	if len(unexplained) > 0 {
		t.Errorf("%d WIT import(s) are registered with the linker but have no witTypeMap entry, "+
			"so they link to the deprecated cbTypeDefault fallback, which writes a u64 0 whatever "+
			"the signature returns:\n  %v\n\nAdd a cbType and a dispatcher, or remove the import "+
			"from wasm.WitToEnvImport if the world no longer declares it.", len(unexplained), unexplained)
	}

	// The other direction: an exemption for an import that now HAS a dispatcher
	// (or that the world no longer declares) describes nothing.
	var stale []string
	for name := range knownUndispatchedWitImports {
		if !missingSet[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("knownUndispatchedWitImports names %v, which no longer link to the fallback; "+
			"delete the entries.", stale)
	}
}

// The known-positive for the check above, run on a table that is wrong on
// purpose, so a pass cannot mean the function stopped reporting.
func TestTheWitTypeMapCheckReportsAnImportWithNoEntry(t *testing.T) {
	imports := map[string]map[string]string{
		"m": {"has-entry": "env_a", "no-entry": "env_b"},
	}
	typeMap := map[string]map[string]cbType{
		"m": {"has-entry": cbTypeAwaitChild},
	}
	got := missingFromWitTypeMap(imports, typeMap)
	if len(got) != 1 || got[0] != "m/no-entry" {
		t.Fatalf("missingFromWitTypeMap = %v, want [m/no-entry]", got)
	}
	// And a whole module with no entries at all, which a nested lookup that
	// returned early on the module would also miss.
	got = missingFromWitTypeMap(map[string]map[string]string{"other": {"f": "env_f"}}, typeMap)
	if len(got) != 1 || got[0] != "other/f" {
		t.Fatalf("missingFromWitTypeMap for an absent module = %v, want [other/f]", got)
	}
}
