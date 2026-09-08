package wasm

import (
	"sort"
	"strings"
	"testing"
)

// TestEveryHostFunctionRowReachesTheUsageScan is cleat#1005.
//
// hostFunctions is a one-to-many relation: a FieldName may be served by more
// than one import. collectHostCallsCalls used to flatten it into a
// map[string]string, so for such a field every row but the last was discarded
// and its import was never marked used.
//
// The consequence was not a build error. The adapter simply emitted no field
// for the lost import, HostCallsImpl's nil branch ran instead, and a call that
// delegated to the missing one failed at RUN time -- with a message about the
// workflow's entry point, which points away from the binding.
//
// The predicate, not a count: NO row is unreachable. That was true when two
// fields had two rows each and stays true as the table grows, where "the two
// WithOptions methods are covered" would have to be re-derived every time
// someone adds a row.
func TestEveryHostFunctionRowReachesTheUsageScan(t *testing.T) {
	// THE LOOKUP THE SCAN ACTUALLY USES, not a copy of it built here. The first
	// version of this test rebuilt the map itself and then asserted the map
	// contained what it had just appended -- true by construction, and green
	// against the flattened implementation it was written to catch.
	reachable := fieldImports()

	var lost []string
	for _, hf := range hostFunctions {
		if hf.ImportName == "" {
			continue // tracked with no import of its own, e.g. RunDetached
		}
		found := false
		for _, imp := range reachable[hf.FieldName] {
			if imp == hf.ImportName {
				found = true
				break
			}
		}
		if !found {
			lost = append(lost, hf.FieldName+" -> "+hf.ImportName)
		}
	}
	sort.Strings(lost)
	if len(lost) > 0 {
		t.Errorf("%d hostFunctions row(s) cannot be reached by the usage scan, so a "+
			"workflow calling that method links without the import and fails at run "+
			"time rather than at build time:\n  %s",
			len(lost), strings.Join(lost, "\n  "))
	}

	// CONTROL, and the half that matters: this test must be able to SEE a
	// one-to-many field. If hostFunctions ever holds one row per field, the
	// loop above passes trivially and stops testing the thing it is named for
	// -- a green that means "there is nothing to get wrong" reads identically
	// to "we got it right".
	multi := 0
	for _, imps := range reachable {
		if len(imps) > 1 {
			multi++
		}
	}
	if multi == 0 {
		t.Fatal("no FieldName in hostFunctions has more than one import row, so this " +
			"test cannot distinguish a correct one-to-many lookup from a flattened " +
			"one. It passed without exercising its subject.")
	}
	t.Logf("%d field(s) served by more than one import; all rows reachable", multi)
}
