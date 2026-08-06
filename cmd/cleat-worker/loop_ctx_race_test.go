package main

import (
	"context"
	"sync"
	"testing"
)

// TestInitLoopCtxIsSafeAgainstRunningLoops reproduces the data race that killed
// a worker in the cluster job.
//
//	fatal error: concurrent map read and map write
//	main.(*Worker).getLoopCtx        setup.go:1070
//	main.(*Worker).reaperLoop        setup.go:1868
//
// Run initialises nine per-loop contexts, launches all nine, and *then* calls
// initLoopCtx once more for the watchdog. That last write went into
// loopCtxMap without holding loopMu, while nine goroutines were already
// running and calling getLoopCtx, which reads the same map under the lock.
//
// "concurrent map read and map write" is a Go runtime fatal, not a panic, so
// withPanicRecovery cannot catch it: the process dies. That is what
// crash-looped cleat-worker-3.
//
// The test models exactly that shape -- readers running, a late registration
// arriving -- rather than calling Run, which would need a database and a
// worker's worth of setup to reach the same two lines. It is worth little
// without -race, which is how the repo runs its race job; without it the race
// is real but usually invisible, which is why this survived.
func TestInitLoopCtxIsSafeAgainstRunningLoops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := &Worker{
		ctx:           ctx,
		loopCtxMap:    make(map[string]*loopContext),
		loopFuncs:     make(map[string]func()),
		healthTracker: newHealthTracker(),
	}

	// Loops that are already up, reading the map the way every background loop
	// does on each iteration.
	const readers = 8
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = w.getLoopCtx("reaper")
				}
			}
		}()
	}

	// The late arrival: Run's watchdog registration, after the others are live.
	for i, name := range []string{"watchdog", "dispatch", "schedule", "retention"} {
		w.initLoopCtx(name)
		w.registerLoopFunc(name, func() {})
		if i == 0 {
			// Give the readers a moment to be genuinely concurrent with the
			// remaining writes rather than racing only the first.
			for j := 0; j < 1000; j++ {
				_ = w.getLoopCtx("watchdog")
			}
		}
	}

	close(stop)
	wg.Wait()

	// Sanity: the writes actually landed, so a version that "fixed" the race by
	// not writing would not pass.
	for _, name := range []string{"watchdog", "dispatch", "schedule", "retention"} {
		w.loopMu.Lock()
		_, ok := w.loopCtxMap[name]
		_, okFn := w.loopFuncs[name]
		w.loopMu.Unlock()
		if !ok {
			t.Errorf("loopCtxMap is missing %q", name)
		}
		if !okFn {
			t.Errorf("loopFuncs is missing %q", name)
		}
	}
}
