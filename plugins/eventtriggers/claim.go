package eventtriggers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// ClaimedEvent is what ClaimOrRegisterAwaiter returns when it finds and
// claims a matching event. A nil *ClaimedEvent with a nil error means no
// matching event existed and workflowID (if non-empty) was registered as an
// awaiter instead.
type ClaimedEvent struct {
	EventID    uuid.UUID
	EventType  string
	EventData  []byte
	ReceivedAt time.Time
}

// afterFirstMissBeforeRegister is a TEST-ONLY hook, called (if non-nil)
// after ClaimOrRegisterAwaiter's first claim attempt misses and before it
// registers an awaiter -- the exact window cleat-review's finding on
// cleat#2695 is about. nil in every production path.
// a_a_publish_racing_the_miss_to_register_window_test.go is the only file
// that sets it, and restores it (via t.Cleanup) after every use.
var afterFirstMissBeforeRegister func()

// ClaimOrRegisterAwaiter is the exact mechanism awaitEvent uses internally:
// attempt to atomically claim the oldest matching unprocessed event for
// (tenantID, eventType, keys); if none exists, register workflowID as an
// awaiter for that (eventType, keys) tuple, so a future PublishEvent's
// signalAwaiters call wakes it.
//
// Exported, and awaitEvent itself is rewritten to call this, rather than
// keeping a second copy of the same claim/register logic -- so a plugin
// other than event-triggers (webhookingest, cleat#2649) reuses the IDENTICAL
// mechanism instead of building a second, divergent one. The design doc
// (docs/contributor/design/event-routing-design.md §13) is explicit that two
// delivery mechanisms for one correlated wait is exactly the failure this
// design exists to avoid -- see cleat#2649's own text on
// plugins/webhookingest/routes.go's now-retired static signal path.
//
// keys is validated and slotted exactly as awaitEvent's own Keys parameter
// (see keySlots): at most 3, 128 bytes each, no trailing space.
//
// beforeCommit, if non-nil, runs INSIDE the claim transaction after the
// event is locked and marked consumed, but BEFORE commit -- exactly where
// awaitEvent used to build its own JSON response inline, so that a failure
// there (cleat#2654: a marshal failing on corrupted event_data) rolls the
// claim back instead of durably consuming an event the caller could never
// actually report having found. A caller with nothing that can fail after
// the claim may pass nil, which commits unconditionally.
//
// TWO CLAIM ATTEMPTS, ONE REGISTRATION IN BETWEEN -- cleat-review's finding
// on cleat#2695. PublishEvent's INSERT commits, THEN it calls signalAwaiters
// in a SEPARATE step (publish.go) -- so a publish can land ENTIRELY inside
// the window between this function's first claim attempt missing and its
// registerAwaiterCore write: signalAwaiters runs its own query before our
// awaiter row exists, finds nothing, and never looks again for that same
// event. Without a second attempt, the row sits in ingested_events
// unprocessed and this awaiter is never signalled -- a lost wakeup. This was
// always possible (the mechanism is unchanged from before cleat#2695) and
// always harmless, because every caller until cleat#2649 was a poll: a
// caller that re-invokes on a timer eventually re-runs this whole function
// and its first claim attempt finds the row directly. cleat#2649 gave
// webhookingest's await_webhook a caller (examples/order-lifecycle) that
// suspends on the registered awaiter's signal instead of polling, which is
// exactly the shape that turns an always-latent race into an actual,
// user-visible timeout. The second attempt closes it: any publish that
// completed (INSERT committed) before this function's registration commits
// is guaranteed to still be sitting in ingested_events, unprocessed, when
// the second attempt runs -- signalAwaiters does not delete or otherwise
// mark rows, only ClaimOrRegisterAwaiter's own UPDATE does -- so nothing
// published before the registration can be missed by BOTH the claim that
// looked for it and the signal that would have announced it. Anything
// published AFTER the registration commits is caught the ordinary way, by
// signalAwaiters finding the now-existing awaiter row.
func ClaimOrRegisterAwaiter(
	ctx context.Context,
	db plugin.PluginDB,
	dialect plugin.Dialect,
	logger *slog.Logger,
	tenantID, workflowID, eventType string,
	keys []string,
	beforeCommit func(*ClaimedEvent) error,
) (*ClaimedEvent, error) {
	key1, key2, key3, err := keySlots(keys)
	if err != nil {
		return nil, err
	}

	claimed, err := tryClaim(ctx, db, dialect, tenantID, eventType, key1, key2, key3, beforeCommit)
	if err != nil {
		return nil, err
	}
	if claimed != nil {
		unregisterAwaiter(ctx, db, logger, workflowID, eventType, key1, key2, key3)
		return claimed, nil
	}
	if workflowID == "" {
		return nil, nil
	}

	if afterFirstMissBeforeRegister != nil {
		afterFirstMissBeforeRegister()
	}

	if err := registerAwaiterCore(ctx, db, dialect, logger, tenantID, workflowID, eventType, key1, key2, key3); err != nil {
		return nil, err
	}

	// The re-check. See this function's own doc comment for why one more
	// attempt, here, closes the window rather than merely narrowing it.
	claimed, err = tryClaim(ctx, db, dialect, tenantID, eventType, key1, key2, key3, beforeCommit)
	if err != nil {
		return nil, err
	}
	if claimed != nil {
		// Deliver directly rather than leaving the awaiter just registered
		// above for signalAwaiters to find later: the event this call just
		// claimed will never publish again, so nothing will ever signal
		// that row, and an un-cleaned awaiter is exactly the stale
		// registration cleat-review's GAP 2 warns a caller's own re-check
		// (order.go) has to tolerate rather than treat as fatal.
		unregisterAwaiter(ctx, db, logger, workflowID, eventType, key1, key2, key3)
	}
	return claimed, nil
}

// tryClaim attempts exactly one atomic claim: lock and consume the oldest
// matching unprocessed row in its own transaction, or report (nil, nil) if
// none exists right now. Extracted so ClaimOrRegisterAwaiter can call it
// twice -- see its own doc comment for why.
func tryClaim(
	ctx context.Context,
	db plugin.PluginDB,
	dialect plugin.Dialect,
	tenantID, eventType, key1, key2, key3 string,
	beforeCommit func(*ClaimedEvent) error,
) (*ClaimedEvent, error) {
	var (
		eventID    uuid.UUID
		gotType    string
		eventData  []byte
		receivedAt time.Time
	)

	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("event-triggers: begin claim transaction: %w", err)
	}
	defer tx.Rollback() // no-op once Commit has succeeded

	err = plugin.ScanRow(tx.QueryRow(ctx,
		queryOldestUnprocessedEventForClaim.For(dialect),
		tenantID, eventType, key1, key2, key3), &eventID, &gotType, &eventData, &receivedAt)

	if errors.Is(err, sql.ErrNoRows) {
		// Nothing was locked, so there is nothing to release beyond the
		// deferred Rollback -- but do it now rather than holding the
		// transaction open any longer than the query that needed it.
		_ = tx.Rollback()
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("event-triggers: query events: %w", err)
	}

	// Mark the event as consumed, in the same transaction that locked it. A
	// failure here surfaces as an error instead of being logged and
	// continued past -- the row stays locked-then-rolled-back rather than
	// being reported to the caller as consumed while still unprocessed.
	if _, err := tx.Exec(ctx, `
		UPDATE ingested_events
		SET processed = true, status = 'consumed'
		WHERE id = $1
	`, eventID); err != nil {
		return nil, fmt.Errorf("event-triggers: mark event consumed: %w", err)
	}

	claimed := &ClaimedEvent{
		EventID:    eventID,
		EventType:  gotType,
		EventData:  eventData,
		ReceivedAt: receivedAt,
	}

	if beforeCommit != nil {
		if err := beforeCommit(claimed); err != nil {
			// Falls through to the deferred Rollback: the row stays
			// unprocessed and the next claim can retry it, rather than
			// reporting an error for an event that is durably consumed with
			// no way to ever report it again (cleat#2654).
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("event-triggers: commit claim: %w", err)
	}

	return claimed, nil
}
