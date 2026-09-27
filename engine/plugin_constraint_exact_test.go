package engine

import (
	"context"
	"database/sql/driver"
	"testing"
	"time"
)

// cleat#1243: an exact constraint matched nothing. `ResolvePlugin(ctx, name,
// "1.0.0")` could not return 1.0.0 -- the one version the constraint named was
// the one version it excluded, because both exact forms were encoded as a range
// whose Max is exclusive by construction.
//
// EVERY SUPPORTED FORM IS HERE, not only the two that were wrong. An
// inclusivity bug lives in a comparison that neighbouring forms share, so the
// forms that already worked are what establish the fix did not move them: "two
// forms were broken, five were not, and all seven are asserted" is a claim a
// reader can check, where "fixed the exact form" is not.
func TestEverySupportedConstraintFormAgreesAboutItsOwnVersion(t *testing.T) {
	const target = "v1.0.0"

	cases := []struct {
		constraint string
		want       bool
		why        string
	}{
		{"", true, "no constraint is a wildcard"},
		{"*", true, "explicit wildcard"},

		// The two cleat#1243 was filed for.
		{"1.0.0", true, "a bare version names exactly this one, so it must match it"},
		{"=1.0.0", true, "the explicit exact form, same requirement"},

		// Unaffected forms, asserted so the repair is shown not to move them.
		{"^1.0.0", true, "minor-locked: >= 1.0.0, < 2.0.0"},
		{"~1.0.0", true, "patch-locked: >= 1.0.0, < 1.1.0"},
		{">=1.0.0", true, "inclusive lower bound"},

		// The boundary the exclusive Max is RIGHT about, which is why the fix
		// is a separate field rather than making Max inclusive.
		{"^1.0.0", true, "1.0.0 is the lower bound of its own caret range"},
	}

	for _, tc := range cases {
		r, err := parseConstraint(tc.constraint)
		if err != nil {
			t.Errorf("parseConstraint(%q) errored: %v -- %s", tc.constraint, err, tc.why)
			continue
		}
		if got := versionInRange(target, r); got != tc.want {
			t.Errorf("constraint %q vs %s = %v, want %v\n  %s\n  parsed as Min=%q Max=%q Exact=%q",
				tc.constraint, target, got, tc.want, tc.why, r.Min, r.Max, r.Exact)
		}
	}
}

// The other half of an exact constraint: it must match its version and NOTHING
// else.
func TestAnExactConstraintExcludesEveryOtherVersion(t *testing.T) {
	for _, form := range []string{"1.0.0", "=1.0.0"} {
		r, err := parseConstraint(form)
		if err != nil {
			t.Fatalf("parseConstraint(%q): %v", form, err)
		}
		for _, other := range []string{"v0.9.9", "v1.0.1", "v1.1.0", "v2.0.0"} {
			if versionInRange(other, r) {
				t.Errorf("constraint %q matched %s, which it does not name", form, other)
			}
		}
		// "1.0.0" and "v1.0.0" are the same version arriving by different
		// routes -- the stored version from plugin_defs, the constraint through
		// ensureVPrefix. String equality would reject one of them.
		if !versionInRange("v1.0.0", r) {
			t.Errorf("constraint %q did not match v1.0.0, the version it names", form)
		}
	}
}

// The rival repair, and the case that rules it out.
//
// Making Max INCLUSIVE also makes "=1.0.0" match 1.0.0, and I assumed that
// meant the tests above would separate the two fixes. They do not -- measured,
// by applying that repair: both pass. The difference only shows at the upper
// bound, because an inclusive Max turns ^1.0.0 from [1.0.0, 2.0.0) into
// [1.0.0, 2.0.0] and ~1.0.0 from [1.0.0, 1.1.0) into [1.0.0, 1.1.0].
//
// So this is the test that discriminates, and without it the suite would have
// accepted a repair that fixes the reported bug by breaking every caret and
// tilde constraint in the tree.
func TestTheCaretAndTildeUpperBoundsStayExclusive(t *testing.T) {
	cases := []struct {
		constraint string
		excluded   string
		why        string
	}{
		{"^1.0.0", "v2.0.0", "caret is < next major: 2.0.0 is the bound, not a member"},
		{"~1.0.0", "v1.1.0", "tilde is < next minor: 1.1.0 is the bound, not a member"},
	}
	for _, tc := range cases {
		r, err := parseConstraint(tc.constraint)
		if err != nil {
			t.Fatalf("parseConstraint(%q): %v", tc.constraint, err)
		}
		if versionInRange(tc.excluded, r) {
			t.Errorf("constraint %q matched %s\n  %s\n\n"+
				"This is what making Max inclusive looks like. It repairs cleat#1243's "+
				"symptom and widens every caret and tilde range by one version, which "+
				"is why the exact forms get their own field instead.",
				tc.constraint, tc.excluded, tc.why)
		}
	}
}

// End to end through the entry point cleat#1243 names, rather than only the two
// helpers underneath it.
//
// The helper tests above would pass just as well if ResolvePlugin never called
// versionInRange -- and that is not hypothetical here: engine has a SECOND
// resolver, ResolvePlugins in plugin_resolver.go, which uses its own
// matchesConstraint and was never affected. A test on the helpers alone cannot
// tell you which of the two you fixed.
func TestResolvePluginReturnsTheVersionAnExactConstraintNames(t *testing.T) {
	for _, constraint := range []string{"1.0.0", "=1.0.0"} {
		db := multiRowDB([][]driver.Value{
			{"llm", "1.0.0", []byte{0}, []byte("{}"), time.Now(), false},
			{"llm", "2.0.0", []byte{0}, []byte("{}"), time.Now(), false},
		}, nil)
		l := &PluginLoader{db: db}

		version, def, err := l.ResolvePlugin(context.Background(), "llm", constraint)
		if err != nil {
			t.Errorf("ResolvePlugin(llm, %q) errored: %v\n\n"+
				"This is cleat#1243: the constraint names one version and excluded it, "+
				"so resolution failed against a database that holds it.", constraint, err)
			continue
		}
		if version != "1.0.0" {
			t.Errorf("ResolvePlugin(llm, %q) = %q, want 1.0.0 -- an exact constraint "+
				"must not resolve to a different version", constraint, version)
		}
		if def == nil {
			t.Errorf("ResolvePlugin(llm, %q) returned a nil definition alongside a version", constraint)
		}
	}
}
