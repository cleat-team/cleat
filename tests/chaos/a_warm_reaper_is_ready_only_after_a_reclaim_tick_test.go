//go:build chaos

package chaos

import (
	"testing"
	"time"
)

// The warm reaper is "ready" when its OWN metrics say the reaper loop completed a tick, not when time has passed. The page below is real: scraped from a live warm reaper in the harness
// (cleat#4350), whose reaper had SKIPPED four ticks (its start-up grace, 4 x 10 s) and then completed one. sumReaperOK must count the "ok" tick, and only that.
const realReaperPage = `cleat_background_loops_total{loop_name="reaper",status="ok",worker_id="fd75b2b8e18cc319b72e06a2ea35cbae"} 1
cleat_background_loops_total{loop_name="heartbeat",status="ok",worker_id="fd75b2b8e18cc319b72e06a2ea35cbae"} 49
cleat_background_loops_total{loop_name="worker_membership",status="ok",worker_id="fd75b2b8e18cc319b72e06a2ea35cbae"} 50
cleat_background_loops_total{loop_name="schedule",status="ok",worker_id="fd75b2b8e18cc319b72e06a2ea35cbae"} 3
cleat_background_loops_total{loop_name="reaper",status="skipped",worker_id="fd75b2b8e18cc319b72e06a2ea35cbae"} 4
`

func TestChaosTheWarmReaperIsReadyOnlyAfterItCompletedAReclaimTick(t *testing.T) {
	if got := sumReaperOK(realReaperPage); got != 1 {
		t.Errorf("sumReaperOK(real page with one reaper ok tick, four skipped, other loops ok) = %v, want 1", got)
	}
	// Still in its start-up grace: four skipped ticks, no ok one. Readiness must read NOT ready, so a fault run does not go on before the reaper can reclaim.
	grace := `cleat_background_loops_total{loop_name="reaper",status="skipped",worker_id="w"} 4
cleat_background_loops_total{loop_name="heartbeat",status="ok",worker_id="w"} 49
`
	if got := sumReaperOK(grace); got != 0 {
		t.Errorf("sumReaperOK(a reaper still in its grace) = %v, want 0", got)
	}
	// No series at all (a page that is not a real worker's) is zero, never a crash.
	if got := sumReaperOK(""); got != 0 {
		t.Errorf("sumReaperOK(empty page) = %v, want 0", got)
	}
}

// What the warm reaper is FOR, pinned: with it up, a killed worker's run is reclaimed in seconds, not after a fresh worker's start-up grace (>= ~40 s plus a 10 s tick, which is what the
// harness paid once the leaked golden workers that used to do this by accident were killed: every +reclaim and +signal case ~54 s, and the PR sample timed out at 20 min).
// The wait for the reaper's first tick is OUTSIDE the clock: it is the one-time cost the design accepts. Falsified by killing the warm reaper first, which leaves only the fresh
// second worker: the case then takes the grace and this fails.
func TestChaosAKilledRunIsReclaimedByTheWarmReaperInSeconds(t *testing.T) {
	e := newEnv(t)
	warmReaper(t, e)
	awaitWarmReaper(t)
	f := &fault{Op: "A", Nth: 1, Mode: "after", Then: "reclaim"}
	start := time.Now()
	o := execute(t, e, scenarios[0], f)
	took := time.Since(start)
	if !o.Applied || o.Status != "done" && o.Status != "completed" {
		t.Fatalf("UNMEASURED: the fault applied=%v and the run ended %q, so how long the reclaim took says nothing", o.Applied, o.Status)
	}
	const bound = 30 * time.Second
	if took > bound {
		t.Errorf("a killed run was reclaimed and finished in %s, want under %s: the warm reaper did not reap it, so the fresh second worker waited out its own start-up grace", took.Round(time.Second), bound)
	}
	t.Logf("kill, reclaim and finish took %s (bound %s)", took.Round(100*time.Millisecond), bound)
}
