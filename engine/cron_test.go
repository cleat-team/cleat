package engine

import (
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Day-of-month / day-of-week: the OR rule
// ---------------------------------------------------------------------------

// TestNextCronTime_DomAndDowAreOredWhenBothRestricted is the regression test
// for the rule stated on cronExpr.dayMatches.
//
// `0 0 13 * 5` means "midnight on the 13th, and midnight every Friday". The
// previous evaluator ANDed the two fields, so it meant "midnight on Friday the
// 13th" -- and the difference is not subtle. From 2024-06-10 the correct next
// firing is three days later; the ANDed reading skips a full quarter to
// 2024-09-13, the next Friday that happens to be a 13th.
//
// Both dates are asserted, so this test fails whichever way the rule is broken
// rather than only detecting one direction.
func TestNextCronTime_DomAndDowAreOredWhenBothRestricted(t *testing.T) {
	from := time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC) // a Monday
	got := NextCronTime("0 0 13 * 5", from)

	want := time.Date(2024, 6, 13, 0, 0, 0, 0, time.UTC) // Thursday the 13th
	if !got.Equal(want) {
		t.Errorf("NextCronTime(%q, %s) = %s, want %s",
			"0 0 13 * 5", from.Format(time.RFC3339), got.Format(time.RFC3339), want.Format(time.RFC3339))
	}

	andWouldGive := time.Date(2024, 9, 13, 0, 0, 0, 0, time.UTC) // Friday the 13th
	if got.Equal(andWouldGive) {
		t.Errorf("day-of-month and day-of-week were ANDed: got %s, the next Friday-the-13th, "+
			"instead of %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestNextCronTime_DomAndDowOrIncludesTheWeekdayArm checks the other half of
// the OR. Starting the day after the 13th, the next firing must come from the
// day-of-week arm.
func TestNextCronTime_DomAndDowOrIncludesTheWeekdayArm(t *testing.T) {
	from := time.Date(2024, 6, 13, 0, 1, 0, 0, time.UTC) // just past the 13th's firing
	got := NextCronTime("0 0 13 * 5", from)
	want := time.Date(2024, 6, 14, 0, 0, 0, 0, time.UTC) // the next Friday
	if !got.Equal(want) {
		t.Errorf("NextCronTime = %s, want %s (the day-of-week arm)",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestNextCronTime_OnlyDomRestricted: with day-of-week left as `*`, the
// day-of-month field alone decides. If the OR were applied unconditionally,
// every day would match and this would return 2024-06-11.
func TestNextCronTime_OnlyDomRestricted(t *testing.T) {
	from := time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC)
	got := NextCronTime("0 0 13 * *", from)
	want := time.Date(2024, 6, 13, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("NextCronTime = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestNextCronTime_OnlyDowRestricted is the mirror of the above.
func TestNextCronTime_OnlyDowRestricted(t *testing.T) {
	from := time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC) // Monday
	got := NextCronTime("0 0 * * 5", from)
	want := time.Date(2024, 6, 14, 0, 0, 0, 0, time.UTC) // Friday
	if !got.Equal(want) {
		t.Errorf("NextCronTime = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestNextCronTime_StarSlashCountsAsStarForTheDayRule pins the Vixie/robfig
// convention that `*/2` carries the star flag. With day-of-month `*/2` treated
// as a star, day-of-week decides alone; if it were treated as a restriction,
// the two fields would be ORed and every Friday would match too.
func TestNextCronTime_StarSlashCountsAsStarForTheDayRule(t *testing.T) {
	// 2024-06-10 is a Monday. Days 11, 13, 15... match `*/2` (odd days),
	// 14 is the Friday.
	from := time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC)
	got := NextCronTime("0 0 */2 * 5", from)
	want := time.Date(2024, 6, 14, 0, 0, 0, 0, time.UTC) // Friday, from the dow arm alone
	if !got.Equal(want) {
		t.Errorf("NextCronTime = %s, want %s (day-of-month `*/2` should carry the star flag, "+
			"leaving day-of-week to decide alone)", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// ---------------------------------------------------------------------------
// Day-of-week 7
// ---------------------------------------------------------------------------

// TestNextCronTime_DayOfWeekSevenIsSunday: 0 and 7 both mean Sunday. The
// previous evaluator capped the field at 6 and compared against time.Weekday(),
// so a schedule written `0 0 * * 7` matched no day at all and simply never
// fired.
func TestNextCronTime_DayOfWeekSevenIsSunday(t *testing.T) {
	from := time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC) // Monday
	want := time.Date(2024, 6, 16, 0, 0, 0, 0, time.UTC) // the following Sunday

	for _, expr := range []string{"0 0 * * 0", "0 0 * * 7"} {
		got := NextCronTime(expr, from)
		if !got.Equal(want) {
			t.Errorf("NextCronTime(%q) = %s, want %s", expr, got.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	}
}

// ---------------------------------------------------------------------------
// Step on a range
// ---------------------------------------------------------------------------

func TestParseCronField_StepOnRange(t *testing.T) {
	f, err := parseCronField("10-20/5", 0, 59, "minute")
	if err != nil {
		t.Fatalf("parseCronField(10-20/5): %v", err)
	}
	for _, v := range []int{10, 15, 20} {
		if !f.matches(v) {
			t.Errorf("10-20/5 should match %d", v)
		}
	}
	for _, v := range []int{9, 11, 14, 21, 25} {
		if f.matches(v) {
			t.Errorf("10-20/5 should not match %d", v)
		}
	}
}

func TestParseCronField_ListOfMixedTerms(t *testing.T) {
	f, err := parseCronField("0,10-12,*/30", 0, 59, "minute")
	if err != nil {
		t.Fatalf("parseCronField: %v", err)
	}
	for _, v := range []int{0, 10, 11, 12, 30} {
		if !f.matches(v) {
			t.Errorf("0,10-12,*/30 should match %d", v)
		}
	}
	for _, v := range []int{1, 13, 29, 31} {
		if f.matches(v) {
			t.Errorf("0,10-12,*/30 should not match %d", v)
		}
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func TestValidateCronExpr_Accepts(t *testing.T) {
	valid := []string{
		"* * * * *",
		"0 0 * * *",
		"*/15 * * * *",
		"30 8,17 * * 1-5",
		"0 0 13 * 5",
		"0 0 29 2 *", // fires once every four years, but is well-formed
		"10-20/5 * * * *",
		"0 0 * * 7",
		"  0   0  *  *  * ", // extra whitespace between fields
	}
	for _, expr := range valid {
		if err := ValidateCronExpr(expr); err != nil {
			t.Errorf("ValidateCronExpr(%q) = %v, want nil", expr, err)
		}
	}
}

// TestValidateCronExpr_Rejects covers the inputs that used to be accepted
// silently. "abc * * * *" is the headline: fmt.Sscanf left 0 in the
// destination, so it ran at minute zero of every hour forever.
func TestValidateCronExpr_Rejects(t *testing.T) {
	tests := []struct {
		expr        string
		wantErrPart string
	}{
		{"abc * * * *", "not a number"},
		{"invalid", "want 5"},
		{"* * * *", "want 5"},
		{"* * * * * *", "want 5"},
		{"", "want 5"},
		{"60 * * * *", "outside the allowed range"},
		{"* 24 * * *", "outside the allowed range"},
		{"* * 0 * *", "outside the allowed range"},
		{"* * 32 * *", "outside the allowed range"},
		{"* * * 13 *", "outside the allowed range"},
		{"* * * * 8", "outside the allowed range"},
		{"*/0 * * * *", "step must be at least 1"},
		{"*/-1 * * * *", "step must be at least 1"},
		{"*/x * * * *", "not a number"},
		{"20-10 * * * *", "inverted"},
		{"1,,2 * * * *", "empty term"},
	}
	for _, tt := range tests {
		err := ValidateCronExpr(tt.expr)
		if err == nil {
			t.Errorf("ValidateCronExpr(%q) = nil, want an error", tt.expr)
			continue
		}
		if !strings.Contains(err.Error(), tt.wantErrPart) {
			t.Errorf("ValidateCronExpr(%q) = %q, want it to mention %q", tt.expr, err, tt.wantErrPart)
		}
	}
}

// TestValidateCronExpr_NamesTheOffendingField: a diagnostic that does not say
// which field was wrong is barely better than a boolean, because a five-field
// expression gives the reader five places to look.
func TestValidateCronExpr_NamesTheOffendingField(t *testing.T) {
	tests := []struct {
		expr      string
		wantField string
	}{
		{"99 * * * *", "minute"},
		{"* 99 * * *", "hour"},
		{"* * 99 * *", "day-of-month"},
		{"* * * 99 *", "month"},
		{"* * * * 99", "day-of-week"},
	}
	for _, tt := range tests {
		err := ValidateCronExpr(tt.expr)
		if err == nil {
			t.Fatalf("ValidateCronExpr(%q) = nil, want an error", tt.expr)
		}
		if !strings.Contains(err.Error(), tt.wantField) {
			t.Errorf("ValidateCronExpr(%q) = %q, want it to name the %s field", tt.expr, err, tt.wantField)
		}
	}
}

// TestNextCronTime_UnparseableStillFallsBackToDaily documents that the fallback
// survives this change on purpose: NextCronTime cannot report an error, and
// re-timing every already-stored unparseable schedule would be a worse
// outcome than leaving them on the daily fallback they already had.
// ValidateCronExpr is what keeps new ones from getting there.
func TestNextCronTime_UnparseableStillFallsBackToDaily(t *testing.T) {
	from := time.Date(2024, 6, 10, 10, 30, 0, 0, time.UTC)
	got := NextCronTime("abc * * * *", from)
	if want := from.Add(24 * time.Hour); !got.Equal(want) {
		t.Errorf("NextCronTime(unparseable) = %s, want the daily fallback %s",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestNextCronTime_ValidButNeverMatching: February 30th parses fine and can
// never occur. The four-year search runs out and yields the same fallback.
func TestNextCronTime_ValidButNeverMatching(t *testing.T) {
	if err := ValidateCronExpr("0 0 30 2 *"); err != nil {
		t.Fatalf("0 0 30 2 * should parse: %v", err)
	}
	from := time.Date(2024, 6, 10, 10, 30, 0, 0, time.UTC)
	got := NextCronTime("0 0 30 2 *", from)
	if want := from.Add(24 * time.Hour); !got.Equal(want) {
		t.Errorf("NextCronTime(0 0 30 2 *) = %s, want the fallback %s",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestNextCronTime_LeapDay is the reason the search window is four years and
// not one.
func TestNextCronTime_LeapDay(t *testing.T) {
	from := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	got := NextCronTime("0 0 29 2 *", from)
	want := time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("NextCronTime(0 0 29 2 *) = %s, want %s",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestMatchField_UnparseablePatternMatchesNothing pins the behaviour change in
// matchField: a pattern that does not parse now matches nothing, where it used
// to match 0 because that is what fmt.Sscanf left behind.
func TestMatchField_UnparseablePatternMatchesNothing(t *testing.T) {
	if matchField("not-a-number", 0, 0, 59) {
		t.Error("matchField('not-a-number', 0) should be false, not true-because-Sscanf-left-a-zero")
	}
	if matchField("", 0, 0, 59) {
		t.Error("matchField('', 0) should be false")
	}
}
