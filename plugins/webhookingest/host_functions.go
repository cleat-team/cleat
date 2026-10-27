package webhookingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/eventtriggers"
	"github.com/google/uuid"
)

// RegisterHostFunctions registers workflow-callable functions on the scoped
// function registry. The plugin name is implicit -- each plugin gets its own
// scope, so function names need not be globally unique.
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if scope == nil {
		return fmt.Errorf("webhook-ingest: nil function registry")
	}
	if err := plugin.RegisterTyped(scope, plugin.FuncOptions{
		Name: "await_webhook",
		// NEITHER, same shape as eventtriggers.await_event: an await over
		// mutable state, consuming from a queue of deliveries. cleat#1318.
		Idempotent:        false,
		SameValueOnReplay: false,
	}, p.awaitWebhook); err != nil {
		return err
	}
	return nil
}

// ---- Input/output types ----
//
// Exported (cleat#2626): these are the Req/Resp types a
// plugin.RegisterTyped call site publishes, and cmd/cleat-gen plugin-client
// reads them off this real registration -- never a hand-maintained
// manifest -- to generate a typed caller-side client. An unexported type
// here would type-check fine but be unusable from the generated client's
// own callers, so the generator refuses one rather than emitting a client
// nobody outside this package can call.

type AwaitWebhookInput struct {
	SourceID  string `json:"source_id"`
	EventType string `json:"event_type,omitempty"`
	// Keys correlates this await to one specific event among the many a
	// shared source can publish -- e.g. an order id -- mirroring
	// eventtriggers.awaitEventInput's Keys field (same validation, same
	// claim/register mechanism underneath). cleat#2649. Additive: a caller
	// upgrading from before this field existed omits it and gets the empty
	// slice, matching today's behaviour of correlating on SourceID alone.
	//
	// THE EFFECTIVE BUDGET IS 2, NOT 3 -- cleat-review's finding on this
	// PR. eventtriggers has three correlation-key slots, but awaitWebhook
	// always prepends SourceID as key1 (host_functions.go's awaitWebhook),
	// so Keys here only ever fills key2 and key3. A caller passing 3
	// entries hits eventtriggers' own "at most 3 correlation keys ... got
	// 4" error, which names the wrong number for this call site -- it is
	// correct about eventtriggers.ClaimOrRegisterAwaiter's budget, not
	// about what a caller of THIS field may pass. Pass at most 2.
	Keys []string `json:"keys,omitempty"`
}

type AwaitWebhookOutput struct {
	Found      bool            `json:"found"`
	ID         string          `json:"id,omitempty"`
	EventType  string          `json:"event_type,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	ReceivedAt string          `json:"received_at,omitempty"`
}

// ---- Host functions ----

// awaitWebhook claims the oldest matching unclaimed webhook for the
// workflow's tenant, correlated by source and (optionally) by Keys --
// cleat#2649, built on the exact same mechanism event-triggers.awaitEvent
// uses (eventtriggers.ClaimOrRegisterAwaiter), so a shared source can
// correlate to whichever run is actually waiting for THIS delivery rather
// than handing every awaiter the same oldest-unprocessed row. If a matching
// event is found, it is claimed and returned. If none is found, the output
// {"found": false} is returned and cc.WorkflowID (if set) is registered as
// an awaiter, so eventtriggers' publish handler can signal it directly
// instead of this call being the only way to notice a match.
//
// This replaced a two-step, non-transactional poll (a SELECT, then a
// separate UPDATE with no row lock and no WHERE processed = false guard) --
// a genuine, previously untested TOCTOU race between two concurrent
// awaiters for the same event. ClaimOrRegisterAwaiter's claim and its
// processed/status update run in one transaction with a locking read
// (queryOldestUnprocessedEventForClaim), which closes it as a side effect
// of adopting the shared mechanism, not as a separate fix.
//
// source_id is now REQUIRED. BREAKING CHANGE, cleat#2649: before this, an
// empty source_id meant "any source for this tenant" -- a real, working
// poll filter. The correlated claim has no way to express that: key1 is
// always the publishing source's own id (routes.go's handleIngestWebhook),
// an EQUALITY match, not a wildcard, and there is no dialect-portable way to
// ask "key1 is anything" without ALSO matching every other tenant source's
// events, which would reopen the exact cross-source collision key1 exists to
// prevent. cleat-review checked every tracked caller (SDK examples, plugin
// harness in all five guest languages, docs) on 2026-09-29 and found none
// that relies on the any-source form. See UPGRADE_NOTES.md.
//
// A deleted source's events are cancelled, not delivered here -- owner
// decision on cleat#2199, applied in cleat-review on #2221, extended in
// cleat#2649: handleDeleteSource (routes.go) now cancels a source's pending
// rows in BOTH webhook_events (the original fix) AND ingested_events (keyed
// by key1 = the source's id), in the same transaction as the soft-delete,
// so this claim can never observe one either way. Practically: once a
// source is deleted, no event of its ever reaches this function again,
// delivered or not, past or future -- an awaiting workflow simply keeps
// getting {"found": false} and is woken only by its own retry policy's
// eventual timeout, the same as if the source had gone quiet rather than
// been deleted. There is no signal here that the source was deleted rather
// than merely idle; a caller that needs to distinguish the two has to check
// GET /ingest/sources/{id} itself.
func (p *Plugin) awaitWebhook(ctx context.Context, input AwaitWebhookInput) (AwaitWebhookOutput, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == "" {
		return AwaitWebhookOutput{}, fmt.Errorf("webhook-ingest: no tenant context")
	}

	if input.SourceID == "" {
		return AwaitWebhookOutput{}, fmt.Errorf("webhook-ingest: source_id is required for correlated await")
	}
	sourceID, err := uuid.Parse(input.SourceID)
	if err != nil {
		return AwaitWebhookOutput{}, fmt.Errorf("webhook-ingest: invalid source_id: %w", err)
	}

	// defaultWebhookEventType, not "": the claim is keyed on an EXACT
	// event_type (eventtriggers.queryOldestUnprocessedEventForClaim), unlike
	// the old poll where an unset EventType meant "no filter". Matches
	// handleIngestWebhook's own default (routes.go) -- see
	// defaultWebhookEventType's doc comment for why the two sides have to
	// agree rather than each treating "unset" independently.
	eventType := input.EventType
	if eventType == "" {
		eventType = defaultWebhookEventType
	}

	// GUARD: input.Keys and the source's own correlation_key_field must AGREE
	// on whether key2 exists at all -- cleat-review's finding on #2697. Key
	// slots are strict equality and "" is a sentinel, not a wildcard
	// (keys.go), so a mismatch is silent in BOTH directions:
	//
	//  1. Keys is non-empty but the source has no correlation_key_field:
	//     handleIngestWebhook never extracts a key2 (routes.go, gated on
	//     CorrelationKeyField != ""), so every published event's key2 is ""
	//     while every awaiter registers key2 = input.Keys[0]. They can never
	//     match -- this is exactly the bug cleat#2697's own scenario script
	//     hit (scripts/run-order-lifecycle-scenario.sh, fixed alongside
	//     this), and it is not specific to that script: any tenant that
	//     passes Keys against a source with no correlation_key_field hits it.
	//  2. Keys is empty but the source HAS a correlation_key_field: published
	//     events carry the extracted key2, but this await registers "" --
	//     so setting correlation_key_field on an existing source silently
	//     breaks every key-less await_webhook call already using it.
	//
	// Both directions produce {"found": false} forever, indistinguishable
	// from "no webhook has arrived yet" -- the same "never-matching awaiter
	// is a silent failure" reasoning the owner used for requiring source_id
	// in the first place (see this function's own doc comment). A tenant-
	// scoped read here, rather than trusting AwaitWebhookInput alone, is the
	// only way to catch a mismatch that Go's type system cannot: the source
	// row is the one place both sides of the comparison actually live.
	// Same query shape as handleGetSource (routes.go) -- deliberately, not
	// a narrower "just the one column" query: the fake DB test harness
	// (webhookingest_behavioral_test.go) recognises queries by matching
	// their exact text, and this shape is already a recognised case there,
	// so this reuses it rather than adding a second one for a single
	// column.
	var src webhookSourceJSON
	err = plugin.ScanRow(p.db.QueryRow(ctx, `
		SELECT id, tenant_id, name, source_type, secret_configured, enabled, correlation_key_field, created_at, updated_at
		FROM webhook_sources
		WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL
	`, sourceID, cc.TenantID), &src.ID, &src.TenantID, &src.Name, &src.SourceType,
		&src.SecretConfigured, &src.Enabled,
		&src.CorrelationKeyField, &src.CreatedAt, &src.UpdatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// NOT an error -- a gone-or-never-existed source is the documented
		// {"found": false} case (this function's own doc comment, owner
		// decision on cleat#2199: "an awaiting workflow simply keeps getting
		// {'found': false}... There is no signal here that the source was
		// deleted rather than merely idle"). TestADeletedSourcesPendingEventIsCancelledNotDelivered
		// pins exactly this: a deleted source's already-cancelled events
		// must read as not-found, not as an error. There is nothing to
		// validate Keys against, so the mismatch guard below is skipped;
		// ClaimOrRegisterAwaiter will not find anything for a source with
		// no live rows either way.
	case err != nil:
		return AwaitWebhookOutput{}, fmt.Errorf("webhook-ingest: look up source: %w", err)
	case len(input.Keys) > 0 && src.CorrelationKeyField == "":
		return AwaitWebhookOutput{}, fmt.Errorf(
			"webhook-ingest: Keys were passed but source %s has no correlation_key_field configured -- "+
				"they would never match a delivered event; set correlation_key_field on the source or omit Keys",
			sourceID)
	case len(input.Keys) == 0 && src.CorrelationKeyField != "":
		return AwaitWebhookOutput{}, fmt.Errorf(
			"webhook-ingest: source %s has correlation_key_field %q configured, but no Keys were passed -- "+
				"this await would never match a delivered event; pass Keys or use a source with no correlation_key_field",
			sourceID, src.CorrelationKeyField)
	}

	// key1 = this source's id, ALWAYS first -- matching exactly what
	// handleIngestWebhook publishes (routes.go). input.Keys, if the caller
	// passes any, becomes key2 (and key3): a caller correlating on an order
	// id passes Keys: []string{orderID}, which must be the SAME value
	// handleIngestWebhook's CorrelationKeyField extraction produced for the
	// matching webhook, or the two never meet -- same as any correlation
	// mismatch, {"found": false} rather than an error. The guard above rules
	// out the one case that mismatch is never intentional; a genuine key2
	// VALUE mismatch (right shape, wrong value) still reads as "not found",
	// which is correct -- that is an ordinary miss, not a configuration bug.
	keys := append([]string{sourceID.String()}, input.Keys...)

	var out AwaitWebhookOutput
	claimed, err := eventtriggers.ClaimOrRegisterAwaiter(ctx, p.db, p.dialect, p.logger,
		cc.TenantID, cc.WorkflowID, eventType, keys,
		func(c *eventtriggers.ClaimedEvent) error {
			// c.EventData is the WRAPPER envelope handleIngestWebhook
			// publishes ({"source_id":...,"headers":...,"payload":...}), not
			// the bare webhook body -- eventtriggers' own subscription/
			// auto-start consumers want the envelope, so publishing is left
			// unchanged. AwaitWebhookOutput.Payload's contract predates that
			// envelope (it was webhook_events.payload, the bare body), so
			// this unwraps back to it rather than changing the field's
			// shape as a side effect of the storage move.
			var envelope struct {
				Payload json.RawMessage `json:"payload"`
			}
			if err := json.Unmarshal(c.EventData, &envelope); err != nil {
				// PoisonEvent, not a bare error -- cleat#2666. c.EventData is
				// the only thing this unmarshal can fail on, and a
				// corrupted row will fail identically on every future
				// claim: deterministic, not transient. See
				// eventtriggers.PoisonEvent's doc comment.
				return eventtriggers.PoisonEvent(fmt.Errorf("webhook-ingest: unwrap event payload: %w", err))
			}
			out = AwaitWebhookOutput{
				Found:      true,
				ID:         c.EventID.String(),
				EventType:  c.EventType,
				Payload:    envelope.Payload,
				ReceivedAt: c.ReceivedAt.Format(time.RFC3339),
			}
			return nil
		})
	if err != nil {
		return AwaitWebhookOutput{}, err
	}
	if claimed == nil {
		return AwaitWebhookOutput{Found: false}, nil
	}

	p.logger.Info("webhook-ingest: event consumed via await_webhook",
		"event_id", claimed.EventID,
		"event_type", claimed.EventType,
		"tenant", cc.TenantID,
		"workflow_id", cc.WorkflowID,
	)

	return out, nil
}
