package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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
	// THE WHOLE DIRECTORY, not one file. This used to read
	// 070_a_callers_json_is_stored_as_text.sql by name, which cleat#2433's
	// compaction folded into 001_schema.sql and deleted -- so the test failed
	// on a missing file rather than on anything about the classifier. Scanning
	// the directory also means a FUTURE migration that adds a JSON_VALID
	// constraint is covered, which naming one file could never do.
	const migrationDir = "../migrations/mysql"

	entries, err := os.ReadDir(migrationDir)
	if err != nil {
		t.Fatalf("reading %s: %v", migrationDir, err)
	}
	var sb strings.Builder
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(migrationDir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		sb.Write(b)
		sb.WriteString("\n")
	}
	src := sb.String()

	// Two forms, because a compaction changes which one the tree uses:
	//
	//	ALTER TABLE t ADD CONSTRAINT <name> CHECK (... JSON_VALID ...)   (a migration)
	//	CREATE TABLE t (..., CONSTRAINT <name> CHECK (... JSON_VALID ...)) (a generated baseline)
	//
	// THE MATCH MUST NOT LEAVE ITS OWN CONSTRAINT, and that is not something
	// the pattern can say here. With a plain `CONSTRAINT\s+(\w+)\s+CHECK\s*\(
	// [^;]*?JSON_VALID`, nothing stops the window from running past one
	// constraint and reaching a LATER JSON_VALID inside the same CREATE TABLE
	// -- SQL puts no semicolon between them. Measured against the compacted
	// 001, that reports 8 constraints of which one is
	// ck_wi_run_instance_timeout_positive, a TIMEOUT constraint with nothing to
	// do with JSON, and the missing-entry branch below then fails on an entry
	// the tree never claimed.
	//
	// The obvious fix is a negative lookahead, and Go cannot take it: regexp is
	// RE2, which has no lookaround, and the first version of this line panicked
	// with "invalid or unsupported Perl syntax: `(?!`" -- a pattern written and
	// validated in Python, pasted into a language whose engine is not the same
	// one. So the constraint's own CHECK expression is taken by matching
	// parentheses instead, which is exact rather than a window that hopes.
	nameRe := regexp.MustCompile(`CONSTRAINT\s+(\w+)\s+CHECK\s*\(`)
	found := map[string]bool{}
	for _, m := range nameRe.FindAllStringSubmatchIndex(src, -1) {
		name := src[m[2]:m[3]]
		expr, ok := balancedParens(src, m[1]-1)
		if !ok {
			t.Fatalf("unbalanced parentheses in the CHECK for %s", name)
		}
		if strings.Contains(strings.ToUpper(expr), "JSON_VALID") {
			found[name] = true
		}
	}

	// A pattern that matches nothing is the third outcome, and without this
	// branch it reads as "every constraint is classified" -- the strongest
	// possible pass, from a test that looked at nothing. See CLAUDE.md, "a
	// verification script needs its own negative control".
	if len(found) == 0 {
		t.Fatalf("the constraint pattern matched nothing under %s.\n\n"+
			"Either the migration was renamed or its CONSTRAINT ... CHECK (JSON_VALID(...)) "+
			"form changed. This test cannot report anything about the classifier until the "+
			"pattern selects something, so it fails rather than passing vacuously.", migrationDir)
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
			len(missing), migrationDir, missing)
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
			"migration, this scan already covers it.", len(stale), migrationDir, stale)
	}
}

// balancedParens returns src[i:] up to and including the parenthesis that
// closes the one at src[i], and whether such a parenthesis was found. Used to
// isolate one CHECK expression, because Go's regexp (RE2) has no lookahead to
// stop a match window at the next CONSTRAINT.
func balancedParens(src string, i int) (string, bool) {
	if i >= len(src) || src[i] != '(' {
		return "", false
	}
	depth := 0
	for j := i; j < len(src); j++ {
		switch src[j] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return src[i : j+1], true
			}
		}
	}
	return "", false
}
