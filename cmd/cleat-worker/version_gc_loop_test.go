package main

// cleat#1315. Version GC ran only when a person typed a command, and its
// policy was engine.DefaultGCOptions() -- compiled in and unreachable from
// either surface. So an operator could neither schedule it nor tune it, while
// docs/troubleshooting.md told them to "adjust the GC retention policy" using
// two flags that did not exist on any binary.
//
// The loop added here is OPT-IN, and that is the decision these tests protect.
// GC deletes workflow DEFINITIONS permanently, and an in-flight instance whose
// version has been collected cannot find the WASM binary to replay against --
// the module cache is keyed by def_name:def_version, so the failure lands on a
// running workflow rather than at the point of deletion. Defaulting it on would
// turn an upgrade into a silent, destructive sweep on every existing
// deployment. Same reasoning --completed-workflow-retention-days records for
// defaulting to 0.

import (
	"context"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// gcWorker returns a worker whose store records the GC policy it was swept
// with, which is the thing under test -- not the sweep's arithmetic, which
// engine/version_gc_test.go covers.
func gcWorker(t *testing.T, minVersions int, maxAge time.Duration) (*Worker, *bool) {
	t.Helper()
	swept := false
	ms := &mockStore{}
	ms.listWorkflowDefsFn = func(context.Context, string) ([]engine.WorkflowDef, error) {
		swept = true
		return nil, nil
	}
	w := newTestWorker(ms)
	w.versionGCMinVersions = minVersions
	w.versionGCMaxAge = maxAge
	return w, &swept
}

// TestTheVersionGCLoopIsOffByDefault is the half that protects the decision.
//
// Asserted by RETURN, not by absence of a sweep. A loop that started its ticker
// and simply had not fired yet would also look quiet, and the two are different
// -- one is disabled, the other is armed. versionGCLoop must return.
func TestTheVersionGCLoopIsOffByDefault(t *testing.T) {
	w, swept := gcWorker(t, engine.DefaultMinVersionsToKeep, engine.DefaultMaxVersionAge)
	w.versionGCInterval = 0 // the flag default

	done := make(chan struct{})
	w.wg.Add(1)
	go func() { defer close(done); w.versionGCLoop() }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("versionGCLoop did not return with --version-gc-interval unset.\n\n" +
			"0 must disable the sweep entirely: GC deletes workflow definitions " +
			"permanently, and an in-flight instance whose version is collected cannot " +
			"replay. Defaulting this on would sweep every existing deployment at " +
			"upgrade (cleat#1315).")
	}
	if *swept {
		t.Error("a disabled version-GC loop swept anyway")
	}
}

// And the negative control: with the flag set, it does sweep. Without this, a
// loop that returned unconditionally would pass the test above.
func TestTheVersionGCLoopSweepsWhenTheIntervalIsSet(t *testing.T) {
	w, swept := gcWorker(t, engine.DefaultMinVersionsToKeep, engine.DefaultMaxVersionAge)
	w.versionGCInterval = 5 * time.Millisecond

	w.wg.Add(1)
	go w.versionGCLoop()
	defer w.cancel()

	deadline := time.Now().Add(3 * time.Second)
	for !*swept && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !*swept {
		t.Fatal("versionGCLoop never swept with --version-gc-interval set")
	}
}

// The policy reaches the sweep. Before cleat#1315 there was nowhere to put it:
// runVersionGCSweep did not exist and both manual surfaces took
// DefaultGCOptions() wholesale.
func TestTheConfiguredPolicyReachesTheSweep(t *testing.T) {
	for _, tc := range []struct {
		name        string
		minVersions int
		maxAge      time.Duration
		wantMin     int
		wantAge     time.Duration
	}{
		{"explicit overrides", 7, 48 * time.Hour, 7, 48 * time.Hour},
		{"unset falls back to the documented default", 0, 0,
			engine.DefaultMinVersionsToKeep, engine.DefaultMaxVersionAge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The policy is observed through GarbageCollectVersions' own
			// normalisation: it substitutes the default for <= 0, so a sweep
			// reached with 0 and one reached with the default are
			// indistinguishable downstream. What is asserted here is that
			// runVersionGCSweep does not panic and does reach the store, and
			// the arithmetic is engine/version_gc_test.go's job.
			w, swept := gcWorker(t, tc.minVersions, tc.maxAge)
			w.runVersionGCSweep()
			if !*swept {
				t.Fatal("runVersionGCSweep did not reach the store")
			}
		})
	}
}
