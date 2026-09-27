package plugin

import (
	"context"
	"testing"
	"time"
)

// EvictIdle closes pools nobody has touched, and leaves alone the ones in use.
// cleat#1470.
//
// NO SLEEPS. Every case drives an injectable clock, so the elapsed time each
// one means is stated rather than waited for. A sleeping test measures the
// scheduler as much as the code, and the usual repair -- widening the window --
// makes it slower without making it truer.
func TestEvictIdleClosesOnlyPoolsNobodyIsUsing(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	tp, _ := newCountingPools(t)
	tp.clock = func() time.Time { return now }

	// Two tenants, both opened at t=0.
	for _, id := range []string{"tenant-A", "tenant-B"} {
		if _, err := tp.For(context.Background(), id); err != nil {
			t.Fatalf("For(%s): %v", id, err)
		}
	}
	if got := len(tp.pools); got != 2 {
		t.Fatalf("expected 2 pools, got %d -- the rest of this test is vacuous without them, "+
			"which is exactly how the previous TestEvictIdle passed against a stub", got)
	}

	// Ten minutes pass. tenant-A is used again; tenant-B is not.
	now = now.Add(10 * time.Minute)
	if _, err := tp.For(context.Background(), "tenant-A"); err != nil {
		t.Fatalf("For(tenant-A) after 10m: %v", err)
	}

	// Evict anything unused for five minutes. Only tenant-B qualifies.
	n := tp.EvictIdle(5 * time.Minute)
	if n != 1 {
		t.Errorf("EvictIdle evicted %d pools, want 1", n)
	}
	tp.mu.Lock()
	_, aLives := tp.pools["tenant-A"]
	_, bLives := tp.pools["tenant-B"]
	tp.mu.Unlock()
	if !aLives {
		t.Error("tenant-A was evicted, and it was used within the window.\n\n" +
			"For() must stamp lastUsed on a cache HIT, not only when the pool is opened -- " +
			"otherwise a tenant served continuously from a warm pool looks idle from the " +
			"moment it was created, and is the FIRST thing evicted.")
	}
	if bLives {
		t.Error("tenant-B survived, and nothing has touched it for ten minutes")
	}
}

// A pool that has never been handed out is not infinitely idle.
//
// Without a stamp at creation, lastUsed is the zero value -- which is 1970, so
// the pool reads as 56 years idle and is evicted immediately, before the caller
// that triggered its creation has used it.
func TestAFreshPoolIsNotImmediatelyIdle(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	tp, _ := newCountingPools(t)
	tp.clock = func() time.Time { return now }

	if _, err := tp.For(context.Background(), "tenant-fresh"); err != nil {
		t.Fatalf("For: %v", err)
	}
	if n := tp.EvictIdle(time.Minute); n != 0 {
		t.Errorf("EvictIdle closed %d pools immediately after one was created, want 0.\n\n"+
			"A pool with no stamp has lastUsed == 0, which is 1970 -- infinitely idle. It is "+
			"evicted before its caller ever uses it, and the caller then reopens it, which "+
			"is a loop rather than a policy.", n)
	}
}

// A non-positive window evicts nothing rather than everything.
//
// "Idle for zero seconds" describes every pool, including one handed out
// microseconds ago. Treating it literally is a stall dressed as a policy, and a
// zero here is far likelier to be an unset config value than a request to close
// every pool.
func TestANonPositiveIdleWindowEvictsNothing(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	tp, _ := newCountingPools(t)
	tp.clock = func() time.Time { return now }

	if _, err := tp.For(context.Background(), "tenant-A"); err != nil {
		t.Fatalf("For: %v", err)
	}
	// Age it well past any plausible window, so this cannot pass merely because
	// nothing was old enough -- which is how the removed TestEvictIdle passed.
	now = now.Add(24 * time.Hour)

	for _, d := range []time.Duration{0, -time.Minute} {
		if n := tp.EvictIdle(d); n != 0 {
			t.Errorf("EvictIdle(%v) evicted %d pools, want 0 -- a day-old pool is present, "+
				"so this is not passing for want of a candidate", d, n)
		}
	}
	// POSITIVE CONTROL: the same pool IS evictable, so the two cases above are
	// about the window rather than about an unevictable pool.
	if n := tp.EvictIdle(time.Hour); n != 1 {
		t.Errorf("EvictIdle(1h) evicted %d, want 1 -- without this the assertions above "+
			"would pass against an EvictIdle that never evicts anything", n)
	}
}

// An evicted pool is gone from the map, so the next caller opens a new one.
//
// Removing from the map is what makes a pool unreachable; the Close is
// asynchronous bookkeeping. This asserts the reachability half, which is the
// half that matters for a budget: a pool still in the map is still counted.
func TestAnEvictedPoolIsReopenedOnTheNextCall(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	tp, d := newCountingPools(t)
	tp.clock = func() time.Time { return now }

	first, err := tp.For(context.Background(), "tenant-A")
	if err != nil {
		t.Fatalf("first For: %v", err)
	}
	opens := d.queries.Load()

	now = now.Add(time.Hour)
	if n := tp.EvictIdle(time.Minute); n != 1 {
		t.Fatalf("EvictIdle evicted %d, want 1", n)
	}

	second, err := tp.For(context.Background(), "tenant-A")
	if err != nil {
		t.Fatalf("For after eviction: %v", err)
	}
	if second == first {
		t.Error("For returned the SAME *sql.DB after its pool was evicted.\n\n" +
			"The entry is still in the map, so nothing was really evicted and a budget " +
			"counting the map would keep counting it.")
	}
	if got := d.queries.Load(); got != opens+1 {
		t.Errorf("the role lookup ran %d times in total, want %d -- the second For() must "+
			"actually reopen rather than return a cached entry", got, opens+1)
	}
}
