package auditlog

// record_event: the workflow-callable host function cleat#2534 asked for, so a
// workflow's own steps can append to its tenant's audit chain the same way an
// HTTP request does.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// workflowEventMethod marks a row as workflow-sourced rather than an HTTP
// request's, so `method=` on GET /audit/events, and the same column in
// cleatctl's export, can select workflow-sourced rows apart from request
// rows.
//
// IT MUST NOT BE A VALID HTTP METHOD TOKEN, and that is not decoration: the
// auditlog middleware records r.Method verbatim for every non-infrastructure
// request, including a failed one, AFTER the handler runs (middleware.go).
// An all-caps word like "PLUGIN_CALL" IS a valid token (RFC 9110's tchar
// grammar has no notion of "looks like an HTTP verb"), so any caller whose
// request reaches this server could plant a row carrying the exact marker
// this file uses to claim "workflow-sourced" -- cleat-review found this on
// #2616 by sending the raw request line "PLUGIN_CALL /tenant.suspended
// HTTP/1.1" to a bare httptest server and getting a 204 with method ==
// "PLUGIN_CALL".
//
// ':' is not a tchar, so "workflow:record_event" is not a syntactically
// valid method token at all: Go's net/http server refuses the request line
// with 400 before any handler, and before this middleware, ever sees it.
// TestWorkflowEventMethodIsNotAValidHTTPMethodToken pins this against a raw
// TCP connection, the same way it was found.
const workflowEventMethod = "workflow:record_event"

// RegisterHostFunctions registers workflow-callable functions on the scoped
// function registry. The plugin name is implicit -- "audit-log" -- so
// "record_event" needs no further qualification (cleat#2534).
//
// plugin.RegisterTyped, not a raw scope.Register (cleat#2626/#2681): the
// Req/Resp types below are exported so cmd/cleat-gen plugin-client can read
// them off this real registration and generate
// cleat/pluginclients/auditlog/client.go, the same way plugins/email and
// plugins/webhookingest already do -- never a hand-maintained manifest.
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if scope == nil {
		return fmt.Errorf("audit-log: nil function registry")
	}
	return plugin.RegisterTyped(scope, plugin.FuncOptions{Name: "record_event"}, p.recordEvent)
}

// RecordEventInput is what a workflow supplies. EventType is the caller's own
// label for what happened ("tenant.suspended", "order.refunded") and is
// stored in `path`, which is indexed and queryable the same way an HTTP
// request's path is (docs/reference/audit-log.md). Details is the caller's
// own JSON object, stored and hashed as `metadata` -- exactly the value
// cleat#2589's fix made safe to hash non-trivial content in.
type RecordEventInput struct {
	EventType string          `json:"event_type"`
	Details   json.RawMessage `json:"details,omitempty"`

	// EventID, when supplied, OVERRIDES the default step-based dedup key --
	// see recordEvent's own doc comment ("EventID: an explicit override") for
	// when a caller would want that. Optional: recordEvent is deduplicated by
	// default (cleat#2618) whether or not this is set.
	EventID string `json:"event_id,omitempty"`
}

type RecordEventOutput struct {
	Recorded bool `json:"recorded"`
}

// recordEvent appends one row to the calling workflow's tenant's audit chain,
// SYNCHRONOUSLY: unlike an HTTP request's audit row (queue.go), this bypasses
// the async queue entirely and calls appendChained directly -- the same
// transaction, and the same per-tenant head lock, a queued HTTP append would
// use (chain_store.go), so the two interleave safely with no second append
// path and no Go-side lock of their own.
//
// It must not return until the append has committed: a workflow's durable
// event history records that this call happened, and if the row were only
// queued rather than written, the history would assert something that might
// still be false when the process is killed.
//
// # The AtLeastOnce residual, and why it is closed by default now
//
// This IS an ordinary AtLeastOnce host function -- the same guarantee every
// other plugin host function has (engine/plugins.go's freshPluginCallInternal
// calls fn, THEN records the event to history; a crash between the two is not
// specially guarded here or anywhere else a plugin registers a function). A
// worker killed after this function's transaction commits but before the
// engine's own event_history record for that call lands will, on resume,
// call this function again with the same input. For a hash-chained log that
// is not a quiet gap -- it would be TWO rows, correctly linked into the
// chain, both asserting an event that happened once.
//
// Originally (cleat#2616) there was no per-call id a plugin function could
// build to catch this with appendOnce's existing errAlreadyRecorded path
// (chain_store.go) -- plugin.CallContext carried no step number. cleat#2618
// closed that: CallContext.RunID and CallContext.Step are now populated at
// every PluginCall dispatch from the SAME s.execRunID/s.stepCount the
// recorded event itself uses (engine/plugin_call_context.go), stable across
// exactly the crash-and-retry this residual describes. So by DEFAULT -- no
// input required -- recordEventStepID derives the row's id from
// (tenant, workflow, RunID, Step), the same shape
// engine.DurableCallIdempotencyKey uses for the separate ServiceCaller path,
// and a retry lands on errAlreadyRecorded instead of a second row.
//
// The one case this does NOT cover: CallContext built directly rather than
// by the engine's PluginCall dispatch (cleattest, a plugin's own unit test,
// an embedder that predates #2618) leaves RunID empty, and this function
// falls back to no dedup at all -- the plain AtLeastOnce behaviour, matching
// what every OTHER plugin host function still has today.
//
// # EventID: an explicit override, for a caller that wants its own key
//
// A caller who supplies EventID does not need the engine's step number at
// all -- it already knows, from its own logic, which of ITS OWN calls are
// "the same event" (cleat-review's finding on #2616). This is useful when
// the same logical event could be triggered from different steps (a retry
// loop inside the workflow itself, not a crash) or when a key stable across
// a workflow definition's own step renumbering is wanted. recordEventDeterministicID
// turns (tenant, workflow, event_id) into the row's id the same way; EventID
// takes priority over the default step-based key when both are available.
func (p *Plugin) recordEvent(ctx context.Context, input RecordEventInput) (RecordEventOutput, error) {
	if p.db == nil {
		return RecordEventOutput{}, fmt.Errorf("audit-log: record_event: no database")
	}

	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == "" {
		return RecordEventOutput{}, fmt.Errorf("audit-log: record_event: no tenant context")
	}
	tenantID, err := uuid.Parse(cc.TenantID)
	if err != nil {
		return RecordEventOutput{}, fmt.Errorf("audit-log: record_event: tenant %q is not a UUID: %w", cc.TenantID, err)
	}

	if input.EventType == "" {
		return RecordEventOutput{}, fmt.Errorf("audit-log: record_event: event_type is required")
	}

	// RegisterTyped's decodeStrict already guarantees input.Details is
	// syntactically valid JSON: a json.RawMessage field cannot decode to
	// anything else, or the decode would already have failed the call before
	// this function ever ran. What it does NOT guarantee is that the value is
	// a JSON OBJECT -- a bare string, number or array is syntactically valid
	// JSON and would decode into Details unchanged. This row's `metadata`
	// column has held only "{}" since the chain existed
	// (docs/reference/audit-log.md), and a consumer reading it back is
	// entitled to assume an object it can index into; refuse anything else
	// here rather than let the first non-object caller define the column's
	// shape by accident. `null` is accepted and treated as "no details", the
	// same as omitting the field.
	var details string
	if len(input.Details) > 0 && string(input.Details) != "null" {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(input.Details, &obj); err != nil {
			return RecordEventOutput{}, fmt.Errorf("audit-log: record_event: details must be a JSON object: %w", err)
		}
		details = string(input.Details)
	}

	// tenantID (not cc.TenantID) is what appendChained's SQL arguments bind:
	// ctx is already tenant-scoped by the engine (execSession.pluginCallContext
	// -> tenantScopedContext, before fn is ever called), which is what lets the
	// row-level policy on audit_events and audit_chain_heads see a tenant at
	// all -- but appendChained's own statements also take tenantID directly as
	// a bound argument, the same way sendMessage's own tenantID does not rely
	// on ctx scoping alone for its non-RLS lookups.
	ce := chainEvent{
		tenantID: tenantID,
		method:   workflowEventMethod,
		path:     input.EventType,
		userID:   cc.WorkflowID,
		metadata: details,
	}
	switch {
	case input.EventID != "":
		ce.id = recordEventDeterministicID(tenantID, cc.WorkflowID, input.EventID)
		ce.retry = true
	case cc.RunID != "":
		// The default path (cleat#2618): cc.RunID is empty only when
		// CallContext was built directly rather than by the engine's
		// PluginCall dispatch (see recordEvent's own doc comment).
		ce.id = recordEventStepID(tenantID, cc.WorkflowID, cc.RunID, cc.Step)
		ce.retry = true
	}
	if err := p.appendChained(ctx, ce); err != nil {
		// Seeing a call's own earlier success again -- a crash-and-retry
		// under the default step-based key, or a caller-supplied EventID
		// repeated on purpose -- is not a failure. See recordEvent's own
		// doc comment.
		if errors.Is(err, errAlreadyRecorded) {
			return RecordEventOutput{Recorded: true}, nil
		}
		return RecordEventOutput{}, fmt.Errorf("audit-log: record_event: %w", err)
	}

	return RecordEventOutput{Recorded: true}, nil
}

// recordEventDeterministicID turns one workflow-supplied EventID into the
// row's id, so a repeat lands on appendOnce's own duplicate-id check
// (chain_store.go's errAlreadyRecorded path) instead of appending twice.
//
// Domain-separated with NUL bytes, the same reasoning
// DurableCallIdempotencyKey gives (engine/idempotency.go) and for the same
// reason: without a separator, tenant "ab" + workflow "c" + event "x" and
// tenant "a" + workflow "bc" + event "x" would hash identically, and two
// unrelated calls could collide. recordEventIDNamespace is a fixed prefix
// rather than a uuid.UUID passed as NewSHA1's "space" argument, because the
// domain separation only has to be unique to THIS derivation, not a
// registered RFC 4122 namespace -- uuid.Nil is passed as the space and the
// prefix does the same job the codebase's other hash domains do (see
// chain.go's chainDomain).
//
// EventID is the CALLER's identifier for its own event, scoped to
// (tenant, workflow): two different workflow instances, or two different
// tenants, using the same EventID string produce different rows, never a
// collision. It is NOT scoped to guard against a hostile caller -- a guest
// that reuses an id on purpose only suppresses ITS OWN later event, which it
// could already do by choosing not to call this function at all
// (cleat-review's non-blocking note on #2616).
const recordEventIDNamespace = "cleat-audit-record-event-v1\x00"

func recordEventDeterministicID(tenantID uuid.UUID, workflowID, eventID string) uuid.UUID {
	var buf bytes.Buffer
	buf.WriteString(recordEventIDNamespace)
	buf.WriteString(tenantID.String())
	buf.WriteByte(0)
	buf.WriteString(workflowID)
	buf.WriteByte(0)
	buf.WriteString(eventID)
	return uuid.NewSHA1(uuid.Nil, buf.Bytes())
}

// recordEventStepID is the DEFAULT dedup key (cleat#2618), derived from the
// engine's own (RunID, Step) rather than anything the caller supplies -- the
// same shape engine.DurableCallIdempotencyKey uses for the ServiceCaller
// path (workflowID, runID, step, NUL-separated), plus tenantID since this
// produces a database row id rather than an opaque header value.
//
// recordEventStepIDNamespace is DELIBERATELY A DIFFERENT fixed prefix from
// recordEventIDNamespace above, not a shared prefix with a "mode" tag glued
// into the middle of the hashed material. Two derivations sharing one prefix
// and differing only in how many NUL-separated parts follow it can collide
// across modes: a crafted EventID equal to "<runID>\x00<step>" would hash
// identically to the step-derived key for that exact (runID, step), because
// NUL-separated concatenation cannot tell "one field containing a NUL" from
// "two fields" apart -- the same ambiguity class DurableCallIdempotencyKey's
// own separators exist to prevent, one level up. A wholly separate namespace
// prefix removes the question rather than trusting no caller ever picks a
// colliding EventID.
//
// The failure that collision would cause has no trace: two calls that
// collide are not recorded as a conflict or an error, they are recorded as
// "already recorded" -- so a colliding EventID would make one of the two
// events silently never appear in the chain at all, discovered (if ever)
// only by noticing an audit trail is missing an event nobody can point to.
// Namespacing costs nothing and removes the possibility outright rather than
// relying on no caller ever choosing a colliding EventID. The NEXT person
// adding a third key source to this function should give it its own
// namespace too, for the same reason.
const recordEventStepIDNamespace = "cleat-audit-record-event-step-v1\x00"

func recordEventStepID(tenantID uuid.UUID, workflowID, runID string, step int) uuid.UUID {
	var buf bytes.Buffer
	buf.WriteString(recordEventStepIDNamespace)
	buf.WriteString(tenantID.String())
	buf.WriteByte(0)
	buf.WriteString(workflowID)
	buf.WriteByte(0)
	buf.WriteString(runID)
	buf.WriteByte(0)
	buf.WriteString(strconv.Itoa(step))
	return uuid.NewSHA1(uuid.Nil, buf.Bytes())
}
