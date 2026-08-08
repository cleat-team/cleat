package main

import (
	"testing"
	"time"

	// The scheduler resolves IANA zones, and these tests use one. See the
	// matching note in engine/cron_timezone_test.go: embedding tzdata in the
	// test binary is what lets an unloadable zone be a failure rather than a
	// silent skip.
	_ "time/tzdata"
)

// scheduleAdvance is where at-least-once meets "do not stampede".
//
// Advancing from the SCHEDULED instant rather than from now() is what lets a
// firing missed during an outage be delivered rather than silently forgotten.
// The bound is what stops a schedule that was down for a day from delivering a
// day's worth of firings as fast as the poll loop turns.

func TestScheduleAdvance_NormalCaseSkipsNothing(t *testing.T) {
	loc := time.UTC
	// Due a moment ago; the next minute has not arrived yet.
	now := time.Date(2024, 6, 10, 12, 0, 30, 0, loc)
	scheduled := time.Date(2024, 6, 10, 12, 0, 0, 0, loc)

	next, dropped := scheduleAdvance("* * * * *", scheduled, loc, now)

	if dropped != 0 {
		t.Errorf("dropped = %d, want 0 for a schedule that is on time", dropped)
	}
	want := time.Date(2024, 6, 10, 12, 1, 0, 0, loc)
	if !next.Equal(want) {
		t.Errorf("next = %s, want %s", next.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestScheduleAdvance_AdvancesFromTheScheduledInstantNotNow is the property
// that makes catch-up possible at all.
//
// A schedule due at 12:00 that is not noticed until 12:05 must advance to
// 12:01 -- the next instant AFTER the one it owes -- not to 12:06. Advancing
// from now() is what silently discarded every missed firing.
func TestScheduleAdvance_AdvancesFromTheScheduledInstantNotNow(t *testing.T) {
	loc := time.UTC
	scheduled := time.Date(2024, 6, 10, 12, 0, 0, 0, loc)
	now := time.Date(2024, 6, 10, 12, 5, 0, 0, loc)

	next, dropped := scheduleAdvance("* * * * *", scheduled, loc, now)

	want := time.Date(2024, 6, 10, 12, 1, 0, 0, loc)
	if !next.Equal(want) {
		t.Errorf("next = %s, want %s -- it should step one interval from the "+
			"scheduled instant, not jump to the next instant after now",
			next.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0: stepping to the next owed instant discards nothing", dropped)
	}

	nowJump := time.Date(2024, 6, 10, 12, 6, 0, 0, loc)
	if next.Equal(nowJump) {
		t.Error("advanced from now() rather than from the scheduled instant; " +
			"every firing between the two would be silently lost")
	}
}

// TestScheduleAdvance_HourlyScheduleIsNeverAffectedByTheBound: the bound is
// there for high-frequency schedules. Anything hourly or slower cannot reach
// it within any plausible outage, and this pins that so a future change to the
// constant does not quietly start dropping daily reports.
func TestScheduleAdvance_HourlyScheduleIsNeverAffectedByTheBound(t *testing.T) {
	loc := time.UTC
	scheduled := time.Date(2024, 6, 10, 0, 0, 0, 0, loc)
	now := time.Date(2024, 6, 12, 0, 0, 0, 0, loc) // two days later

	_, dropped := scheduleAdvance("0 * * * *", scheduled, loc, now)

	// 48 hourly instants owed, under the bound of 60, so the backlog is walked
	// one instant per tick and none of them is dropped.
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0; a two-day outage on an hourly schedule "+
			"is inside the bound of %d and should be fully caught up", dropped, maxCatchUpFirings)
	}
}

// TestScheduleAdvance_BoundedWhenTooFarBehind: a per-minute schedule down for
// a day owes 1440 firings. Delivering them all is a self-inflicted stampede,
// so the scheduler stops at the bound and resumes at the next future instant.
func TestScheduleAdvance_BoundedWhenTooFarBehind(t *testing.T) {
	loc := time.UTC
	scheduled := time.Date(2024, 6, 10, 0, 0, 0, 0, loc)
	now := time.Date(2024, 6, 11, 0, 0, 0, 0, loc) // 1440 minutes later

	next, dropped := scheduleAdvance("* * * * *", scheduled, loc, now)

	if dropped <= maxCatchUpFirings {
		t.Errorf("dropped = %d, want more than the bound %d", dropped, maxCatchUpFirings)
	}
	// Having given up on catching up, it must resume in the FUTURE -- not
	// somewhere in the middle of the backlog, which would leave it permanently
	// behind and re-trigger this path forever.
	if !next.After(now) {
		t.Errorf("next = %s is not after now = %s; the schedule would stay behind indefinitely",
			next.Format(time.RFC3339), now.Format(time.RFC3339))
	}
}

// TestScheduleAdvance_ModestBacklogDropsNothing is the case the bound must not
// touch. Ten minutes behind is well inside it, so the schedule steps one
// interval and the loop delivers 00:01 through 00:10 one per tick. Reporting a
// drop here would mean the at-least-once promise was being broken for a
// ten-minute hiccup.
func TestScheduleAdvance_ModestBacklogDropsNothing(t *testing.T) {
	loc := time.UTC
	scheduled := time.Date(2024, 6, 10, 0, 0, 0, 0, loc)
	now := time.Date(2024, 6, 10, 0, 10, 0, 0, loc) // ten minutes behind

	next, dropped := scheduleAdvance("* * * * *", scheduled, loc, now)

	if dropped != 0 {
		t.Errorf("dropped = %d, want 0: a ten-interval backlog is caught up one "+
			"instant per tick, discarding nothing", dropped)
	}
	if want := time.Date(2024, 6, 10, 0, 1, 0, 0, loc); !next.Equal(want) {
		t.Errorf("next = %s, want %s (one interval on)",
			next.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestScheduleAdvance_ReportsWhatItDropped: the count is the whole reason this
// returns two values. An at-least-once promise that silently drops firings is
// not a promise, so the number has to reach the caller to be logged.
func TestScheduleAdvance_ReportsWhatItDropped(t *testing.T) {
	loc := time.UTC
	scheduled := time.Date(2024, 6, 10, 0, 0, 0, 0, loc)
	// Far enough past the bound that the schedule cannot walk out of it.
	now := time.Date(2024, 6, 10, 4, 0, 0, 0, loc) // 240 minutes behind

	_, dropped := scheduleAdvance("* * * * *", scheduled, loc, now)

	if dropped == 0 {
		t.Error("dropped = 0 for a backlog well past the bound; the caller has " +
			"nothing to log and the drop would be invisible")
	}
	if dropped <= maxCatchUpFirings {
		t.Errorf("dropped = %d, want more than the bound %d", dropped, maxCatchUpFirings)
	}
}

// TestScheduleAdvance_UnparseableExpressionTerminates: NextCronTimeIn falls
// back to +24h for an expression it cannot parse, which always advances -- so
// the loop cannot spin. Pinned because an infinite loop inside a background
// daemon is not a failure mode worth leaving to a proof.
func TestScheduleAdvance_UnparseableExpressionTerminates(t *testing.T) {
	loc := time.UTC
	scheduled := time.Date(2020, 1, 1, 0, 0, 0, 0, loc)
	now := time.Date(2024, 6, 10, 0, 0, 0, 0, loc) // years behind

	done := make(chan struct{})
	go func() {
		defer close(done)
		scheduleAdvance("this is not a cron expression", scheduled, loc, now)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("scheduleAdvance did not terminate on an unparseable expression")
	}
}

// TestScheduleAdvance_HonoursTheScheduleZone: the walk is in the schedule's
// zone, so a daily schedule stays on its wall clock across a DST transition
// rather than drifting an hour.
func TestScheduleAdvance_HonoursTheScheduleZone(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("America/New_York failed to load despite the embedded tzdata: %v", err)
	}
	// 07:00 the day before the spring-forward transition.
	scheduled := time.Date(2024, 3, 9, 7, 0, 0, 0, ny)
	now := time.Date(2024, 3, 9, 7, 0, 30, 0, ny)

	next, dropped := scheduleAdvance("0 7 * * *", scheduled, ny, now)

	if dropped != 0 {
		t.Errorf("dropped = %d, want 0", dropped)
	}
	if local := next.In(ny); local.Hour() != 7 || local.Minute() != 0 {
		t.Errorf("next = %s local, want 07:00 -- the walk did not use the schedule's zone",
			local.Format("2006-01-02 15:04 MST"))
	}
}
