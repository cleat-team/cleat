package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// writeVersionJSON / writeVersionError tests
// ---------------------------------------------------------------------------

func TestWriteVersionJSON(t *testing.T) {
	w := httptest.NewRecorder()
	data := map[string]string{"status": "ok"}
	writeVersionJSON(w, http.StatusOK, data)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}

	var decoded map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if decoded["status"] != "ok" {
		t.Errorf("expected status=ok, got %v", decoded)
	}
}

func TestWriteVersionJSON_NilBody(t *testing.T) {
	w := httptest.NewRecorder()
	// Passing nil as the body should still encode as JSON null.
	writeVersionJSON(w, http.StatusOK, nil)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
	var decoded any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func TestWriteVersionJSON_CustomStatus(t *testing.T) {
	w := httptest.NewRecorder()
	writeVersionJSON(w, http.StatusCreated, map[string]int{"id": 42})

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("expected status 201, got %d", resp.StatusCode)
	}
	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if v := decoded["id"]; v != float64(42) {
		t.Errorf("expected id=42, got %v", v)
	}
}

func TestWriteVersionError(t *testing.T) {
	w := httptest.NewRecorder()
	writeVersionError(w, http.StatusBadRequest, "invalid input")

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}

	var decoded map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if decoded["error"] != "invalid input" {
		t.Errorf("expected error=invalid input, got %v", decoded)
	}
}

func TestWriteVersionError_NotFound(t *testing.T) {
	w := httptest.NewRecorder()
	writeVersionError(w, http.StatusNotFound, "not found")

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", resp.StatusCode)
	}
	var decoded map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if decoded["error"] != "not found" {
		t.Errorf("expected error=not found, got %v", decoded)
	}
}

func TestWriteVersionError_InternalServerError(t *testing.T) {
	w := httptest.NewRecorder()
	writeVersionError(w, http.StatusInternalServerError, "internal error")

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", resp.StatusCode)
	}
	var decoded map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if decoded["error"] != "internal error" {
		t.Errorf("expected error=internal error, got %v", decoded)
	}
}

// ---------------------------------------------------------------------------
// RegisterVersionHandler and HTTP routing tests
// ---------------------------------------------------------------------------

// versionHandlerMockStore embeds stubWorkflowStore and overrides only the
// methods needed by the version handler endpoints.
type versionHandlerMockStore struct {
	*stubWorkflowStore
	defs   []WorkflowDef
	counts map[string]int
}

func (m *versionHandlerMockStore) ListWorkflowDefs(_ context.Context, name string) ([]WorkflowDef, error) {
	if name == "" {
		return m.defs, nil
	}
	var filtered []WorkflowDef
	for _, d := range m.defs {
		if d.Name == name {
			filtered = append(filtered, d)
		}
	}
	return filtered, nil
}

func (m *versionHandlerMockStore) GetActiveInstanceCountsByVersion(_ context.Context) (map[string]int, error) {
	return m.counts, nil
}

func (m *versionHandlerMockStore) MarkVersionDeprecated(_ context.Context, name string, version int, deprecated bool) error {
	return nil
}

func (m *versionHandlerMockStore) PurgeWorkflowDef(_ context.Context, name string, version int) error {
	return nil
}

func TestRegisterVersionHandler_ListAllEmpty(t *testing.T) {
	store := &versionHandlerMockStore{
		stubWorkflowStore: &stubWorkflowStore{},
		defs:              []WorkflowDef{},
		counts:            map[string]int{},
	}
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, StaticVersionStore(store))

	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/versions")
	if err != nil {
		t.Fatalf("GET /api/versions: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result == nil {
		t.Error("expected non-nil JSON response")
	}
}

func TestRegisterVersionHandler_ListAllWithData(t *testing.T) {
	store := &versionHandlerMockStore{
		defs: []WorkflowDef{
			{Name: "wf-test", Version: 1, Deprecated: false, CreatedAt: time.Now(), ABIVersion: 1, MinVersion: 1},
		},
		counts: map[string]int{"wf-test:1": 3},
	}
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, StaticVersionStore(store))

	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/versions")
	if err != nil {
		t.Fatalf("GET /api/versions: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if total, ok := result["total_versions"]; !ok || total != float64(1) {
		t.Errorf("expected total_versions=1, got %v", total)
	}
}

func TestRegisterVersionHandler_ListByName(t *testing.T) {
	store := &versionHandlerMockStore{
		defs: []WorkflowDef{
			{Name: "wf-alpha", Version: 1, Deprecated: false, CreatedAt: time.Now(), ABIVersion: 1, MinVersion: 1},
			{Name: "wf-beta", Version: 1, Deprecated: true, CreatedAt: time.Now(), ABIVersion: 1, MinVersion: 1},
		},
		counts: map[string]int{},
	}
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, StaticVersionStore(store))

	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/versions/wf-alpha")
	if err != nil {
		t.Fatalf("GET /api/versions/wf-alpha: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	var defs []any
	if err := json.NewDecoder(resp.Body).Decode(&defs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(defs) != 1 {
		t.Errorf("expected 1 def, got %d", len(defs))
	}
}

func TestRegisterVersionHandler_BadRequest(t *testing.T) {
	store := &versionHandlerMockStore{
		defs:   []WorkflowDef{},
		counts: map[string]int{},
	}
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, StaticVersionStore(store))

	server := httptest.NewServer(mux)
	defer server.Close()

	// Missing action in URL path should return 400.
	resp, err := http.Get(server.URL + "/api/versions/wf-test/1")
	if err != nil {
		t.Fatalf("GET /api/versions/wf-test/1: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestRegisterVersionHandler_UnknownAction(t *testing.T) {
	store := &versionHandlerMockStore{
		defs:   []WorkflowDef{},
		counts: map[string]int{},
	}
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, StaticVersionStore(store))

	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/versions/wf-test/1/unknown-action", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/versions/wf-test/1/unknown-action: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestRegisterVersionHandler_InvalidVersion(t *testing.T) {
	store := &versionHandlerMockStore{
		defs:   []WorkflowDef{},
		counts: map[string]int{},
	}
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, StaticVersionStore(store))

	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/versions/wf-test/abc/deprecate", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/versions/wf-test/abc/deprecate: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestRegisterVersionHandler_Deprecate(t *testing.T) {
	store := &versionHandlerMockStore{
		defs:   []WorkflowDef{},
		counts: map[string]int{},
	}
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, StaticVersionStore(store))

	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/versions/wf-test/1/deprecate", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/versions/wf-test/1/deprecate: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result["status"] != "deprecated" {
		t.Errorf("expected status=deprecated, got %v", result)
	}
}

func TestRegisterVersionHandler_Restore(t *testing.T) {
	store := &versionHandlerMockStore{
		defs:   []WorkflowDef{},
		counts: map[string]int{},
	}
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, StaticVersionStore(store))

	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/versions/wf-test/1/restore", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/versions/wf-test/1/restore: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result["status"] != "restored" {
		t.Errorf("expected status=restored, got %v", result)
	}
}

func TestRegisterVersionHandler_Purge(t *testing.T) {
	store := &versionHandlerMockStore{
		defs:   []WorkflowDef{},
		counts: map[string]int{},
	}
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, StaticVersionStore(store))

	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/versions/wf-test/1/purge", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/versions/wf-test/1/purge: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result["status"] != "purged" {
		t.Errorf("expected status=purged, got %v", result)
	}
}

func TestRegisterVersionHandler_Stale(t *testing.T) {
	store := &versionHandlerMockStore{
		defs:   []WorkflowDef{},
		counts: map[string]int{},
	}
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, StaticVersionStore(store))

	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/versions/stale")
	if err != nil {
		t.Fatalf("GET /api/versions/stale: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	var alerts []any
	if err := json.NewDecoder(resp.Body).Decode(&alerts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if alerts == nil {
		t.Error("expected non-nil JSON array")
	}
}

func TestRegisterVersionHandler_GC(t *testing.T) {
	store := &versionHandlerMockStore{
		defs:   []WorkflowDef{},
		counts: map[string]int{},
	}
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, StaticVersionStore(store))

	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/versions/gc", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/versions/gc: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := result["VersionsRemoved"]; !ok {
		t.Error("expected VersionsRemoved in response")
	}
}

func TestRegisterVersionHandler_GCDryRun(t *testing.T) {
	store := &versionHandlerMockStore{
		defs:   []WorkflowDef{},
		counts: map[string]int{},
	}
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, StaticVersionStore(store))

	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/versions/gc?dry_run=true", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/versions/gc?dry_run=true: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestRegisterVersionHandler_MethodNotAllowedOnListVersions(t *testing.T) {
	store := &versionHandlerMockStore{
		defs:   []WorkflowDef{},
		counts: map[string]int{},
	}
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, StaticVersionStore(store))

	server := httptest.NewServer(mux)
	defer server.Close()

	// POST to /api/versions should return 405.
	resp, err := http.Post(server.URL+"/api/versions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST /api/versions: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", resp.StatusCode)
	}
}

func TestRegisterVersionHandler_GetOnGC(t *testing.T) {
	store := &versionHandlerMockStore{
		defs:   []WorkflowDef{},
		counts: map[string]int{},
	}
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, StaticVersionStore(store))

	server := httptest.NewServer(mux)
	defer server.Close()

	// GET on /api/versions/gc should also match the /api/versions/ pattern,
	// but the runGC handler checks for POST so it should trigger the method
	// check in handleVersions. Actually /api/versions/gc matches the
	// "gc" check in handleVersions, which requires POST. GET on this path
	// will not match the gc handler, so it falls through to the name/version/action
	// path parsing and ultimately to a 404.
	resp, err := http.Get(server.URL + "/api/versions/gc")
	if err != nil {
		t.Fatalf("GET /api/versions/gc: %v", err)
	}
	defer resp.Body.Close()

	// handleVersions path: path="gc", not "stale", not "" - falls through
	// to Split("/"): parts=["gc"], len(parts)=1 -> listWorkflowVersions with name="gc"
	// listWorkflowVersions checks for GET -> passes, calls store.ListWorkflowDefs(ctx, "gc")
	// Expect 200.
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Error-path tests: mock store that returns errors from all version methods
// ---------------------------------------------------------------------------

type versionHandlerErrorStore struct {
	*stubWorkflowStore
}

func (m *versionHandlerErrorStore) ListWorkflowDefs(_ context.Context, name string) ([]WorkflowDef, error) {
	return nil, errors.New("store error")
}

func (m *versionHandlerErrorStore) GetActiveInstanceCountsByVersion(_ context.Context) (map[string]int, error) {
	return nil, errors.New("store error")
}

func (m *versionHandlerErrorStore) MarkVersionDeprecated(_ context.Context, name string, version int, deprecated bool) error {
	return errors.New("store error")
}

func (m *versionHandlerErrorStore) PurgeWorkflowDef(_ context.Context, name string, version int) error {
	return errors.New("store error")
}

func TestRegisterVersionHandler_ListAllStoreError(t *testing.T) {
	store := &versionHandlerErrorStore{stubWorkflowStore: &stubWorkflowStore{}}
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, StaticVersionStore(store))

	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/versions")
	if err != nil {
		t.Fatalf("GET /api/versions: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", resp.StatusCode)
	}
}

func TestRegisterVersionHandler_ListByNameStoreError(t *testing.T) {
	store := &versionHandlerErrorStore{stubWorkflowStore: &stubWorkflowStore{}}
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, StaticVersionStore(store))

	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/versions/wf-test")
	if err != nil {
		t.Fatalf("GET /api/versions/wf-test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", resp.StatusCode)
	}
}

func TestRegisterVersionHandler_StaleStoreError(t *testing.T) {
	store := &versionHandlerErrorStore{stubWorkflowStore: &stubWorkflowStore{}}
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, StaticVersionStore(store))

	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/versions/stale")
	if err != nil {
		t.Fatalf("GET /api/versions/stale: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", resp.StatusCode)
	}
}

func TestRegisterVersionHandler_DeprecateStoreError(t *testing.T) {
	store := &versionHandlerErrorStore{stubWorkflowStore: &stubWorkflowStore{}}
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, StaticVersionStore(store))

	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/versions/wf-test/1/deprecate", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/versions/wf-test/1/deprecate: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", resp.StatusCode)
	}
}

func TestRegisterVersionHandler_PurgeStoreError(t *testing.T) {
	store := &versionHandlerErrorStore{stubWorkflowStore: &stubWorkflowStore{}}
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, StaticVersionStore(store))

	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/versions/wf-test/1/purge", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/versions/wf-test/1/purge: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", resp.StatusCode)
	}
}

func TestRegisterVersionHandler_GCStoreError(t *testing.T) {
	store := &versionHandlerErrorStore{stubWorkflowStore: &stubWorkflowStore{}}
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, StaticVersionStore(store))

	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/versions/gc", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/versions/gc: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", resp.StatusCode)
	}
}

func TestRegisterVersionHandler_ListByNameMethodNotAllowed(t *testing.T) {
	store := &versionHandlerMockStore{
		stubWorkflowStore: &stubWorkflowStore{},
		defs:              []WorkflowDef{},
		counts:            map[string]int{},
	}
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, StaticVersionStore(store))

	server := httptest.NewServer(mux)
	defer server.Close()

	// POST to /api/versions/wf-alpha should hit listWorkflowVersions → 405.
	resp, err := http.Post(server.URL+"/api/versions/wf-alpha", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/versions/wf-alpha: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", resp.StatusCode)
	}
}

func TestRegisterVersionHandler_StaleMethodNotAllowed(t *testing.T) {
	store := &versionHandlerMockStore{
		stubWorkflowStore: &stubWorkflowStore{},
		defs:              []WorkflowDef{},
		counts:            map[string]int{},
	}
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, StaticVersionStore(store))

	server := httptest.NewServer(mux)
	defer server.Close()

	// POST to /api/versions/stale should hit listStaleAlerts → 405.
	resp, err := http.Post(server.URL+"/api/versions/stale", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/versions/stale: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Multi-tenant tests -- Finding B3.
//
// RegisterVersionHandler used to take a single process-wide WorkflowStore,
// opened once at boot against the default tenant (engine.DefaultTenantUUID).
// Every version endpoint -- including POST /api/versions/<name>/<v>/purge,
// which permanently deletes a workflow definition -- was then served from
// that one store regardless of who authenticated the request. These tests
// exercise VersionStoreResolver, the mechanism that closes that gap: each
// request must reach only the store its own resolution picks, and a request
// the resolver refuses must never reach any store at all.
// ---------------------------------------------------------------------------

// recordingVersionStore is a per-tenant mock: it tracks its defs and which
// mutating calls reached it, so a test can assert not just "the response was
// correct" but "this store, and no other, was touched."
type recordingVersionStore struct {
	*stubWorkflowStore
	label string // which tenant this store stands in for, for failure messages

	defs   []WorkflowDef
	counts map[string]int

	deprecateCalls []string // "name/version/deprecated"
	purgeCalls     []string // "name/version"
}

func (m *recordingVersionStore) ListWorkflowDefs(_ context.Context, name string) ([]WorkflowDef, error) {
	if name == "" {
		return m.defs, nil
	}
	var filtered []WorkflowDef
	for _, d := range m.defs {
		if d.Name == name {
			filtered = append(filtered, d)
		}
	}
	return filtered, nil
}

func (m *recordingVersionStore) GetActiveInstanceCountsByVersion(_ context.Context) (map[string]int, error) {
	return m.counts, nil
}

func (m *recordingVersionStore) MarkVersionDeprecated(_ context.Context, name string, version int, deprecated bool) error {
	m.deprecateCalls = append(m.deprecateCalls, fmt.Sprintf("%s/%d/%v", name, version, deprecated))
	for i := range m.defs {
		if m.defs[i].Name == name && m.defs[i].Version == version {
			m.defs[i].Deprecated = deprecated
		}
	}
	return nil
}

func (m *recordingVersionStore) PurgeWorkflowDef(_ context.Context, name string, version int) error {
	m.purgeCalls = append(m.purgeCalls, fmt.Sprintf("%s/%d", name, version))
	kept := m.defs[:0]
	for _, d := range m.defs {
		if d.Name == name && d.Version == version {
			continue
		}
		kept = append(kept, d)
	}
	m.defs = kept
	return nil
}

// versionTenantKey is a context key private to this test file. Production
// wiring resolves the tenant via auth.TenantIDFromContext, used by
// cmd/cleat-worker's apiServer.storeFor -- engine cannot import auth (auth
// already imports engine), so this test builds its own minimal stand-in to
// exercise VersionStoreResolver's contract directly rather than through
// cmd/cleat-worker's HTTP plumbing.
type versionTenantKey struct{}

func withVersionTenant(r *http.Request, tenant string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), versionTenantKey{}, tenant))
}

// twoTenantVersionResolver returns a VersionStoreResolver that mirrors
// apiServer.scopedStore's contract: a request carrying a known tenant in
// context is routed to that tenant's store; a request carrying no tenant (or
// an unrecognised one) is refused with 401 and never reaches any store.
func twoTenantVersionResolver(byTenant map[string]*recordingVersionStore) VersionStoreResolver {
	return func(w http.ResponseWriter, r *http.Request) (WorkflowStore, bool) {
		tenant, _ := r.Context().Value(versionTenantKey{}).(string)
		store, ok := byTenant[tenant]
		if !ok {
			writeVersionError(w, http.StatusUnauthorized, "authentication required")
			return nil, false
		}
		return store, true
	}
}

// TestRegisterVersionHandler_TenantIsolation is the regression test Finding
// B3 asked for: tenant B must not see, deprecate, or purge tenant A's
// definitions, even when both tenants have a definition with the identical
// name -- the ordinary case, since definitions are per-tenant rather than
// globally unique, not an edge case.
func TestRegisterVersionHandler_TenantIsolation(t *testing.T) {
	storeA := &recordingVersionStore{
		stubWorkflowStore: &stubWorkflowStore{},
		label:             "A",
		defs: []WorkflowDef{
			{Name: "wf-shared-name", Version: 1, Deprecated: false, CreatedAt: time.Now(), ABIVersion: 1, MinVersion: 1},
		},
		counts: map[string]int{},
	}
	storeB := &recordingVersionStore{
		stubWorkflowStore: &stubWorkflowStore{},
		label:             "B",
		defs: []WorkflowDef{
			{Name: "wf-shared-name", Version: 1, Deprecated: false, CreatedAt: time.Now(), ABIVersion: 1, MinVersion: 1},
		},
		counts: map[string]int{},
	}

	resolve := twoTenantVersionResolver(map[string]*recordingVersionStore{
		"tenant-a": storeA,
		"tenant-b": storeB,
	})
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, resolve)

	// mux.ServeHTTP directly, not httptest.NewServer + http.Client: an HTTP
	// request context set via req.WithContext lives only on the client-side
	// *http.Request and does not cross the wire, so a real round trip would
	// always land in the server's handler with a background context -- i.e.
	// this test would measure "was there a tenant" as always-false, not the
	// per-tenant routing it exists to check. This is the same reason
	// cmd/cleat-worker's twoTenantServer tests call mux.ServeHTTP(w, req)
	// in-process instead of dialing a real server.
	serve := func(method, path, tenant string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req = withVersionTenant(req, tenant)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}

	// GET /api/versions/wf-shared-name as tenant B must return only tenant
	// B's copy.
	resp := serve(http.MethodGet, "/api/versions/wf-shared-name", "tenant-b")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", resp.Code, resp.Body.String())
	}

	// The worst case in the finding: POST .../purge as tenant B must delete
	// only tenant B's copy. If this reached storeA instead -- the old,
	// single-hardcoded-store behaviour -- storeA would lose its definition
	// despite tenant B being the caller.
	presp := serve(http.MethodPost, "/api/versions/wf-shared-name/1/purge", "tenant-b")
	if presp.Code != http.StatusOK {
		t.Fatalf("purge status = %d, want 200; body = %s", presp.Code, presp.Body.String())
	}

	if len(storeB.purgeCalls) != 1 {
		t.Fatalf("tenant B's store received %d purge calls, want 1: %v", len(storeB.purgeCalls), storeB.purgeCalls)
	}
	if len(storeA.purgeCalls) != 0 {
		t.Errorf("tenant A's store received a purge call from a tenant-B request: %v", storeA.purgeCalls)
	}
	if len(storeA.defs) != 1 {
		t.Errorf("tenant A's definition was deleted by tenant B's purge request; storeA.defs = %v", storeA.defs)
	}
	if len(storeB.defs) != 0 {
		t.Errorf("tenant B's own definition was not purged; storeB.defs = %v", storeB.defs)
	}

	// Same check for deprecate: tenant A marking its own version deprecated
	// must not touch tenant B's copy.
	dresp := serve(http.MethodPost, "/api/versions/wf-shared-name/1/deprecate", "tenant-a")
	if dresp.Code != http.StatusOK {
		t.Fatalf("deprecate status = %d, want 200; body = %s", dresp.Code, dresp.Body.String())
	}
	if len(storeA.deprecateCalls) != 1 {
		t.Fatalf("tenant A's store received %d deprecate calls, want 1", len(storeA.deprecateCalls))
	}
	if len(storeB.deprecateCalls) != 0 {
		t.Errorf("tenant B's store received a deprecate call from a tenant-A request: %v", storeB.deprecateCalls)
	}
}

// TestRegisterVersionHandler_RefusesUnauthenticatedRequest is the fail-closed
// half of the same fix: a request with no resolvable tenant must be refused
// (401) rather than silently served from some default/fallback store. This
// proves RegisterVersionHandler's handlers actually stop at that refusal
// rather than, say, treating a nil store as "no scoping needed" and calling a
// method on it anyway.
func TestRegisterVersionHandler_RefusesUnauthenticatedRequest(t *testing.T) {
	storeA := &recordingVersionStore{
		stubWorkflowStore: &stubWorkflowStore{},
		label:             "A",
		defs: []WorkflowDef{
			{Name: "wf-x", Version: 1, CreatedAt: time.Now(), ABIVersion: 1, MinVersion: 1},
		},
	}
	resolve := twoTenantVersionResolver(map[string]*recordingVersionStore{"tenant-a": storeA})
	mux := http.NewServeMux()
	RegisterVersionHandler(mux, resolve)
	server := httptest.NewServer(mux)
	defer server.Close()

	// No withVersionTenant call: this request carries no tenant at all.
	resp, err := http.Get(server.URL + "/api/versions")
	if err != nil {
		t.Fatalf("GET /api/versions: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}

	purgeResp, err := http.Post(server.URL+"/api/versions/wf-x/1/purge", "application/json", nil)
	if err != nil {
		t.Fatalf("POST purge (unauthenticated): %v", err)
	}
	defer purgeResp.Body.Close()
	if purgeResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("purge status = %d, want 401", purgeResp.StatusCode)
	}
	if len(storeA.purgeCalls) != 0 {
		t.Errorf("an unauthenticated purge request reached the store: %v", storeA.purgeCalls)
	}
}
