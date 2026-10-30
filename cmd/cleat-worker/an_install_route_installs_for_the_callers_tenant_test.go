package main

// cleat#3540, PR 3a: POST /api/libraries/{library}/install.
//
// The route is driven through registerRoutes, the one route table, so a registration
// that is missing or mistyped fails here and not in production. The store behind it is a
// mock that records what the route asked of it, because what is under test is the
// route's own decisions: what it validates before touching a store, what it hands the
// store, which refusal comes from where, and above all that the entitlement hook is
// consulted BEFORE the store, so a refusal from it leaves nothing written.
//
// The stores' behaviour is the engine's tests (an_install_copies_every_column_of_its_members_test.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
)

// installerMock is a store that can install a bundle and records every request.
type installerMock struct {
	*mockStore
	reqs   []engine.InstallLibraryRequest
	result engine.InstallLibraryResult
	err    error
	// allowsPublic is what the store reports the operator's public opt-in to be.
	allowsPublic bool
}

func (m *installerMock) InstallLibraryBundle(_ context.Context, req engine.InstallLibraryRequest) (engine.InstallLibraryResult, error) {
	m.reqs = append(m.reqs, req)
	return m.result, m.err
}

func (m *installerMock) AllowsPublicExposure(context.Context, string) (bool, error) {
	return m.allowsPublic, nil
}

var installTenant = uuid.MustParse("f4000000-0000-4000-8000-0000000035a0")

// installRoute serves one POST through the real route table as an authenticated tenant.
func installRoute(t *testing.T, api *apiServer, path, body string, authed bool) *httptest.ResponseRecorder {
	t.Helper()
	mux := registerRoutes(http.NewServeMux(), api)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if authed {
		ctx := auth.WithTenantID(req.Context(), installTenant)
		ctx = auth.WithSubject(ctx, "key-of-tenant-a")
		req = req.WithContext(ctx)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func newInstallAPI(m *installerMock) *apiServer {
	api := newTestAPIServer(m.mockStore)
	api.factory = &fakeStoreFactory{fallback: m}
	return api
}

func TestInstallRouteHandsTheStoreWhatTheCallerAsked(t *testing.T) {
	m := &installerMock{mockStore: &mockStore{}, result: engine.InstallLibraryResult{
		Library: "acme-flows", BundleVersion: "1.2.0", PreviousBundleVersion: "1.1.0",
		Members: []engine.InstalledLibraryMember{{Name: "order", Version: 3, Exposure: engine.ExposureAuth}},
	}}
	rec := installRoute(t, newInstallAPI(m), "/api/libraries/acme-flows/install", `{"bundle_version":"1.2.0"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(m.reqs) != 1 {
		t.Fatalf("the store was asked %d times, want once", len(m.reqs))
	}
	got := m.reqs[0]
	if got.Library != "acme-flows" || got.BundleVersion != "1.2.0" {
		t.Errorf("the store was asked for %q @ %q, want the path's library and the body's bundle version", got.Library, got.BundleVersion)
	}
	if got.InstalledBy != "key-of-tenant-a" {
		t.Errorf("InstalledBy = %q, want the caller's subject", got.InstalledBy)
	}
	if got.TenantAllowsPublic {
		t.Errorf("TenantAllowsPublic is true for a tenant the operator has not opted in")
	}
	var body engine.InstallLibraryResult
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not the install result: %v: %s", err, rec.Body.String())
	}
	if body.PreviousBundleVersion != "1.1.0" || len(body.Members) != 1 || body.Members[0].Name != "order" {
		t.Errorf("response = %+v", body)
	}
}

func TestInstallRouteHandsTheStoreTheOperatorsPublicOptIn(t *testing.T) {
	m := &installerMock{mockStore: &mockStore{}, allowsPublic: true}
	if rec := installRoute(t, newInstallAPI(m), "/api/libraries/lib/install", `{"bundle_version":"1"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(m.reqs) != 1 || !m.reqs[0].TenantAllowsPublic {
		t.Errorf("the opt-in the store reported did not reach the install: %+v", m.reqs)
	}
}

// THE REFUSAL TEST (decision on #3540): a refusal from the entitlement hook is
// answered before the store is touched, so nothing can have been written.
func TestAnEntitlementRefusalReachesNoStore(t *testing.T) {
	m := &installerMock{mockStore: &mockStore{}}
	api := newInstallAPI(m)
	var asked struct{ tenant, library string }
	api.libraryEntitlement = func(_ context.Context, tenantID, library string) error {
		asked.tenant, asked.library = tenantID, library
		return errors.New("this tenant is not entitled to acme-flows")
	}
	rec := installRoute(t, api, "/api/libraries/acme-flows/install", `{"bundle_version":"1.0.0"}`, true)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not entitled") {
		t.Errorf("status %d body %s, want 403 carrying the hook's reason", rec.Code, rec.Body.String())
	}
	if len(m.reqs) != 0 {
		t.Errorf("the store was asked to install %d time(s) after the hook refused: a refusal must write nothing", len(m.reqs))
	}
	if asked.tenant != installTenant.String() || asked.library != "acme-flows" {
		t.Errorf("the hook was asked about %+v, want the caller's tenant and the path's library", asked)
	}

	// The same hook, allowing, lets the install through: the refusal above was the hook's.
	api.libraryEntitlement = func(context.Context, string, string) error { return nil }
	if rec := installRoute(t, api, "/api/libraries/acme-flows/install", `{"bundle_version":"1.0.0"}`, true); rec.Code != http.StatusOK {
		t.Errorf("an allowing hook still answered %d: %s", rec.Code, rec.Body.String())
	}
}

func TestInstallRouteRefusesBeforeAnyStoreIsReached(t *testing.T) {
	long := strings.Repeat("a", 129)
	for _, tc := range []struct {
		name, path, body string
		authed           bool
		want             int
	}{
		{"a library name with a space", "/api/libraries/bad%20name/install", `{"bundle_version":"1"}`, true, 400},
		{"a library name too long", "/api/libraries/" + long + "/install", `{"bundle_version":"1"}`, true, 400},
		{"a missing bundle version", "/api/libraries/lib/install", `{}`, true, 400},
		{"a bundle version with a space", "/api/libraries/lib/install", `{"bundle_version":"1 0"}`, true, 400},
		{"a body that is not JSON", "/api/libraries/lib/install", `not json`, true, 400},
		{"no authenticated tenant", "/api/libraries/lib/install", `{"bundle_version":"1"}`, false, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &installerMock{mockStore: &mockStore{}}
			api := newInstallAPI(m)
			api.requireAuth = true
			rec := installRoute(t, api, tc.path, tc.body, tc.authed)
			if rec.Code != tc.want {
				t.Errorf("status %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if len(m.reqs) != 0 {
				t.Errorf("the store was reached %d time(s)", len(m.reqs))
			}
		})
	}
}

func TestInstallRouteIsPOSTOnly(t *testing.T) {
	m := &installerMock{mockStore: &mockStore{}}
	mux := registerRoutes(http.NewServeMux(), newInstallAPI(m))
	req := httptest.NewRequest(http.MethodGet, "/api/libraries/lib/install", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET answered %d, want 405", rec.Code)
	}
}

func TestInstallRouteMapsEachRefusalToItsStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"a bundle that does not exist", &engine.LibraryBundleNotFoundError{Library: "lib", BundleVersion: "9"}, http.StatusNotFound},
		{"a definition in the way", &engine.LibraryDefinitionCollisionError{Name: "order", Version: 1}, http.StatusConflict},
		{"a collision the store wrapped", fmt.Errorf("install library: %w", engine.ErrLibraryDefinitionCollision), http.StatusConflict},
		{"a public member for a tenant that has not opted in", fmt.Errorf("member x: %w", &engine.ErrExposurePublicNotOptedIn{TenantID: "t"}), http.StatusBadRequest},
		{"a store with no library support", engine.ErrLibrariesUnsupported, http.StatusNotImplemented},
		{"anything else", errors.New("connection reset"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &installerMock{mockStore: &mockStore{}, err: tc.err}
			rec := installRoute(t, newInstallAPI(m), "/api/libraries/lib/install", `{"bundle_version":"1"}`, true)
			if rec.Code != tc.want {
				t.Errorf("status %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want != http.StatusInternalServerError && strings.Contains(rec.Body.String(), "connection reset") {
				t.Errorf("an error that is not the store's own leaked into a %d: %s", tc.want, rec.Body.String())
			}
		})
	}
}

// A store that cannot install (a sharded deployment, today) is told so in words that
// name the issue that tracks it, not a bare 404 or 500. That is cleat#3606, the home of the
// sharded multi-tenancy question; cleat#3597 named it until its sharded half became publish
// replication alone.
func TestInstallRouteSaysWhyAStoreCannotInstall(t *testing.T) {
	ms := &mockStore{}
	api := newTestAPIServer(ms) // *mockStore has no InstallLibraryBundle
	rec := installRoute(t, api, "/api/libraries/lib/install", `{"bundle_version":"1"}`, true)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cleat#3606") {
		t.Errorf("the 501 does not name the issue that tracks it: %s", rec.Body.String())
	}
}
