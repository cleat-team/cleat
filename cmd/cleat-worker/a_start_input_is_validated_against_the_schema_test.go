package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// amountRequiredSchema is "run"'s param schema: an object with one required
// integer field, matching EntryPointParamSchema's own shape for a multi-field
// Go entry point (additionalProperties:true, since the binding ignores extra
// keys -- including this request's own merged "__entry_point").
const amountRequiredSchema = `{"type":"object","properties":{"amount":{"type":"integer"}},"required":["amount"],"additionalProperties":true}`

func defWithSchema(schemaJSON string, disabled bool) *engine.WorkflowDef {
	return &engine.WorkflowDef{
		Name:    "d",
		Version: 1,
		EntryPointSchemas: map[string]engine.EntryPointSchema{
			"run": {Params: json.RawMessage(schemaJSON)},
		},
		InputValidationDisabled: disabled,
	}
}

// TestStartWorkflowValidatesInputAgainstSchema is cleat#1981's core
// acceptance criterion: "A mismatch is a 400. The body names the failing
// field and the schema rule, and no run is created."
func TestStartWorkflowValidatesInputAgainstSchema(t *testing.T) {
	var startCalled bool
	st := &mockStore{
		getWorkflowDefFn: func(_ context.Context, name string, version int) (*engine.WorkflowDef, error) {
			return defWithSchema(amountRequiredSchema, false), nil
		},
		startNewRunFn: func(_ context.Context, _ string, _ string, _ int, _ json.RawMessage, _ string, _ string, _ int) (string, bool, error) {
			startCalled = true
			return "run-1", false, nil
		},
	}
	api := &apiServer{store: st, worker: newTestWorker(st), maxBodySize: 1 << 20}

	req := httptest.NewRequest(http.MethodPost, "/api/workflows/d/start",
		strings.NewReader(`{"input":{},"entry_point":"run"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	api.handleStartWorkflow(resp, req, "d")

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", resp.Code, resp.Body.String())
	}
	if startCalled {
		t.Error("StartNewRun was called despite a schema violation -- no run should be created")
	}
	var body map[string]string
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("400 body did not parse as JSON: %v: %s", err, resp.Body.String())
	}
	if body["field"] != "amount" {
		t.Errorf("field = %q, want \"amount\": %s", body["field"], resp.Body.String())
	}
	if body["rule"] != "required" {
		t.Errorf("rule = %q, want \"required\": %s", body["rule"], resp.Body.String())
	}
}

// TestStartWorkflowConformingInputStarts is the positive control for the
// test above: the same schema, a conforming input, must start normally.
func TestStartWorkflowConformingInputStarts(t *testing.T) {
	st := &mockStore{
		getWorkflowDefFn: func(_ context.Context, name string, version int) (*engine.WorkflowDef, error) {
			return defWithSchema(amountRequiredSchema, false), nil
		},
	}
	api := &apiServer{store: st, worker: newTestWorker(st), maxBodySize: 1 << 20}

	req := httptest.NewRequest(http.MethodPost, "/api/workflows/d/start",
		strings.NewReader(`{"input":{"amount":5},"entry_point":"run"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	api.handleStartWorkflow(resp, req, "d")

	if resp.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201: %s", resp.Code, resp.Body.String())
	}
}

// TestStartWorkflowNoSchemaStartsUntyped is the owner decision cleat#1981
// records verbatim: "Definitions without one start untyped, as today." A
// definition with no EntryPointSchemas at all (every mockStore default, and
// every Rust/Java/AssemblyScript build in production) must accept input that
// would fail the schema in the tests above, because there IS no schema to
// fail.
func TestStartWorkflowNoSchemaStartsUntyped(t *testing.T) {
	st := &mockStore{
		getWorkflowDefFn: func(_ context.Context, name string, version int) (*engine.WorkflowDef, error) {
			return &engine.WorkflowDef{Name: "d", Version: 1}, nil // EntryPointSchemas is nil
		},
	}
	api := &apiServer{store: st, worker: newTestWorker(st), maxBodySize: 1 << 20}

	req := httptest.NewRequest(http.MethodPost, "/api/workflows/d/start",
		strings.NewReader(`{"input":{},"entry_point":"run"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	api.handleStartWorkflow(resp, req, "d")

	if resp.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201 (no schema means untyped): %s", resp.Code, resp.Body.String())
	}
}

// TestStartWorkflowNilDefStartsUntyped covers GetWorkflowDef returning
// (nil, nil) -- every existing mockStore-based test in this package that
// does not set getWorkflowDefFn, and the shape a real store returns for a
// definition deleted between ListVersions and this lookup. Must not panic
// on the nil def and must not refuse the start.
func TestStartWorkflowNilDefStartsUntyped(t *testing.T) {
	st := &mockStore{} // getWorkflowDefFn unset: returns (nil, nil)
	api := &apiServer{store: st, worker: newTestWorker(st), maxBodySize: 1 << 20}

	req := httptest.NewRequest(http.MethodPost, "/api/workflows/d/start",
		strings.NewReader(`{"input":{}}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	api.handleStartWorkflow(resp, req, "d")

	if resp.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201: %s", resp.Code, resp.Body.String())
	}
}

// TestStartWorkflowInputValidationDisabledSkipsValidation is cleat#1981's
// escape hatch: a definition deployed with --no-validate-input must accept
// input that would otherwise be rejected, even though it carries a schema.
func TestStartWorkflowInputValidationDisabledSkipsValidation(t *testing.T) {
	st := &mockStore{
		getWorkflowDefFn: func(_ context.Context, name string, version int) (*engine.WorkflowDef, error) {
			return defWithSchema(amountRequiredSchema, true), nil // disabled=true
		},
	}
	api := &apiServer{store: st, worker: newTestWorker(st), maxBodySize: 1 << 20}

	req := httptest.NewRequest(http.MethodPost, "/api/workflows/d/start",
		strings.NewReader(`{"input":{},"entry_point":"run"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	api.handleStartWorkflow(resp, req, "d")

	if resp.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201 (validation disabled for this version): %s", resp.Code, resp.Body.String())
	}
}

// TestStartWorkflowCorruptStoredSchemaFailsClosed is the "fail closed on
// corruption" half of cleat#1981's design, distinct from "no schema starts
// untyped" above: a schema that IS present but does not parse as JSON is the
// server's own broken artifact, not the caller's mistake, so it must refuse
// with a 500 rather than silently let the request through as if untyped.
func TestStartWorkflowCorruptStoredSchemaFailsClosed(t *testing.T) {
	var startCalled bool
	st := &mockStore{
		getWorkflowDefFn: func(_ context.Context, name string, version int) (*engine.WorkflowDef, error) {
			return defWithSchema(`not valid json`, false), nil
		},
		startNewRunFn: func(_ context.Context, _ string, _ string, _ int, _ json.RawMessage, _ string, _ string, _ int) (string, bool, error) {
			startCalled = true
			return "run-1", false, nil
		},
	}
	api := &apiServer{store: st, worker: newTestWorker(st), maxBodySize: 1 << 20}

	req := httptest.NewRequest(http.MethodPost, "/api/workflows/d/start",
		strings.NewReader(`{"input":{"amount":5},"entry_point":"run"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	api.handleStartWorkflow(resp, req, "d")

	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500 (corrupt stored schema is a server fault, not a 400): %s", resp.Code, resp.Body.String())
	}
	if startCalled {
		t.Error("StartNewRun was called despite an unparseable stored schema -- no run should be created")
	}
}

// TestStartWorkflowUnresolvableEntryPointDefersToExecution covers a request
// with NO entry_point field at all against a definition that DOES carry
// schemas: determineEntryPoint cannot resolve one from the input alone (this
// test's WASMBytes is empty, so the WASM-metadata and
// firstHandleExport fallbacks both fail too), so validation must be skipped
// here rather than refuse the start -- exactly the same case execution time
// already handles on its own, unaffected by this feature existing.
func TestStartWorkflowUnresolvableEntryPointDefersToExecution(t *testing.T) {
	st := &mockStore{
		getWorkflowDefFn: func(_ context.Context, name string, version int) (*engine.WorkflowDef, error) {
			return defWithSchema(amountRequiredSchema, false), nil
		},
	}
	api := &apiServer{store: st, worker: newTestWorker(st), maxBodySize: 1 << 20}

	req := httptest.NewRequest(http.MethodPost, "/api/workflows/d/start",
		strings.NewReader(`{"input":{}}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	api.handleStartWorkflow(resp, req, "d")

	if resp.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201 (entry point unresolvable here, deferred to execution): %s", resp.Code, resp.Body.String())
	}
}
