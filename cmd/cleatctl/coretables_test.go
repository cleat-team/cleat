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
	return foldPartitionChildren(out)
}

// partitionChildRE matches the name the baseline's generator gives a partition:
// <parent>_p<digits>.
var partitionChildRE = regexp.MustCompile(`^(.*)_p[0-9]+$`)

// foldPartitionChildren removes entries that are partition children of a table
// already in the same set, so a partitioned table is counted once.
//
// A PARTITION IS NOT A SEPARATE TABLE, and this file is the third place that had
// to learn it. Hash-partitioning event_history into 64 children (cleat#2059) put
// 65 `CREATE TABLE` statements in 001_schema.sql -- the parent and its children
// -- and `information_schema` lists partitions as tables, so every enumeration
// of "the tables this schema has" grew by 64 entries for one table:
//
//   - engine/the_documented_tenant_coverage_is_measured_test.go  (RLS on 81, not 17)
//   - engine/a_dropped_tenants_rows_all_go_with_it_test.go       (88 in the universe)
//   - here, in both of this package's table enumerations
//
// The first two each folded it locally, which is what let the third arrive on CI
// with the PR already under review. Hence one helper, used by everything in this
// package that enumerates tables -- a fourth copy is the thing to avoid, not a
// fourth bug to fix.
//
// Folded only when the PARENT is in the set too, so the rule is "a partition of
// a table I already know about", not "a name ending in _p<digits>". A real table
// named foo_p1 with no foo stays counted as itself.
func foldPartitionChildren(names []string) []string {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	// A fresh slice, NOT `names[:0]`. Filtering in place returns the right
	// answer and overwrites the caller's own backing array, which is invisible
	// until a caller keeps the argument -- and this helper exists to be reused,
	// so the third caller is the one that would find it. Tested by
	// TestFoldPartitionChildren asserting the input is unchanged.
	out := make([]string, 0, len(names))
	for _, n := range names {
		if parent := partitionChildRE.ReplaceAllString(n, "$1"); parent != n && set[parent] {
			continue
		}
		out = append(out, n)
	}
	return out
}

// TestFoldPartitionChildren pins the helper's NEGATIVE case, which is the half
// its comments assert and nothing exercised.
//
// The positive fold -- 81 tables read as 17, 88 read as 24 -- is covered
// incidentally by the tests that call it, because those are the failures that
// brought the helper into existence. But nothing anywhere constructs a foo_p1
// with no foo, so deleting the `set[parent]` condition leaves every one of those
// tests GREEN while the rule they rely on is gone. That is the failure this
// commit is itself an instance of: a local fix that generalised to nothing,
// recurring in a third package. Testing the helper makes it durable; testing
// only its positive case would not.
func TestFoldPartitionChildren(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want []string
		why  string
	}{
		{
			name: "children fold into a parent that is present",
			in:   []string{"event_history", "event_history_p0", "event_history_p63", "workflow_tags"},
			want: []string{"event_history", "workflow_tags"},
			why:  "the case the helper exists for",
		},
		{
			name: "an ORPHAN partition-looking name is kept",
			in:   []string{"event_history", "foo_p1"},
			want: []string{"event_history", "foo_p1"},
			why: "the condition -- a partition OF a table I know about, not a name " +
				"ending in _p<digits>. Delete `set[parent]` and this is the only " +
				"case that notices",
		},
		{
			name: "an orphan alone is kept, with no parent anywhere",
			in:   []string{"foo_p1", "bar"},
			want: []string{"foo_p1", "bar"},
			why:  "same condition, when the parent is absent from the set entirely",
		},
		{
			name: "a schema-qualified parent folds its child",
			in:   []string{"admin.tenants", "admin.tenants_p0"},
			want: []string{"admin.tenants"},
			why: "callers pass qualified names too, and the regex is not anchored to " +
				"a bare identifier",
		},
		{
			name: "digits only: foo_p is not a child",
			in:   []string{"foo_p", "foo_px"},
			want: []string{"foo_p", "foo_px"},
			why:  "the suffix must be digits; a name that merely starts like one is kept",
		},
		{
			name: "empty input",
			in:   nil,
			want: []string{},
			why:  "no panic, and nothing invented",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := foldPartitionChildren(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("foldPartitionChildren(%v) = %v, want %v\n%s", tc.in, got, tc.want, tc.why)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("foldPartitionChildren(%v) = %v, want %v\n%s", tc.in, got, tc.want, tc.why)
				}
			}
		})
	}

	// The input must survive. Filtering in place would pass every case above and
	// silently rewrite whatever the caller still holds.
	//
	// EVERY element is compared, not one of them. The first draft checked only
	// the last index, and filtering in place rewrites the kept elements from
	// index 0 -- so the corruption landed at index 1 and the assertion passed
	// against the very implementation it exists to reject. Measured: reverting
	// to `names[:0]` with the one-index check gives `ok`. A control that cannot
	// disagree is not a control.
	in := []string{"event_history", "event_history_p0", "keep_me"}
	want := append([]string(nil), in...)
	_ = foldPartitionChildren(in)
	for i := range want {
		if in[i] != want[i] {
			t.Errorf("foldPartitionChildren overwrote its argument at %d: got %q, want %q "+
				"(whole slice now %v). A caller that keeps the slice it passed sees its own "+
				"data change.", i, in[i], want[i], in)
		}
	}
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
