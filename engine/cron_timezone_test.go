package engine

import (
	"testing"
	"time"
)

// These tests all use America/New_York, which has both kinds of DST
// transition on well-known dates:
//
//	2024-03-10  01:59:59 EST -> 03:00:00 EDT   (02:00-02:59 never happens)
//	2024-11-03  01:59:59 EDT -> 01:00:00 EST   (01:00-01:59 happens twice)
//
// If the host has no zoneinfo database, LoadLocation fails and there is
// nothing to test -- but that is an environmental precondition, not a
// behaviour, so it is a legitimate skip. cmd/cleat-worker embeds tzdata so the
// shipped binary does not depend on this.
func newYork(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no zoneinfo database on this host: %v", err)
	}
	return loc
}

// TestNextCronTimeIn_SameExpressionDifferentZones is the defect this whole
// change is about: before the schedule carried a timezone, the expression was
// evaluated in whatever zone the worker process happened to run in, so two
// workers in a fleet computed different firing times for the same schedule.
func TestNextCronTimeIn_SameExpressionDifferentZones(t *testing.T) {
	ny := newYork(t)
	from := time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC)

	utcNext := NextCronTimeIn("0 7 * * *", from, time.UTC)
	nyNext := NextCronTimeIn("0 7 * * *", from, ny)

	if utcNext.Equal(nyNext) {
		t.Fatalf("07:00 UTC and 07:00 New York resolved to the same instant (%s); "+
			"the location argument is not being used", utcNext.Format(time.RFC3339))
	}
	// June: New York is UTC-4, so 07:00 local is 11:00 UTC.
	wantUTC := time.Date(2024, 6, 10, 7, 0, 0, 0, time.UTC)
	wantNY := time.Date(2024, 6, 10, 11, 0, 0, 0, time.UTC)
	if !utcNext.Equal(wantUTC) {
		t.Errorf("UTC: got %s, want %s", utcNext.UTC().Format(time.RFC3339), wantUTC.Format(time.RFC3339))
	}
	if !nyNext.Equal(wantNY) {
		t.Errorf("New York: got %s, want %s", nyNext.UTC().Format(time.RFC3339), wantNY.Format(time.RFC3339))
	}
}

// TestNextCronTimeIn_SpringForwardFiresAtTheTransition covers the wall time
// that does not exist.
//
// `30 2 * * *` on 2024-03-10 in New York asks for 02:30, and the clock jumps
// straight from 01:59:59 EST to 03:00:00 EDT. The promised rule is that the
// firing moves forward to the first instant that exists -- 03:00 EDT, 07:00
// UTC.
//
// The value this guards against is 06:30 UTC. That is 01:30 EST, an hour
// EARLIER than asked for, and it is what time.Date returns for a nonexistent
// wall time: it normalises backwards, not forwards. Reading that value
// straight out of time.Date would fire the job early, once a year.
func TestNextCronTimeIn_SpringForwardFiresAtTheTransition(t *testing.T) {
	ny := newYork(t)
	from := time.Date(2024, 3, 10, 0, 0, 0, 0, ny) // local midnight, before the jump
	got := NextCronTimeIn("30 2 * * *", from, ny)

	want := time.Date(2024, 3, 10, 7, 0, 0, 0, time.UTC) // 03:00 EDT
	if !got.Equal(want) {
		t.Errorf("got %s (%s local), want %s (03:00 EDT)",
			got.UTC().Format(time.RFC3339), got.In(ny).Format("15:04 MST"), want.Format(time.RFC3339))
	}

	backwards := time.Date(2024, 3, 10, 6, 30, 0, 0, time.UTC) // 01:30 EST
	if got.Equal(backwards) {
		t.Error("fired at 01:30 EST -- an hour before the requested 02:30. " +
			"time.Date's backwards normalisation of a nonexistent wall time was used as-is.")
	}

	// And it must not simply skip the day either.
	if got.In(ny).Day() != 10 {
		t.Errorf("skipped the transition day entirely: fired on day %d", got.In(ny).Day())
	}
}

// TestNextCronTimeIn_FallBackFiresOnceNotTwice covers the wall time that
// happens twice. 01:30 occurs at 05:30 UTC (EDT) and again at 06:30 UTC (EST).
// The promised rule is the first one, once.
func TestNextCronTimeIn_FallBackFiresOnceNotTwice(t *testing.T) {
	ny := newYork(t)
	from := time.Date(2024, 11, 3, 0, 0, 0, 0, ny)

	first := NextCronTimeIn("30 1 * * *", from, ny)
	want := time.Date(2024, 11, 3, 5, 30, 0, 0, time.UTC) // 01:30 EDT, the first
	if !first.Equal(want) {
		t.Fatalf("first firing: got %s, want %s (01:30 EDT)",
			first.UTC().Format(time.RFC3339), want.Format(time.RFC3339))
	}

	// The next firing must be the following day, NOT the second 01:30.
	next := NextCronTimeIn("30 1 * * *", first, ny)
	repeated := time.Date(2024, 11, 3, 6, 30, 0, 0, time.UTC) // 01:30 EST, the second
	if next.Equal(repeated) {
		t.Error("fired twice on the fall-back day: the repeated 01:30 EST was treated " +
			"as a separate firing")
	}
	wantNext := time.Date(2024, 11, 4, 6, 30, 0, 0, time.UTC) // 01:30 EST next day
	if !next.Equal(wantNext) {
		t.Errorf("second firing: got %s, want %s (next day)",
			next.UTC().Format(time.RFC3339), wantNext.Format(time.RFC3339))
	}
}

// TestNextCronTimeIn_DailyScheduleKeepsItsWallClockAcrossDST: the point of
// storing a zone is that "07:00 every day" stays 07:00 to the person who wrote
// it, even though the UTC offset moves underneath.
func TestNextCronTimeIn_DailyScheduleKeepsItsWallClockAcrossDST(t *testing.T) {
	ny := newYork(t)

	before := NextCronTimeIn("0 7 * * *", time.Date(2024, 3, 8, 12, 0, 0, 0, ny), ny)
	after := NextCronTimeIn("0 7 * * *", time.Date(2024, 3, 12, 12, 0, 0, 0, ny), ny)

	for _, tc := range []struct {
		name string
		got  time.Time
	}{{"before the transition", before}, {"after the transition", after}} {
		local := tc.got.In(ny)
		if local.Hour() != 7 || local.Minute() != 0 {
			t.Errorf("%s: fired at %s local, want 07:00", tc.name, local.Format("15:04 MST"))
		}
	}
	// ...and the UTC offset really did change, or this test proves nothing.
	if before.In(ny).Format("-0700") == after.In(ny).Format("-0700") {
		t.Errorf("the UTC offset did not change across the transition (%s both sides); "+
			"this test is not exercising DST", before.In(ny).Format("-0700"))
	}
}

// TestNextCronTime_DelegatesToTheCallersLocation pins the compatibility
// behaviour: NextCronTime evaluates in from's own zone, so every existing
// caller that passes a UTC time keeps getting UTC answers.
func TestNextCronTime_DelegatesToTheCallersLocation(t *testing.T) {
	ny := newYork(t)
	from := time.Date(2024, 6, 10, 0, 0, 0, 0, ny)
	if got, want := NextCronTime("0 7 * * *", from), NextCronTimeIn("0 7 * * *", from, ny); !got.Equal(want) {
		t.Errorf("NextCronTime = %s, NextCronTimeIn(loc=from's) = %s",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestNextCronTimeIn_NilLocationIsUTC(t *testing.T) {
	from := time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC)
	if got, want := NextCronTimeIn("0 7 * * *", from, nil), NextCronTimeIn("0 7 * * *", from, time.UTC); !got.Equal(want) {
		t.Errorf("nil location = %s, want the UTC answer %s",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// ---------------------------------------------------------------------------
// Timezone validation
// ---------------------------------------------------------------------------

func TestValidateTimezone(t *testing.T) {
	if err := ValidateTimezone(""); err != nil {
		t.Errorf(`ValidateTimezone("") = %v, want nil (the caller substitutes the default)`, err)
	}
	if err := ValidateTimezone("UTC"); err != nil {
		t.Errorf(`ValidateTimezone("UTC") = %v, want nil`, err)
	}
	if err := ValidateTimezone("Not/AZone"); err == nil {
		t.Error(`ValidateTimezone("Not/AZone") = nil, want an error`)
	}
	// A plain offset is not an IANA name and must be rejected: it carries no
	// DST rules, so accepting it would silently produce a schedule that drifts
	// against the wall clock it was meant to track.
	if err := ValidateTimezone("-05:00"); err == nil {
		t.Error(`ValidateTimezone("-05:00") = nil, want an error -- an offset is not a zone`)
	}
}

// TestLoadScheduleLocation_ReportsTheFallback: "this schedule is UTC" and
// "this schedule asked for a zone this process cannot load" are very different
// operational situations, and the second one must not be silent.
func TestLoadScheduleLocation_ReportsTheFallback(t *testing.T) {
	tests := []struct {
		name         string
		wantUTC      bool
		wantFellBack bool
	}{
		{"UTC", true, false},
		{"", true, true},
		{"Not/AZone", true, true},
	}
	for _, tt := range tests {
		loc, fellBack := LoadScheduleLocation(tt.name)
		if (loc == time.UTC) != tt.wantUTC {
			t.Errorf("LoadScheduleLocation(%q) location = %v, wantUTC=%v", tt.name, loc, tt.wantUTC)
		}
		if fellBack != tt.wantFellBack {
			t.Errorf("LoadScheduleLocation(%q) fellBack = %v, want %v", tt.name, fellBack, tt.wantFellBack)
		}
	}

	ny := newYork(t)
	loc, fellBack := LoadScheduleLocation("America/New_York")
	if fellBack {
		t.Error("LoadScheduleLocation(America/New_York) reported a fallback")
	}
	if loc.String() != ny.String() {
		t.Errorf("LoadScheduleLocation(America/New_York) = %v, want %v", loc, ny)
	}
}
