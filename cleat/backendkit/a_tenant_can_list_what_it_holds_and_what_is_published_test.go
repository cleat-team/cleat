package backendkit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// cleat#3627: Client.InstalledLibraries and Client.PublishedLibraries.
//
// The four bodies below are the worker's own, captured from its real route table and pinned there
// too (cmd/cleat-worker TestTheLibraryReadBodiesArePinnedForTheClient), so a change to the shape
// fails on both sides. They are not written from what this client expects: the install client was
// once built against keys its author guessed and decoded a field to "" without a word.
const (
	realInstallsBody       = `{"installs":[{"library":"acme-flows","bundle_version":"1.2.0","installed_at":"2026-10-31T12:00:00Z","installed_by":"key-of-tenant-a"}]}` + "\n"
	realCatalogueBody      = `{"bundles":[{"library":"acme-flows","bundle_version":"1.2.0","published_at":"2026-10-31T12:00:00Z","members":[{"name":"acme-flows.order","version":3,"wasm_sha256":"ab12","wasm_length":7,"exposure":"auth"}]}]}` + "\n"
	realNoInstallsBody     = `{"installs":[]}` + "\n"
	realEmptyCatalogueBody = `{"bundles":[]}` + "\n"
)

func readServer(t *testing.T, status int, body string) (*httptest.Server, *http.Request) {
	t.Helper()
	var seen http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = *r.Clone(r.Context())
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestATenantCanListWhatItHoldsAndWhatIsPublished(t *testing.T) {
	ctx := context.Background()

	t.Run("installs: the request is a GET to the installs route, and every field decodes", func(t *testing.T) {
		srv, seen := readServer(t, 200, realInstallsBody)
		got, err := (&Client{BaseURL: srv.URL, HTTPClient: srv.Client()}).InstalledLibraries(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if seen.Method != http.MethodGet || seen.URL.Path != "/api/library-installs" {
			t.Errorf("sent %s %s, want GET /api/library-installs", seen.Method, seen.URL.Path)
		}
		want := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
		if len(got) != 1 || got[0].Library != "acme-flows" || got[0].BundleVersion != "1.2.0" || !got[0].InstalledAt.Equal(want) || got[0].InstalledBy != "key-of-tenant-a" {
			t.Errorf("decoded %+v: a field is empty or wrong", got)
		}
	})

	t.Run("catalogue: every field decodes, and a filter is sent escaped", func(t *testing.T) {
		srv, seen := readServer(t, 200, realCatalogueBody)
		c := &Client{BaseURL: srv.URL, HTTPClient: srv.Client()}
		got, err := c.PublishedLibraries(ctx, "acme flows&x")
		if err != nil {
			t.Fatal(err)
		}
		if seen.URL.Path != "/api/libraries" || seen.URL.Query().Get("library") != "acme flows&x" {
			t.Errorf("sent %s?%s, want the filter carried as one escaped parameter", seen.URL.Path, seen.URL.RawQuery)
		}
		if len(got) != 1 || got[0].Library != "acme-flows" || got[0].BundleVersion != "1.2.0" || got[0].PublishedAt.IsZero() || len(got[0].Members) != 1 {
			t.Fatalf("decoded %+v", got)
		}
		m := got[0].Members[0]
		if m.Name != "acme-flows.order" || m.Version != 3 || m.WasmSHA256 != "ab12" || m.WasmLength != 7 || m.Exposure != "auth" {
			t.Errorf("member decoded as %+v: a field is empty or wrong", m)
		}
		if _, err := c.PublishedLibraries(ctx, ""); err != nil || seen.URL.RawQuery != "" {
			t.Errorf("no filter must send no query, got %q (%v)", seen.URL.RawQuery, err)
		}
	})

	t.Run("an empty answer is an empty slice, not nil", func(t *testing.T) {
		srv, _ := readServer(t, 200, realNoInstallsBody)
		if got, err := (&Client{BaseURL: srv.URL, HTTPClient: srv.Client()}).InstalledLibraries(ctx); err != nil || got == nil || len(got) != 0 {
			t.Errorf("got %v (nil=%v), %v", got, got == nil, err)
		}
		srv2, _ := readServer(t, 200, realEmptyCatalogueBody)
		if got, err := (&Client{BaseURL: srv2.URL, HTTPClient: srv2.Client()}).PublishedLibraries(ctx, ""); err != nil || got == nil || len(got) != 0 {
			t.Errorf("got %v (nil=%v), %v", got, got == nil, err)
		}
	})

	t.Run("refusals are matchable, and each keeps the upstream status", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			status int
			body   string
			want   error
		}{
			{"501 is unsupported by design", 501, `{"error":"this deployment's store cannot list workflow libraries"}`, ErrLibraryReadUnsupported},
			// A list has no not-found of its own, so a 404 is the worker without the route.
			{"404 is a worker without the route", 404, "{\"error\":\"not found\"}\n", ErrLibraryRouteMissing},
		} {
			t.Run(tc.name, func(t *testing.T) {
				srv, _ := readServer(t, tc.status, tc.body)
				c := &Client{BaseURL: srv.URL, HTTPClient: srv.Client()}
				for _, call := range []func() error{
					func() error { _, err := c.InstalledLibraries(ctx); return err },
					func() error { _, err := c.PublishedLibraries(ctx, ""); return err },
				} {
					err := call()
					var up *UpstreamStatusError
					if !errors.Is(err, tc.want) || !errors.As(err, &up) || up.Status != tc.status {
						t.Errorf("err = %v, want %v with the upstream status %d reachable", err, tc.want, tc.status)
					}
				}
			})
		}
	})
}
