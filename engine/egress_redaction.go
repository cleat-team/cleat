package engine

import (
	"encoding/json"
)

// Redaction happens where data LEAVES the system, not where a workflow reads it
// (cleat#3581, owner decision D49-1a). A store hands back what it stored,
// replay acts on that, and the worker's observability surfaces call these
// before they write a record to a client, a log or a terminal.
//
// What is and is not egress is drawn on cleat#3581: a tenant reading its own
// workflow's status, result and inputs is the data plane and is NOT redacted;
// the history and event APIs, the stream, the log lines and the operator's
// terminal output are observability and ARE.

// eventDataFields is every EventRecord string field that can carry data a
// workflow or a service produced. Each is redacted with Redact, which reads a
// JSON document and leaves anything else alone.
//
// TestEventRecordFieldsAreAllClassified fails when EventRecord gains a string
// field that is in neither this table nor the metadata list kept beside that
// test, so a new field is a decision and not an omission.
var eventDataFields = map[string]func(*EventRecord) *string{
	"Request":          func(r *EventRecord) *string { return &r.Request },
	"Response":         func(r *EventRecord) *string { return &r.Response },
	"Err":              func(r *EventRecord) *string { return &r.Err },
	"SignalPayload":    func(r *EventRecord) *string { return &r.SignalPayload },
	"PromiseResult":    func(r *EventRecord) *string { return &r.PromiseResult },
	"PromiseError":     func(r *EventRecord) *string { return &r.PromiseError },
	"ChildInput":       func(r *EventRecord) *string { return &r.ChildInput },
	"NewInput":         func(r *EventRecord) *string { return &r.NewInput },
	"PluginInput":      func(r *EventRecord) *string { return &r.PluginInput },
	"PluginOutput":     func(r *EventRecord) *string { return &r.PluginOutput },
	"PluginError":      func(r *EventRecord) *string { return &r.PluginError },
	"UpdatePayload":    func(r *EventRecord) *string { return &r.UpdatePayload },
	"UpdateResponse":   func(r *EventRecord) *string { return &r.UpdateResponse },
	"UpdateError":      func(r *EventRecord) *string { return &r.UpdateError },
	"StateValue":       func(r *EventRecord) *string { return &r.StateValue },
	"FetchHeaders":     func(r *EventRecord) *string { return &r.FetchHeaders },
	"FetchBody":        func(r *EventRecord) *string { return &r.FetchBody },
	"FetchResponse":    func(r *EventRecord) *string { return &r.FetchResponse },
	"CronInput":        func(r *EventRecord) *string { return &r.CronInput },
	"CronResult":       func(r *EventRecord) *string { return &r.CronResult },
	"DetachedInput":    func(r *EventRecord) *string { return &r.DetachedInput },
	"SideEffectResult": func(r *EventRecord) *string { return &r.SideEffectResult },
	"ReplacedErrorMsg": func(r *EventRecord) *string { return &r.ReplacedErrorMsg },
	"Message":          func(r *EventRecord) *string { return &r.Message },
	"LogKV":            func(r *EventRecord) *string { return &r.LogKV },
}

// RedactEventRecordForEgress returns a copy of rec with every data-bearing
// field redacted. The argument is not modified.
func RedactEventRecordForEgress(rec EventRecord) EventRecord {
	for _, field := range eventDataFields {
		p := field(&rec)
		*p = Redact(*p)
	}
	return rec
}

// RedactEventRecordsForEgress is RedactEventRecordForEgress over a slice. It
// returns a new slice and leaves its argument alone; a nil slice stays nil, so a
// response that encoded as `null` still does.
func RedactEventRecordsForEgress(recs []EventRecord) []EventRecord {
	if recs == nil {
		return nil
	}
	out := make([]EventRecord, len(recs))
	for i := range recs {
		out[i] = RedactEventRecordForEgress(recs[i])
	}
	return out
}

// RedactWorkflowInstanceForEgress returns a copy of wf with its Input, Result
// and Error redacted. It is NOT applied to the data-plane reads of a workflow;
// a caller asks for it (the dashboard does, cleat#3581).
func RedactWorkflowInstanceForEgress(wf WorkflowInstance) WorkflowInstance {
	if len(wf.Input) > 0 {
		wf.Input = json.RawMessage(Redact(string(wf.Input)))
	}
	wf.Result = Redact(wf.Result)
	wf.Error = Redact(wf.Error)
	return wf
}
