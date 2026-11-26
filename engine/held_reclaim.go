package engine

import (
	"context"
	"fmt"
	"time"
)

// cleat#4275: a run held in memory through a wait is reclaimed sooner than a running one when its holder dies.
//
// A held run is the one running run that is provably not executing: it makes no outbound call while held, and the check that ends a hold (the worker's claimStillMine)
// is a fenced write that refuses a run whose claim has moved. So the stall allowance that makes the ordinary reclaim window long (a database stall must not reclaim the
// whole running set, where a false reclaim repeats an effect) is not owed to it, and a held row may be reclaimed once its heartbeat is `held` stale instead of `timeout` stale.
//
// WHICH ROWS. Only a row whose held_generation equals its generation: the marker is written by the holder, fenced on (assigned_to, generation), and cleared by the holder, fenced
// the same way, BEFORE the hold's claim check returns. A reclaim bumps generation, so a marker the old holder could no longer clear is inert from the next claim on; it
// cannot make the next owner's executing row look held.
//
// HOW IT REACHES THE REAPER. By the context, so ReapStaleInstances' signature, and every double of it, is unchanged: a caller that says nothing gets exactly the ordinary
// predicate. reapAfterHeld turns the two thresholds into the one the statements use.

type heldReclaimKey struct{}

// WithHeldReclaimAfter arms the held arm of the reclaim statements for the calls made with the returned context: a row marked held (held_generation = generation) is reclaimed
// once its heartbeat is d stale, where d is shorter than the timeout the call is given. d <= 0, or d at least the timeout, arms nothing.
func WithHeldReclaimAfter(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, heldReclaimKey{}, d)
}

// heldStaleness is the staleness the held arm uses: the shorter of the context's value and the timeout, which is the timeout itself (no held arm) when the context carries none.
func heldStaleness(ctx context.Context, timeout time.Duration) time.Duration {
	d, _ := ctx.Value(heldReclaimKey{}).(time.Duration)
	if d <= 0 || d >= timeout {
		return timeout
	}
	return d
}

// HeldRunMarker is implemented by a store that can record that a run is held. It is its own interface, like StaleHolderReaper, so a store without it (most doubles) simply has
// no fast path: its held runs are reclaimed at the ordinary window, which is the safe reading.
type HeldRunMarker interface {
	// MarkRunHeld records that runID, at generation, is held by workerID: the run's held_generation becomes generation. Fenced on (assigned_to, generation): a run whose claim
	// has moved returns ErrFenceLost and nothing is written. The caller refuses the hold on any error.
	MarkRunHeld(ctx context.Context, runID, workerID string, generation int64) error

	// ClearRunHeld clears the marker with the same fence. It is called when a hold ends, BEFORE the claim check returns, so that a run the guest is about to resume is never marked.
	// A run whose marker is already clear and whose claim still holds returns nil; a run whose claim has moved returns ErrFenceLost.
	ClearRunHeld(ctx context.Context, runID, workerID string, generation int64) error
}

// heldIntervalText is heldStaleness as the "<n> milliseconds" interval text the PostgreSQL statements take.
func heldIntervalText(d time.Duration) string {
	return fmt.Sprintf("%d milliseconds", d.Milliseconds())
}
