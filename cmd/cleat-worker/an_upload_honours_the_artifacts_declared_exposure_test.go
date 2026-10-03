package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#1986 slice 2c-ii on the UPLOAD route.
//
// This route is the one deploy path with a different reason for existing in
// this slice: `createDefRequest` carries no exposure class and never has, so
// there is nothing a caller can tighten with and the artifact's own declaration
// is the only class it can honour. Before this it stored `auth` for every
// upload -- including one whose source declares `internal`, which is the
// fail-open half of the gap: the column said reachable while the source said
// otherwise.
//
// Deliberately NOT database-backed. The neighbouring HTTP deploy test is
// (deploy_ownership_http_test.go) and it skips without a DSN, which is right for
// what it proves -- cross-tenant visibility the store enforces -- but would mean
// this one reports nothing in most runs, and a check that reports nothing
// agrees with every tree.
func TestTheUploadRouteHonoursTheArtifactsDeclaredExposure(t *testing.T) {
	upload := func(t *testing.T, declared string) (int, string, *engine.WorkflowDef) {
		t.Helper()

		body, err := wasm.WriteMetadata([]byte("\x00asm\x01\x00\x00\x00"), &wasm.Metadata{Exposure: declared})
		if err != nil {
			t.Fatalf("WriteMetadata(exposure=%q): %v", declared, err)
		}
		// The control: if the stamp is not readable back, then every case below
		// is exercising the no-declaration path while claiming otherwise.
		back, err := wasm.ReadMetadata(body)
		if err != nil || back.Exposure != declared {
			t.Fatalf("the artifact does not carry %q (read back %q, err %v)", declared, back.Exposure, err)
		}

		var captured *engine.WorkflowDef
		ms := &mockStore{
			deployWorkflowDefFn: func(_ context.Context, def *engine.WorkflowDef) error {
				captured = def
				return nil
			},
		}
		api := &apiServer{store: ms, worker: newTestWorker(ms), maxBodySize: 1 << 20}

		payload, _ := json.Marshal(map[string]string{
			"name":              "uploaded",
			"wasm_bytes_base64": base64.StdEncoding.EncodeToString(body),
		})
		req := httptest.NewRequest(http.MethodPost, "/api/definitions", strings.NewReader(string(payload)))
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()

		api.handleCreateDefinition(resp, req)

		var decoded map[string]string
		_ = json.Unmarshal(resp.Body.Bytes(), &decoded)
		return resp.Code, decoded["error"], captured
	}

	t.Run("a declared internal is stored as internal", func(t *testing.T) {
		code, msg, def := upload(t, string(engine.ExposureInternal))
		if def == nil {
			t.Fatalf("nothing was deployed (HTTP %d: %s)", code, msg)
		}
		if def.Exposure != engine.ExposureInternal {
			t.Errorf("stored Exposure = %q, want %q: the artifact declares internal and this route has "+
				"no class of its own to ask for", def.Exposure, engine.ExposureInternal)
		}
	})

	t.Run("an artifact declaring nothing still defaults to auth", func(t *testing.T) {
		_, msg, def := upload(t, "")
		if def == nil {
			t.Fatalf("an artifact with no declaration was refused: %s", msg)
		}
		if def.Exposure != engine.ExposureAuth {
			t.Errorf("stored Exposure = %q, want %q -- the pre-existing default must not have moved",
				def.Exposure, engine.ExposureAuth)
		}
	})

	t.Run("a declared public is refused, not stored", func(t *testing.T) {
		code, msg, def := upload(t, string(engine.ExposurePublic))
		if def != nil {
			t.Errorf("an artifact declaring `public` was stored as %q, and no per-tenant opt-in exists "+
				"yet -- that definition becomes world-readable the moment enforcement ships", def.Exposure)
		}
		if code != http.StatusBadRequest {
			t.Errorf("HTTP %d, want 400: this is the caller's artifact, not a server fault", code)
		}
		if !strings.Contains(msg, "opt-in") {
			t.Errorf("the refusal does not name the missing per-tenant opt-in: %q", msg)
		}
	})

	t.Run("a malformed declared class is refused", func(t *testing.T) {
		// Untrusted input: anyone who can upload chooses these bytes, so a stamp
		// that does not parse must not be read as "no declaration" -- that would
		// store as `auth` the workflow whose source asked for protection.
		for _, bad := range []string{"Internal", "internal ", "secrets"} {
			code, msg, def := upload(t, bad)
			if def != nil {
				t.Errorf("a stamp of %q was accepted and stored as %q", bad, def.Exposure)
			}
			if code != http.StatusBadRequest || !strings.Contains(msg, "not one of") {
				t.Errorf("a stamp of %q gave %d %q; want 400 naming the accepted classes", bad, code, msg)
			}
		}
	})
}
