package main

import (
	"strings"
	"testing"
	"time"
)

// TestReclaimAfterDefaultsToTheHistoricalDerivation is the compatibility half.
//
// The flag defaults to 0 = derive, so an operator who tuned --heartbeat keeps
// exactly the window they had. That matters more than it looks: hardcoding a
// 10s default would have silently shortened the lease from 120s to 10s for
// anyone running --heartbeat 60s, which is a behaviour change dressed as a new
// feature. cleat#1717.
func TestReclaimAfterDefaultsToTheHistoricalDerivation(t *testing.T) {
	// The expression this replaced, verbatim, as the oracle. Written out rather
	// than referenced so that changing reclaimAfter cannot change the thing it
	// is being compared against.
	historical := func(hb time.Duration) time.Duration {
		return max(hb*2, 10*time.Second)
	}

	for _, hb := range []time.Duration{
		time.Second, 2 * time.Second, 5 * time.Second, // floor binds
		10 * time.Second, 30 * time.Second, 60 * time.Second, 150 * time.Second,
	} {
		w := &Worker{heartbeatInterval: hb}
		got, want := w.reclaimAfter(), historical(hb)
		if got != want {
			t.Errorf("heartbeat %v: reclaimAfter() = %v, historical derivation = %v", hb, got, want)
		}
	}

	// KNOWN-POSITIVE: the oracle must disagree when the value really differs,
	// or "they match" is also what an oracle that always agrees would print.
	w := &Worker{heartbeatInterval: 5 * time.Second, reclaimTimeout: 5 * time.Minute}
	if w.reclaimAfter() == historical(5*time.Second) {
		t.Fatal("an explicit reclaimTimeout did not override the derivation, so the " +
			"comparison above cannot distinguish deriving from overriding")
	}
}

// TestAnExplicitReclaimTimeoutWins is the decoupling half: the whole point is
// that a long lease no longer forces sparse heartbeats.
func TestAnExplicitReclaimTimeoutWins(t *testing.T) {
	w := &Worker{heartbeatInterval: 5 * time.Second, reclaimTimeout: 5 * time.Minute}
	if got := w.reclaimAfter(); got != 5*time.Minute {
		t.Errorf("reclaimAfter() = %v, want 5m: an explicit value must win over the "+
			"derivation, otherwise the flag does nothing", got)
	}

	// The property that motivated the flag, stated as an assertion: a 5-minute
	// lease is now reachable while still heartbeating every 5 seconds. Before
	// this, the only way to a 5-minute lease was --heartbeat 150s.
	if w.heartbeatInterval != 5*time.Second {
		t.Fatalf("precondition: heartbeat should still be 5s, is %v", w.heartbeatInterval)
	}
}

func TestValidateReclaimTimeoutRefusesAFootgun(t *testing.T) {
	const hb = 5 * time.Second

	cases := []struct {
		name    string
		reclaim time.Duration
		wantErr bool
	}{
		{"zero derives, always allowed", 0, false},
		{"negative is treated as unset", -time.Second, false},
		{"exactly two heartbeats is allowed", 2 * hb, false},
		{"above two heartbeats is allowed", 5 * time.Minute, false},
		{"one heartbeat is refused", hb, true},
		{"below one heartbeat is refused", time.Second, true},
	}
	for _, c := range cases {
		err := validateReclaimTimeout(c.reclaim, hb)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: validateReclaimTimeout(%v, %v) error = %v, wantErr = %v",
				c.name, c.reclaim, hb, err, c.wantErr)
		}
	}

	// The message has to name BOTH values and the floor, because an operator
	// reading it is deciding which of the two flags to move.
	err := validateReclaimTimeout(time.Second, hb)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"--reclaim-timeout", "--heartbeat", "10s"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so it does not tell the operator "+
				"what to change: %v", want, err)
		}
	}
}
