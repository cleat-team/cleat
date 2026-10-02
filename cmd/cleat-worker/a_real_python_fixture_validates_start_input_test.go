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

// cleat#1981's acceptance criterion, Python half:
//
//	"A Go and a Python fixture reject a wrong-typed field and a missing field
//	 with 400 and no run row. They accept a request with an extra field."
//
// The Go half is a_real_fixture_validates_start_input_test.go, next door. This
// is the same three cases for a real Python build, and it exists because the
// Python path is not the Go path: it compiles through componentize-py to a
// Component Model binary and its schema comes from cleat_sdk's own emitter
// (cleat#2914), so "the schema the build emits" is a different producer that
// can drift from the Go one independently.
//
// WHERE THIS RUNS. Python is tier 1 (tiers.yaml), and the tier-1 gate installs
// componentize-py and the SDK on every DB-backed shard precisely so tests like
// this one run there. It is NOT gated on a database -- the store is a mock --
// so it runs on the shard that carries cmd/cleat-worker, not on a DB dialect.
//
// WHAT IT FOUND. The first version used the bare `@cleat_entry` form, and the
// emitter died in json.dumps with "keys must be str, ... not function" --
// cleat_sdk/entry.py's dual-form branch passes the decorated function into
// `_make_entry`'s `name` slot, so the registry is keyed by a function object.
// build_python.go treats a schema failure as non-fatal, so the build reported
// success and wrote no schema, and the definition then started UNTYPED with
// validation silently off. The fixture uses the preferred parenthesised form;
// the bare form is reported separately rather than worked around here.

// requireToolchainEnv mirrors engine/rust_workflow_test.go's constant of the
// same name. It is a copy rather than an import because that one lives in
// package engine's test files, which package main cannot reach; the value is
// the contract, and both must agree.
const requireToolchainEnv = "CLEAT_REQUIRE_TOOLCHAINS"

// toolchainRequired reports whether the current job declared that it provides
// the named toolchain. When it did, a missing toolchain is a FAILURE, not a
// skip: a job that promised python and silently skipped would report green
// while measuring nothing, which is the failure mode tiers.yaml's "a skip is a
// failure" rule exists to prevent.
func toolchainRequired(name string) bool {
	for _, want := range strings.Split(os.Getenv(requireToolchainEnv), ",") {
		if strings.TrimSpace(want) == name {
			return true
		}
	}
	return false
}

// pythonWASMUnavailable returns a reason if this machine cannot build a Python
// workflow, or "" if it can.
func pythonWASMUnavailable() string {
	if _, err := exec.LookPath("componentize-py"); err != nil {
		return "componentize-py is not on PATH (pip install componentize-py)"
	}
	if _, err := exec.LookPath("python3"); err != nil {
		return "python3 is not on PATH"
	}
	return ""
}

// buildTypedPythonFixture compiles testdata/pytyped with `cleat build --target
// python` and returns the module plus the schema sidecar the build emitted.
func buildTypedPythonFixture(t *testing.T) ([]byte, map[string]engine.EntryPointSchema) {
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
		"build", "--target", "python", "-o", tmpDir,
		filepath.Join(projectRoot, "testdata", "pytyped", "count_workflow.py"))
	cmd.Dir = projectRoot
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleat build --target python failed:\n%s\n%v", string(out), err)
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
		// NOT a skip. The fixture's entry point is decorated in the preferred
		// form, so the emitter has something to say; no sidecar means the
		// emitter refused or crashed, which is the defect this test exists to
		// surface. Failing here is what turns a silent no-schema build into a
		// red test.
		t.Fatalf("cleat build --target python produced no .wasm.schema.json in %s -- "+
			"the Python entry-point schema emitter did not run. The definition would "+
			"start untyped, with start-input validation silently off (cleat#1981).", tmpDir)
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

// TestARealPythonFixtureWorkflowValidatesStartInput is the Python half.
func TestARealPythonFixtureWorkflowValidatesStartInput(t *testing.T) {
	if reason := pythonWASMUnavailable(); reason != "" {
		if toolchainRequired("python") {
			t.Fatalf("cannot build a Python workflow, but %s declares python, so this job "+
				"installs componentize-py and treats Python as tier 1: %s",
				requireToolchainEnv, reason)
		}
		t.Skip("cannot build a Python workflow: " + reason)
	}

	wasm, schemas := buildTypedPythonFixture(t)

	schema, ok := schemas["count_workflow"]
	if !ok {
		t.Fatalf("the build emitted no schema for count_workflow; keys were %v", entryPointSchemaKeys(schemas))
	}
	var params map[string]any
	if err := json.Unmarshal(schema.Params, &params); err != nil {
		t.Fatalf("count_workflow params did not parse: %v: %s", err, schema.Params)
	}
	if req, _ := params["required"].([]any); len(req) != 1 || req[0] != "count" {
		t.Fatalf("count_workflow params changed shape: required=%v (full: %s); "+
			"every case below is chosen from it", params["required"], schema.Params)
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
			body:       `{"input":{},"entry_point":"count_workflow"}`,
			wantStatus: http.StatusBadRequest,
			wantField:  "count",
			wantRule:   "required",
		},
		{
			name:       "a wrong-typed field is a 400 naming the field and the rule",
			body:       `{"input":{"count":"not-an-integer"},"entry_point":"count_workflow"}`,
			wantStatus: http.StatusBadRequest,
			wantField:  "count",
			wantRule:   "type",
		},
		{
			name:       "an extra field is accepted",
			body:       `{"input":{"count":1,"extra":"whatever"},"entry_point":"count_workflow"}`,
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
