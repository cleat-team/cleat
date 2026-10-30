package backendkit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cleat#3540 PR 4: Client.InstallLibrary wraps POST /api/libraries/{library}/install.
//
// What a caller needs from a refusal is WHICH refusal, and the statuses are not enough:
// 404 is two different things, 400 is two, and a 422 carries the list of plugins the caller
// has to deploy. So these tests are about the error a caller can match, not about the
// transport.

// realUnmetPluginsBody is what the worker's install route sends for a bundle that declares a
// plugin no worker has registered. It was produced by marshalling the worker's own types
// (engine.UnmetPluginDepsError and engine.MissingPlugin) rather than written by hand, which
// matters: MissingPlugin has no JSON tags, so the keys are Name, Constraint and DeclaredBy,
// and a client written against the keys one would guess (name, declared_by) decodes
// "DeclaredBy" as an empty string.
const realUnmetPluginsBody = `{"error":"plugin dependencies are not met; no worker has ever registered a plugin named: \"slack-notify\" (>=1.2), declared by acme-wf@3","missing_plugins":[{"Name":"slack-notify","Constraint":">=1.2","DeclaredBy":"acme-wf@3"}]}`

// The two 404 bodies a worker can send for an install, as literals. cmd/cleat-worker is package main
// in another module and cannot be imported here, so the worker's own test
// (an_install_404_says_which_not_found_it_is_test.go) pins the same two bodies through its real
// route table, and a change to either side fails there. This is the half that reads them.
//
// realUnmatchedAPIPathBody is what the worker's apiAware404 writes for an /api/ path no handler
// claims, which is what a worker WITHOUT the install route answers. It is a JSON 404 with an
// "error", the same status and shape as a missing bundle; the first version of this client read
// that as "publish the bundle" and the assumption was only caught in review.
const (
	realBundleNotFoundBody   = `{"detail":"library_bundle_not_found","error":"library \"acme\" bundle \"9\" is not published"}`
	realUnmatchedAPIPathBody = "{\"error\":\"not found\"}\n"
)

func installServer(t *testing.T, status int, body string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestATenantCanInstallALibrary(t *testing.T) {
	ctx := context.Background()

	t.Run("the request reaches the install route with the key and the bundle version", func(t *testing.T) {
		var method, path, auth, ctype, body string
		var seen bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = true
			method, path, auth, ctype = r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization"), r.Header.Get("Content-Type")
			b, _ := io.ReadAll(r.Body)
			body = string(b)
			_, _ = w.Write([]byte(`{"library":"acme","bundle_version":"1.0.0","members":[{"name":"acme-wf","version":3,"exposure":"auth"}]}`))
		}))
		defer srv.Close()

		res, err := New(srv.URL, WithAPIKey("tenant-key")).InstallLibrary(ctx, "acme", "1.0.0")
		if err != nil {
			t.Fatalf("install: %v", err)
		}
		if !seen {
			t.Fatal("the server handler never ran, so this test asserted nothing")
		}
		if method != http.MethodPost || path != "/api/libraries/acme/install" {
			t.Errorf("the request was %s %s, want POST /api/libraries/acme/install", method, path)
		}
		if auth != "Bearer tenant-key" {
			t.Errorf("Authorization = %q, want the tenant's key as a bearer token", auth)
		}
		if ctype != "application/json" || body != `{"bundle_version":"1.0.0"}` {
			t.Errorf("content type %q body %q, want JSON {\"bundle_version\":\"1.0.0\"}", ctype, body)
		}
		if res.Library != "acme" || res.BundleVersion != "1.0.0" || res.PreviousBundleVersion != "" ||
			len(res.Members) != 1 || res.Members[0] != (InstalledLibraryMember{Name: "acme-wf", Version: 3, Exposure: "auth"}) {
			t.Errorf("result = %+v, want the one member decoded and no previous version", res)
		}
	})

	t.Run("an upgrade reports what the tenant held before", func(t *testing.T) {
		srv, _ := installServer(t, 200, `{"library":"acme","bundle_version":"2.0.0","previous_bundle_version":"1.0.0","members":[]}`)
		res, err := New(srv.URL).InstallLibrary(ctx, "acme", "2.0.0")
		if err != nil || res.PreviousBundleVersion != "1.0.0" {
			t.Errorf("got %+v, %v; want previous_bundle_version 1.0.0", res, err)
		}
	})

	t.Run("a library name is escaped into the path and cannot reach another route", func(t *testing.T) {
		var path string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path = r.URL.EscapedPath()
			_, _ = w.Write([]byte(`{"library":"x","bundle_version":"1","members":[]}`))
		}))
		defer srv.Close()
		if _, err := New(srv.URL).InstallLibrary(ctx, "a/../b", "1"); err != nil {
			t.Fatal(err)
		}
		if path != "/api/libraries/a%2F..%2Fb/install" {
			t.Errorf("path = %q, want the slashes escaped", path)
		}
	})

	t.Run("an empty name or version is refused without a request", func(t *testing.T) {
		srv, calls := installServer(t, 200, `{}`)
		c := New(srv.URL)
		for _, args := range [][2]string{{"", "1"}, {"acme", ""}, {"", ""}} {
			if _, err := c.InstallLibrary(ctx, args[0], args[1]); err == nil {
				t.Errorf("InstallLibrary(%q, %q) succeeded", args[0], args[1])
			}
		}
		if *calls != 0 {
			t.Errorf("the worker was called %d time(s); an install with nothing to name must not be sent", *calls)
		}
	})
}

// Each status is its own error, and NO OTHER. A table that only checked "matches its own"
// passes a client that returns every sentinel joined together for every refusal.
func TestAnInstallRefusalIsMatchableAndOnlyAsItself(t *testing.T) {
	sentinels := map[string]error{
		"bundle not found":     ErrLibraryBundleNotFound,
		"definition collision": ErrLibraryDefinitionCollision,
		"plugins unmet":        ErrLibraryPluginsUnmet,
		"bad request":          ErrLibraryInstallBadRequest,
		"forbidden":            ErrLibraryInstallForbidden,
		"unsupported":          ErrLibraryInstallUnsupported,
		"route missing":        ErrLibraryRouteMissing,
	}
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string // a key of sentinels, or "" for no library sentinel at all
	}{
		{"404 with the install handler's detail code is a missing bundle", 404, realBundleNotFoundBody, "bundle not found"},
		{"404 from the worker's own apiAware404 is a worker without the route", 404, realUnmatchedAPIPathBody, "route missing"},
		{"a JSON 404 with an error and no detail code is NOT a missing bundle", 404, `{"error":"library \"acme\" bundle \"9\" is not published"}`, "route missing"},
		{"a plain-text 404 is a worker without the route", 404, "404 page not found\n", "route missing"},
		{"409 is a definition collision", 409, `{"error":"the tenant already has a definition"}`, "definition collision"},
		{"422 is unmet plugins", 422, realUnmetPluginsBody, "plugins unmet"},
		{"400 is a bad request", 400, `{"error":"bundle version \"\" is not valid"}`, "bad request"},
		{"403 is forbidden", 403, `{"error":"not entitled"}`, "forbidden"},
		{"501 is unsupported by design", 501, `{"error":"this deployment's store cannot install workflow libraries"}`, "unsupported"},
		{"401 is not a library refusal", 401, `{"error":"invalid or revoked API key"}`, ""},
		{"500 is not a library refusal", 500, `{"error":"boom"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, calls := installServer(t, tc.status, tc.body)
			res, err := New(srv.URL).InstallLibrary(context.Background(), "acme", "1.0.0")
			if *calls != 1 {
				t.Fatalf("the worker was called %d time(s), so this test asserted nothing", *calls)
			}
			if err == nil || res != nil {
				t.Fatalf("got %+v, %v; want an error and no result", res, err)
			}
			for name, s := range sentinels {
				if got, want := errors.Is(err, s), name == tc.want; got != want {
					t.Errorf("errors.Is(err, %s) = %v, want %v: %v", name, got, want, err)
				}
			}
			// Whatever the refusal, the status and body stay reachable.
			var up *UpstreamStatusError
			if !errors.As(err, &up) || up.Status != tc.status {
				t.Errorf("errors.As *UpstreamStatusError = %v (%+v), want status %d", errors.As(err, &up), up, tc.status)
			}
			if up != nil && strings.TrimSpace(string(up.Body)) != strings.TrimSpace(tc.body) {
				t.Errorf("the upstream body %q lost the worker's own words", up.Body)
			}
		})
	}
}

func TestUnmetPluginsAreReachableThroughErrorsAs(t *testing.T) {
	t.Run("the list the worker sent, with every field", func(t *testing.T) {
		srv, _ := installServer(t, 422, realUnmetPluginsBody)
		_, err := New(srv.URL).InstallLibrary(context.Background(), "acme", "1.0.0")
		var unmet *UnmetLibraryPluginsError
		if !errors.As(err, &unmet) {
			t.Fatalf("errors.As *UnmetLibraryPluginsError failed on %v", err)
		}
		if unmet.Truncated {
			t.Error("a complete reply was reported as truncated")
		}
		want := MissingPlugin{Name: "slack-notify", Constraint: ">=1.2", DeclaredBy: "acme-wf@3"}
		if len(unmet.Missing) != 1 || unmet.Missing[0] != want {
			t.Errorf("Missing = %+v, want [%+v] -- DeclaredBy is the field a lower-case tag drops", unmet.Missing, want)
		}
		if !strings.Contains(err.Error(), "slack-notify") {
			t.Errorf("the message %q does not name the plugin", err)
		}
	})

	t.Run("a list longer than the read limit says it was cut, not that it was empty", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(`{"error":"plugin dependencies are not met","missing_plugins":[`)
		for i := 0; i < 200; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"Name":"plugin-%03d","Constraint":"","DeclaredBy":"acme-wf@3"}`, i)
		}
		b.WriteString(`]}`)
		// The control: this body really does exceed what doRequest keeps. Without it the
		// subtest could pass on a reply that was never cut.
		if b.Len() <= doRequestErrorBodyLimit {
			t.Fatalf("UNMEASURED: the fixture is %d bytes, not over the %d-byte limit", b.Len(), doRequestErrorBodyLimit)
		}
		srv, _ := installServer(t, 422, b.String())
		_, err := New(srv.URL).InstallLibrary(context.Background(), "acme", "1.0.0")
		var unmet *UnmetLibraryPluginsError
		if !errors.As(err, &unmet) || !errors.Is(err, ErrLibraryPluginsUnmet) {
			t.Fatalf("a cut 422 is no longer an unmet-plugins error: %v", err)
		}
		if !unmet.Truncated {
			t.Error("the list was cut off and Truncated is false: the caller would read an empty list as 'nothing missing'")
		}
		if !strings.Contains(err.Error(), "cut off") {
			t.Errorf("the message does not say the list was cut off: %v", err)
		}
	})

	t.Run("a short body that is not the worker's JSON is not called truncated", func(t *testing.T) {
		srv, _ := installServer(t, 422, "Unprocessable Entity")
		_, err := New(srv.URL).InstallLibrary(context.Background(), "acme", "1.0.0")
		var unmet *UnmetLibraryPluginsError
		if !errors.As(err, &unmet) {
			t.Fatalf("got %v", err)
		}
		if unmet.Truncated || len(unmet.Missing) != 0 {
			t.Errorf("Truncated=%v Missing=%v for a 20-byte body; 'cut off' would be a false claim", unmet.Truncated, unmet.Missing)
		}
	})
}
