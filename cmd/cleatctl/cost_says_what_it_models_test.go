package main

import (
	"strings"
	"testing"
)

// TestCostOutputCarriesTheStorageCaveat is cleat#1295.
//
// `cleatctl cost`'s storage figures come from a model the engine does not
// implement: it assumes every event is retained for the retention window,
// while finalize_workflow_status deletes event history at 'done'/'failed', and
// 'terminated'/'dead_lettered' rows are retained indefinitely on default flags.
//
// The model is deliberately unchanged -- a correct one is a decision with an
// owner. What must not silently disappear is the caveat saying so, BESIDE THE
// NUMBER. A caveat that lives only in --help reaches whoever already suspected
// something was wrong, which is not who is being misled.
func TestCostOutputCarriesTheStorageCaveat(t *testing.T) {
	c := &costCommand{
		workload: 10, avgDuration: 3, eventsPerWF: 15,
		retentionDays: 90, replication: 1, provider: "aws", concurrency: 25,
	}
	out := c.Format()

	// Vacuity: an empty or error output would satisfy every "does not contain"
	// check and several "contains" ones by accident.
	if !strings.Contains(out, "Retained storage:") {
		t.Fatalf("Format() produced no storage section at all; this test measured "+
			"nothing:\n%s", out)
	}

	for _, want := range []string{
		"NOTE ON STORAGE",
		"cleat#1295",
		"terminated",         // the indefinitely-retained case
		"workflow_instances", // the unmodelled term
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the storage caveat no longer mentions %q. The model is still "+
				"wrong (cleat#1295); if it has been fixed, this test and the caveat "+
				"should go together and the issue should be closed with the "+
				"measurement.", want)
		}
	}

	// The caveat has to come after the figures it qualifies, or a reader who
	// stops at the number never meets it.
	//
	// Guarded on presence first: strings.Index returns -1 for an absent needle,
	// and -1 is less than any real offset -- so the naive form reports "printed
	// before the figures" when the caveat is not printed AT ALL, which sends the
	// reader after an ordering bug that does not exist. The absence is already
	// reported above; this assertion is only meaningful when both are present.
	at := strings.Index(out, "NOTE ON STORAGE")
	fig := strings.Index(out, "Retained storage:")
	if at >= 0 && fig >= 0 && at < fig {
		t.Error("the caveat is printed before the storage figures; it qualifies them " +
			"and belongs after")
	}
}
