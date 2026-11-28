package main

// cleat#3627: GET /api/library-installs and GET /api/libraries.
//
// Driven through registerRoutes, the one route table, like the install route's tests, so a
// registration that is missing or mistyped fails here. Before this change both paths were a 404
// for a tenant key (and the install path a 405 with Allow: POST), which is the first thing pinned.
// The store behind the routes is a mock that records what the routes asked of it, because what is
// under test is the routes' own decisions: whose tenant they ask for, what they leave out of the
// catalogue, what the entitlement hook removes, and which refusal comes from where. The tenant
// boundary itself is the engine's test (a_tenant_lists_only_its_own_library_installs_test.go),
// against two real tenants on PostgreSQL and SQL Server.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
)

// readerMock is a store that can list installs and the catalogue and records every call.
type readerMock struct {
	*mockStore
	installs    []engine.InstalledLibrary
	installsErr error
	catalogue   []engine.LibraryBundleSummary
	catalogErr  error
	askedLib    []string
	installCall int
}

func (m *readerMock) ListInstalledLibraries(context.Context) ([]engine.InstalledLibrary, error) {
	m.installCall++
	return m.installs, m.installsErr
}

func (m *readerMock) ListPublishedLibraries(_ context.Context, library string) ([]engine.LibraryBundleSummary, error) {
	m.askedLib = append(m.askedLib, library)
	return m.catalogue, m.catalogErr
}

func newReaderAPI(m *readerMock) *apiServer {
	api := newTestAPIServer(m.mockStore)
	api.factory = &fakeStoreFactory{fallback: m}
	return api
}

// readRoute serves one GET through the real route table, as the authenticated tenant unless authed is false.
func readRoute(t *testing.T, api *apiServer, method, path string, authed bool) *httptest.ResponseRecorder {
	t.Helper()
	mux := registerRoutes(http.NewServeMux(), api)
	req := httptest.NewRequest(method, path, nil)
	if authed {
		req = req.WithContext(auth.WithSubject(auth.WithTenantID(req.Context(), installTenant), "key-of-tenant-a"))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestTheLibraryReadRoutesExistForATenantKey(t *testing.T) {
	for _, path := range []string{"/api/library-installs", "/api/libraries"} {
		t.Run(path, func(t *testing.T) {
			m := &readerMock{mockStore: &mockStore{}}
			rec := readRoute(t, newReaderAPI(m), http.MethodGet, path, true)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s answered %d: %s -- a tenant key could not ask this before cleat#3627 (404)", path, rec.Code, rec.Body.String())
			}
			// The body is an object whose list is [], not null: a client ranges over it without a nil check.
			if !strings.Contains(rec.Body.String(), "[]") || strings.Contains(rec.Body.String(), "null") {
				t.Errorf("an empty answer must carry an empty list, got %s", rec.Body.String())
			}
			// Read-only: nothing but GET is served.
			if rec := readRoute(t, newReaderAPI(m), http.MethodPost, path, true); rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("POST %s answered %d, want 405", path, rec.Code)
			}
		})
	}
}

func TestTheInstalledLibrariesAreWhatTheCallersStoreSays(t *testing.T) {
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	m := &readerMock{mockStore: &mockStore{}, installs: []engine.InstalledLibrary{
		{Library: "acme-flows", BundleVersion: "1.2.0", InstalledAt: at, InstalledBy: "key-of-tenant-a"},
	}}
	rec := readRoute(t, newReaderAPI(m), http.MethodGet, "/api/library-installs", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Installs []engine.InstalledLibrary `json:"installs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Installs) != 1 {
		t.Fatalf("response %s (%v), want one install", rec.Body.String(), err)
	}
	if got := body.Installs[0]; got.Library != "acme-flows" || got.BundleVersion != "1.2.0" || !got.InstalledAt.Equal(at) || got.InstalledBy != "key-of-tenant-a" {
		t.Errorf("install = %+v", got)
	}
	if m.installCall != 1 {
		t.Errorf("the store was asked %d times, want once", m.installCall)
	}
}

func TestTheCatalogueLeavesOutWhoPublishedAndHonoursTheEntitlementHook(t *testing.T) {
	bundle := func(library, bv string) engine.LibraryBundleSummary {
		return engine.LibraryBundleSummary{
			Library: library, BundleVersion: bv, PublishedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
			PublishedBy: "operator@example.com",
			Members:     []engine.LibraryMemberSummary{{Name: library + ".order", Version: 1, WasmSHA256: "ab", WasmLength: 7, Exposure: engine.ExposureAuth}},
		}
	}
	m := &readerMock{mockStore: &mockStore{}, catalogue: []engine.LibraryBundleSummary{
		bundle("acme-flows", "2.0.0"), bundle("acme-flows", "1.0.0"), bundle("secret-lib", "1.0.0"),
	}}
	api := newReaderAPI(m)
	var asked []string
	api.libraryEntitlement = func(_ context.Context, tenantID, library string) error {
		asked = append(asked, tenantID+"/"+library)
		if library == "secret-lib" {
			return errors.New("not entitled")
		}
		return nil
	}
	rec := readRoute(t, api, http.MethodGet, "/api/libraries?library=", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "operator@example.com") || strings.Contains(rec.Body.String(), "published_by") {
		t.Errorf("the tenant-facing catalogue names who published: %s", rec.Body.String())
	}
	var body struct {
		Bundles []engine.LibraryBundleSummary `json:"bundles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body.String())
	}
	var got []string
	for _, b := range body.Bundles {
		got = append(got, b.Library+"@"+b.BundleVersion)
	}
	if strings.Join(got, ",") != "acme-flows@2.0.0,acme-flows@1.0.0" {
		t.Errorf("listed %v, want both acme-flows bundles in the store's order and not secret-lib, which the hook refused", got)
	}
	// Once per LIBRARY, with the caller's tenant, so the list never offers what the install would 403.
	if strings.Join(asked, ",") != installTenant.String()+"/acme-flows,"+installTenant.String()+"/secret-lib" {
		t.Errorf("the hook was asked %v, want once per library for the caller's tenant", asked)
	}
	// A library filter reaches the store.
	m.askedLib = nil
	_ = readRoute(t, api, http.MethodGet, "/api/libraries?library=acme-flows", true)
	if len(m.askedLib) != 1 || m.askedLib[0] != "acme-flows" {
		t.Errorf("the store was asked for %v, want the filter acme-flows", m.askedLib)
	}
	// Hook allows everything by default.
	api.libraryEntitlement = nil
	rec = readRoute(t, api, http.MethodGet, "/api/libraries", true)
	if !strings.Contains(rec.Body.String(), "secret-lib") {
		t.Errorf("with no entitlement check every published library is listed (D45), got %s", rec.Body.String())
	}
}

func TestTheLibraryReadRoutesRefuseWhatTheyCannotAnswer(t *testing.T) {
	for _, path := range []string{"/api/library-installs", "/api/libraries"} {
		t.Run(path+" unauthenticated", func(t *testing.T) {
			m := &readerMock{mockStore: &mockStore{}}
			api := newReaderAPI(m)
			api.requireAuth = true
			if rec := readRoute(t, api, http.MethodGet, path, false); rec.Code != http.StatusUnauthorized {
				t.Errorf("status %d, want 401: %s", rec.Code, rec.Body.String())
			}
			if m.installCall != 0 || len(m.askedLib) != 0 {
				t.Errorf("the store was reached with no authenticated tenant")
			}
		})
		t.Run(path+" on a store that cannot (sharded)", func(t *testing.T) {
			// mockStore alone implements neither interface: that is what a sharded store looks like to the route.
			api := newTestAPIServer(&mockStore{})
			api.factory = &fakeStoreFactory{fallback: &mockStore{}}
			rec := readRoute(t, api, http.MethodGet, path, true)
			if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), "sharded") {
				t.Errorf("status %d body %s, want 501 saying why", rec.Code, rec.Body.String())
			}
		})
		t.Run(path+" on MySQL", func(t *testing.T) {
			m := &readerMock{mockStore: &mockStore{}, installsErr: engine.ErrLibrariesUnsupported, catalogErr: engine.ErrLibrariesUnsupported}
			if rec := readRoute(t, newReaderAPI(m), http.MethodGet, path, true); rec.Code != http.StatusNotImplemented {
				t.Errorf("status %d, want 501: %s", rec.Code, rec.Body.String())
			}
		})
		t.Run(path+" store failure", func(t *testing.T) {
			m := &readerMock{mockStore: &mockStore{}, installsErr: errors.New("connection reset"), catalogErr: errors.New("connection reset")}
			rec := readRoute(t, newReaderAPI(m), http.MethodGet, path, true)
			if rec.Code != http.StatusInternalServerError {
				t.Errorf("status %d, want 500: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// The exact bodies, as literals. cleat/backendkit is another module and cannot import this package,
// so its client test (a_tenant_can_list_what_it_holds_and_what_is_published_test.go) carries the same
// four strings, and a change to the shape fails on both sides. These were captured from the real route
// table rather than written by hand: the install client was once written against keys its author
// guessed, and decoded a field to the empty string without a word.
func TestTheLibraryReadBodiesArePinnedForTheClient(t *testing.T) {
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	m := &readerMock{mockStore: &mockStore{},
		installs: []engine.InstalledLibrary{{Library: "acme-flows", BundleVersion: "1.2.0", InstalledAt: at, InstalledBy: "key-of-tenant-a"}},
		catalogue: []engine.LibraryBundleSummary{{Library: "acme-flows", BundleVersion: "1.2.0", PublishedAt: at, PublishedBy: "op",
			Members: []engine.LibraryMemberSummary{{Name: "acme-flows.order", Version: 3, WasmSHA256: "ab12", WasmLength: 7, Exposure: engine.ExposureAuth}}}}}
	for _, tc := range []struct {
		name, path string
		api        *apiServer
		want       string
	}{
		{"installs", "/api/library-installs", newReaderAPI(m),
			`{"installs":[{"library":"acme-flows","bundle_version":"1.2.0","installed_at":"2026-10-31T12:00:00Z","installed_by":"key-of-tenant-a"}]}` + "\n"},
		{"catalogue", "/api/libraries", newReaderAPI(m),
			`{"bundles":[{"library":"acme-flows","bundle_version":"1.2.0","published_at":"2026-10-31T12:00:00Z","members":[{"name":"acme-flows.order","version":3,"wasm_sha256":"ab12","wasm_length":7,"exposure":"auth"}]}]}` + "\n"},
		{"no installs", "/api/library-installs", newReaderAPI(&readerMock{mockStore: &mockStore{}}), `{"installs":[]}` + "\n"},
		{"empty catalogue", "/api/libraries", newReaderAPI(&readerMock{mockStore: &mockStore{}}), `{"bundles":[]}` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := readRoute(t, tc.api, http.MethodGet, tc.path, true).Body.String(); got != tc.want {
				t.Errorf("body\n got  %q\n want %q", got, tc.want)
			}
		})
	}
}
