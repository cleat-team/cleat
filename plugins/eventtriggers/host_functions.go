package eventtriggers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/cleat-team/cleat/plugin"
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

	// The claim/register mechanism itself lives in ClaimOrRegisterAwaiter
	// (claim.go), exported so webhookingest (cleat#2649) reuses the same
	// mechanism rather than building a second, divergent one -- awaitEvent
	// is now a thin wrapper over it, not a second copy.
	var outJSON []byte
	claimed, err := ClaimOrRegisterAwaiter(ctx, p.db, p.dialect, p.logger,
		cc.TenantID, cc.WorkflowID, input.EventType, input.Keys,
		func(c *ClaimedEvent) error {
			// Built and marshaled BEFORE Commit, deliberately, via this
			// hook: marshaling after commit would mean a marshal failure --
			// e.g. corrupted event_data, as happened on cleat#2645's own CI
			// run -- reports an error while the event stays durably
			// consumed with no way to ever report it again: a lost event
			// dressed up as a failure. A failure here instead rolls the
			// claim back, so the row stays unprocessed and the next claim
			// can retry it (cleat#2654).
			out := awaitEventOutput{
				Found:      true,
				EventID:    c.EventID.String(),
				EventType:  c.EventType,
				EventData:  json.RawMessage(c.EventData),
				ReceivedAt: c.ReceivedAt.Format(time.RFC3339),
			}
			var marshalErr error
			outJSON, marshalErr = json.Marshal(out)
			if marshalErr != nil {
				return fmt.Errorf("event-triggers: marshal await_event output: %w", marshalErr)
			}
			return nil
		})
	if err != nil {
		return "", err
	}
	if claimed == nil {
		// No matching event found -- ClaimOrRegisterAwaiter already
		// registered cc.WorkflowID as an awaiter (if non-empty), so the
		// publish handler can signal this workflow when a matching event
		// arrives. Not `Found: false` reported as an error: that would be
		// the lie cleat#1473 is about, from the other direction.
		out := awaitEventOutput{Found: false}
		notFoundJSON, marshalErr := json.Marshal(out)
		if marshalErr != nil {
			// Nothing was consumed on this path, so there is no durability
			// concern here -- but a discarded error here used to return ""
			// on failure, indistinguishable from Found:false's own JSON. A
			// caller could not tell "no event yet" from "marshalling
			// broke", which is the same lie cleat#1473 is about.
			return "", fmt.Errorf("event-triggers: marshal await_event output: %w", marshalErr)
		}
		return string(notFoundJSON), nil
	}

	// ClaimOrRegisterAwaiter already unregistered any pending awaiter row for
	// this workflow + event type + keys as part of the same claim -- see its
	// own doc comment for why that reuses the exact key1/key2/key3 the claim
	// matched against, never a differently-keyed row still legitimately
	// waiting.
	p.logger.Info("event-triggers: event consumed via await_event",
		"event_id", claimed.EventID,
		"event_type", claimed.EventType,
		"tenant", cc.TenantID,
		"workflow_id", cc.WorkflowID,
	)

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
	return registerAwaiterCore(ctx, p.db, p.dialect, p.logger, tenantID, workflowID, eventType, key1, key2, key3)
}

// registerAwaiterCore is registerAwaiter's actual body, as a free function
// so ClaimOrRegisterAwaiter (claim.go) can call the SAME write rather than
// keeping a second copy -- the same reason ClaimOrRegisterAwaiter itself
// exists (see its doc comment).
func registerAwaiterCore(ctx context.Context, db plugin.PluginDB, dialect plugin.Dialect, logger *slog.Logger, tenantID, workflowID, eventType, key1, key2, key3 string) error {
	regKey := registrationKey(workflowID, eventType, key1, key2, key3)
	_, err := db.Exec(ctx, plugin.Rebind(upsertAwaiter.For(dialect), dialect),
		workflowID, tenantID, eventType, key1, key2, key3, regKey)
	if err != nil {
		logger.Warn("event-triggers: register awaiter", "error", err, "workflow_id", workflowID)
		return fmt.Errorf("event-triggers: register awaiter: %w", err)
	}
	return nil
}
