package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The defect cleat#1216 records is not that four table names were wrong. It is
// that a list of tables lived in Go with nothing tying it to the schema, so it
// could drift in either direction and nothing would say so. It had drifted in
// BOTH: four names no migration ever created, and ten real core tables it never
// looked at.
//
// Correcting the list does not fix that. This test does, and it is written to
// fail in both directions for the reason tiers.yaml gives about tracked
// failure lists: a list that is only checked one way is an allowlist, and an
// allowlist is how a guard stops guarding.
//
// CACHING. This reads migrations/ through `..`, and `go test` computes its
// cache key from files opened INSIDE the package directory -- so adding a
// migration does NOT invalidate this test locally and it will report `(cached)`
// against a schema it has not read. CI passes -count=1 to every package, so CI
// is sound; a local run needs -count=1 to mean anything. See cleat's CLAUDE.md,
// "a fixture read through `..` is invisible to it".
var createTableRE = regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_.]*)`)

// lineComment strips `--` comments. Without this, a migration whose HEADER
// quotes a CREATE TABLE -- several do, describing what they supersede --
// registers as creating that table. That is the "a text search cannot tell a
// thing from a sentence about the thing" trap from CLAUDE.md, and it would add
// phantom names to the expected set: exactly the bug being fixed, reintroduced
// by the test meant to prevent it.
var lineComment = regexp.MustCompile(`(?m)--.*$`)

func tablesCreatedByMigrations(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join("..", "..", "migrations", "postgres")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	seen := map[string]bool{}
	var files int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		files++
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		for _, m := range createTableRE.FindAllStringSubmatch(lineComment.ReplaceAllString(string(raw), ""), -1) {
			seen[strings.ToLower(m[1])] = true
		}
	}
	// A floor on the EXTRACTION, not on the file count, and the difference is
	// the lesson. This said `files < 30`, with the note that "the number of
	// migrations only ever goes UP, so a floor cannot rot the way the counts
	// CLAUDE.md warns about do". cleat#2059's rebaseline makes it go DOWN, to
	// three -- so the count rotted in the one direction the note promised was
	// impossible, and this guard failed on a correct tree.
	//
	// What has to be true is that the scan PRODUCED something. If the path or
	// the suffix filter breaks, the extracted set is empty and the comparison
	// below blames the list for every entry; this blames the reader. It cannot
	// rot: it asks whether the extraction worked, not how many files exist today.
	if files == 0 || len(seen) == 0 {
		t.Fatalf("read %d .sql file(s) from %s and extracted %d table name(s): the "+
			"extraction is broken, not the list", files, dir, len(seen))
	}
	var out []string
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func TestCoreTablesMatchTheMigrations(t *testing.T) {
	want := tablesCreatedByMigrations(t)

	got := append([]string(nil), coreTables...)
	for i := range got {
		got[i] = strings.ToLower(got[i])
	}
	sort.Strings(got)

	inWant := map[string]bool{}
	for _, n := range want {
		inWant[n] = true
	}
	inGot := map[string]bool{}
	for _, n := range got {
		inGot[n] = true
	}

	// Reported as two SETS rather than as a count difference. A count says a
	// number moved; the membership says which way, and the two failures need
	// opposite fixes -- add the table to coreTables, or delete a name that no
	// migration creates.
	var missing, extra []string
	for _, n := range want {
		if !inGot[n] {
			missing = append(missing, n)
		}
	}
	for _, n := range got {
		if !inWant[n] {
			extra = append(extra, n)
		}
	}

	if len(missing) > 0 {
		t.Errorf("migrations/postgres creates %d table(s) that coreTables omits, so "+
			"check-db reports on a subset of the schema and says nothing about the rest:\n  %s\n"+
			"Add them to coreTables.", len(missing), strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("coreTables names %d table(s) that no migration creates, so check-db "+
			"reports them MISSING on every correct database:\n  %s\n"+
			"This is cleat#1216 recurring. Delete them, or point them at the real table.",
			len(extra), strings.Join(extra, "\n  "))
	}
}
