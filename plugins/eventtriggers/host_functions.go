package eventtriggers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// RegisterHostFunctions registers workflow-callable functions on the scoped
// function registry under the "event-triggers" plugin namespace.
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if scope == nil {
		return fmt.Errorf("event-triggers: nil function registry")
	}
	if err := scope.Register(plugin.FuncOptions{
		Name: "await_event",
		// NEITHER. It selects the oldest UNPROCESSED event, so a replay can
		// match a different one -- and on the not-found path it WRITES, calling
		// registerAwaiter before returning a successful "no event" output.
		// That output is recorded, so under cleat#1318 a replay returns it and
		// does not re-register.
		//
		// WHAT THAT GIVES UP, stated because it was load-bearing by accident:
		// re-invoking on replay used to re-create an awaiter row that had been
		// lost. The row is written durably on the original call, so replay does
		// not need to redo it -- but a deployment that lost the row was being
		// repaired by a code path whose stated purpose was something else.
		Idempotent:        false,
		SameValueOnReplay: false,
	}, p.awaitEvent); err != nil {
		return err
	}
	return nil
}

// ---- Input/output types ----

type awaitEventInput struct {
	EventType string   `json:"event_type"`
	TimeoutMs int64    `json:"timeout_ms"`
	Keys      []string `json:"keys,omitempty"`
}

type awaitEventOutput struct {
	Found      bool            `json:"found"`
	EventID    string          `json:"event_id,omitempty"`
	EventType  string          `json:"event_type,omitempty"`
	EventData  json.RawMessage `json:"event_data,omitempty"`
	ReceivedAt string          `json:"received_at,omitempty"`
}

// ---- Host functions ----

// awaitEvent queries for the oldest matching unprocessed event for the
// workflow's tenant -- oldest, not newest, so a backlog of the same event
// type drains in order rather than starving whichever event arrived first
// (cleat#2641). If a matching event is found, it is returned and the
// workflow proceeds.  If none is found, the output {"found": false} is
// returned and the workflow engine will retry according to its retry policy.
//
// The publish handler (handlePublishEvent) also broadcasts a signal named
// "__evt:<eventType>" so that workflows awaiting this event type can be
// woken up promptly instead of waiting for the next poll cycle.
func (p *Plugin) awaitEvent(ctx context.Context, inputJSON string) (string, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == "" {
		return "", fmt.Errorf("event-triggers: no tenant context")
	}

	var input awaitEventInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("event-triggers: invalid input: %w", err)
	}
	if input.EventType == "" {
		return "", fmt.Errorf("event-triggers: event_type is required")
	}
	key1, key2, key3, err := keySlots(input.Keys)
	if err != nil {
		return "", err
	}

	// Find and claim the oldest matching unprocessed event for this tenant +
	// type in one transaction, so a failure to mark it consumed cannot be
	// logged-and-continued into a double consume (cleat#2641). The claiming
	// query locks the row (FOR UPDATE SKIP LOCKED / WITH (UPDLOCK, READPAST,
	// ROWLOCK)) for exactly the lifetime of this transaction -- see
	// queryOldestUnprocessedEventForClaim's doc comment for why that is what
	// makes two concurrent await_event calls safe rather than merely usually
	// fine.
	var (
		eventID    uuid.UUID
		eventType  string
		eventData  []byte
		receivedAt time.Time
	)

	tx, err := p.db.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("event-triggers: begin claim transaction: %w", err)
	}
	defer tx.Rollback() // no-op once Commit has succeeded

	err = plugin.ScanRow(tx.QueryRow(ctx,
		queryOldestUnprocessedEventForClaim.For(p.dialect),
		cc.TenantID, input.EventType, key1, key2, key3), &eventID, &eventType, &eventData, &receivedAt)

	if errors.Is(err, sql.ErrNoRows) {
		// Nothing was locked, so there is nothing to release beyond the
		// deferred Rollback -- but do it now rather than holding the
		// transaction open across registerAwaiter's separate write.
		_ = tx.Rollback()

		// No matching event found -- register as an awaiter so the publish
		// handler can signal this workflow when a matching event arrives.
		if cc.WorkflowID != "" {
			// input.Keys, via the same keySlots() validation the claim query
			// above was matched against -- key1/key2/key3 are already
			// computed and validated once, up front, so the awaiter this
			// registers and the event this call just failed to find agree on
			// what "matching" means. cleat#2625, P1's read side (cleat#2647).
			//
			// Not `Found: false` on failure: that is a success report, and it
			// is exactly the lie cleat#1473 is about.
			if err := p.registerAwaiter(ctx, cc.TenantID, cc.WorkflowID, input.EventType, key1, key2, key3); err != nil {
				return "", err
			}
		}

		output := awaitEventOutput{Found: false}
		outJSON, err := json.Marshal(output)
		if err != nil {
			// Nothing was consumed on this path, so there is no durability
			// concern here -- but a discarded error here used to return ""
			// on failure, indistinguishable from Found:false's own JSON. A
			// caller could not tell "no event yet" from "marshalling broke",
			// which is the same lie cleat#1473 is about, from the other
			// side (cleat#2654).
			return "", fmt.Errorf("event-triggers: marshal await_event output: %w", err)
		}
		return string(outJSON), nil
	}
	if err != nil {
		return "", fmt.Errorf("event-triggers: query events: %w", err)
	}

	// Mark the event as consumed, in the same transaction that locked it.
	// A failure here now surfaces as an error instead of being logged and
	// continued past -- the row stays locked-then-rolled-back rather than
	// being reported to the caller as consumed while still unprocessed.
	if _, err := tx.Exec(ctx, `
		UPDATE ingested_events
		SET processed = true, status = 'consumed'
		WHERE id = $1
	`, eventID); err != nil {
		return "", fmt.Errorf("event-triggers: mark event consumed: %w", err)
	}

	// Built and marshaled BEFORE Commit, deliberately. Marshaling after commit
	// would mean a marshal failure -- e.g. corrupted event_data, as happened
	// on cleat#2645's own CI run -- reports an error while the event stays
	// durably consumed with no way to ever report it again: a lost event
	// dressed up as a failure. Built here, a marshal failure instead falls
	// through to the deferred Rollback, so the row stays unprocessed and the
	// next claim can retry it (cleat#2654).
	output := awaitEventOutput{
		Found:      true,
		EventID:    eventID.String(),
		EventType:  eventType,
		EventData:  json.RawMessage(eventData),
		ReceivedAt: receivedAt.Format(time.RFC3339),
	}
	outJSON, err := json.Marshal(output)
	if err != nil {
		return "", fmt.Errorf("event-triggers: marshal await_event output: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("event-triggers: commit claim: %w", err)
	}

	p.logger.Info("event-triggers: event consumed via await_event",
		"event_id", eventID,
		"event_type", eventType,
		"tenant", cc.TenantID,
		"workflow_id", cc.WorkflowID,
	)

	// Clean up any pending awaiter registration for this workflow + event
	// type + keys -- the same key1/key2/key3 computed above, so this only
	// ever removes the awaiter row this exact call would itself have
	// registered on a "not found" path, never a differently-keyed one still
	// legitimately waiting (unregisterAwaiter's own doc comment).
	unregisterAwaiter(ctx, p.db, p.logger, cc.WorkflowID, input.EventType, key1, key2, key3)

	return string(outJSON), nil
}

// registerAwaiter records that the given workflow is waiting for an event of
// the specified type.  This allows the publish handler to deliver a signal
// when a matching event arrives.
// RETURNS ITS ERROR, and that is the whole of cleat#1473.
//
// It used to log and return nothing, so awaitEvent's caller could not tell a
// registration that happened from one that did not -- and awaitEvent went on
// to return a SUCCESSFUL `{"found": false}` either way. A workflow told "no
// event yet" settles down to wait, and the row that would have woken it does
// not exist. The failure was observed and then discarded into a log line, which
// is the worst place for it: the run hangs and nothing above it knows why.
//
// The caller propagates rather than degrading. awaitEvent already fails loudly
// for every other database error on this path -- the `query events` branch
// returns its error -- so registration was the one write whose failure was
// swallowed, and propagating makes the function uniform. A visible error beats
// an invisible wait.
func (p *Plugin) registerAwaiter(ctx context.Context, tenantID, workflowID, eventType, key1, key2, key3 string) error {
	regKey := registrationKey(workflowID, eventType, key1, key2, key3)
	_, err := p.db.Exec(ctx, plugin.Rebind(upsertAwaiter.For(p.dialect), p.dialect),
		workflowID, tenantID, eventType, key1, key2, key3, regKey)
	if err != nil {
		p.logger.Warn("event-triggers: register awaiter", "error", err, "workflow_id", workflowID)
		return fmt.Errorf("event-triggers: register awaiter: %w", err)
	}
	return nil
}
