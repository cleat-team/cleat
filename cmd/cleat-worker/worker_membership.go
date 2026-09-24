package main

import (
	"context"
	"errors"
	"fmt"
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
	staleAfter := membershipStaleAfter(interval)

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

// membershipStaleAfter is the gap between two successful membership ticks that this
// worker treats as a lapse, and the age at which its lease is swept. One function,
// so the loop and validateHeartbeat cannot disagree about it.
func membershipStaleAfter(heartbeat time.Duration) time.Duration {
	if heartbeat <= 0 {
		heartbeat = 5 * time.Second
	}
	return max(heartbeat*2, 10*time.Second)
}

// validateHeartbeat refuses a --heartbeat at which a stalled worker can be invisible
// to a secret writer AND not know it (cleat#2167).
//
// A writer counts a worker as live for engine.SecretKeyLiveWindow after its last
// heartbeat. The worker, for its part, re-registers and re-checks only after a gap
// longer than membershipStaleAfter. If that threshold is not below the writer's
// window, a stall can outlast the window (the writer writes a key version this worker
// cannot open) yet end before the threshold (the worker resumes, sees no lapse, and
// serves). Above a heartbeat of the window itself it is worse: a healthy worker's own
// beats are further apart than the writer will wait, so it is not counted between them.
//
// Measured on the code as it was, with the worker's row 6 minutes old and a 4 minute
// heartbeat: the write succeeded and the worker was not stopped; at a 10s threshold it
// was stopped.
//
// REFUSED, NOT CLAMPED, for the reason validateReclaimTimeout gives: clamping would
// hand the operator a heartbeat they did not ask for.
func validateHeartbeat(heartbeat time.Duration) error {
	stale := membershipStaleAfter(heartbeat)
	if stale < engine.SecretKeyLiveWindow {
		return nil
	}
	// The largest heartbeat that passes: the threshold is 2x the heartbeat above 5s.
	limit := (engine.SecretKeyLiveWindow - time.Nanosecond) / 2
	return fmt.Errorf(
		"--heartbeat %v is too long: a worker's membership threshold is max(2 x --heartbeat, 10s) = %v, "+
			"which is not below the %v a secret writer waits before it stops counting a worker as live.\n"+
			"A worker stalled for longer than that could be written past (a key version it cannot open) "+
			"and resume without noticing.\n"+
			"Use a --heartbeat below %ds.",
		heartbeat, stale, engine.SecretKeyLiveWindow, int((limit.Truncate(time.Second)+time.Second)/time.Second))
}

func (w *Worker) membershipTick(staleAfter time.Duration) {
	ctx := w.ctx

	// A lapse is a gap longer than the stale window between two successful
	// ticks. Nothing was watching during it: a writer can have judged this
	// worker gone and written a key version it cannot open, and the row may not
	// even have been swept, in which case the heartbeat below succeeds and
	// would say nothing. So a lapse is treated like a swept row -- register
	// again and run the secrets check again, as one span under the gate -- and
	// the worker does not resume unless the check passes. (cleat#1991. A stall
	// still leaves the stall itself unobserved; this bounds it, it does not
	// remove it -- and only while membershipStaleAfter is below the writers' live
	// window, which validateHeartbeat enforces. cleat#2167.)
	lapsed := !w.membershipLastBeat.IsZero() && time.Since(w.membershipLastBeat) > staleAfter

	hbErr := w.workerRegistry.Heartbeat(ctx, w.id)
	notRegistered := errors.Is(hbErr, engine.ErrWorkerNotRegistered)
	if hbErr != nil && !notRegistered {
		w.logger.ErrorContext(ctx, "worker registry: heartbeat failed",
			"worker_id", w.id, "error", hbErr)
		w.Metrics.RecordBackgroundLoop(ctx, "worker_membership", "error")
		return
	}
	if notRegistered || lapsed {
		// A sweep removed this worker, or its membership loop stalled: it was
		// paused, or its heartbeat was blocked, for longer than the expiry
		// window. Re-register rather than heartbeat into a row that is not
		// there -- an UPDATE matching nothing reports no error, so nothing else
		// would ever notice.
		w.logger.WarnContext(ctx, "worker registry: re-registering after a lapse",
			"worker_id", w.id, "stale_after", staleAfter, "row_was_swept", notRegistered)
		if rerr := w.registerInWorkerRegistry(ctx); rerr != nil {
			var refusal *secretsRefusal
			if errors.As(rerr, &refusal) {
				// A definite answer: while this worker was not being watched,
				// something was stored that it cannot open. Serving on would
				// fail the first workflow that resolves it, so stop.
				w.logger.ErrorContext(ctx, "CRITICAL: after a lapse this worker cannot open "+
					"every stored secret; shutting down rather than serve", "worker_id", w.id, "error", rerr)
				w.cancel()
				return
			}
			w.logger.ErrorContext(ctx, "worker registry: re-registration failed",
				"worker_id", w.id, "error", rerr)
			w.Metrics.RecordBackgroundLoop(ctx, "worker_membership", "error")
			return
		}
	}
	w.membershipLastBeat = time.Now()

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

// registerInWorkerRegistry records this worker as present and re-runs the
// secrets check under the gate. See registerWithKeyCheck.
func (w *Worker) registerInWorkerRegistry(ctx context.Context) error {
	return registerWithKeyCheck(ctx, w.workerRegistry, w.secrets, engine.WorkerRegistration{
		WorkerID:         w.id,
		Hostname:         hostnameOrEmpty(),
		PID:              os.Getpid(),
		Concurrency:      w.concurrency,
		ConnectionBudget: w.clusterConnectionBudget,
	}, func(msg string) { w.logger.WarnContext(ctx, msg, "worker_id", w.id) })
}
