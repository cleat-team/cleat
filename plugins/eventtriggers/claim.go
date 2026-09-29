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
		// transaction open across the registration write below.
		_ = tx.Rollback()

		if workflowID != "" {
			if err := registerAwaiterCore(ctx, db, dialect, logger, tenantID, workflowID, eventType, key1, key2, key3); err != nil {
				return nil, err
			}
		}
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

	// Clean up any pending awaiter registration for this workflow + event
	// type + keys -- the same key1/key2/key3 computed above, so this only
	// ever removes the awaiter row this exact call would itself have
	// registered on a "not found" path, never a differently-keyed one still
	// legitimately waiting (unregisterAwaiter's own doc comment).
	unregisterAwaiter(ctx, db, logger, workflowID, eventType, key1, key2, key3)

	return claimed, nil
}
