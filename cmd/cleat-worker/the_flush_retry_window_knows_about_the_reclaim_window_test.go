package main

import (
	"strings"
	"testing"
	"time"
)

// cleat#1717. --flush-retry-window and --reclaim-timeout are separate knobs with
// a relationship nothing in their units expresses: retrying a flush for longer
// than a run stays this worker's own is work that cannot land.
//
// This file pins the advice that says so, and the arithmetic it shares with
// reclaimAfter.

// TestTheAdviceFiresExactlyWhenTheRetryOutlastsTheReclaimWindow.
//
// Note the derived cases. --reclaim-timeout 0 does not mean "no reclaim window",
// it means max(2x --heartbeat, 10s) -- so a 30s flush window against an
// unconfigured reclaim timeout is the commonest way to reach this, and an
// advisory that only looked at the flag would stay silent for it.
func TestTheAdviceFiresExactlyWhenTheRetryOutlastsTheReclaimWindow(t *testing.T) {
	for _, tc := range []struct {
		name      string
		flush     time.Duration
		reclaim   time.Duration
		heartbeat time.Duration
		want      bool
	}{
		{name: "nothing configured", flush: 0, reclaim: 0, heartbeat: 5 * time.Second, want: false},
		{name: "default window against the derived 10s", flush: 750 * time.Millisecond, reclaim: 0, heartbeat: 5 * time.Second, want: false},
		{name: "exactly the derived window", flush: 10 * time.Second, reclaim: 0, heartbeat: 5 * time.Second, want: false},
		{name: "a failover window against the derived 10s", flush: 5 * time.Minute, reclaim: 0, heartbeat: 5 * time.Second, want: true},
		{name: "a failover window with reclaim raised to match", flush: 5 * time.Minute, reclaim: 5 * time.Minute, heartbeat: 5 * time.Second, want: false},
		{name: "a failover window with reclaim raised past it", flush: 5 * time.Minute, reclaim: 10 * time.Minute, heartbeat: 5 * time.Second, want: false},
		{name: "a slow heartbeat raises the derived window", flush: 30 * time.Second, reclaim: 0, heartbeat: 20 * time.Second, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := flushRetryWindowAdvice(tc.flush, tc.reclaim, tc.heartbeat)
			if (got != "") != tc.want {
				t.Fatalf("advice=%q, want fired=%v (flush=%v reclaim=%v heartbeat=%v, window=%v)",
					got, tc.want, tc.flush, tc.reclaim, tc.heartbeat, reclaimWindow(tc.reclaim, tc.heartbeat))
			}
			// An advisory nobody can act on is a log line, not advice: it has
			// to name the value to change and what to change it to.
			if tc.want {
				if !strings.Contains(got, "--reclaim-timeout") {
					t.Errorf("the advice does not name the flag to raise: %q", got)
				}
				if !strings.Contains(got, tc.flush.String()) {
					t.Errorf("the advice does not name the value to raise it to (%v): %q", tc.flush, got)
				}
			}
		})
	}
}

// TestTheAdviceAndTheReaperComputeTheSameWindow.
//
// The advice runs in main() before any Worker exists and the reaper runs on one,
// so the arithmetic is shared through reclaimWindow rather than written twice.
// This asserts the sharing rather than the formula: a second copy would pass
// every test above and diverge the first time either was tuned.
func TestTheAdviceAndTheReaperComputeTheSameWindow(t *testing.T) {
	for _, hb := range []time.Duration{time.Second, 3 * time.Second, 5 * time.Second, 20 * time.Second} {
		for _, rc := range []time.Duration{0, 45 * time.Second, 5 * time.Minute} {
			w := &Worker{reclaimTimeout: rc, heartbeatInterval: hb}
			if got, want := reclaimWindow(rc, hb), w.reclaimAfter(); got != want {
				t.Errorf("reclaimWindow(%v, %v)=%v but reclaimAfter()=%v", rc, hb, got, want)
			}
		}
	}
}
