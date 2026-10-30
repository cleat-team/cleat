package tenantdrop

import "testing"

// A table a drop keeps is never also a table it removes (cleat#3552, B3b-2). The two lists are the plan's
// statement of what happens to a tenant's rows, and a table on both would make the plan say two things.
func TestNoRetainedTableIsAlsoRemoved(t *testing.T) {
	if len(Retained) == 0 {
		t.Fatal("Retained is empty: the tombstone is the record of the drop and is kept by it")
	}
	removed := map[string]bool{}
	for _, tb := range PostgresTables {
		removed[tb.Label] = true
	}
	for _, r := range Retained {
		if r.Reason == "" {
			t.Errorf("%s is retained with no reason; a reader is owed one", r.Table)
		}
		if removed[r.Table] {
			t.Errorf("%s is on both lists: PostgresTables removes it and Retained keeps it", r.Table)
		}
		if !IsRetained(r.Table) {
			t.Errorf("IsRetained(%q) = false for a table in Retained", r.Table)
		}
	}
	// The negative control: a table the drop does remove is not retained, so IsRetained is not "always true".
	for _, tb := range PostgresTables {
		if IsRetained(tb.Label) {
			t.Errorf("IsRetained(%q) = true for a table the drop removes", tb.Label)
		}
	}
}
