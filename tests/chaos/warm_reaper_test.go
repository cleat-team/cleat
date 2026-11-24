//go:build chaos

package chaos

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// ONE WARM REAPER FOR THE WHOLE SWEEP (cleat#4350).
//
// A killed worker leaves its run `running` with a heartbeat the harness then ages (ageLease). Something has to notice, and that something is the REAPER of a LIVE worker. A worker
// that has just started does not reap: lastDBTrouble is seeded to its start time and reapingIsSafe wants a full reclaim window (>= ~40 s, whatever --heartbeat says, cleat#2005)
// since it, then the reaper ticks every 10 s. So after a kill the harness's own second worker needs ~50 s before it reclaims anything.
//
// The harness never paid that, by accident: every golden run's worker was left alive until the sweep ended (the leak this fixes), and those warm workers' reapers reclaimed the
// killed runs within seconds. Killing them (the fix for 88 of 100 connection slots held) made every +reclaim and +signal case ~54 s instead of ~4 s, and the PR sample timed out at 20 min.
//
// So the warm reaper is made deliberate: ONE long-lived worker per test binary, on a task queue of its own so it claims no run, holding one pool of ~11 connections instead of one per script.
// It models production, where other live workers reclaim a dead worker's runs. execute starts it, a fault run waits until its reaper has completed a tick, and TestMain kills it.

const warmReaperReady = 3 * time.Minute

var warm struct {
	once sync.Once
	w    *worker
	addr string
	mu   sync.Mutex
}

// warmReaper starts the warm reaper once per test binary. It is lazy so that a test that never runs a fault does not pay for it.
func warmReaper(t *testing.T, e *env) {
	t.Helper()
	warm.once.Do(func() {
		warm.addr = freeLoopbackAddr(t)
		w := launchWorker(t, e.bin, fmt.Sprintf("chaos-warm-reaper-%d", time.Now().UnixNano()), e.svc.srv.URL, "--api-addr", warm.addr)
		warm.mu.Lock()
		warm.w = w
		warm.mu.Unlock()
	})
}

// reaperTicksOK reads how many ticks the warm reaper has COMPLETED, from its own /metrics (cleat_background_loops_total, loop "reaper", status "ok"). ok is false when the page cannot
// be read, which is UNMEASURED and not zero.
func reaperTicksOK(addr string) (n float64, ok bool) {
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + addr + "/metrics")
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		return 0, false
	}
	return sumReaperOK(string(b)), true
}

// sumReaperOK is the pure half of reaperTicksOK, so a test can feed it a page.
func sumReaperOK(page string) float64 {
	var total float64
	for _, line := range strings.Split(page, "\n") {
		if !strings.HasPrefix(line, "cleat_background_loops_total") || !strings.Contains(line, `"reaper"`) || !strings.Contains(line, `"ok"`) {
			continue
		}
		f := strings.Fields(line)
		var v float64
		if _, err := fmt.Sscanf(f[len(f)-1], "%g", &v); err == nil {
			total += v
		}
	}
	return total
}

// awaitWarmReaper blocks until the warm reaper has completed one reclaim tick, so a fault run's killed worker is reaped at once and not after a start-up grace. It fails the test,
// naming the cause, if the reaper dies or never ticks: a fault run that went on without it would measure the wrong thing.
func awaitWarmReaper(t *testing.T) {
	t.Helper()
	warm.mu.Lock()
	w, addr := warm.w, warm.addr
	warm.mu.Unlock()
	if w == nil {
		t.Fatal("UNMEASURED: the warm reaper was never started")
	}
	deadline := time.Now().Add(warmReaperReady)
	for time.Now().Before(deadline) {
		select {
		case <-w.exited:
			t.Fatalf("UNMEASURED: the warm reaper worker exited: %s", w.exitReason())
		default:
		}
		if n, ok := reaperTicksOK(addr); ok && n >= 1 {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("UNMEASURED: the warm reaper completed no reclaim tick in %s, so a killed run would wait out a fresh worker's grace", warmReaperReady)
}

func stopWarmReaper() {
	warm.mu.Lock()
	w := warm.w
	warm.mu.Unlock()
	if w != nil {
		w.kill()
	}
}
