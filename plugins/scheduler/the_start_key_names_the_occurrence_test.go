package scheduler

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestTheStartKeyNamesTheOccurrence pins the property that makes the scheduler's
// idempotency key worth having, and it is not "a key is passed".
//
// The key exists so that a crash between the claim commit and the start can be
// retried without double-firing. That works only if the key is the SAME for two
// dispatches of one occurrence. A key derived from the dispatch clock passes
// every "is a key set" assertion, differs on every attempt, and deduplicates
// nothing — with no symptom, because duplicate runs look identical to having no
// key at all. cleat#1555.
func TestTheStartKeyNamesTheOccurrence(t *testing.T) {
	id := uuid.MustParse("3f2b1c00-0000-4000-8000-000000001555")
	due := time.Date(2026, 9, 14, 10, 30, 0, 0, time.UTC)

	t.Run("two dispatches of one occurrence agree", func(t *testing.T) {
		// The two calls are separated in wall-clock time by executing at all.
		// A key that reads the clock fails here; one that reads the row does not.
		first := scheduleStartKey(id, due)
		second := scheduleStartKey(id, due)
		if first != second {
			t.Fatalf("same schedule, same due time, different keys:\n  %q\n  %q\n\n"+
				"The retry after a crash presents the second key, so the engine sees "+
				"a new start and creates a duplicate run.", first, second)
		}
	})

	t.Run("consecutive occurrences differ", func(t *testing.T) {
		// The negative control. A constant key would pass the test above and
		// collapse every firing of a schedule into one run, forever.
		next := scheduleStartKey(id, due.Add(5*time.Minute))
		if scheduleStartKey(id, due) == next {
			t.Fatal("two different occurrences produced the same key; after the first " +
				"firing every later one would be deduplicated away and never run")
		}
	})

	t.Run("different schedules at the same instant differ", func(t *testing.T) {
		other := uuid.MustParse("7c8d9e00-0000-4000-8000-000000001555")
		if scheduleStartKey(id, due) == scheduleStartKey(other, due) {
			t.Fatal("two schedules firing at the same instant share a key; one would " +
				"be deduplicated against the other")
		}
	})

	t.Run("the zone a worker happens to hold does not change the key", func(t *testing.T) {
		// Same instant, different zone. Workers in one fleet need not agree on
		// a session time zone, and a formatted local time would make the key
		// depend on which worker retried.
		//
		// FixedZone, NOT LoadLocation. LoadLocation needs tzdata, which is not
		// guaranteed present, so it forces a t.Skip -- and a skipped subtest is
		// indistinguishable from a passing one, which is the whole objection
		// scripts/check-skips.sh raises. A fixed offset proves the same thing
		// (the key must not vary with the zone) and is always satisfiable, so
		// this assertion cannot quietly stop running.
		east := time.FixedZone("UTC+10", 10*60*60)
		if got, want := scheduleStartKey(id, due.In(east)), scheduleStartKey(id, due); got != want {
			t.Errorf("same instant in another zone produced %q, want %q", got, want)
		}
	})
}
