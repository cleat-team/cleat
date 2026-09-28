package auditlog

// record_event: the workflow-callable host function cleat#2534 asked for, so a
// workflow's own steps can append to its tenant's audit chain the same way an
// HTTP request does.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// workflowEventMethod marks a row as workflow-sourced rather than an HTTP
// request's. No HTTP method is ever this string, so `method=` on
// GET /audit/events, and the same column in cleatctl's export, can select
// workflow-sourced rows apart from request rows.
const workflowEventMethod = "PLUGIN_CALL"

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
	if err := p.appendChained(ctx, chainEvent{
		tenantID: tenantID,
		method:   workflowEventMethod,
		path:     input.EventType,
		userID:   cc.WorkflowID,
		metadata: details,
	}); err != nil {
		return "", fmt.Errorf("audit-log: record_event: %w", err)
	}

	out, _ := json.Marshal(recordEventOutput{Recorded: true})
	return string(out), nil
}
