package engine

import (
	"context"
	"encoding/json"
	"errors"
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
	var decoded interface{}
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
	var decoded map[string]interface{}
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
	RegisterVersionHandler(mux, store)

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
	var result map[string]interface{}
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
	RegisterVersionHandler(mux, store)

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
	var result map[string]interface{}
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
	RegisterVersionHandler(mux, store)

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
	var defs []interface{}
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
	RegisterVersionHandler(mux, store)

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
	RegisterVersionHandler(mux, store)

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
	RegisterVersionHandler(mux, store)

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
	RegisterVersionHandler(mux, store)

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
	RegisterVersionHandler(mux, store)

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
	RegisterVersionHandler(mux, store)

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
	RegisterVersionHandler(mux, store)

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
	var alerts []interface{}
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
	RegisterVersionHandler(mux, store)

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
	var result map[string]interface{}
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
	RegisterVersionHandler(mux, store)

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
	RegisterVersionHandler(mux, store)

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
	RegisterVersionHandler(mux, store)

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
	RegisterVersionHandler(mux, store)

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
	RegisterVersionHandler(mux, store)

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
	RegisterVersionHandler(mux, store)

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
	RegisterVersionHandler(mux, store)

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
	RegisterVersionHandler(mux, store)

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
	RegisterVersionHandler(mux, store)

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
	RegisterVersionHandler(mux, store)

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
	RegisterVersionHandler(mux, store)

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
