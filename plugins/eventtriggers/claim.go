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
//
// The actual "find and lock" step is dialect-split as of cleat#2821/#2866:
// Postgres and MySQL keep the original single-query shape
// (claimOldestUnprocessedEventLocking), because FOR UPDATE SKIP LOCKED on
// those two dialects locks only the row it returns. SQL Server's closest
// equivalent does not have that guarantee -- see queryCandidateUnprocessedEventIDsMSSQL's
// doc comment in queries.go for the measured mechanism -- so it gets its own
// path (claimOldestUnprocessedEventMSSQL) rather than reusing one that is
// provably unsafe on this dialect.
func tryClaim(
	ctx context.Context,
	db plugin.PluginDB,
	dialect plugin.Dialect,
	tenantID, eventType, key1, key2, key3 string,
	beforeCommit func(*ClaimedEvent) error,
) (*ClaimedEvent, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("event-triggers: begin claim transaction: %w", err)
	}
	defer tx.Rollback() // no-op once Commit has succeeded

	var claimed *ClaimedEvent
	if dialect == plugin.DialectMSSQL {
		claimed, err = claimOldestUnprocessedEventMSSQL(ctx, tx, tenantID, eventType, key1, key2, key3)
	} else {
		claimed, err = claimOldestUnprocessedEventLocking(ctx, tx, dialect, tenantID, eventType, key1, key2, key3)
	}
	if err != nil {
		return nil, err
	}
	if claimed == nil {
		// Nothing was locked (or, on MSSQL, nothing was claimable), so there
		// is nothing to release beyond the deferred Rollback -- but do it now
		// rather than holding the transaction open any longer than the
		// query that needed it.
		_ = tx.Rollback()
		return nil, nil
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

// claimOldestUnprocessedEventLocking is the Postgres/MySQL claim path,
// unchanged in behavior from before cleat#2821/#2866: a single query locks
// and returns the oldest matching row (FOR UPDATE SKIP LOCKED locks only
// that row on these two dialects), then a plain UPDATE by id marks it
// consumed in the same transaction.
func claimOldestUnprocessedEventLocking(
	ctx context.Context,
	tx plugin.PluginTx,
	dialect plugin.Dialect,
	tenantID, eventType, key1, key2, key3 string,
) (*ClaimedEvent, error) {
	var (
		eventID    uuid.UUID
		gotType    string
		eventData  []byte
		receivedAt time.Time
	)

	err := plugin.ScanRow(tx.QueryRow(ctx,
		queryOldestUnprocessedEventForClaim.For(dialect),
		tenantID, eventType, key1, key2, key3), &eventID, &gotType, &eventData, &receivedAt)
	if errors.Is(err, sql.ErrNoRows) {
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

	return &ClaimedEvent{
		EventID:    eventID,
		EventType:  gotType,
		EventData:  eventData,
		ReceivedAt: receivedAt,
	}, nil
}

// claimOldestUnprocessedEventMSSQL is cleat#2821/#2866's fix. It reads a
// small, UNLOCKED, seq-ordered (formerly received_at-ordered; cleat#2652)
// candidate id list (queryCandidateUnprocessedEventIDsMSSQL), then attempts to claim
// candidates one at a time, oldest first, via an equality UPDATE on the
// primary key (queryClaimEventByIDMSSQL). A point UPDATE on `id` cannot need
// to prove anything about any other row to execute, so it cannot lock more
// than the one row it targets -- seeing queries.go's doc comment on those
// two queries for the mechanism this avoids and why the candidate read must
// stay unlocked for that to hold.
//
// READPAST on the claim attempt means a candidate another transaction is
// already mid-claim on is skipped (zero rows affected, reported here as
// sql.ErrNoRows) rather than blocked, so this moves on to the next
// candidate instead of waiting on it.
func claimOldestUnprocessedEventMSSQL(
	ctx context.Context,
	tx plugin.PluginTx,
	tenantID, eventType, key1, key2, key3 string,
) (*ClaimedEvent, error) {
	rows, err := tx.Query(ctx, queryCandidateUnprocessedEventIDsMSSQL, tenantID, eventType, key1, key2, key3)
	if err != nil {
		return nil, fmt.Errorf("event-triggers: query candidate events: %w", err)
	}
	var candidates []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		// plugin.ScanRow, not a bare rows.Scan -- cleat#1137. SQL Server
		// returns UNIQUEIDENTIFIER in mixed-endian byte order; a raw Scan
		// into *uuid.UUID accepts those bytes without error and yields a
		// value that silently fails to match the SAME row when bound back
		// into a later query's WHERE clause, which is exactly what the
		// claim attempt below does with this id. Measured directly while
		// building this function: a bare rows.Scan here made every claim
		// attempt report sql.ErrNoRows against a row that demonstrably
		// existed and was demonstrably unprocessed.
		if err := plugin.ScanRow(rows, &id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("event-triggers: scan candidate event: %w", err)
		}
		candidates = append(candidates, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("event-triggers: list candidate events: %w", err)
	}
	rows.Close()

	for _, id := range candidates {
		var (
			eventID    uuid.UUID
			gotType    string
			eventData  []byte
			receivedAt time.Time
		)
		err := plugin.ScanRow(tx.QueryRow(ctx, queryClaimEventByIDMSSQL, id),
			&eventID, &gotType, &eventData, &receivedAt)
		if errors.Is(err, sql.ErrNoRows) {
			// Already claimed, or locked by a concurrent claimer, between
			// our unlocked read above and this attempt -- try the next
			// oldest candidate rather than giving up.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("event-triggers: claim event %s: %w", id, err)
		}
		return &ClaimedEvent{
			EventID:    eventID,
			EventType:  gotType,
			EventData:  eventData,
			ReceivedAt: receivedAt,
		}, nil
	}
	return nil, nil
}
