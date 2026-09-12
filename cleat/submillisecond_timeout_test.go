package cleat

// cleat#1331. A timeout under 1ms walked past the guard that exists to reject
// it, because the guard checked the Duration and the next line truncated to
// milliseconds. The host then suspended with a deadline of now, so the run was
// immediately re-claimable: it woke, replayed to this step, suspended again --
// about fourteen claims a second, forever, with event_history never advancing.
//
// THE ASSERTION THAT MATTERS IS "THE HOST WAS NOT CALLED". A test that only
// checked the returned error would pass against a version that guards after
// dispatching the await, which is the version that livelocks. So every case
// below counts host calls, and the zero is the point.

import (
	"strings"
	"testing"
	"time"
)

// sawHost returns a runtime whose only recorded behaviour is whether the
// durable await reached the host, and with what.
func sawHost(t *testing.T) (HostCalls, *[]int64) {
	t.Helper()
	var seen []int64
	h := NewHostCalls(HostCallsOptions{
		DurableAwaitSignals: func(_ []string, timeoutMs int64) (string, string, bool, error) {
			seen = append(seen, timeoutMs)
			return "", "", true, nil
		},
	})
	return h, &seen
}

func TestASubMillisecondAwaitIsRefusedBeforeItReachesTheHost(t *testing.T) {
	// 999999ns is the largest value that truncates to 0, and 1ns the smallest
	// non-zero one. Both are positive, so the OLD guard admitted both.
	for _, timeout := range []time.Duration{
		1 * time.Nanosecond,
		1 * time.Microsecond,
		100 * time.Microsecond,
		999999 * time.Nanosecond,
	} {
		t.Run(timeout.String(), func(t *testing.T) {
			h, seen := sawHost(t)

			res := h.AwaitSignals([]string{"never"}, timeout)

			if len(*seen) != 0 {
				t.Errorf("a %s timeout reached the host as %dms. A 0ms await suspends with a "+
					"deadline of now, so the run is re-claimed and replayed forever without "+
					"advancing a step (cleat#1331).", timeout, (*seen)[0])
			}
			if res.Err == nil {
				t.Fatalf("a %s timeout returned no error", timeout)
			}
			// The message must name the ROUNDING. The caller's value was
			// positive, so "requires a positive timeout" reads as wrong to the
			// person who wrote 100*time.Microsecond, and sends them looking in
			// the wrong place.
			if !strings.Contains(res.Err.Error(), "0ms") {
				t.Errorf("the error for %s does not mention rounding to 0ms: %q", timeout, res.Err)
			}
			if !res.TimedOut {
				t.Errorf("a refused await for %s did not report TimedOut", timeout)
			}
		})
	}
}

// The negative control. Without it, a guard that refused EVERY timeout would
// pass every assertion above.
func TestAMillisecondAwaitStillReachesTheHost(t *testing.T) {
	for _, tc := range []struct {
		timeout time.Duration
		wantMs  int64
	}{
		{1 * time.Millisecond, 1},
		{1500 * time.Microsecond, 1}, // truncates, but not to zero
		{2 * time.Second, 2000},
	} {
		t.Run(tc.timeout.String(), func(t *testing.T) {
			h, seen := sawHost(t)

			res := h.AwaitSignals([]string{"never"}, tc.timeout)

			if len(*seen) != 1 {
				t.Fatalf("a %s timeout reached the host %d times, want 1", tc.timeout, len(*seen))
			}
			if (*seen)[0] != tc.wantMs {
				t.Errorf("a %s timeout reached the host as %dms, want %dms",
					tc.timeout, (*seen)[0], tc.wantMs)
			}
			if res.Err != nil {
				t.Errorf("a %s timeout was refused: %v", tc.timeout, res.Err)
			}
		})
	}
}

// AwaitCondition's pollInterval is a PUBLIC parameter, so its failure was a
// livelock in library code rather than in the workflow author's. It answers
// bool and has no error channel, so it clamps.
//
// Asserted at the host boundary rather than by timing: what must be true is
// that the poll suspends for a real interval. A wall-clock assertion here
// would be the kind this repo's CLAUDE.md says to remove rather than widen.
func TestAwaitConditionClampsASubMillisecondPollInterval(t *testing.T) {
	// A DRIVEN CLOCK, not the wall clock. AwaitCondition's deadline is checked
	// against h.Now(), which reads the workflow's durable clock -- unwired it
	// answers the epoch forever and the loop cannot terminate at all. Advancing
	// it one millisecond per poll also makes the test independent of how fast
	// the machine is, which is what CLAUDE.md asks for instead of a widened
	// timing assertion.
	var seen []int64
	nowMs := int64(1_000_000)
	h := NewHostCalls(HostCallsOptions{
		Now: func() int64 { return nowMs },
		DurableAwaitSignals: func(_ []string, timeoutMs int64) (string, string, bool, error) {
			seen = append(seen, timeoutMs)
			nowMs += timeoutMs
			return "", "", true, nil
		},
	})
	calls := 0
	// Never true, so the loop runs until the deadline and we observe the
	// interval it actually polls with.
	h.AwaitCondition(func() bool { calls++; return false }, 100*time.Microsecond, 3*time.Millisecond)

	if calls == 0 {
		t.Fatal("the predicate was never evaluated")
	}
	if len(seen) == 0 {
		t.Fatalf("AwaitCondition never suspended: a sub-millisecond poll interval used to "+
			"livelock the workflow, and unclamped it would now spin in the guest for the "+
			"whole wait instead (cleat#1331). predicate evaluated %d times", calls)
	}
	for i, ms := range seen {
		if ms < 1 {
			t.Errorf("poll %d suspended for %dms; the durable wait has millisecond "+
				"resolution, so anything under 1 never returns", i, ms)
		}
	}
}
