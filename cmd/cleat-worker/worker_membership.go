package main

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// workerMembershipLoop renews this worker's registry lease, counts the live
// workers, and resizes this worker's share of the cluster connection budget.
//
// cleat#1487. One loop rather than three, because all three want the same
// cadence and the count is only meaningful immediately after the heartbeat that
// makes this worker part of it.
//
// It rides the heartbeat interval for a reason worth stating: the share is
// stale between ticks, so during a join the sum of every worker's share can
// briefly exceed the budget. That window is one tick wide, which is why the
// tick is the existing 5s heartbeat and not a leisurely reaper interval.
func (w *Worker) workerMembershipLoop() {
	defer w.wg.Done()

	interval := w.heartbeatInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	// The same shape as the zombie reaper's staleTimeout: a lease is stale at
	// twice the heartbeat, and never sooner than 10s.
	staleAfter := max(interval*2, 10*time.Second)

	w.healthTracker.setInterval("worker_membership", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Deregister on the way out, so the share this worker held is released
	// immediately instead of after the expiry window. A fresh context because
	// w.ctx is already cancelled by the time a shutdown reaches here.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := w.workerRegistry.Deregister(ctx, w.id); err != nil {
			// Not fatal and not silent: the row expires on its own, the
			// cluster just takes staleAfter longer to notice.
			w.logger.WarnContext(ctx, "worker registry: could not deregister on shutdown",
				"worker_id", w.id, "error", err)
		}
	}()

	for {
		select {
		case <-w.getLoopCtx("worker_membership").Done():
			return
		case <-ticker.C:
			w.healthTracker.recordRun("worker_membership")
			start := time.Now()
			w.membershipTick(staleAfter)
			w.Metrics.SetBackgroundLoopDuration(w.ctx, "worker_membership", time.Since(start).Seconds())
		}
	}
}

func (w *Worker) membershipTick(staleAfter time.Duration) {
	ctx := w.ctx

	if err := w.workerRegistry.Heartbeat(ctx, w.id); err != nil {
		if errors.Is(err, engine.ErrWorkerNotRegistered) {
			// A sweep removed this worker: it was paused, or its heartbeat
			// was blocked, for longer than the expiry window. Re-register
			// rather than heartbeat into a row that is not there -- an
			// UPDATE matching nothing reports no error, so nothing else
			// would ever notice.
			w.logger.WarnContext(ctx, "worker registry: re-registering after expiry",
				"worker_id", w.id, "stale_after", staleAfter)
			if rerr := w.registerInWorkerRegistry(ctx); rerr != nil {
				w.logger.ErrorContext(ctx, "worker registry: re-registration failed",
					"worker_id", w.id, "error", rerr)
				w.Metrics.RecordBackgroundLoop(ctx, "worker_membership", "error")
				return
			}
		} else {
			w.logger.ErrorContext(ctx, "worker registry: heartbeat failed",
				"worker_id", w.id, "error", err)
			w.Metrics.RecordBackgroundLoop(ctx, "worker_membership", "error")
			return
		}
	}

	// Every worker sweeps. A DELETE matching nothing is free, and two workers
	// removing the same expired row is not a conflict -- the second removes
	// zero. That is the reasoning the existing reaper runs on, and it avoids
	// needing an election to run a tidy-up.
	//
	// It runs AFTER the heartbeat above, which reads as though a worker would
	// otherwise sweep itself. Measured: it would not. Reversing the two leaves
	// the behaviour unchanged, because a swept worker's heartbeat returns
	// ErrWorkerNotRegistered and the branch above re-registers it. The order is
	// the more obvious one to read, not a correctness requirement.
	if n, err := w.workerRegistry.SweepExpired(ctx, staleAfter); err != nil {
		w.logger.WarnContext(ctx, "worker registry: sweep failed",
			"worker_id", w.id, "error", err)
	} else if n > 0 {
		w.logger.InfoContext(ctx, "worker registry: removed expired workers",
			"worker_id", w.id, "count", n, "stale_after", staleAfter)
	}

	if w.connectionShare == nil {
		w.Metrics.RecordBackgroundLoop(ctx, "worker_membership", "ok")
		return
	}

	live, err := w.workerRegistry.CountLive(ctx, staleAfter)
	if err != nil {
		// The share is deliberately NOT changed here. A failed count is not
		// evidence that this worker is alone, and defaulting to "alone" is
		// the one reading that would have it take the whole budget.
		w.logger.WarnContext(ctx, "worker registry: could not count live workers; "+
			"the connection share is unchanged", "worker_id", w.id, "error", err)
		w.Metrics.RecordBackgroundLoop(ctx, "worker_membership", "error")
		return
	}

	before := w.connectionShare.Current()
	share := w.connectionShare.Observe(live)
	if share != before {
		w.applyConnectionShare(ctx, share, live)
	} else if pending, heldFor := w.connectionShare.Pending(); pending > 0 {
		w.logger.InfoContext(ctx, "connection share is entitled to grow and is holding",
			"worker_id", w.id, "live_workers", live, "current", share,
			"pending", pending, "held_for", heldFor)
	}
	w.Metrics.RecordBackgroundLoop(ctx, "worker_membership", "ok")
}

// applyConnectionShare hands the tenant pools what this worker's share leaves
// after its fixed pools.
//
// The fixed pools are NOT resized here, and that is a real limit rather than an
// oversight: shrinking the core pool below --concurrency changes how much work
// a worker can run at once, not merely how many connections it holds, so it is
// a behaviour change that wants its own decision. When the share cannot cover
// the fixed pools this says so, loudly, and clamps the tenant pools to their
// minimum rather than pretending the budget is met.
func (w *Worker) applyConnectionShare(ctx context.Context, share, live int) {
	// Both budgets bind when both are set: the cluster share says what this
	// worker may take of a shared pool, --connection-budget says what it may
	// take at all. The smaller wins, which is the only reading under which
	// setting both means what an operator would expect.
	if w.perWorkerConnectionBudget > 0 && share > w.perWorkerConnectionBudget {
		share = w.perWorkerConnectionBudget
	}

	fixed := w.connectionBudgetParts.Fixed()
	tenantShare := share - fixed

	if tenantShare < 1 {
		// Note the clamp is 1, not 0: SetConnectionBudget reads zero as
		// "unbounded", so passing the arithmetic straight through would
		// remove the bound at exactly the moment the budget is tightest.
		w.logger.ErrorContext(ctx,
			"connection share does not cover this worker's fixed pools; "+
				"tenant pools clamped to 1 and the fixed pools are over the share",
			"worker_id", w.id, "live_workers", live, "share", share,
			"fixed_pools_need", fixed, "over_by", fixed-share+1)
		tenantShare = 1
	}

	if w.tenantPools != nil {
		w.tenantPools.SetConnectionBudget(tenantShare)
	}
	w.logger.InfoContext(ctx, "connection share resized",
		"worker_id", w.id, "live_workers", live, "share", share,
		"fixed_pools", fixed, "tenant_connection_budget", tenantShare)
}

// registerInWorkerRegistry records this worker as present.
func (w *Worker) registerInWorkerRegistry(ctx context.Context) error {
	return w.workerRegistry.Register(ctx, engine.WorkerRegistration{
		WorkerID:         w.id,
		Hostname:         hostnameOrEmpty(),
		PID:              os.Getpid(),
		Concurrency:      w.concurrency,
		ConnectionBudget: w.clusterConnectionBudget,
	})
}
