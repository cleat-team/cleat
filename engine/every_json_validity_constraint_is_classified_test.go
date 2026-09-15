package engine

import (
	"os"
	"regexp"
	"sort"
	"testing"
)

// Every JSON_VALID CHECK constraint the migrations create is classified as a
// JSON rejection. cleat#1022.
//
// WHY THIS IS A TEST AND NOT A COMMENT. jsonValidityConstraints is a
// hand-written list, and the failure mode of a hand-written list is that it
// stops matching the thing it mirrors -- silently, and in the direction that
// looks fine: an unlisted constraint does not break a write, it just makes a
// refusal arrive unclassified, as driver text with error_code "unknown". That
// is cleat#1460, which cost a whole issue to diagnose the first time.
//
// The list moved the validation of these columns from the column TYPE to a
// CHECK constraint, so MySQL's 3141/3157 (SQLSTATE 22032, "invalid JSON text")
// became 3819 (HY000, "check constraint violated"). Only one of the eight
// constraints is covered by an end-to-end test --
// TestAResultTheStoreRefusesIsClassifiedNotJustReported writes a result -- so
// without this the other seven are asserted by nothing.
//
// It reads the MIGRATION rather than the database, so it runs on every job with
// no DSN and cannot be skipped into uselessness.
func TestEveryJSONValidityConstraintIsClassified(t *testing.T) {
	const migration = "../migrations/mysql/070_a_callers_json_is_stored_as_text.sql"

	src, err := os.ReadFile(migration)
	if err != nil {
		t.Fatalf("reading %s: %v", migration, err)
	}

	// ADD CONSTRAINT <name> CHECK (... JSON_VALID(...) ...)
	re := regexp.MustCompile(`ADD CONSTRAINT\s+(\w+)\s+CHECK\s*\([^;]*?JSON_VALID`)
	found := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		found[m[1]] = true
	}

	// A pattern that matches nothing is the third outcome, and without this
	// branch it reads as "every constraint is classified" -- the strongest
	// possible pass, from a test that looked at nothing. See CLAUDE.md, "a
	// verification script needs its own negative control".
	if len(found) == 0 {
		t.Fatalf("the constraint pattern matched nothing in %s.\n\n"+
			"Either the migration was renamed or its ADD CONSTRAINT ... CHECK (JSON_VALID(...)) "+
			"form changed. This test cannot report anything about the classifier until the "+
			"pattern selects something, so it fails rather than passing vacuously.", migration)
	}

	var missing []string
	for name := range found {
		if !jsonValidityConstraints[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d JSON_VALID constraint(s) in %s are not in jsonValidityConstraints: %v\n\n"+
			"A refusal by one of these reaches the caller as raw driver text with error_code "+
			"\"unknown\", saying nothing about the RESULT being the problem, which field, or "+
			"that the workflow body already ran. That is cleat#1460 arriving through a side "+
			"door. Add them to jsonValidityConstraints in engine/store_result_rejection.go.",
			len(missing), migration, missing)
	}

	// The other direction. A stale entry is not as costly as a missing one, but
	// it is a claim about a constraint that no longer exists, and an exemption
	// that outlives its subject silently covers whatever arrives at that name
	// next.
	var stale []string
	for name := range jsonValidityConstraints {
		if !found[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("jsonValidityConstraints names %d constraint(s) that %s does not create: %v\n\n"+
			"If a column stopped being JSON-validated, drop the entry. If it moved to another "+
			"migration, point this test at that file too.", len(stale), migration, stale)
	}
}
