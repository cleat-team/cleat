package plugin

import (
	"encoding/json"
	"os"
	"testing"
)

// The index side of the plugin-constraint grammar. cleat#1554.
//
// The fixture and the reasoning live with the engine half, in
// engine/plugin_constraint_grammar_test.go -- read that first. This file exists
// because parseConstraint and versionInRange here are unexported and a test in
// package engine cannot reach them, which is the "it crosses engine/ and
// plugin/" constraint the issue names.
//
// ONE FIXTURE, DELIBERATELY, AND IT COSTS SOMETHING. The table is read through
// "../engine/testdata/", and `go test` computes its cache key from files opened
// INSIDE the package directory -- so editing the fixture does NOT invalidate
// this package's cached result. A local `go test ./plugin/` can report `(cached)`
// against a table that has changed underneath it.
//
// That is accepted rather than designed away, for a stated reason: two copies of
// a table whose whole purpose is to be the single record of what these matchers
// do would be the defect this issue is about, reproduced in the fixture. CI is
// unaffected -- ci.yml passes -count=1 to every matrix package. Locally, pass
// -count=1 when you touch the table, and treat the word `(cached)` where a
// duration should be as the tell.
const indexFixturePath = "../engine/testdata/constraint_grammar.json"

// The index column is coarser than the other two -- matched or not, with no
// parse-error/false distinction -- and that is deliberate: Resolve returns an
// error either way, so "did not resolve" is the whole of what a caller on this
// path can observe. Recording a distinction the seam cannot express would be
// recording the implementation rather than the behaviour.
type indexRow struct {
	Version    string `json:"version"`
	Constraint string `json:"constraint"`
	Index      any    `json:"index"`
	Note       string `json:"note"`
}

func TestPluginIndexConstraintGrammarIsPinned(t *testing.T) {
	b, err := os.ReadFile(indexFixturePath)
	if err != nil {
		t.Fatalf("read %s: %v", indexFixturePath, err)
	}
	var fx struct {
		Rows []indexRow `json:"rows"`
	}
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatalf("parse %s: %v", indexFixturePath, err)
	}
	if len(fx.Rows) == 0 {
		t.Fatal("UNMEASURED: the constraint fixture is empty, so this guard compared nothing")
	}

	for _, row := range fx.Rows {
		row := row
		t.Run(row.Version+" vs "+row.Constraint, func(t *testing.T) {
			// THROUGH Resolve, THE PRODUCTION SEAM -- not by re-implementing
			// its loop. The first version of this test did re-implement it,
			// copying the exact-then-range sequencing out of index.go, and a
			// known-positive proved it worthless: changing index.go's exact
			// comparison from string equality to semver.Compare -- the precise
			// divergence cleat#1554 is about -- left this test GREEN, because
			// the test was a mirror of production rather than a caller of it.
			//
			// Going through Resolve also corrected the table. Resolve
			// short-circuits "" and "latest" BEFORE parsing, so the empty
			// constraint matches here even though parseConstraint alone rejects
			// it; and it does NOT short-circuit "*", which is why "*" is a hard
			// rejection on this side and "any version" on the engine's.
			idx := &PluginIndex{Plugins: []IndexEntry{{
				Name:     "p",
				Versions: []IndexVersion{{Version: row.Version, WasmURL: "u", Checksum: "c"}},
			}}}
			_, v, err := idx.Resolve("p", row.Constraint)
			got := err == nil && v != nil

			if answerStr(got) == answerStr(row.Index) {
				return
			}
			note := row.Note
			if note == "" {
				note = "(this row previously agreed across all matchers, which makes a change " +
					"here more surprising than one on a row already known to diverge)"
			}
			t.Errorf("index.Resolve(%q, %q) matched = %v, fixture says %s\n\n"+
				"  note on this row: %s\n\n"+
				"The fixture records what the matchers DO, not what they should do. If this "+
				"changed deliberately, update engine/testdata/constraint_grammar.json in the "+
				"SAME commit as the behaviour -- and check whether the engine-side matchers "+
				"should follow, because the same table pins them.",
				row.Version, row.Constraint, got, answerStr(row.Index), note)
		})
	}
}

func answerStr(v any) string {
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
