package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// cleat#3792, item 2. A healthy single worker logged four WARNs in its first
// ~30 s: "recent database trouble on this worker" (x4, consecutive_skips 1..4),
// then "has not reclaimed a stale run in a while; check its database
// connectivity". Nothing was wrong. lastDBTrouble is seeded to the start time on
// purpose (a new worker has proven nothing and must not reclaim anyone's run), so
// the gate is closed for the first reclaimAfter() and the log said trouble.
//
// Three cases: the start-up window says so at INFO and does not count; real
// trouble inside the window still WARNs and counts; and past the window a gate
// that is still closed with no trouble recorded is back to the old behaviour,
// because the bound is what keeps this from hiding a gate that never opens.
func TestANewWorkersReaperGraceIsNotLoggedAsDatabaseTrouble(t *testing.T) {
	setup := func(t *testing.T, startedAgo time.Duration) (*Worker, *bytes.Buffer) {
		t.Helper()
		w := newTestWorkerFromStore(&mockStore{})
		w.reclaimTimeout = 10 * time.Second
		w.startedAt = time.Now().Add(-startedAgo)
		w.lastDBTrouble.Store(time.Now().UnixNano()) // the seed newWorker makes
		var buf bytes.Buffer
		w.logger = slog.New(slog.NewTextHandler(&buf, nil))
		return w, &buf
	}

	t.Run("the start-up window is INFO and uncounted", func(t *testing.T) {
		w, buf := setup(t, time.Second)
		for i := 0; i < 4; i++ {
			w.reapOnce()
		}
		out := buf.String()
		if strings.Contains(out, "recent database trouble") || strings.Contains(out, "skipped its last several ticks") {
			t.Errorf("a new worker's grace period was logged as database trouble:\n%s", out)
		}
		if n := strings.Count(out, "start-up grace period"); n != 4 {
			t.Errorf("start-up INFO line count = %d, want 4:\n%s", n, out)
		}
		if strings.Contains(out, "level=WARN") {
			t.Errorf("a WARN was logged during the start-up window:\n%s", out)
		}
		if got := w.consecutiveReapSkips.Load(); got != 0 {
			t.Errorf("consecutiveReapSkips = %d, want 0: the streak alarm is for a gate that stays closed", got)
		}
	})

	t.Run("real trouble inside the window still warns and counts", func(t *testing.T) {
		w, buf := setup(t, time.Second)
		w.recordDBTrouble()
		w.reapOnce()
		if !strings.Contains(buf.String(), "recent database trouble") {
			t.Errorf("trouble was recorded and the skip did not say so:\n%s", buf.String())
		}
		if got := w.consecutiveReapSkips.Load(); got != 1 {
			t.Errorf("consecutiveReapSkips = %d, want 1", got)
		}
	})

	t.Run("past the window a closed gate with no recorded trouble is the old behaviour", func(t *testing.T) {
		w, buf := setup(t, 25*time.Second) // > 2 x 10 s
		w.reapOnce()
		if !strings.Contains(buf.String(), "recent database trouble") {
			t.Errorf("a gate still closed after the start-up window was not reported:\n%s", buf.String())
		}
		if got := w.consecutiveReapSkips.Load(); got != 1 {
			t.Errorf("consecutiveReapSkips = %d, want 1", got)
		}
	})
}
