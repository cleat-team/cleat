package main

import (
	"strings"
	"testing"
	"time"
)

// cleat#4275: --held-reclaim-after. The fast path's window is minimumReclaimAfter without the database-stall allowance, a negative value turns it off, and nothing shorter than that
// floor is accepted. The numbers are derived from the same function the ordinary window uses, so a change to either term moves both and is seen here.
func TestTheHeldReclaimWindowIsTheReclaimWindowWithoutItsStallAllowance(t *testing.T) {
	hb := 5 * time.Second
	got := heldReclaimWindow(0, 0, hb)
	if want := minimumReclaimAfter(hb) - maxTolerableStall; got != want {
		t.Errorf("heldReclaimWindow(0, 0, 5s) = %v, want %v", got, want)
	}
	if got != 17*time.Second {
		t.Errorf("at the default heartbeat the held window is %v; the design (cleat#4275) and the flag help say about 17s, so one of them is stale", got)
	}
	if got >= reclaimWindow(0, hb) {
		t.Errorf("the held window %v is not shorter than the ordinary window %v", got, reclaimWindow(0, hb))
	}
}

func TestANegativeHeldReclaimAfterTurnsTheFastPathOff(t *testing.T) {
	if got := heldReclaimWindow(-1, 0, 5*time.Second); got != 0 {
		t.Errorf("heldReclaimWindow(-1, ...) = %v, want 0 (no fast path)", got)
	}
}

// A window that is not shorter than the ordinary one buys nothing and would only add a predicate to the reclaim statements, so it is no fast path at all.
func TestAHeldReclaimWindowNotShorterThanTheReclaimWindowIsNoFastPath(t *testing.T) {
	hb := 5 * time.Second
	if got := heldReclaimWindow(0, 15*time.Second, hb); got != 0 {
		t.Errorf("with --reclaim-timeout 15s (below the derived held window) the held window is %v, want 0", got)
	}
	if got := heldReclaimWindow(47*time.Second, 0, hb); got != 0 {
		t.Errorf("a --held-reclaim-after equal to the ordinary window gave %v, want 0", got)
	}
	if got := heldReclaimWindow(30*time.Second, 0, hb); got != 30*time.Second {
		t.Errorf("an explicit 30s gave %v, want 30s", got)
	}
}

func TestAHeldReclaimAfterBelowItsFloorIsRefusedNotClamped(t *testing.T) {
	hb := 5 * time.Second
	floor := minimumHeldReclaimAfter(hb)
	if err := validateHeldReclaimAfter(floor-time.Second, hb); err == nil || !strings.Contains(err.Error(), "--held-reclaim-after") {
		t.Errorf("validateHeldReclaimAfter(floor-1s) = %v, want a refusal naming the flag", err)
	}
	for name, v := range map[string]time.Duration{"derive (0)": 0, "off (negative)": -time.Second, "at the floor": floor, "above the floor": floor + time.Minute} {
		if err := validateHeldReclaimAfter(v, hb); err != nil {
			t.Errorf("validateHeldReclaimAfter(%s) = %v, want nil", name, err)
		}
	}
}
