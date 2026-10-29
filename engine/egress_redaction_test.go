package engine

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// eventMetadataFields are the EventRecord string fields that name or classify
// an event and carry no workflow data: identifiers, operation names, codes.
// FetchURL is here knowingly: a secret in a URL's query string is not a
// field-name match, which docs/how-to/use-secrets.md already says of Redact.
var eventMetadataFields = []string{
	"EventType", "Service", "Op", "ErrCode", "ResolvedBy", "SignalNames", "SignalName",
	"DeferDescription", "DeferID", "PromiseName", "PromiseID", "ChildName", "RunID",
	"ParentWorkflowID", "ParentClosePolicy", "PluginName", "PluginFunc",
	"UpdateHandlerName", "UpdateRequestID", "StateKey", "StateOp", "StateKeys",
	"FetchMethod", "FetchURL", "CronWorkflowName", "CronExpr", "CronTimezone",
	"CronScheduleID", "DetachedName", "DetachedRunID", "LockKey", "ReplacedStatus",
	"ReplacedErrorCode", "ReplacedErrorOp", "ReplacedCompletedAt", "ScopeKey", "LogLevel",
}

// TestEventRecordFieldsAreAllClassified fails when EventRecord gains a string
// field that egress redaction has not been told about. A field that carries
// data and is missing from eventDataFields would leave a secret in the history
// API; this makes that a failing test rather than an omission nobody sees.
func TestEventRecordFieldsAreAllClassified(t *testing.T) {
	meta := map[string]bool{}
	for _, n := range eventMetadataFields {
		meta[n] = true
	}
	rt := reflect.TypeOf(EventRecord{})
	seen := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.Type.Kind() != reflect.String {
			continue
		}
		seen[f.Name] = true
		_, isData := eventDataFields[f.Name]
		switch {
		case isData && meta[f.Name]:
			t.Errorf("EventRecord.%s is in BOTH eventDataFields and eventMetadataFields", f.Name)
		case !isData && !meta[f.Name]:
			t.Errorf("EventRecord.%s is a string field that egress redaction has not classified: add it to eventDataFields if it can carry workflow data, otherwise to eventMetadataFields", f.Name)
		}
	}
	// And the reverse: a name in either table that is not a field is a typo that
	// would otherwise pass silently.
	for n := range eventDataFields {
		if !seen[n] {
			t.Errorf("eventDataFields names %q, which is not a string field of EventRecord", n)
		}
	}
	for _, n := range eventMetadataFields {
		if !seen[n] {
			t.Errorf("eventMetadataFields names %q, which is not a string field of EventRecord", n)
		}
	}
}

// secretBody carries a secret under a key Redact always rewrites. The control is
// asserted wherever it is used: a test of "redacted at egress" that fed a body
// Redact leaves alone would pass whether or not egress redacts.
const secretBody = `{"api_key":"sk-live-SECRET123","version":"1.2.3","max_tokens":5}`

func TestEgressRedactionRewritesEveryDataField(t *testing.T) {
	if got := Redact(secretBody); !strings.Contains(got, `"api_key":"[REDACTED]"`) {
		t.Fatalf("CONTROL: Redact does not rewrite the secret in %s: %s", secretBody, got)
	}
	var rec EventRecord
	for _, field := range eventDataFields {
		*field(&rec) = secretBody
	}
	rec.Service, rec.Op, rec.RunID = "billing", "charge", "run-1"

	out := RedactEventRecordForEgress(rec)

	for name, field := range eventDataFields {
		got := *field(&out)
		if strings.Contains(got, "sk-live-SECRET123") {
			t.Errorf("%s still holds the secret after egress redaction: %s", name, got)
		}
		// The rest of the document survives, so redaction is not "blank the field".
		if !strings.Contains(got, `"version":"1.2.3"`) || !strings.Contains(got, `"max_tokens":5`) {
			t.Errorf("%s lost data redaction should have kept: %s", name, got)
		}
	}
	if out.Service != "billing" || out.Op != "charge" || out.RunID != "run-1" {
		t.Errorf("a metadata field changed: %+v", out)
	}
	// The argument is a copy: replay reads the same record and must see the value.
	for name, field := range eventDataFields {
		if got := *field(&rec); got != secretBody {
			t.Errorf("RedactEventRecordForEgress modified its argument's %s: %s", name, got)
		}
	}
}

func TestEgressRedactionOfASliceCopiesAndKeepsNil(t *testing.T) {
	in := []EventRecord{{Response: secretBody}, {Request: secretBody}}
	out := RedactEventRecordsForEgress(in)
	if strings.Contains(out[0].Response, "sk-live") || strings.Contains(out[1].Request, "sk-live") {
		t.Errorf("not redacted: %+v", out)
	}
	if in[0].Response != secretBody || in[1].Request != secretBody {
		t.Errorf("the input slice was modified: %+v", in)
	}
	if RedactEventRecordsForEgress(nil) != nil {
		t.Error("a nil slice must stay nil, or a response that encoded as null encodes as []")
	}
	if got := RedactEventRecordsForEgress([]EventRecord{}); got == nil || len(got) != 0 {
		t.Errorf("an empty slice must stay an empty slice: %#v", got)
	}
}

func TestEgressRedactionOfAWorkflowInstance(t *testing.T) {
	wf := WorkflowInstance{
		ID:     "wf-1",
		Input:  json.RawMessage(secretBody),
		Result: secretBody,
		Error:  `{"password":"hunter2","why":"bad"}`,
	}
	out := RedactWorkflowInstanceForEgress(wf)
	for name, got := range map[string]string{"Input": string(out.Input), "Result": out.Result, "Error": out.Error} {
		if strings.Contains(got, "sk-live-SECRET123") || strings.Contains(got, "hunter2") {
			t.Errorf("%s still holds a secret: %s", name, got)
		}
	}
	if !strings.Contains(out.Result, `"max_tokens":5`) || !strings.Contains(out.Error, `"why":"bad"`) {
		t.Errorf("redaction removed more than the secret: result=%s error=%s", out.Result, out.Error)
	}
	if out.ID != "wf-1" {
		t.Errorf("a non-data field changed: %q", out.ID)
	}
	if string(wf.Input) != secretBody || wf.Result != secretBody {
		t.Error("RedactWorkflowInstanceForEgress modified its argument")
	}
	// An instance with nothing to redact keeps its absent input absent.
	if got := RedactWorkflowInstanceForEgress(WorkflowInstance{}); len(got.Input) != 0 || got.Result != "" || got.Error != "" {
		t.Errorf("an empty instance changed: %+v", got)
	}
}
