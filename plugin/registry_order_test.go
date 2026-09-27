package plugin

import (
	"fmt"
	"strings"
	"testing"
)

// topologicalSort must be a pure function of its input: the same entries must
// give the same order, in this process and in the next one.
//
// cleat#1566. Plugin order decides middleware NESTING, so a randomised order
// means two workers running identical binaries wrap a call in a different
// sequence -- and one worker restarting changes its own behaviour. That is the
// kind of defect that is reported as "it only happens on one box".
//
// WHY THIS SHAPE OF TEST. The claim is about variation BETWEEN runs, so an
// assertion about one ordering proves nothing: it would pass against a
// randomised implementation on whatever order that run happened to produce.
// Go randomises map iteration on every `range`, so the reproduction is to sort
// the same input many times in one process and require every result identical.
//
// A test that asserts one hard-coded order would be strictly worse than this
// even though it looks stricter -- it pins an arbitrary choice, and it is the
// version that passes before the fix on a lucky run.
const orderingRuns = 40

// distinctOrderings sorts the same entries orderingRuns times and returns every
// distinct result. A correct implementation returns exactly one.
func distinctOrderings(t *testing.T, entries map[string]registryEntry) []string {
	t.Helper()
	seen := map[string]struct{}{}
	var distinct []string
	for i := 0; i < orderingRuns; i++ {
		got, err := topologicalSort(entries)
		if err != nil {
			t.Fatalf("topologicalSort: %v", err)
		}
		key := strings.Join(got, ",")
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			distinct = append(distinct, key)
		}
	}
	return distinct
}

func entry(name string, requires ...string) registryEntry {
	return registryEntry{info: PluginInfo{Name: name, Version: "1.0.0", Requires: requires}}
}

// The seed of Kahn's queue is built by ranging over a map, so independent
// plugins -- which is nearly all of them -- come out in a different order each
// time.
func TestDiscoveryOrderIsStableForIndependentPlugins(t *testing.T) {
	entries := map[string]registryEntry{}
	for i := 0; i < 10; i++ {
		n := fmt.Sprintf("plugin-%02d", i)
		entries[n] = entry(n)
	}

	got := distinctOrderings(t, entries)
	if len(got) != 1 {
		t.Errorf("%d distinct orderings across %d runs of the SAME input, want 1.\n\n"+
			"Kahn's queue is seeded by ranging over a map, and Go randomises map\n"+
			"iteration on every range. First two:\n  %s\n  %s",
			len(got), orderingRuns, got[0], got[1])
	}
}

// THE CASE A SEED-ONLY FIX PASSES, which is why it is a separate test.
//
// Every dependent here requires the same root, so exactly one plugin has
// in-degree 0 and the seed queue is a single deterministic element. All the
// variation that remains comes from graph[root] -- the adjacency list built by
// the OTHER map iteration, in the loop that computes in-degrees.
//
// Sorting only the seed determinises the test above and leaves this one random.
// That is the dangerous fix rather than a harmless partial one: nearly every
// bundled plugin declares no Requires, so a seed-only fix looks verified
// against the whole tree and is random for the first plugin that declares one.
func TestDiscoveryOrderIsStableForDependentsOfOneRoot(t *testing.T) {
	entries := map[string]registryEntry{"root": entry("root")}
	for i := 0; i < 10; i++ {
		n := fmt.Sprintf("dependent-%02d", i)
		entries[n] = entry(n, "root")
	}

	got := distinctOrderings(t, entries)
	if len(got) != 1 {
		t.Errorf("%d distinct orderings across %d runs of the SAME input, want 1.\n\n"+
			"The seed here is a single element, so this is NOT the seed iteration:\n"+
			"graph[root] is built by ranging over a map, so the dependents are\n"+
			"unblocked in a random order. A fix that sorts only the seed queue\n"+
			"passes the sibling test and fails this one. First two:\n  %s\n  %s",
			len(got), orderingRuns, got[0], got[1])
	}
}
