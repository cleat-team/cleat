package engine

import "time"

// StartOptions carries the per-run values a start request can set.
//
// # Why this exists rather than a third method
//
// cleat#1186 added StartNewRunWithConcurrencyKey because StartNewRun has four
// real implementations and nine test doubles, and a parameter would have edited
// all thirteen. cleat#1187 needs to set three more per-run values at the same
// moment. A second suffixed variant would have started a family --
// StartNewRunWithConcurrencyKeyAndLimits is the name that was coming -- so the
// two collapse into one options struct here, and the concurrency-key method
// stays as a thin delegate rather than a second code path.
//
// The zero value is the ordinary start: no key, no overrides, resolve
// everything to the tenant and then the operator.
type StartOptions struct {
	// ConcurrencyKey is the key this run wants before it may execute. Empty
	// means none. Recorded on the row so the claim can defer the run while
	// another holds it (cleat#1186).
	ConcurrencyKey string

	// RunLimits are this run's own bounds, each clamped to the tenant's and
	// then to the operator's (cleat#1187). A zero field means "no override",
	// matching ClampToCeiling's convention -- which is also why the columns
	// refuse a stored zero: there it would be indistinguishable from
	// "unbounded".
	//
	// Typed as TenantSettings because the three fields, their meaning and their
	// zero convention are identical. The tier is carried by the variable, not
	// by the type.
	RunLimits TenantSettings
}

// Validate refuses a StartOptions that asks for something cleat will not run: a per-run instance timeout above 0 and under
// MinWasmInstanceTimeout (cleat#3922, owner decision 2026-11-18). The same floor, through the same function and with the same
// wording, as the worker's --wasm-instance-timeout and a tenant's wasm_instance_timeout_ms; see MinWasmInstanceTimeout for why.
//
// Called at the top of every store's StartNewRunWithOptions, so a library caller is held to it as well as an HTTP one, and by the
// start handler before any store work so that the client gets a 400 rather than a store error. 0 means "no override" and passes.
func (o StartOptions) Validate() error {
	return CheckWasmInstanceTimeoutFloor(o.RunLimits.WasmInstanceTimeout, "per-run wasm_instance_timeout_ms",
		"omit it (or send 0) to use the tenant's and the operator's bounds")
}

// msOrNil renders a duration as nullable milliseconds for the per-run limit
// columns.
//
// Non-positive becomes NULL rather than 0, and that is the whole point: the
// columns CHECK against a stored zero because zero is how ClampToCeiling spells
// "unset", so a 0 in the database would be indistinguishable from "unbounded" --
// the escalation the clamp exists to refuse. NULL says "no override" without
// ambiguity.
func msOrNil(d time.Duration) *int64 {
	if d <= 0 {
		return nil
	}
	ms := d.Milliseconds()
	return &ms
}
