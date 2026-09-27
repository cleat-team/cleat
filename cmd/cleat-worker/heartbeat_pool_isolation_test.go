package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// TestHeartbeatBatchFencedIsolatedFromASaturatedExecutionPool is the
// real-database reproduction of cleat#2009's heartbeat-pool-exhaustion cause:
// a burst of long-held connections on `store`'s pool can queue a heartbeat
// write behind them with no priority, which the reaper cannot distinguish
// from the worker being genuinely gone.
//
// It saturates a one-connection execution pool with a long-running query,
// then calls HeartbeatBatchFenced through heartbeatBatchStore() twice against
// the SAME saturated pool: once with no isolated heartbeatStore configured
// (the pre-#2009 shape, still what a Worker with --heartbeat-max-connections=0
// gets) and once with an isolated heartbeatStore pool wired in. The first
// must block behind the saturating query; the second must not.
//
// Both arms are exercised by the SAME running server (no source mutation, no
// revert-and-restore): heartbeatBatchStore's nil-fallback IS the behavior
// being falsified against, so toggling heartbeatStore nil/non-nil selects
// between them directly.
func TestHeartbeatBatchFencedIsolatedFromASaturatedExecutionPool(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping the heartbeat-pool-isolation reproduction")
	}
	if testing.Short() {
		t.Skip("Skipping a real heartbeat-pool-isolation reproduction in short mode")
	}

	// Two independent *sql.DB pools against the SAME database -- exactly
	// main.go's own shape, where heartbeatDB is opened with the identical DSN
	// as the execution pool's `db`. A *sql.DB is its own connection pool
	// regardless of how many others share its DSN, which is the whole
	// mechanism this test exists to exercise.
	execDB := testutil.SuiteTestDB(t, "cleat_worker_hbpool")
	heartbeatDB := testutil.SuiteTestDB(t, "cleat_worker_hbpool")
	execDB.SetMaxOpenConns(1)
	execDB.SetMaxIdleConns(1)
	heartbeatDB.SetMaxOpenConns(1)
	heartbeatDB.SetMaxIdleConns(1)

	execStore := engine.NewPostgresStore(execDB)
	heartbeatStore := engine.NewPostgresStore(heartbeatDB)

	// Saturate execDB's single connection with a long-running query, and
	// confirm it is actually held before either heartbeat call runs -- not
	// merely dispatched. db.Stats().InUse is the fact, not the sleep's
	// duration: a slow CI runner make the sleep's own wall-clock start late,
	// but InUse==1 is unambiguous the moment the connection is taken.
	saturateCtx, cancelSaturate := context.WithCancel(context.Background())
	defer cancelSaturate()
	saturationStarted := make(chan struct{})
	saturationDone := make(chan error, 1)
	go func() {
		close(saturationStarted)
		_, err := execDB.ExecContext(saturateCtx, "SELECT pg_sleep(5)")
		saturationDone <- err
	}()
	<-saturationStarted

	deadline := time.After(3 * time.Second)
	for {
		if execDB.Stats().InUse >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("execDB never reported an in-use connection; saturation never took hold")
		case <-time.After(10 * time.Millisecond):
		}
	}

	w := &Worker{
		id:             "hbpool-test-worker",
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:          execStore,
		heartbeatStore: nil, // pre-#2009 shape: falls back to the saturated `store`
	}

	dummyRuns := []engine.GenerationKey{{WorkflowID: "hbpool-test-nonexistent", Generation: 1}}

	// Case A: no isolated pool. heartbeatBatchStore() falls back to `store`,
	// which is execDB -- saturated. This call must NOT complete before the
	// saturating query releases its connection; a short deadline confirms it
	// blocks rather than merely runs slowly.
	blockedCtx, blockedCancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer blockedCancel()
	_, err := w.heartbeatBatchStore().HeartbeatBatchFenced(blockedCtx, w.id, dummyRuns)
	if err == nil {
		t.Fatalf("expected HeartbeatBatchFenced to block behind the saturated pool and time out, but it returned nil error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the blocked call to fail with context.DeadlineExceeded, got: %v", err)
	}

	// Case B: an isolated heartbeatStore pool is wired in. Same worker, same
	// saturated execDB, same dummy runs -- but heartbeatBatchStore() now
	// prefers heartbeatStore, which has its own untouched connection.
	w.heartbeatStore = heartbeatStore
	fastCtx, fastCancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer fastCancel()
	if _, err := w.heartbeatBatchStore().HeartbeatBatchFenced(fastCtx, w.id, dummyRuns); err != nil {
		t.Fatalf("expected HeartbeatBatchFenced through the isolated heartbeat pool to succeed "+
			"despite the saturated execution pool, got: %v", err)
	}

	// Cancelling here (rather than waiting out the full 5s sleep) is a
	// cleanup speedup, not part of what is asserted -- the driver's exact
	// cancellation error text is not something this test pins.
	cancelSaturate()
	<-saturationDone
}
