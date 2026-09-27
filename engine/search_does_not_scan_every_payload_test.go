package engine

import (
	"strings"
	"testing"
)

// Search used to OR four columns -- input, result, error_msg and def_name --
// and two of those are JSONB cast to text, which no index covers.
//
// A DISJUNCTION IS ONLY AS INDEXABLE AS ITS WORST BRANCH, so every Search
// forced a sequential scan over the whole filter. It also dragged error_msg
// down with it: that column has carried a pg_trgm GIN index since migration
// 033, maintained on every write, and no Search could ever use it.
//
// These assert the generated SQL rather than query results, because the defect
// is not about which rows come back -- the old form returned a superset -- but
// about what the database has to read to find them. A results test passes
// against both.
func TestSearchDoesNotTouchThePayloadColumns(t *testing.T) {
	sql, args := buildFor(t, DialectPostgres, WorkflowFilter{Search: "boom"})

	for _, forbidden := range []string{"input", "result"} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("Search still reads %q: %s\n\n"+
				"That column is JSONB cast to text and no index covers it, so its "+
				"presence in the OR forces a sequential scan for every branch -- "+
				"including error_msg, which has had a trigram index since migration 033.",
				forbidden, sql)
		}
	}
	for _, want := range []string{"error_msg", "def_name"} {
		if !strings.Contains(sql, want) {
			t.Errorf("Search no longer covers %q: %s", want, sql)
		}
	}
	// Four placeholders became two. Counted by VALUE rather than by len(args),
	// because buildFor also applies paging and binds a LIMIT.
	if n := countArg(args, "%boom%"); n != 2 {
		t.Errorf("Search bound the pattern %d times, want 2 (error_msg, def_name): %v", n, args)
	}
}

// Payload search still exists -- it is explicit now rather than hidden inside
// the cheap question. Removing the capability would be a different change, and
// a worse one.
func TestPayloadSearchIsStillAvailableExplicitly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		filter WorkflowFilter
		column string
	}{
		{"input_contains", WorkflowFilter{InputContains: "x"}, "input"},
		{"result_contains", WorkflowFilter{ResultContains: "x"}, "result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sql, args := buildFor(t, DialectPostgres, tc.filter)
			if !strings.Contains(sql, tc.column) {
				t.Errorf("%s does not reach the %s column: %s", tc.name, tc.column, sql)
			}
			if n := countArg(args, "%x%"); n != 1 {
				t.Errorf("bound the pattern %d times, want 1: %v", n, args)
			}
		})
	}
}

// The two stay independent: asking for a payload scan must not re-widen Search,
// and Search must not silently perform one.
func TestSearchAndPayloadSearchCombineWithoutMerging(t *testing.T) {
	sql, args := buildFor(t, DialectPostgres, WorkflowFilter{Search: "boom", InputContains: "x"})
	if !strings.Contains(sql, "input") {
		t.Error("input_contains was dropped when combined with search")
	}
	if !strings.Contains(sql, "def_name") {
		t.Error("search was dropped when combined with input_contains")
	}
	if n := countArg(args, "%x%"); n != 1 {
		t.Errorf("input_contains bound its pattern %d times, want 1: %v", n, args)
	}
	if n := countArg(args, "%boom%"); n != 2 {
		t.Errorf("search bound its pattern %d times, want 2: %v", n, args)
	}
}

func countArg(args []any, want string) int {
	n := 0
	for _, a := range args {
		if s, ok := a.(string); ok && s == want {
			n++
		}
	}
	return n
}
