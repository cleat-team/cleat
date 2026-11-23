package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/wasm"
)

// A built Python workflow component is about 19.5 MB, and POST /api/definitions carries it as base64 inside JSON, about a third larger. The
// endpoint's ceiling was a fixed 10 MiB, so a Python workflow could not be deployed through the API at all (cleat#4400). cleat deploy and
// cleatctl deploy write the store directly, so the API upload is the one path that hit it.
//
// The component here is a stamped module header plus one zero-filled custom section, sized to the real one. The handler reads the stamp and
// stores the bytes and never instantiates them, so the size is what is being tested; the real component was measured separately on the issue.
func TestAPythonSizedComponentUploadsThroughTheAPI(t *testing.T) {
	const componentBytes = 19_500_000

	component := func(t *testing.T) []byte {
		t.Helper()
		stamped, err := wasm.WriteMetadata([]byte("\x00asm\x01\x00\x00\x00"),
			&wasm.Metadata{WorkflowName: "py", WorkflowVersion: 1, ABIVersion: 1, MinCompatibleVersion: 1})
		if err != nil {
			t.Fatalf("WriteMetadata: %v", err)
		}
		name := []byte("pad")
		sec := append([]byte{byte(len(name))}, name...)
		sec = append(sec, make([]byte, componentBytes-len(stamped))...)
		var leb []byte
		for n := len(sec); ; {
			b := byte(n & 0x7f)
			n >>= 7
			if n == 0 {
				leb = append(leb, b)
				break
			}
			leb = append(leb, b|0x80)
		}
		out := append(append([]byte(nil), stamped...), 0x00)
		out = append(out, leb...)
		return append(out, sec...)
	}

	upload := func(t *testing.T, api *apiServer, comp []byte) (int, string) {
		t.Helper()
		payload, _ := json.Marshal(map[string]string{"name": "py", "wasm_bytes_base64": base64.StdEncoding.EncodeToString(comp)})
		mux := http.NewServeMux()
		registerRoutes(mux, api)
		req := httptest.NewRequest(http.MethodPost, "/api/definitions", strings.NewReader(string(payload)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}

	comp := component(t)
	// The control: the body must really be over the old ceiling, or this test would pass on the old limit too.
	if wire := int64(base64.StdEncoding.EncodedLen(len(comp))); wire <= 10*1024*1024 {
		t.Fatalf("PRECONDITION FAILED: the %d-byte body is not over the old 10 MiB ceiling, so this test cannot tell the old limit from the new", wire)
	}

	newServer := func(stored *int) *apiServer {
		ms := &mockStore{
			listWorkflowDefsFn: func(context.Context, string) ([]engine.WorkflowDef, error) { return nil, nil },
			deployWorkflowDefFn: func(_ context.Context, d *engine.WorkflowDef) error {
				*stored = len(d.WASMBytes)
				return nil
			},
		}
		return newTestAPIServer(ms)
	}

	t.Run("the default accepts it, and stores every byte", func(t *testing.T) {
		var stored int
		code, body := upload(t, newServer(&stored), comp)
		if code != http.StatusCreated {
			t.Fatalf("HTTP %d, want 201: %.300s", code, body)
		}
		if stored != len(comp) {
			t.Errorf("stored %d bytes, want %d", stored, len(comp))
		}
	})

	t.Run("it is not bounded by --max-body-size", func(t *testing.T) {
		var stored int
		api := newServer(&stored)
		api.maxBodySize = 4096
		if code, body := upload(t, api, comp); code != http.StatusCreated {
			t.Fatalf("HTTP %d with --max-body-size 4096, want 201: %.300s. The upload has its own ceiling and a 4 KiB general one must not reach it.", code, body)
		}
	})

	t.Run("--max-definition-body-size moves it, and the 413 names that flag", func(t *testing.T) {
		var stored int
		api := newServer(&stored)
		api.maxDefinitionBodySize = 1 << 20
		code, body := upload(t, api, comp)
		if code != http.StatusRequestEntityTooLarge {
			t.Fatalf("HTTP %d with a 1 MiB definition ceiling, want 413: %.300s", code, body)
		}
		for _, want := range []string{fmt.Sprint(1 << 20), "--max-definition-body-size"} {
			if !strings.Contains(body, want) {
				t.Errorf("the 413 body %q does not name %q", body, want)
			}
		}
		if stored != 0 {
			t.Errorf("a refused upload stored %d bytes", stored)
		}
	})
}
