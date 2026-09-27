package engine

import (
	"context"
	"testing"
	"time"
)

// A guest sleep is served by the DURABLE clock: satisfied at once when the wait
// has already happened, really waited when it has not, and advancing h.Now()
// either way. cleat#1633.
//
// WHY THIS IS NOT A FLAG. The predicate is DurableSleep's -- compare the
// virtual deadline against real time -- and the reason is written on
// DurableSleep: "am I replaying" is a question a sleep CANNOT ANSWER FOR
// ITSELF, because it records no event. IMPROVEMENT-PLAN 3.67 records the
// earlier attempt, a replayJustEnded flag, which asked whether some OTHER
// durable call had crossed the replay frontier; in a faithful replay none does,
// so the sleep re-suspended forever.
//
// The same predicate also covers a case a flag cannot: a run resumed after the
// worker was down. The wait happened -- nobody was watching, but the time
// passed -- and replay and downtime are the same case to a deadline.
//
// WHAT WOULD FAIL WITHOUT THIS. Every production segment replays its recorded
// history before continuing, and wasmtime's poll_oneoff really blocks, so a
// guest time.Sleep(30s) sitting in already-replayed code cost 30 real seconds
// on EVERY resume, forever.
func TestAReplayedGuestSleepDoesNotWaitAgain(t *testing.T) {
	ctx := context.Background()

	newSession := func(nowMs int64) *execSession {
		return &execSession{engine: &Engine{}, nowMs: nowMs}
	}

	t.Run("a wait still ahead of real time is really waited", func(t *testing.T) {
		s := newSession(realNowMsForTest() + 50)
		got := s.ServeWasiSleep(ctx, 400)
		if got <= 0 {
			t.Fatalf("ServeWasiSleep returned %v for a deadline in the future; the guest "+
				"would not wait at all and a live sleep would be instant", got)
		}
		if got > 600*time.Millisecond {
			t.Errorf("ServeWasiSleep returned %v, which is longer than the sleep asked for; "+
				"the remainder is computed against real time and should not exceed it", got)
		}
	})

	t.Run("a wait already behind real time returns at once", func(t *testing.T) {
		// The anchor is far enough in the past that anchor+duration is still
		// behind now. That is what a replayed sleep looks like: the recorded
		// timestamps are old, so every deadline computed from them has passed.
		s := newSession(realNowMsForTest() - 10_000)
		if got := s.ServeWasiSleep(ctx, 400); got != 0 {
			t.Errorf("ServeWasiSleep returned %v for a wait that already happened, want 0.\n\n"+
				"This is cleat#1633: a replayed guest sleep waiting AGAIN. Every production "+
				"segment replays its history before continuing, so this is paid on every "+
				"resume, forever.", got)
		}
	})

	t.Run("the durable clock advances by the sleep, in both cases", func(t *testing.T) {
		// This is the half a timing assertion cannot see. h.Now() must reflect
		// time passing across a guest sleep, or the workflow's own clock
		// disagrees with the one it just waited on.
		for _, tc := range []struct {
			name   string
			anchor int64
		}{
			{"live", realNowMsForTest() + 50},
			{"already waited", realNowMsForTest() - 10_000},
		} {
			t.Run(tc.name, func(t *testing.T) {
				s := newSession(tc.anchor)
				before := s.Now(ctx)
				s.ServeWasiSleep(ctx, 400)
				after := s.Now(ctx)
				if after-before != 400 {
					t.Errorf("the durable clock moved %dms across a 400ms sleep, want 400.\n\n"+
						"It must advance in BOTH cases and by the REQUESTED duration, not the "+
						"elapsed one: replay recomputes the same sleeps from the same anchors "+
						"and must arrive at the same number.", after-before)
				}
			})
		}
	})

	t.Run("a non-positive sleep is not a wait", func(t *testing.T) {
		s := newSession(realNowMsForTest() + 50)
		before := s.Now(ctx)
		if got := s.ServeWasiSleep(ctx, 0); got != 0 {
			t.Errorf("a 0ms sleep returned %v, want 0", got)
		}
		if s.Now(ctx) != before {
			t.Errorf("a 0ms sleep moved the durable clock from %d to %d", before, s.Now(ctx))
		}
	})

	t.Run("two sleeps in a row each advance the clock", func(t *testing.T) {
		// DurableSleep's own max() reasoning: Now() reads history while
		// stepCount is inside it and sleeps do not advance stepCount, so two
		// sleeps reading the same anchor would let the second complete a wait
		// it never performed.
		s := newSession(realNowMsForTest() - 10_000)
		start := s.Now(ctx)
		s.ServeWasiSleep(ctx, 100)
		s.ServeWasiSleep(ctx, 100)
		if got := s.Now(ctx) - start; got != 200 {
			t.Errorf("two 100ms sleeps moved the durable clock %dms, want 200: the second "+
				"sleep re-used the first's anchor", got)
		}
	})
}

// realNowMsForTest mirrors what Engine.realNowMs returns, so the fixtures below
// are positioned relative to the same clock the code under test consults.
func realNowMsForTest() int64 {
	return (&Engine{}).realNowMs()
}
