package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#3540 PR 4: backendkit.Client.InstallLibrary has to tell "the bundle is not published" from
// "this worker has no install route", and both are a 404 with a JSON body. apiAware404 answers every
// unmatched /api/ path with {"error":"not found"}, so the status and the shape cannot separate them;
// only a detail code written by the install handler can. The client reads that code, and these tests
// pin both sides of it through the real route table (registerRoutes, which mounts the real
// apiAware404 whenever api.spa is set), so a change to either side fails here and not in a caller.
//
// The backendkit test uses the same two bodies as literals, because cmd/cleat-worker is package main
// in another module and cannot be imported from there. These are the other half of that contract.

func TestAnInstall404SaysWhichNotFoundItIs(t *testing.T) {
	spaReached := false
	newAPI := func(installErr error) *apiServer {
		api := newInstallAPI(&installerMock{mockStore: &mockStore{}, err: installErr})
		api.spa = http.HandlerFunc(func(http.ResponseWriter, *http.Request) { spaReached = true })
		return api
	}

	t.Run("a bundle that is not published carries the detail code", func(t *testing.T) {
		rec := installRoute(t, newAPI(&engine.LibraryBundleNotFoundError{Library: "lib", BundleVersion: "9"}),
			"/api/libraries/lib/install", `{"bundle_version":"9"}`, true)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404: %s", rec.Code, rec.Body.String())
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("the 404 is not a JSON object: %q", rec.Body.String())
		}
		if body["detail"] != "library_bundle_not_found" {
			t.Errorf("detail = %q, want library_bundle_not_found; backendkit reads exactly this to tell a missing bundle from a missing route: %s",
				body["detail"], rec.Body.String())
		}
		if body["error"] == "" {
			t.Errorf("the 404 lost the worker's own sentence: %s", rec.Body.String())
		}
	})

	t.Run("a path no handler claims is a 404 WITHOUT it, from the real apiAware404", func(t *testing.T) {
		rec := installRoute(t, newAPI(nil), "/api/libraries/lib/no-such-action", `{}`, true)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404: %s", rec.Code, rec.Body.String())
		}
		if spaReached {
			t.Fatal("the SPA handler answered an /api/ path, so apiAware404 was not what was measured")
		}
		if got := rec.Body.String(); got != "{\"error\":\"not found\"}\n" {
			t.Errorf("apiAware404's body is %q; the backendkit test pins {\"error\":\"not found\"} and must change with it", got)
		}
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if _, has := body["detail"]; has {
			t.Errorf("an unmatched path carries a detail code %q, so a client could not tell it from a missing bundle", body["detail"])
		}
	})

	t.Run("control: an install that finds its bundle is not a 404 at all", func(t *testing.T) {
		rec := installRoute(t, newAPI(nil), "/api/libraries/lib/install", `{"bundle_version":"1"}`, true)
		if rec.Code != http.StatusOK {
			t.Errorf("status %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})
}
