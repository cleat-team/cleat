package engine

import (
	"encoding/json"
	"os"
	"testing"
)

// constraintRow is one cell of the plugin-constraint grammar: what each
// production-reachable matcher answers for a (version, constraint) pair.
//
// A field is either a bool or the string "err", meaning that matcher failed to
// PARSE the constraint. Refusing by error and refusing by false are different
// answers and the fixture keeps them apart -- several rows below differ only in
// that way, and collapsing them would hide a real difference in how a bad
// constraint surfaces to a caller.
type constraintRow struct {
	Version    string `json:"version"`
	Constraint string `json:"constraint"`
	Loader     any    `json:"loader"`
	Resolver   any    `json:"resolver"`
	Index      any    `json:"index"`
	Divergent  bool   `json:"divergent"`
	Note       string `json:"note"`
}

type constraintFixture struct {
	Rows []constraintRow `json:"rows"`
}

const constraintFixturePath = "testdata/constraint_grammar.json"

// TestPluginConstraintGrammarIsPinned holds the plugin-constraint grammar still.
// cleat#1554.
//
// WHAT THIS DOES NOT DO: it does not make the matchers agree. 14 of the 26 rows
// below are divergent and stay that way. Unifying them changes behaviour at
// every call site, and the issue names the reason that is dangerous --
// cmd/cleat/plugin_cmd.go:428 passes "" deliberately to mean "latest", so a
// majority vote among the matchers would overturn a caller that depends on it.
// cleat#1243 is the local cautionary case: a constraint defect shipped quietly
// and was then pinned as expected behaviour in a test before anyone noticed.
//
// So this records what each matcher answers TODAY, divergences included, and
// fails when any of them changes. The next divergence becomes a red test rather
// than something found by reading four implementations side by side.
//
// CALL EACH MATCHER THE WAY PRODUCTION CALLS IT. This is the whole difficulty
// and the first version of the probe got it wrong. versionInRange begins with
// `if !semver.IsValid(v)`, and golang.org/x/mod/semver requires a leading "v" --
// so a probe passing a bare "1.2.3" gets false for EVERY row and reports a
// spectacular fake divergence. Production never does that: both call sites
// ensureVPrefix first (plugin_loader.go:286, plugin/index.go:132). The fixture
// is generated through the same seam.
//
// The mirror of that trap: matchesConstraint is NOT a separate seam. Its only
// caller is VersionSatisfies, which short-circuits "" and "*" to true before
// reaching it, so calling it directly with "" reports a divergence that no
// caller can produce. It is tested here through VersionSatisfies, which is what
// cmd/cleat-worker/setup.go:4370 actually calls.
func TestPluginConstraintGrammarIsPinned(t *testing.T) {
	fx := loadConstraintFixture(t)

	for _, row := range fx.Rows {
		row := row
		t.Run(row.Version+" vs "+row.Constraint, func(t *testing.T) {
			// Matcher 1: the engine loader's parse+range pair.
			var loader any = "err"
			if cr, err := parseConstraint(row.Constraint); err == nil {
				loader = versionInRange(ensureVPrefix(row.Version), cr)
			}
			assertConstraintAnswer(t, "loader", loader, row.Loader, row)

			// Matcher 2: the exported resolver seam.
			assertConstraintAnswer(t, "resolver",
				VersionSatisfies(row.Version, row.Constraint), row.Resolver, row)
		})
	}
	t.Logf("grammar rows pinned: %d (%d divergent)", len(fx.Rows), countDivergent(fx.Rows))
}

func countDivergent(rows []constraintRow) int {
	n := 0
	for _, r := range rows {
		if r.Divergent {
			n++
		}
	}
	return n
}

// assertConstraintAnswer compares a measured answer against the fixture,
// normalising bool and the "err" sentinel through JSON's own types.
func assertConstraintAnswer(t *testing.T, which string, got, want any, row constraintRow) {
	t.Helper()
	gs, ws := answerString(got), answerString(want)
	if gs == ws {
		return
	}
	note := row.Note
	if note == "" {
		note = "(this row previously AGREED across all matchers, which makes a change here " +
			"more surprising than one on a row already known to diverge)"
	}
	t.Errorf("%s(%q, %q) = %s, fixture says %s\n\n"+
		"  note on this row: %s\n\n"+
		"This fixture records what the matchers DO, not what they should do. A change here means "+
		"one implementation moved; decide deliberately whether the others should follow, and "+
		"update testdata/constraint_grammar.json in the same commit as the behaviour, never "+
		"separately.",
		which, row.Version, row.Constraint, gs, ws, note)
}

func answerString(v any) string {
	switch t := v.(type) {
	case bool:
		if t {
			return "true"
		}
		return "false"
	case string:
		return t
	default:
		return "UNKNOWN"
	}
}

func loadConstraintFixture(t *testing.T) constraintFixture {
	t.Helper()
	b, err := os.ReadFile(constraintFixturePath)
	if err != nil {
		t.Fatalf("read %s: %v", constraintFixturePath, err)
	}
	var fx constraintFixture
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatalf("parse %s: %v", constraintFixturePath, err)
	}
	// UNMEASURED rather than a silent pass: an empty fixture would report a
	// clean run over nothing at all.
	if len(fx.Rows) == 0 {
		t.Fatal("UNMEASURED: the constraint fixture is empty, so this guard compared nothing")
	}
	return fx
}
