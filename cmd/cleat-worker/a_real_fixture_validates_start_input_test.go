package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#1981's remaining acceptance criterion, and the one its own issue says
// was left open:
//
//	"A Go and a Python fixture reject a wrong-typed field and a missing field
//	 with 400 and no run row. They accept a request with an extra field."
//
// What shipped in #2927 was covered by a_start_input_is_validated_against_the_schema_test.go,
// whose schema is a HAND-WRITTEN string constant. That is the gap this file
// closes: here the schema is not written by the test at all — it is the one
// `cleat build` emits for a real fixture workflow, read back from the
// `.schema.json` sidecar the build writes beside the `.wasm`. A hand-written
// schema tests the validator; an EMITTED one tests that what the build
// produces is what the start path enforces, which is the thing that can drift.
//
// The store is still mocked, and that is deliberate rather than a shortcut —
// see the note on the "no run row" assertion below.

// buildTypedFixture compiles testdata/bindingconformance the way `cleat build`
// does in production, and returns BOTH halves of what it writes: the module and
// the `.schema.json` sidecar.
//
// buildDeferFixture next door returns only the `.wasm`; the sidecar is the part
// this test exists for, and it is silently absent if the emitter declines to
// produce it — which is exactly why the helper fails rather than returning an
// empty map.
func buildTypedFixture(t *testing.T) ([]byte, map[string]engine.EntryPointSchema) {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	projectRoot := filepath.Dir(filepath.Dir(cwd)) // cmd/cleat-worker -> repo root

	tmpDir := t.TempDir()
	cmd := exec.Command("go", "run", filepath.Join(projectRoot, "cmd", "cleat"),
		"build", "--target", "go", "-o", tmpDir, filepath.Join(projectRoot, "testdata", "bindingconformance"))
	cmd.Dir = projectRoot
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleat build failed:\n%s\n%v", string(out), err)
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("reading build output: %v", err)
	}
	var wasmPath, schemaPath string
	for _, e := range entries {
		switch {
		case filepath.Ext(e.Name()) == ".wasm":
			wasmPath = filepath.Join(tmpDir, e.Name())
		case strings.HasSuffix(e.Name(), ".wasm.schema.json"):
			schemaPath = filepath.Join(tmpDir, e.Name())
		}
	}
	if wasmPath == "" {
		t.Fatalf("no .wasm in %s", tmpDir)
	}
	if schemaPath == "" {
		// Not a skip. The fixture declares typed entry points, so the emitter
		// has something to say about them; producing no sidecar means the
		// build stopped emitting, which is a defect this test is here to catch.
		t.Fatalf("cleat build produced no .wasm.schema.json in %s -- the entry-point schema emitter did not run", tmpDir)
	}

	wasm, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("reading %s: %v", wasmPath, err)
	}
	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("reading %s: %v", schemaPath, err)
	}
	schemas := map[string]engine.EntryPointSchema{}
	if err := json.Unmarshal(raw, &schemas); err != nil {
		t.Fatalf("parsing %s: %v", schemaPath, err)
	}
	return wasm, schemas
}

// TestARealGoFixtureWorkflowValidatesStartInput drives the start handler with
// the schema a real build emitted.
//
// The entry point is `bind_int` from testdata/bindingconformance, whose emitted
// params are {"properties":{"count":{"type":"integer"}},"required":["count"],
// "additionalProperties":true} -- asserted below rather than assumed, because
// every case here is chosen from that shape and the test is meaningless if the
// build produced something else.
func TestARealGoFixtureWorkflowValidatesStartInput(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a WASM module with the real toolchain; skipped in short mode")
	}
	wasm, schemas := buildTypedFixture(t)

	schema, ok := schemas["bind_int"]
	if !ok {
		t.Fatalf("the build emitted no schema for bind_int; keys were %v", entryPointSchemaKeys(schemas))
	}
	var params map[string]any
	if err := json.Unmarshal(schema.Params, &params); err != nil {
		t.Fatalf("bind_int params did not parse: %v: %s", err, schema.Params)
	}
	if req, _ := params["required"].([]any); len(req) != 1 || req[0] != "count" {
		t.Fatalf("bind_int params changed shape: required=%v (full: %s); every case below is chosen from it", params["required"], schema.Params)
	}

	def := &engine.WorkflowDef{
		Name:              "d",
		Version:           1,
		WASMBytes:         wasm,
		EntryPointSchemas: schemas,
	}

	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantField  string
		wantRule   string
		wantStart  bool
	}{
		{
			name:       "a missing required field is a 400 naming the field and the rule",
			body:       `{"input":{},"entry_point":"bind_int"}`,
			wantStatus: http.StatusBadRequest,
			wantField:  "count",
			wantRule:   "required",
		},
		{
			name:       "a wrong-typed field is a 400 naming the field and the rule",
			body:       `{"input":{"count":"not-an-integer"},"entry_point":"bind_int"}`,
			wantStatus: http.StatusBadRequest,
			wantField:  "count",
			wantRule:   "type",
		},
		{
			name:       "an extra field is accepted",
			body:       `{"input":{"count":1,"extra":"whatever"},"entry_point":"bind_int"}`,
			wantStatus: http.StatusCreated,
			wantStart:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var startCalled bool
			st := &mockStore{
				getWorkflowDefFn: func(_ context.Context, _ string, _ int) (*engine.WorkflowDef, error) {
					return def, nil
				},
				startNewRunFn: func(_ context.Context, _ string, _ string, _ int, _ json.RawMessage, _ string, _ string, _ int) (string, bool, error) {
					startCalled = true
					return "run-1", false, nil
				},
			}
			api := &apiServer{store: st, worker: newTestWorker(st), maxBodySize: 1 << 20}

			req := httptest.NewRequest(http.MethodPost, "/api/workflows/d/start", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()
			api.handleStartWorkflow(resp, req, "d")

			if resp.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", resp.Code, tc.wantStatus, resp.Body.String())
			}
			if startCalled != tc.wantStart {
				t.Errorf("StartNewRun called = %v, want %v -- a rejected start must create no run, "+
					"and an accepted one must create it (cleat#1981). response: %s",
					startCalled, tc.wantStart, resp.Body.String())
			}
			if tc.wantStatus == http.StatusBadRequest {
				var body map[string]string
				if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
					t.Fatalf("400 body did not parse as JSON: %v: %s", err, resp.Body.String())
				}
				if body["field"] != tc.wantField {
					t.Errorf("field = %q, want %q: %s", body["field"], tc.wantField, resp.Body.String())
				}
				if body["rule"] != tc.wantRule {
					t.Errorf("rule = %q, want %q: %s", body["rule"], tc.wantRule, resp.Body.String())
				}
			}
		})
	}
}

func entryPointSchemaKeys(m map[string]engine.EntryPointSchema) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
