package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#3497 on the upload route. The store refuses to replace a version's WASM
// while runs of that version are open (engine/workflow_def_replacement.go). Over
// HTTP that is a conflict with the definition's state, not a server fault: a 500
// would read as "retry" and would be counted against the worker's error budget.
//
// Not database-backed, like its neighbour an_upload_honours_the_artifacts_declared_
// exposure_test.go: the store's own rule is tested on every dialect in engine/, and
// this proves only what the handler does with the error it is handed.
//
// The generic-error case is the control. A handler that turned EVERY deploy error
// into a 409 would pass the first case and be wrong.
func TestAnUploadRefusedForOpenRunsIsAConflictNotAServerError(t *testing.T) {
	upload := func(t *testing.T, deployErr error) (int, string) {
		t.Helper()
		ms := &mockStore{
			deployWorkflowDefFn: func(context.Context, *engine.WorkflowDef) error { return deployErr },
		}
		api := &apiServer{store: ms, worker: newTestWorker(ms), maxBodySize: 1 << 20}
		payload, _ := json.Marshal(map[string]any{
			"name":              "uploaded",
			"version":           1,
			"wasm_bytes_base64": base64.StdEncoding.EncodeToString([]byte("\x00asm\x01\x00\x00\x00")),
		})
		req := httptest.NewRequest(http.MethodPost, "/api/definitions", strings.NewReader(string(payload)))
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		api.handleCreateDefinition(resp, req)
		var decoded map[string]string
		_ = json.Unmarshal(resp.Body.Bytes(), &decoded)
		return resp.Code, decoded["error"]
	}

	t.Run("a refusal for open runs is a 409 that says how to proceed", func(t *testing.T) {
		code, msg := upload(t, &engine.WorkflowDefInUseError{Name: "uploaded", Version: 1, OpenRuns: 3})
		if code != http.StatusConflict {
			t.Fatalf("HTTP %d (%q), want 409", code, msg)
		}
		for _, want := range []string{"uploaded", "version 1", "3 run(s)", "--version", `with that same "version"`} {
			if !strings.Contains(msg, want) {
				t.Errorf("the 409 body %q does not mention %q", msg, want)
			}
		}
		// Measured on cleat#3513: leaving "version" out gives a row whose number need
		// not match the binary's stamp, and the worker never runs such a version.
		if strings.Contains(msg, `leave "version" out`) {
			t.Errorf("the 409 body %q advises leaving \"version\" out, which can create a version the worker never runs (cleat#3513)", msg)
		}
	})

	// cleat#3540: a definition installed from a library cannot be given bytes. The
	// same kind of conflict, and its message already says what to do.
	t.Run("a refusal because the definition is a library member is a 409 that says to install a newer library version", func(t *testing.T) {
		code, msg := upload(t, &engine.WorkflowDefIsLibraryMemberError{Name: "uploaded", Version: 1, Library: "acme-lib"})
		if code != http.StatusConflict {
			t.Fatalf("HTTP %d (%q), want 409", code, msg)
		}
		for _, want := range []string{"uploaded", "version 1", `"acme-lib"`, "immutable", "install a newer version of the library"} {
			if !strings.Contains(msg, want) {
				t.Errorf("the 409 body %q does not mention %q", msg, want)
			}
		}
		// It is not the open-runs refusal, and must not borrow that one's advice
		// about building a higher --version: that would not help here.
		if strings.Contains(msg, "--version") {
			t.Errorf("the 409 body %q gives the open-runs advice, which does not apply to a library member", msg)
		}
	})

	t.Run("control: any other deploy error is still a 500", func(t *testing.T) {
		code, msg := upload(t, errors.New("disk on fire"))
		if code != http.StatusInternalServerError {
			t.Fatalf("HTTP %d (%q), want 500: only the open-runs refusal is a conflict", code, msg)
		}
	})
}
