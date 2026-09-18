package main

import (
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// contextOrderDependencies lists (consumer, provider) plugin-name pairs where
// consumer's middleware reads a context value provider's middleware sets --
// so provider must be strictly MORE OUTER than consumer in the wrapping order
// built below (main.go:1118-1124: each iteration over plugList wraps the
// accumulated handler, so a plugin later in that order wraps, and therefore
// runs before, everything already wrapped).
//
// cleat#1881 is why this exists rather than being read off plugin.Discover()'s
// current output: audit-log read oauthprovider's session identity this way
// and it worked, for a reason nobody chose -- neither plugin declares a
// dependency on the other, so their relative order fell through to
// topologicalSort's alphabetical tie-break (plugin/registry.go:186), and
// "audit-log" happens to sort before "oauth-provider". Renaming either
// plugin, or giving either an unrelated Requires that moved it in the sort,
// would have silently reverted audit-log's user_id to always-empty, with no
// test failing.
//
// WHY NOT plugin.PluginInfo.Requires. Requires makes Discover() refuse to run
// AT ALL if the required plugin is not registered (plugin/registry.go:193) --
// a real functional coupling, not a documentation nicety. audit-log and
// oauth-provider are independently optional features; an operator running one
// without the other must not lose the ability to start the worker over a
// middleware-ordering concern neither plugin's own function depends on. So
// this list is the declaration instead: add a row here when a new plugin
// reads a context value another plugin's middleware sets, rather than
// reaching for Requires or leaving the order to alphabetical accident.
//
// See docs/contributor/plugins/plugin-contract.md, C14.
var contextOrderDependencies = []struct {
	consumer, provider string
}{
	{consumer: "audit-log", provider: "oauth-provider"},
}

// TestEveryContextOrderDependencyHasProviderOuterOfConsumer is the guard for
// plugin-contract.md's C14: a plugin reading another plugin's context value
// declares the ordering dependency, and it is enforced against the REAL
// registered plugin set -- not a synthetic one -- so a rename or an unrelated
// Requires that reshuffles the discovery order fails this test by name
// instead of silently reverting to whatever the alphabet produces.
func TestEveryContextOrderDependencyHasProviderOuterOfConsumer(t *testing.T) {
	loaded, err := plugin.Discover()
	if err != nil {
		t.Fatalf("discovering registered plugins: %v", err)
	}

	// A floor on the INPUT, not the answer -- see
	// TestEveryLinkedPluginSupportsEveryDialectTheWorkerRunsOn in this same
	// package for why: if Discover returns nothing, every assertion below
	// passes vacuously and this test reports a clean bill of health for a
	// registry that never populated.
	if len(loaded) == 0 {
		t.Fatal("plugin.Discover() returned no plugins; this test's assertions would pass " +
			"vacuously against an empty registry, which is not evidence of anything")
	}

	index := make(map[string]int, len(loaded))
	for i, lp := range loaded {
		index[lp.Plugin.Info().Name] = i
	}

	for _, dep := range contextOrderDependencies {
		ci, ok := index[dep.consumer]
		if !ok {
			t.Errorf("contextOrderDependencies names consumer %q, which is not a registered "+
				"plugin. If it was renamed, update this list and its middleware's context read "+
				"to match -- a silently stale entry here is worse than none, because it reads "+
				"as an enforced guarantee that no longer exists.", dep.consumer)
			continue
		}
		pi, ok := index[dep.provider]
		if !ok {
			t.Errorf("contextOrderDependencies names provider %q, which is not a registered "+
				"plugin. If it was renamed, update this list and the plugin that sets the "+
				"context value to match.", dep.provider)
			continue
		}
		if pi <= ci {
			t.Errorf("%q (index %d) must be strictly OUTER than %q (index %d) in "+
				"plugin.Discover()'s order, i.e. LATER in the list, but it is not.\n\n"+
				"%q's middleware reads a context value %q's middleware sets, after calling "+
				"next -- so it only sees that value if %q wraps it from the outside. Today's "+
				"order came from the alphabetical tie-break in topologicalSort "+
				"(plugin/registry.go:186) rather than a declaration, which is exactly the "+
				"failure mode cleat#1881 fixed: give one of these two plugins an unrelated "+
				"name, or an unrelated Requires that reshuffles the sort, and %q silently "+
				"stops seeing %q's context value again with no test failing -- except this "+
				"one, now.", dep.provider, pi, dep.consumer, ci,
				dep.consumer, dep.provider, dep.provider, dep.consumer, dep.provider)
		}
	}
}
