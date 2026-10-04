package catalogdiff

import "testing"

// The known-positive for the `collation=%s` token in canonicalize. cleat#2882.
//
// WHY THIS TEST EXISTS SEPARATELY FROM THE LIVE-DIALECT ONES. Those prove the
// snapshot READS a collation; this proves the comparison SEES one, and they are
// different failures. canonicalize's column line is the only thing that carries
// Column.Collation into a Diff, and a struct field the line does not render is
// carried by nothing -- which is written down in this package already, in
// mysql.go's autoIncrementAttribute comment, from the other side: a field
// canonicalize does not print "would look like a fix and change no
// comparison".
//
// So deleting `collation=%s` from that format string must redden this test. If
// it does not, the field is inert on every dialect and the live tests cannot
// say so, because they assert on the snapshot rather than on a difference.
func TestACollationChangeIsReported(t *testing.T) {
	withCollation := func(c string) *Catalog {
		return &Catalog{Tables: map[string]*Table{
			"public.t": {
				Name: "public.t",
				Columns: []Column{{
					Name:      "c",
					DataType:  "text",
					Nullable:  true,
					Collation: c,
				}},
			},
		}}
	}

	base := withCollation("en_US.utf8")

	if d := Diff(base, withCollation("C")); len(d) == 0 {
		t.Error("a column whose collation changed reported NO difference.\n\n" +
			"Column.Collation is populated by all three dialect snapshots and\n" +
			"rendered by the collation token on canonicalize's column line; if\n" +
			"that token is gone, a collation change is invisible to this\n" +
			"instrument on every dialect, which is the gap cleat#2882 closes.")
	}

	// The negative control. Without it the assertion above would pass against a
	// canonicalize that reports every column unconditionally, or against a Diff
	// that is simply never empty -- the failure mode
	// TestSnapshotIsIdenticalForTwoBuildsOfTheSameChain guards at the database
	// level rather than here.
	if d := Diff(base, withCollation("en_US.utf8")); len(d) != 0 {
		t.Errorf("two catalogs differing only in nothing reported: %v", d)
	}
}
