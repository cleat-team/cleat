package engine

import (
	"context"
	"database/sql/driver"
	"testing"
)

// An unconstrained plugin dependency must resolve, and it must resolve through
// ResolvePlugins rather than through the matcher.
//
// WHY THROUGH ResolvePlugins. cleat#1252 was filed against matchesConstraint,
// which rejects "" and "*" because it parses them as a bare version. That is
// true and is not the defect a caller can see: resolveOnePlugin is the only
// thing that calls it, and a test that pins matchesConstraint would pass
// unchanged if resolveOnePlugin stopped calling it. The exported entry point is
// the one with a contract, so these go through it.
//
// The fix is a delegation, not a new behaviour -- VersionSatisfies already held
// the ""/"*" short-circuit and landed 30 lines below this call site in #1263,
// where only the worker used it. Nothing here asserts anything VersionSatisfies
// did not already do; what is asserted is that resolveOnePlugin now asks it.
func TestResolvePlugins_UnconstrainedDependencyResolves(t *testing.T) {
	// ORDER BY created_at DESC, so the first row is what an "any version"
	// constraint should pick.
	rows := [][]driver.Value{
		{"2.0.0"},
		{"1.5.0"},
		{"1.0.0"},
	}

	for _, tc := range []struct {
		name       string
		constraint string
	}{
		{"empty constraint", `{"llm": ""}`},
		{"wildcard constraint", `{"llm": "*"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := multiRowDB(rows, nil)
			got, err := ResolvePlugins(context.Background(), db, tc.constraint)
			if err != nil {
				t.Fatalf("an unconstrained dependency must resolve, got error: %v", err)
			}
			if got["llm"] != "2.0.0" {
				t.Errorf("expected the newest version %q, got %q", "2.0.0", got["llm"])
			}
		})
	}
}

// The malformed-version filter in resolveOnePlugin is load-bearing and is NOT
// redundant with the parse inside VersionSatisfies.
//
// VersionSatisfies answers "" and "*" with true BEFORE it validates the
// version, which is correct for its own caller: the worker is asking whether an
// installed plugin satisfies a dependency, and that version is the plugin's own
// Info().Version. Here the version is a plugin_defs row and can be anything.
//
// So the obvious form of this fix -- delete the parse, call VersionSatisfies --
// makes an unconstrained dependency resolve to a junk registry row and return
// it to the caller as a concrete version. This test is what separates the fix
// from that near-miss, and it is the case a test written only against ""/"*"
// would not have.
func TestResolvePlugins_UnconstrainedSkipsMalformedVersions(t *testing.T) {
	rows := [][]driver.Value{
		{"not-a-version"},
		{"1.5.0"},
	}
	db := multiRowDB(rows, nil)

	got, err := ResolvePlugins(context.Background(), db, `{"llm": ""}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["llm"] == "not-a-version" {
		t.Fatal("a malformed plugin_defs row was returned as a resolved version; " +
			"the parseVersion filter in resolveOnePlugin was dropped")
	}
	if got["llm"] != "1.5.0" {
		t.Errorf("expected the newest well-formed version %q, got %q", "1.5.0", got["llm"])
	}
}
