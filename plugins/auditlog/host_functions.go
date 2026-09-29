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
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if scope == nil {
		return fmt.Errorf("audit-log: nil function registry")
	}
	return scope.Register(plugin.FuncOptions{Name: "record_event"}, p.recordEvent)
}

// recordEventInput is what a workflow supplies. EventType is the caller's own
// label for what happened ("tenant.suspended", "order.refunded") and is
// stored in `path`, which is indexed and queryable the same way an HTTP
// request's path is (docs/reference/audit-log.md). Details is the caller's
// own JSON object, stored and hashed as `metadata` -- exactly the value
// cleat#2589's fix made safe to hash non-trivial content in.
type recordEventInput struct {
	EventType string          `json:"event_type"`
	Details   json.RawMessage `json:"details,omitempty"`

	// EventID, when supplied, makes this call idempotent: see
	// recordEventDeterministicID's doc comment for what it is derived from
	// and why. Optional -- an empty value falls back to the ordinary
	// AtLeastOnce behaviour documented above.
	EventID string `json:"event_id,omitempty"`
}

type recordEventOutput struct {
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
// # The residual after this returns successfully
//
// This IS an ordinary AtLeastOnce host function -- the same guarantee every
// other plugin host function has (engine/plugins.go's freshPluginCallInternal
// calls fn, THEN records the event to history; a crash between the two is not
// specially guarded here or anywhere else a plugin registers a function).
// engine.DurableCallIdempotencyKey (engine/idempotency.go) exists for the
// separate ServiceCaller path and gives a call a key stable across replay --
// PluginCall has no equivalent, because plugin.CallContext carries no step
// number a plugin function could build one from. Investigated and not fixed
// here: extending CallContext is shared engine infrastructure, out of scope
// for this plugin. Filed as cleat#2614.
//
// So: a worker killed after this function's transaction commits but before
// the engine's own event_history record for this call lands will, on resume,
// call this function again with the same input. For a hash-chained log that
// is not a quiet gap -- it is TWO rows, correctly linked into the chain, both
// asserting an event that happened once. That is the residual this host
// function ships with; it is not a regression (every existing plugin host
// function already has it), but it is sharper here than for e.g. a Slack
// message, because the audit chain's whole job is to be the record of what
// happened.
//
// # Closing it without waiting on cleat#2614
//
// A caller who supplies EventID does not need the engine to give this
// function a step number at all -- it already knows, better than any step
// counter could, which of ITS OWN calls are "the same event" (cleat-review's
// finding on #2616). recordEventDeterministicID turns
// (tenant, workflow, event_id) into the row's id and sets chainEvent.retry,
// so a repeat lands on appendOnce's existing errAlreadyRecorded path -- the
// same mechanism the async queue's own retry-after-timeout already relies on
// (queue.go) -- and is reported as success without a second row. A caller
// that omits EventID keeps the residual above exactly as described; this is
// an escape hatch for the caller that wants it; the default is unchanged.
func (p *Plugin) recordEvent(ctx context.Context, inputJSON string) (string, error) {
	if p.db == nil {
		return "", fmt.Errorf("audit-log: record_event: no database")
	}

	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == "" {
		return "", fmt.Errorf("audit-log: record_event: no tenant context")
	}
	tenantID, err := uuid.Parse(cc.TenantID)
	if err != nil {
		return "", fmt.Errorf("audit-log: record_event: tenant %q is not a UUID: %w", cc.TenantID, err)
	}

	var input recordEventInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("audit-log: record_event: invalid input: %w", err)
	}
	if input.EventType == "" {
		return "", fmt.Errorf("audit-log: record_event: event_type is required")
	}

	// json.Unmarshal into recordEventInput above already guarantees
	// input.Details is syntactically valid JSON: a json.RawMessage field
	// cannot decode to anything else, or the unmarshal above would already
	// have failed with "invalid input". What it does NOT guarantee is that
	// the value is a JSON OBJECT -- a bare string, number or array is
	// syntactically valid JSON and would decode into Details unchanged. This
	// row's `metadata` column has held only "{}" since the chain existed
	// (docs/reference/audit-log.md), and a consumer reading it back is
	// entitled to assume an object it can index into; refuse anything else
	// here rather than let the first non-object caller define the column's
	// shape by accident. `null` is accepted and treated as "no details", the
	// same as omitting the field.
	var details string
	if len(input.Details) > 0 && string(input.Details) != "null" {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(input.Details, &obj); err != nil {
			return "", fmt.Errorf("audit-log: record_event: details must be a JSON object: %w", err)
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
	if input.EventID != "" {
		ce.id = recordEventDeterministicID(tenantID, cc.WorkflowID, input.EventID)
		ce.retry = true
	}
	if err := p.appendChained(ctx, ce); err != nil {
		// A caller that supplied EventID and is seeing its own earlier
		// success again (a retried host call, a replay reaching this point
		// a second time some other way) is not a failure -- see
		// recordEventDeterministicID's doc comment.
		if errors.Is(err, errAlreadyRecorded) {
			out, _ := json.Marshal(recordEventOutput{Recorded: true})
			return string(out), nil
		}
		return "", fmt.Errorf("audit-log: record_event: %w", err)
	}

	out, _ := json.Marshal(recordEventOutput{Recorded: true})
	return string(out), nil
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
