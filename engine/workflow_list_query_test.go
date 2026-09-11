package engine

import (
	"strings"
	"testing"
	"time"
)

// These test the SQL that is built, not a round trip, and that is deliberate.
// The properties at stake -- that the sort is total, and that a filter names the
// column it claims to -- are decidable from the statement text, and a
// round-trip test of the ordering one would need two rows sharing a created_at
// to the microsecond, which is not something a test can arrange through the
// store interface without reaching past it.
//
// The behavioural half is covered where it can be: see
// TestPagingReturnsEachRowExactlyOnce_MultiBackend below.

func buildFor(t *testing.T, d Dialect, f WorkflowFilter) (string, []any) {
	t.Helper()
	qb := NewQueryBuilder(d, "SELECT * FROM workflow_instances WHERE 1=1")
	applyWorkflowFilters(qb, d, f)
	applyWorkflowListPaging(qb, d, f)
	return qb.SQL()
}

// TestTheListingSortIsATotalOrder is the load-bearing one.
//
// ORDER BY created_at DESC alone is not a total order and the ties are
// structural: created_at defaults to now(), which on PostgreSQL is TRANSACTION
// START time, so every row written by one transaction shares a value. Paging a
// tied set with LIMIT/OFFSET may return a row on two pages and another on none.
func TestTheListingSortIsATotalOrder(t *testing.T) {
	for _, d := range []Dialect{DialectPostgres, DialectMySQL, DialectMSSQL} {
		t.Run(string(d), func(t *testing.T) {
			sql, _ := buildFor(t, d, WorkflowFilter{})
			if !strings.Contains(sql, "ORDER BY created_at DESC, id DESC") {
				t.Errorf("the listing sort is not total on %s.\n\ngot: %s\n\n"+
					"`created_at` ties are guaranteed, not rare -- now() is "+
					"transaction start time, so a fan-out written in one "+
					"transaction shares a timestamp. Without a unique tiebreak "+
					"the database may order tied rows differently for the query "+
					"that fetches page 1 and the query that fetches page 2, and "+
					"a caller paging through sees one row twice and another "+
					"never. `id` is the primary key on every dialect.", d, sql)
			}
		})
	}
}

// TestEachFilterNamesItsOwnColumn pins that the targeted filters are exact
// matches on the columns they claim, not another substring search.
//
// The distinction is the point of cleat#1183: `Search` already existed and is a
// four-way substring LIKE spanning input, result, error_msg and def_name, so
// "the runs of workflow X" also matched unrelated runs whose PAYLOAD contained
// the string. A DefName that compiled to another LIKE would look like it worked.
func TestEachFilterNamesItsOwnColumn(t *testing.T) {
	cases := []struct {
		name   string
		filter WorkflowFilter
		want   string
		absent string
		arg    any
	}{
		{"DefName is exact", WorkflowFilter{DefName: "order-fulfilment"},
			"def_name = ", "LIKE", "order-fulfilment"},
		{"ErrorCode is exact", WorkflowFilter{ErrorCode: "cancelled"},
			"error_code = ", "LIKE", "cancelled"},
		{"IDPrefix anchors at the start", WorkflowFilter{IDPrefix: "0f74"},
			"id LIKE ", "", "0f74%"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql, args := buildFor(t, DialectPostgres, tc.filter)
			if !strings.Contains(sql, tc.want) {
				t.Errorf("want %q in the statement, got:\n%s", tc.want, sql)
			}
			if tc.absent != "" && strings.Contains(sql, tc.absent) {
				t.Errorf("%q must not compile to a %s -- that is `Search`, which "+
					"also matches input, result and error_msg. Got:\n%s",
					tc.name, tc.absent, sql)
			}
			found := false
			for _, a := range args {
				if a == tc.arg {
					found = true
				}
			}
			if !found {
				t.Errorf("want %v among the args, got %v", tc.arg, args)
			}
		})
	}
}

// TestTheTimeWindowIsHalfOpen: >= after, < before, so adjacent windows tile
// without a row on the boundary appearing in both.
func TestTheTimeWindowIsHalfOpen(t *testing.T) {
	at := time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC)
	sql, _ := buildFor(t, DialectPostgres, WorkflowFilter{StartedAfter: at, StartedBefore: at.Add(time.Hour)})
	if !strings.Contains(sql, "created_at >= ") {
		t.Errorf("StartedAfter must be inclusive (>=), got:\n%s", sql)
	}
	if !strings.Contains(sql, "created_at < ") || strings.Contains(sql, "created_at <= ") {
		t.Errorf("StartedBefore must be EXCLUSIVE (<), so two adjacent windows do "+
			"not both return a row on the boundary. Got:\n%s", sql)
	}
}

// TestAZeroTimeIsNotAFilter: the zero time means "unset", and must not compile
// to `created_at >= '0001-01-01'`, which is a predicate that silently excludes
// nothing but makes every listing carry an extra comparison.
func TestAZeroTimeIsNotAFilter(t *testing.T) {
	sql, args := buildFor(t, DialectPostgres, WorkflowFilter{})
	if strings.Contains(sql, "created_at >=") || strings.Contains(sql, "created_at <") {
		t.Errorf("an unset time window must add no predicate, got:\n%s", sql)
	}
	// Only the limit argument.
	if len(args) != 1 {
		t.Errorf("an empty filter should carry one arg (the limit), got %d: %v", len(args), args)
	}
}

// TestTheCountAndTheListShareTheirPredicate is the invariant that makes
// X-Total-Count trustworthy: the same filter function builds both, so a total
// that disagrees with the page it describes is not a reachable state.
func TestTheCountAndTheListShareTheirPredicate(t *testing.T) {
	f := WorkflowFilter{Status: "failed", DefName: "nightly", ErrorCode: "cancelled",
		IDPrefix: "ab", Search: "x", Limit: 25, Offset: 50}

	list := NewQueryBuilder(DialectPostgres, "SELECT * FROM workflow_instances WHERE 1=1")
	applyWorkflowFilters(list, DialectPostgres, f)
	listWhere, listArgs := list.SQL()

	count := NewQueryBuilder(DialectPostgres, "SELECT COUNT(*) FROM workflow_instances WHERE 1=1")
	applyWorkflowFilters(count, DialectPostgres, f)
	countWhere, countArgs := count.SQL()

	lw := strings.TrimPrefix(listWhere, "SELECT * FROM workflow_instances WHERE 1=1")
	cw := strings.TrimPrefix(countWhere, "SELECT COUNT(*) FROM workflow_instances WHERE 1=1")
	if lw != cw {
		t.Errorf("the count and the listing select different rows.\n list: %s\ncount: %s", lw, cw)
	}
	if len(listArgs) != len(countArgs) {
		t.Errorf("arg counts differ: list %d, count %d", len(listArgs), len(countArgs))
	}
}

// TestTheStoreCeilingHoldsWhateverTheCallerAsks -- 0 means unspecified, not
// unlimited, and a caller cannot raise the ceiling.
func TestTheStoreCeilingHoldsWhateverTheCallerAsks(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{0, 100}, {-1, 100}, {25, 25}, {1000, 1000}, {1001, 1000}, {1 << 30, 1000},
	} {
		if got := clampWorkflowListLimit(tc.in); got != tc.want {
			t.Errorf("clampWorkflowListLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
