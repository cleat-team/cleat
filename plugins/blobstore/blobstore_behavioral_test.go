package blobstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/plugintest"
)

// ---------------------------------------------------------------------------
// RegisterRoutes — error paths
// ---------------------------------------------------------------------------

func TestRegisterRoutesNilMux(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterRoutes(nil)
	if err == nil {
		t.Fatal("expected error for nil mux")
	}
	if !strings.Contains(err.Error(), "nil mux") {
		t.Errorf("expected 'nil mux' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// splitTag — direct unit testing of the helper function
// ---------------------------------------------------------------------------

func TestSplitTag(t *testing.T) {
	tests := []struct {
		input string
		want  []string // nil means expect nil return
	}{
		{"key:value", []string{"key", "value"}},
		{" env:prod ", []string{"env", "prod"}},
		{"a:b:c", []string{"a", "b:c"}},
		{"nocolon", nil},
		{":nokey", nil},
		{"novalue:", nil},
		{"", nil},
		{"   ", nil},
		{"x:y:z:w", []string{"x", "y:z:w"}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := splitTag(tt.input)
			if tt.want == nil {
				if got != nil {
					t.Errorf("expected nil, got %v", got)
				}
			} else {
				if got == nil {
					t.Fatalf("expected %v, got nil", tt.want)
				}
				if got[0] != tt.want[0] || got[1] != tt.want[1] {
					t.Errorf("expected %v, got %v", tt.want, got)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Route error paths: empty body, empty key, content-type handling
// ---------------------------------------------------------------------------

func TestHandlePutEmptyBody(t *testing.T) {
	_, handler, _, _ := setupTestPlugin(t)

	// A PUT request with no body should return 400 "empty body".
	req := authedRequest("PUT", "/blobs/empty-body-key", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty body, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "empty body") {
		t.Errorf("expected 'empty body' error, got: %s", rec.Body.String())
	}
}

func TestHandlePutEmptyBodyExplicitEmpty(t *testing.T) {
	_, handler, _, _ := setupTestPlugin(t)

	// A PUT with an explicitly empty reader should also return 400.
	req := authedRequest("PUT", "/blobs/empty-key", bytes.NewReader([]byte{}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty body, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlePutEmptyKey(t *testing.T) {
	_, handler, _, _ := setupTestPlugin(t)

	req := authedRequest("PUT", "/blobs/", bytes.NewReader([]byte("data")))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty key, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleGetEmptyKey(t *testing.T) {
	_, handler, _, _ := setupTestPlugin(t)

	req := authedRequest("GET", "/blobs/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty key, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleHeadEmptyKey(t *testing.T) {
	_, handler, _, _ := setupTestPlugin(t)

	req := authedRequest("HEAD", "/blobs/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty key, got %d", rec.Code)
	}
}

func TestHandleDeleteEmptyKey(t *testing.T) {
	_, handler, _, _ := setupTestPlugin(t)

	req := authedRequest("DELETE", "/blobs/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty key, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleGetMissingBlob(t *testing.T) {
	_, handler, _, _ := setupTestPlugin(t)

	req := authedRequest("GET", "/blobs/does-not-exist", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for missing blob, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleHeadMissingBlob(t *testing.T) {
	_, handler, _, _ := setupTestPlugin(t)

	req := authedRequest("HEAD", "/blobs/does-not-exist", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for missing blob HEAD, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Content-type handling: default and explicit
// ---------------------------------------------------------------------------

func TestHandlePutDefaultContentType(t *testing.T) {
	_, handler, _, _ := setupTestPlugin(t)
	body := "plain data"

	// PUT without Content-Type header — should default to application/octet-stream.
	req := authedRequest("PUT", "/blobs/default-ct", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// GET should return the default content type.
	req = authedRequest("GET", "/blobs/default-ct", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("expected default Content-Type 'application/octet-stream', got %q", ct)
	}
}

func TestHandlePutExplicitContentType(t *testing.T) {
	_, handler, _, _ := setupTestPlugin(t)
	body := "json data"

	// PUT with explicit Content-Type.
	req := authedRequest("PUT", "/blobs/explicit-ct", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// GET should return the explicit content type.
	req = authedRequest("GET", "/blobs/explicit-ct", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got %q", ct)
	}
}

// ---------------------------------------------------------------------------
// Headers: SHA256 header is present on GET/HEAD
// ---------------------------------------------------------------------------

func TestHandleGetReturnsSHA256Header(t *testing.T) {
	_, handler, _, _ := setupTestPlugin(t)
	body := "sha256-check"

	req := authedRequest("PUT", "/blobs/sha256-test", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d", rec.Code)
	}

	req = authedRequest("GET", "/blobs/sha256-test", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d", rec.Code)
	}

	sha256 := rec.Header().Get("X-Blob-SHA256")
	if sha256 == "" {
		t.Error("expected X-Blob-SHA256 header on GET response")
	}
	if len(sha256) != 64 {
		t.Errorf("expected 64-char hex SHA256, got %q (len=%d)", sha256, len(sha256))
	}
}

func TestHandleHeadReturnsSHA256Header(t *testing.T) {
	_, handler, _, _ := setupTestPlugin(t)
	body := "head-sha256"

	req := authedRequest("PUT", "/blobs/head-sha256", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d", rec.Code)
	}

	req = authedRequest("HEAD", "/blobs/head-sha256", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD: expected 200, got %d", rec.Code)
	}

	sha256 := rec.Header().Get("X-Blob-SHA256")
	if sha256 == "" {
		t.Error("expected X-Blob-SHA256 header on HEAD response")
	}
	if len(sha256) != 64 {
		t.Errorf("expected 64-char hex SHA256, got %q (len=%d)", sha256, len(sha256))
	}

	ct := rec.Header().Get("Content-Type")
	if ct == "" {
		t.Error("expected Content-Type header on HEAD response")
	}
}

// ---------------------------------------------------------------------------
// Blob metadata JSON roundtrip
// ---------------------------------------------------------------------------

func TestBlobPutOutputJSONRoundtrip(t *testing.T) {
	original := blobPutOutput{
		Key:    "test-key",
		SHA256: "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
		Size:   42,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded blobPutOutput
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Key != original.Key {
		t.Errorf("key: got %q, want %q", decoded.Key, original.Key)
	}
	if decoded.SHA256 != original.SHA256 {
		t.Errorf("sha256: got %q, want %q", decoded.SHA256, original.SHA256)
	}
	if decoded.Size != original.Size {
		t.Errorf("size: got %d, want %d", decoded.Size, original.Size)
	}
}

func TestBlobPutOutputJSONZeroValues(t *testing.T) {
	// Zero-value blobPutOutput should marshal/unmarshal without error.
	original := blobPutOutput{}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal zero: %v", err)
	}

	var decoded blobPutOutput
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal zero: %v", err)
	}
}

func TestBlobGetOutputJSONRoundtrip(t *testing.T) {
	original := blobGetOutput{
		Key:         "get-key",
		SHA256:      "deadbeef1234567890deadbeef1234567890deadbeef1234567890deadbeef1234",
		Size:        7,
		ContentType: "text/plain",
		Data:        []byte("content"),
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded blobGetOutput
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Key != original.Key {
		t.Errorf("key: got %q, want %q", decoded.Key, original.Key)
	}
	if decoded.SHA256 != original.SHA256 {
		t.Errorf("sha256: got %q, want %q", decoded.SHA256, original.SHA256)
	}
	if decoded.Size != original.Size {
		t.Errorf("size: got %d, want %d", decoded.Size, original.Size)
	}
	if decoded.ContentType != original.ContentType {
		t.Errorf("content_type: got %q, want %q", decoded.ContentType, original.ContentType)
	}
	if string(decoded.Data) != string(original.Data) {
		t.Errorf("data: got %q, want %q", string(decoded.Data), string(original.Data))
	}
}

func TestBlobGetOutputJSONBinaryData(t *testing.T) {
	// Binary data (non-UTF-8) in blobGetOutput should survive JSON roundtrip
	// via base64 encoding (json.Marshal encodes []byte as base64).
	original := blobGetOutput{
		Key:    "binary-key",
		SHA256: "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20",
		Size:   4,
		Data:   []byte{0x00, 0xff, 0xfe, 0x01},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded blobGetOutput
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(decoded.Data) != 4 {
		t.Fatalf("expected 4 bytes, got %d", len(decoded.Data))
	}
	for i, b := range decoded.Data {
		if b != original.Data[i] {
			t.Errorf("byte %d: got %02x, want %02x", i, b, original.Data[i])
		}
	}
}

// ---------------------------------------------------------------------------
// Migrations: version > 0, Up/Down non-empty, sequential ordering
// ---------------------------------------------------------------------------

func TestMigrationValid(t *testing.T) {
	p := &Plugin{}
	migrations := p.Migrations()

	// One shared predicate for what a migration must do, rather than a copy per
	// plugin -- thirteen plugins carried their own and they had already drifted
	// (cleat#1513). The copy that stood here rejected a TenantScoped migration
	// in both halves: v4 declares a table for the runtime to put a policy on
	// and carries no SQL in either direction, because there is none to write
	// and no policy an author could drop. cleat#1512.
	plugintest.AssertMigrationsDoSomething(t, migrations)

	// Kept local: strictly increasing versions is blobstore's own rule, and
	// cleat#1513's doc comment asks for exactly this split rather than folding
	// one plugin's rule onto twelve others.
	for i := 1; i < len(migrations); i++ {
		if migrations[i].Version <= migrations[i-1].Version {
			t.Errorf("migrations not sequential: v%d follows v%d",
				migrations[i].Version, migrations[i-1].Version)
		}
	}
}

func TestMigrationUpContainsSQL(t *testing.T) {
	p := &Plugin{}
	for _, m := range p.Migrations() {
		// A TenantScoped migration has no SQL and that is the point: the
		// runtime emits the policy from the field. This assertion is about a
		// migration that WRITES SQL saying nothing, so it does not apply --
		// and it is the one cleat#1512's table records blobstore as not
		// having, which is how it was nearly shipped red. cleat#1513's shared
		// helper covers "the migration does something"; these two cover "the
		// SQL it wrote is SQL".
		// A SweepTables-only migration has no SQL either: the runtime emits
		// the GRANT, same as it emits the policy for TenantScoped. cleat#1490.
		if len(m.TenantScoped) > 0 || len(m.SweepTables) > 0 {
			continue
		}
		up := m.Up
		if !strings.Contains(up, "CREATE") && !strings.Contains(up, "ALTER") && !strings.Contains(up, "INSERT") {
			t.Errorf("migration v%d Up SQL does not contain CREATE, ALTER, or INSERT: %s", m.Version, up[:min(len(up), 80)])
		}
	}
}

func TestMigrationDownContainsSQL(t *testing.T) {
	p := &Plugin{}
	for _, m := range p.Migrations() {
		// See TestMigrationUpContainsSQL. A TenantScoped migration has no Down
		// either -- the policy is the runtime's to drop, not an author's.
		if len(m.TenantScoped) > 0 || len(m.SweepTables) > 0 {
			continue
		}
		down := m.Down
		if !strings.Contains(down, "DROP") && !strings.Contains(down, "ALTER") {
			t.Errorf("migration v%d Down SQL does not contain DROP or ALTER: %s", m.Version, down[:min(len(down), 80)])
		}
	}
}

// ---------------------------------------------------------------------------
// List endpoint edge cases
// ---------------------------------------------------------------------------

func TestHandleListNoResults(t *testing.T) {
	_, handler, _, _ := setupTestPlugin(t)

	// GET /blobs with no data should return an empty JSON array.
	req := authedRequest("GET", "/blobs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("LIST: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Response should be a valid JSON array (possibly empty).
	var results []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &results); err != nil {
		t.Fatalf("LIST decode: %v", err)
	}
	if results == nil {
		t.Error("expected empty JSON array '[]', got null")
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results with no data, got %d", len(results))
	}
}

func TestHandleListDefaultLimit(t *testing.T) {
	_, handler, _, _ := setupTestPlugin(t)

	// Insert more than default limit (50) blobs.
	for i := 0; i < 55; i++ {
		key := fmt.Sprintf("limit-key-%d", i)
		body := fmt.Sprintf("data-%d", i)
		req := authedRequest("PUT", "/blobs/"+key, bytes.NewReader([]byte(body)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s: expected 201, got %d", key, rec.Code)
		}
	}

	req := authedRequest("GET", "/blobs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("LIST: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var results []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &results); err != nil {
		t.Fatalf("LIST decode: %v", err)
	}
	if len(results) > 55 {
		t.Errorf("expected at most 55 results, got %d", len(results))
	}
}

// ---------------------------------------------------------------------------
// PUT response body verification
// ---------------------------------------------------------------------------

func TestHandlePutResponseBody(t *testing.T) {
	_, handler, _, _ := setupTestPlugin(t)
	body := "hello"

	req := authedRequest("PUT", "/blobs/response-check", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp["key"] != "response-check" {
		t.Errorf("key: got %v, want 'response-check'", resp["key"])
	}
	sha256, ok := resp["sha256"].(string)
	if !ok || len(sha256) != 64 {
		t.Errorf("sha256: got %v (len=%d), want 64-char hex", resp["sha256"], len(sha256))
	}
	size, ok := resp["size"].(float64)
	if !ok || size != 5 {
		t.Errorf("size: got %v, want 5", resp["size"])
	}
}

// ---------------------------------------------------------------------------
// Host function input validation edge cases
// ---------------------------------------------------------------------------

func TestBlobPutInvalidJSON(t *testing.T) {
	p, _, _ := setupHostFuncTest(t)
	ctx := hostFuncContext(context.Background(), testTenantID, "")

	_, err := p.blobPut(ctx, "{not valid json}")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid input") {
		t.Errorf("expected 'invalid input' error, got: %v", err)
	}
}

func TestBlobGetInvalidJSON(t *testing.T) {
	p, _, _ := setupHostFuncTest(t)
	ctx := hostFuncContext(context.Background(), testTenantID, "")

	_, err := p.blobGet(ctx, "{bad}")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid input") {
		t.Errorf("expected 'invalid input' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Helper
// ---------------------------------------------------------------------------

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Error injection infrastructure
// ---------------------------------------------------------------------------

// failBackend is a Backend implementation that always returns errors.
type failBackend struct{}

func (b *failBackend) Put(_ context.Context, _ string, _ []byte, _ string) error {
	return fmt.Errorf("backend error")
}

func (b *failBackend) Get(_ context.Context, _ string) ([]byte, error) {
	return nil, fmt.Errorf("backend error")
}

func (b *failBackend) Delete(_ context.Context, _ string) error {
	return fmt.Errorf("backend error")
}

// deploymentSecretsUnavailableBackend is a Backend that always fails with
// errDeploymentSecretsUnavailable in its chain -- the shape a real s3Backend
// produces when the deployment secrets it needs cannot be resolved (backend.go).
type deploymentSecretsUnavailableBackend struct{}

func (b *deploymentSecretsUnavailableBackend) Put(_ context.Context, _ string, _ []byte, _ string) error {
	return fmt.Errorf("blobstore: s3 put: %w", errDeploymentSecretsUnavailable)
}

func (b *deploymentSecretsUnavailableBackend) Get(_ context.Context, _ string) ([]byte, error) {
	return nil, fmt.Errorf("blobstore: s3 get: %w", errDeploymentSecretsUnavailable)
}

func (b *deploymentSecretsUnavailableBackend) Delete(_ context.Context, _ string) error {
	return fmt.Errorf("blobstore: s3 delete: %w", errDeploymentSecretsUnavailable)
}

// selectiveErrorConn wraps a fakeConn and injects errors for SQL statements
// matching the configured patterns; all other queries pass through.
type selectiveErrorConn struct {
	*fakeConn
	failExecPatterns  []string
	failQueryPatterns []string
}

func (c *selectiveErrorConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	for _, pattern := range c.failExecPatterns {
		if strings.Contains(query, pattern) {
			return nil, fmt.Errorf("injected exec error: %s", pattern)
		}
	}
	return c.fakeConn.ExecContext(ctx, query, args)
}

func (c *selectiveErrorConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	for _, pattern := range c.failQueryPatterns {
		if strings.Contains(query, pattern) {
			return nil, fmt.Errorf("injected query error: %s", pattern)
		}
	}
	return c.fakeConn.QueryContext(ctx, query, args)
}

type selectiveErrorConnector struct {
	store             *fakeDBStore
	failExecPatterns  []string
	failQueryPatterns []string
}

func (c *selectiveErrorConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &selectiveErrorConn{
		fakeConn:          &fakeConn{store: c.store},
		failExecPatterns:  c.failExecPatterns,
		failQueryPatterns: c.failQueryPatterns,
	}, nil
}

func (c *selectiveErrorConnector) Driver() driver.Driver {
	return &fakeDrv{}
}

// ---------------------------------------------------------------------------
// Plugin.Init tests
// ---------------------------------------------------------------------------

func TestPluginInitDefaults(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{}
	ctx := context.Background()

	env := &plugin.Environment{
		DB:     &engine.SQLDBAdapter{DB: db},
		Mux:    http.NewServeMux(),
		Logger: slog.Default(),
	}

	err := p.Init(ctx, env)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.config.Backend != "memory" {
		t.Errorf("expected default backend 'memory', got %q", p.config.Backend)
	}
	if p.logger == nil {
		t.Error("expected logger to be set")
	}
}

func TestPluginInitWithConfig(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{}
	ctx := context.Background()

	env := &plugin.Environment{
		DB:     &engine.SQLDBAdapter{DB: db},
		Mux:    http.NewServeMux(),
		Logger: slog.Default(),
		Config: []byte(`{"backend":"memory"}`),
	}

	err := p.Init(ctx, env)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.config.Backend != "memory" {
		t.Errorf("expected backend 'memory', got %q", p.config.Backend)
	}
	if p.backend == nil {
		t.Error("expected backend to be set")
	}
}

func TestPluginInitInvalidConfig(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{}
	ctx := context.Background()

	env := &plugin.Environment{
		DB:     &engine.SQLDBAdapter{DB: db},
		Mux:    http.NewServeMux(),
		Logger: slog.Default(),
		Config: []byte(`{bad json`),
	}

	err := p.Init(ctx, env)
	if err == nil {
		t.Fatal("expected error for invalid config")
	}
	if !strings.Contains(err.Error(), "invalid config") {
		t.Errorf("expected 'invalid config' error, got: %v", err)
	}
}

func TestPluginInitNilLogger(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{}
	ctx := context.Background()

	env := &plugin.Environment{
		DB:  &engine.SQLDBAdapter{DB: db},
		Mux: http.NewServeMux(),
		// Logger is nil — Init should use slog.Default()
	}

	err := p.Init(ctx, env)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.logger == nil {
		t.Error("expected logger to be non-nil after Init with nil env.Logger")
	}
}

func TestPluginInitS3Backend(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{}
	ctx := context.Background()

	env := &plugin.Environment{
		DB:     &engine.SQLDBAdapter{DB: db},
		Mux:    http.NewServeMux(),
		Logger: slog.Default(),
		Config: []byte(`{"backend":"s3","bucket":"test-bucket","region":"us-east-1","endpoint":"localhost:9000"}`),
	}

	// newS3Backend constructs a client without making HTTP calls,
	// so this should succeed even with a local endpoint.
	err := p.Init(ctx, env)
	if err != nil {
		t.Fatalf("Init with s3 config: %v", err)
	}
	if p.config.Backend != "s3" {
		t.Errorf("expected backend 's3', got %q", p.config.Backend)
	}
	if p.backend == nil {
		t.Error("expected backend to be set")
	}
}

// TestPluginInitS3BackendUseIAMCredentials is the opt-out path's own
// construction test: with use_iam_credentials true, newS3Backend must build
// a client from the EnvAWS/IAM chain rather than reaching for
// p.deploymentSecrets -- env.DeploymentSecrets is left nil here, so a client
// that mistakenly went through the deployment-secrets path would still
// construct without error (neither path makes an HTTP call at construction
// time -- see the comment above), but this pins the wiring textually rather
// than relying on that absence of a crash to mean anything.
func TestPluginInitS3BackendUseIAMCredentials(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{}
	ctx := context.Background()

	env := &plugin.Environment{
		DB:     &engine.SQLDBAdapter{DB: db},
		Mux:    http.NewServeMux(),
		Logger: slog.Default(),
		Config: []byte(`{"backend":"s3","bucket":"test-bucket","region":"us-east-1","endpoint":"localhost:9000","use_iam_credentials":true}`),
	}

	if err := p.Init(ctx, env); err != nil {
		t.Fatalf("Init with use_iam_credentials: %v", err)
	}
	if !p.config.UseIAMCredentials {
		t.Error("expected UseIAMCredentials to be true")
	}
	if p.backend == nil {
		t.Error("expected backend to be set")
	}
}

// TestBlobstoreInitWarnsOnLeftoverKeys is the mirror of scheduledbackup's
// TestSB_InitWarnsOnLeftoverDSN: access_key_id/secret_access_key left over
// in --plugin-config from before cleat#1992 part 1b no longer do anything --
// Config has no field for either -- so Init must WARN naming the dead
// fields and the replacement commands, rather than silently ignoring them.
func TestBlobstoreInitWarnsOnLeftoverKeys(t *testing.T) {
	var buf bytes.Buffer
	p := &Plugin{}
	env := &plugin.Environment{
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
		Config: []byte(`{"access_key_id":"AKIAOLD","secret_access_key":"old-secret"}`),
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "access_key_id") || !strings.Contains(got, "secret_access_key") {
		t.Errorf("expected a WARN naming both dead fields, got log output: %q", got)
	}
	if !strings.Contains(got, "no longer read") {
		t.Errorf("expected the WARN to say the fields are no longer read, got: %q", got)
	}
	if !strings.Contains(got, "set-deployment-secret") {
		t.Errorf("expected the WARN to name the replacement command, got: %q", got)
	}
}

// TestBlobstoreInitWarnsOnOneLeftoverKey proves the WARN fires on EITHER
// field alone, not only when both are present -- an operator could have
// migrated one and not the other.
func TestBlobstoreInitWarnsOnOneLeftoverKey(t *testing.T) {
	var buf bytes.Buffer
	p := &Plugin{}
	env := &plugin.Environment{
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
		Config: []byte(`{"secret_access_key":"old-secret"}`),
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "no longer read") {
		t.Errorf("expected a WARN with only secret_access_key set, got: %q", got)
	}
}

// TestBlobstoreInitNoWarnWithoutLeftoverKeys is the negative control: a
// config with neither field at all must not log the leftover-key WARN.
func TestBlobstoreInitNoWarnWithoutLeftoverKeys(t *testing.T) {
	var buf bytes.Buffer
	p := &Plugin{}
	env := &plugin.Environment{
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
		Config: []byte(`{"backend":"memory"}`),
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if got := buf.String(); strings.Contains(got, "no longer read") {
		t.Errorf("did not expect a leftover-key WARN with neither field in config, got: %q", got)
	}
}

// TestBlobstoreRequiredDeploymentSecrets_S3RequiresBothUnconditionally is
// the case that used to read the opposite way and was wrong: an s3 backend
// not using use_iam_credentials must require BOTH
// blobstore.access_key_id/blobstore.secret_access_key even with no legacy
// access_key_id/secret_access_key anywhere in --plugin-config. On develop
// this exact config -- {"backend":"s3"}, no static keys -- fell back
// silently to the AWS env/instance-profile credential chain, so it is the
// COMMON case, not an edge case: every IAM-role s3 deployment looks like
// this. Requiring the secrets regardless of legacy-key presence is what
// stops such a deployment from booting green and then failing every S3
// call at request time (deploymentSecretsCredentialsProvider has no
// fallback to that chain).
func TestBlobstoreRequiredDeploymentSecrets_S3RequiresBothUnconditionally(t *testing.T) {
	p := &Plugin{}
	cfg := []byte(`{"backend":"s3","bucket":"b","region":"us-east-1"}`)
	if err := p.Init(context.Background(), &plugin.Environment{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: cfg,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	names, err := p.RequiredDeploymentSecrets(cfg)
	if err != nil {
		t.Fatalf("RequiredDeploymentSecrets: %v", err)
	}
	want := []string{"blobstore.access_key_id", "blobstore.secret_access_key"}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] {
		t.Errorf("RequiredDeploymentSecrets(s3, no legacy key) = %v, want %v", names, want)
	}
}

// TestBlobstoreRequiredDeploymentSecrets_LegacyKeyPresenceDoesNotChangeOutcome
// proves legacy-key presence is no longer the deciding factor: the SAME s3
// config plus a leftover access_key_id/secret_access_key pair gets the SAME
// answer as TestBlobstoreRequiredDeploymentSecrets_S3RequiresBothUnconditionally.
// This is what remains of the old _LegacyKeyPresent_S3 test, which used to be
// the ONLY case that required the two names -- that made it look like the
// interesting case, when the interesting case was the one above it did not
// cover.
func TestBlobstoreRequiredDeploymentSecrets_LegacyKeyPresenceDoesNotChangeOutcome(t *testing.T) {
	p := &Plugin{}
	cfg := []byte(`{"backend":"s3","bucket":"b","region":"us-east-1","access_key_id":"AKIAOLD","secret_access_key":"old-secret"}`)
	if err := p.Init(context.Background(), &plugin.Environment{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: cfg,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	names, err := p.RequiredDeploymentSecrets(cfg)
	if err != nil {
		t.Fatalf("RequiredDeploymentSecrets: %v", err)
	}
	want := []string{"blobstore.access_key_id", "blobstore.secret_access_key"}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] {
		t.Errorf("RequiredDeploymentSecrets(s3, legacy key present) = %v, want %v", names, want)
	}
}

// TestBlobstoreRequiredDeploymentSecrets_MemoryBackend is the exclusion
// guard for the DEFAULT (memory) backend: it must not require anything,
// legacy key or not, because the memory backend never reads either secret.
func TestBlobstoreRequiredDeploymentSecrets_MemoryBackend(t *testing.T) {
	p := &Plugin{}
	cfg := []byte(`{"access_key_id":"AKIAOLD","secret_access_key":"old-secret"}`)
	if err := p.Init(context.Background(), &plugin.Environment{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: cfg,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	names, err := p.RequiredDeploymentSecrets(cfg)
	if err != nil {
		t.Fatalf("RequiredDeploymentSecrets: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("RequiredDeploymentSecrets(memory backend) = %v, want none", names)
	}
}

// TestBlobstoreRequiredDeploymentSecrets_UseIAMCredentials is the exclusion
// guard for use_iam_credentials: that flag opts the deployment out of the
// deployment-secrets table entirely, in favor of the EnvAWS/IAM chain, so
// blobstore.access_key_id/blobstore.secret_access_key are never read
// regardless of what --plugin-config carries.
func TestBlobstoreRequiredDeploymentSecrets_UseIAMCredentials(t *testing.T) {
	p := &Plugin{}
	cfg := []byte(`{"backend":"s3","bucket":"b","region":"us-east-1","use_iam_credentials":true,"access_key_id":"AKIAOLD","secret_access_key":"old-secret"}`)
	if err := p.Init(context.Background(), &plugin.Environment{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: cfg,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	names, err := p.RequiredDeploymentSecrets(cfg)
	if err != nil {
		t.Fatalf("RequiredDeploymentSecrets: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("RequiredDeploymentSecrets(use_iam_credentials) = %v, want none", names)
	}
}

// ---------------------------------------------------------------------------
// Route handler backend error paths
// ---------------------------------------------------------------------------

func TestHandlePutBackendError(t *testing.T) {
	p, handler, _, _ := setupTestPlugin(t)

	// Replace backend with a failing one so that backend.Put fails.
	p.backend = &failBackend{}

	req := authedRequest("PUT", "/blobs/test-key", bytes.NewReader([]byte("data")))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for backend error, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "failed to store content") {
		t.Errorf("expected 'failed to store content', got: %s", rec.Body.String())
	}
}

func TestHandleGetBackendError(t *testing.T) {
	p, handler, _, _ := setupTestPlugin(t)

	// PUT a blob first with the working backend.
	req := authedRequest("PUT", "/blobs/test-key", bytes.NewReader([]byte("data")))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d", rec.Code)
	}

	// Swap backend to failBackend so that backend.Get fails.
	p.backend = &failBackend{}

	// GET should fail with 500.
	req = authedRequest("GET", "/blobs/test-key", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for backend error, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "failed to retrieve blob data") {
		t.Errorf("expected 'failed to retrieve blob data', got: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Route handler DB error paths
// ---------------------------------------------------------------------------

func TestHandlePutDBExecError(t *testing.T) {
	// Fail on blob_content INSERT.
	_, handler, _, _ := setupSelectiveErrorDB(t,
		[]string{"INSERT INTO blob_content"},
		nil,
	)

	req := authedRequest("PUT", "/blobs/test-key", bytes.NewReader([]byte("data")))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for DB exec error, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "failed to store content") {
		t.Errorf("expected 'failed to store content', got: %s", rec.Body.String())
	}
}

func TestHandleGetDBQueryError(t *testing.T) {
	// Fail on blob metadata SELECT.
	_, handler, _, _ := setupSelectiveErrorDB(t,
		nil,
		[]string{"SELECT c.sha256, i.content_type, i.size, i.expires_at"},
	)

	req := authedRequest("GET", "/blobs/test-key", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for DB query error, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "failed to retrieve blob") {
		t.Errorf("expected 'failed to retrieve blob', got: %s", rec.Body.String())
	}
}

func TestHandleHeadDBQueryError(t *testing.T) {
	// Fail on blob metadata SELECT.
	_, handler, _, _ := setupSelectiveErrorDB(t,
		nil,
		[]string{"SELECT c.sha256, i.content_type, i.size, i.expires_at"},
	)

	req := authedRequest("HEAD", "/blobs/test-key", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for DB query error, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "failed to retrieve metadata") {
		t.Errorf("expected 'failed to retrieve metadata', got: %s", rec.Body.String())
	}
}

func TestHandleDeleteDBExecError(t *testing.T) {
	// Fail on blob_index UPDATE (soft delete).
	_, handler, store, _ := setupSelectiveErrorDB(t,
		[]string{"UPDATE blob_index SET deleted_at"},
		nil,
	)

	// Manually insert a blob so the delete has something to act on.
	hash := sha256.Sum256([]byte("data"))
	store.mu.Lock()
	blobIdxKey := indexKey(testTenantStr, "del-key")
	store.blobIndex[blobIdxKey] = &fiRow{
		key:         "del-key",
		tenantID:    testTenantStr,
		sha256Bytes: hash[:],
		size:        4,
		contentType: "application/octet-stream",
		tags:        "{}",
		createdAt:   time.Now(),
	}
	store.mu.Unlock()

	req := authedRequest("DELETE", "/blobs/del-key", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for DB exec error, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "failed to delete blob") {
		t.Errorf("expected 'failed to delete blob', got: %s", rec.Body.String())
	}
}

func TestHandleListDBQueryError(t *testing.T) {
	// Fail on list SELECT.
	_, handler, _, _ := setupSelectiveErrorDB(t,
		nil,
		[]string{"SELECT i.key, i.sha256, i.size"},
	)

	req := authedRequest("GET", "/blobs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for DB query error, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "failed to list blobs") {
		t.Errorf("expected 'failed to list blobs', got: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Host function backend error paths
// ---------------------------------------------------------------------------

func TestBlobPutBackendError(t *testing.T) {
	p := &Plugin{
		backend: &failBackend{},
		logger:  slog.Default(),
		config:  Config{Backend: "memory"},
	}
	ctx := hostFuncContext(context.Background(), testTenantID, "")

	input := blobPutInput{
		Key:  "fail-key",
		Data: []byte("data"),
	}
	inputJSON, _ := json.Marshal(input)

	_, err := p.blobPut(ctx, string(inputJSON))
	if err == nil {
		t.Fatal("expected error for backend failure")
	}
	if !strings.Contains(err.Error(), "store content") {
		t.Errorf("expected 'store content' error, got: %v", err)
	}
}

func TestBlobGetBackendError(t *testing.T) {
	p, store, _ := setupHostFuncTest(t)
	ctx := hostFuncContext(context.Background(), testTenantID, "")

	// Put a blob with the working setup first.
	input := blobPutInput{
		Key:  "get-fail-key",
		Data: []byte("data"),
	}
	inputJSON, _ := json.Marshal(input)
	_, err := p.blobPut(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("blobPut: %v", err)
	}

	// Replace backend with failBackend after the put.
	p.backend = &failBackend{}

	// Get should fail because backend.Get returns error.
	_, err = p.blobGet(ctx, string(inputJSON))
	if err == nil {
		t.Fatal("expected error for backend failure")
	}
	if !strings.Contains(err.Error(), "get data") {
		t.Errorf("expected 'get data' error, got: %v", err)
	}

	_ = store
}

// TestBlobPutDeploymentSecretsUnavailableIsGeneric is the coordinator/
// cleat-review tenant-error-leak fix: a backend error carrying
// errDeploymentSecretsUnavailable must reach the tenant's workflow as the
// GENERIC blobstoreDeploymentSecretsUnavailableMessage, not the underlying
// "deployment secrets unavailable: ..." text -- which names
// deployment-secret plumbing that is none of the tenant's business and
// nothing they can act on.
func TestBlobPutDeploymentSecretsUnavailableIsGeneric(t *testing.T) {
	p := &Plugin{
		backend: &deploymentSecretsUnavailableBackend{},
		logger:  slog.Default(),
		config:  Config{Backend: "s3"},
	}
	ctx := hostFuncContext(context.Background(), testTenantID, "")

	input := blobPutInput{Key: "leak-key", Data: []byte("data")}
	inputJSON, _ := json.Marshal(input)

	_, err := p.blobPut(ctx, string(inputJSON))
	if err == nil {
		t.Fatal("expected an error")
	}
	if err.Error() != blobstoreDeploymentSecretsUnavailableMessage {
		t.Errorf("got %q, want the generic message %q -- the tenant-facing error leaked deployment-secret plumbing",
			err.Error(), blobstoreDeploymentSecretsUnavailableMessage)
	}
}

// TestBlobGetDeploymentSecretsUnavailableIsGeneric is
// TestBlobPutDeploymentSecretsUnavailableIsGeneric's Get counterpart.
func TestBlobGetDeploymentSecretsUnavailableIsGeneric(t *testing.T) {
	p, store, _ := setupHostFuncTest(t)
	ctx := hostFuncContext(context.Background(), testTenantID, "")

	input := blobPutInput{Key: "leak-get-key", Data: []byte("data")}
	inputJSON, _ := json.Marshal(input)
	if _, err := p.blobPut(ctx, string(inputJSON)); err != nil {
		t.Fatalf("blobPut: %v", err)
	}

	p.backend = &deploymentSecretsUnavailableBackend{}

	getInput := blobGetInput{Key: "leak-get-key"}
	getInputJSON, _ := json.Marshal(getInput)
	_, err := p.blobGet(ctx, string(getInputJSON))
	if err == nil {
		t.Fatal("expected an error")
	}
	if err.Error() != blobstoreDeploymentSecretsUnavailableMessage {
		t.Errorf("got %q, want the generic message %q -- the tenant-facing error leaked deployment-secret plumbing",
			err.Error(), blobstoreDeploymentSecretsUnavailableMessage)
	}

	_ = store
}

// TestBlobPutBackendErrorMessageIsNotGeneric is the negative control for
// TestBlobPutDeploymentSecretsUnavailableIsGeneric: an ORDINARY backend
// error -- no errDeploymentSecretsUnavailable in its chain -- must NOT be
// replaced by the generic message, or every backend failure would read the
// same and an operator debugging a real S3 outage would lose the detail
// TestHandlePutBackendError already asserts on ("failed to store content").
func TestBlobPutBackendErrorMessageIsNotGeneric(t *testing.T) {
	p := &Plugin{
		backend: &failBackend{},
		logger:  slog.Default(),
		config:  Config{Backend: "memory"},
	}
	ctx := hostFuncContext(context.Background(), testTenantID, "")

	input := blobPutInput{Key: "ordinary-fail-key", Data: []byte("data")}
	inputJSON, _ := json.Marshal(input)

	_, err := p.blobPut(ctx, string(inputJSON))
	if err == nil {
		t.Fatal("expected an error")
	}
	if err.Error() == blobstoreDeploymentSecretsUnavailableMessage {
		t.Error("an ordinary backend error must not be replaced by the deployment-secrets generic message")
	}
	if !strings.Contains(err.Error(), "store content") {
		t.Errorf("expected 'store content' error, got: %v", err)
	}
}

// TestBlobPutSentinelWrapReachesGenericMessageThroughRealProvider is
// TestBlobPutDeploymentSecretsUnavailableIsGeneric's real-path counterpart --
// cleat-review's finding was that the fake-backend tests above leave the
// %w errDeploymentSecretsUnavailable wrap in RetrieveWithCredContext
// (backend.go) unpinned: removing it would leave every test in this file
// green, because none of them go through the real provider.
//
// This one does: newS3Backend builds a REAL s3Backend around a REAL
// deploymentSecretsCredentialsProvider wired to a fakeBlobstoreSecrets that
// fails every Get, so p.blobPut here drives minio-go's PutObject ->
// RetrieveWithCredContext's actual %w wrap -> blobPut's errors.Is check,
// with no fake standing in for any of those three. No mock HTTP transport
// is needed: credential retrieval fails before minio-go signs or sends
// anything, so this never reaches the network.
func TestBlobPutSentinelWrapReachesGenericMessageThroughRealProvider(t *testing.T) {
	backend, err := newS3Backend(context.Background(),
		Config{Backend: "s3", Bucket: "test-bucket", Region: "us-east-1"},
		&fakeBlobstoreSecrets{fail: true})
	if err != nil {
		t.Fatalf("newS3Backend: %v", err)
	}

	p := &Plugin{
		backend: backend,
		logger:  slog.Default(),
		config:  Config{Backend: "s3"},
	}
	ctx := hostFuncContext(context.Background(), testTenantID, "")

	input := blobPutInput{Key: "real-provider-put-key", Data: []byte("data")}
	inputJSON, _ := json.Marshal(input)

	_, err = p.blobPut(ctx, string(inputJSON))
	if err == nil {
		t.Fatal("expected an error")
	}
	if err.Error() != blobstoreDeploymentSecretsUnavailableMessage {
		t.Errorf("got %q, want the generic message %q -- either RetrieveWithCredContext's sentinel wrap or blobPut's errors.Is check is broken",
			err.Error(), blobstoreDeploymentSecretsUnavailableMessage)
	}
}

// TestBlobGetSentinelWrapReachesGenericMessageThroughRealProvider is
// TestBlobPutSentinelWrapReachesGenericMessageThroughRealProvider's Get
// counterpart, and blobGet's real-path analogue of
// TestBlobGetDeploymentSecretsUnavailableIsGeneric.
func TestBlobGetSentinelWrapReachesGenericMessageThroughRealProvider(t *testing.T) {
	p, store, _ := setupHostFuncTest(t)
	ctx := hostFuncContext(context.Background(), testTenantID, "")

	// Seed a real blob_index/blob_content row via the memory backend --
	// blobGet looks the key up in the database before ever calling
	// p.backend.Get, so the S3 credential failure below is the only thing
	// under test.
	input := blobPutInput{Key: "real-provider-get-key", Data: []byte("data")}
	inputJSON, _ := json.Marshal(input)
	if _, err := p.blobPut(ctx, string(inputJSON)); err != nil {
		t.Fatalf("blobPut: %v", err)
	}

	backend, err := newS3Backend(context.Background(),
		Config{Backend: "s3", Bucket: "test-bucket", Region: "us-east-1"},
		&fakeBlobstoreSecrets{fail: true})
	if err != nil {
		t.Fatalf("newS3Backend: %v", err)
	}
	p.backend = backend

	getInput := blobGetInput{Key: "real-provider-get-key"}
	getInputJSON, _ := json.Marshal(getInput)
	_, err = p.blobGet(ctx, string(getInputJSON))
	if err == nil {
		t.Fatal("expected an error")
	}
	if err.Error() != blobstoreDeploymentSecretsUnavailableMessage {
		t.Errorf("got %q, want the generic message %q -- either RetrieveWithCredContext's sentinel wrap or blobGet's errors.Is check is broken",
			err.Error(), blobstoreDeploymentSecretsUnavailableMessage)
	}

	_ = store
}

// ---------------------------------------------------------------------------
// Tag handling
// ---------------------------------------------------------------------------

func TestHandlePutWithTags(t *testing.T) {
	_, handler, store, _ := setupTestPlugin(t)

	body := "tagged content"
	req := authedRequest("PUT", "/blobs/tagged-key?tag=env:prod&tag=owner:team-a", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT with tags: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify tags are stored.
	store.mu.RLock()
	row, ok := store.blobIndex[indexKey(testTenantStr, "tagged-key")]
	store.mu.RUnlock()
	if !ok {
		t.Fatal("expected blob_index entry")
	}
	if !strings.Contains(row.tags, "env") || !strings.Contains(row.tags, "owner") {
		t.Errorf("expected tags to include env and owner, got: %s", row.tags)
	}
}

func TestHandleListWithTagFilter(t *testing.T) {
	_, handler, _, _ := setupTestPlugin(t)

	// The fake DB does not filter on tags, but this still exercises the
	// code path in handleList where tag filter SQL is constructed.
	req := authedRequest("GET", "/blobs?tag=env:prod", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("LIST with tag: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// Verify the response is a valid JSON array.
	var results []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &results); err != nil {
		t.Fatalf("LIST decode: %v", err)
	}
}

func TestHandleListInvalidLimit(t *testing.T) {
	_, handler, _, _ := setupTestPlugin(t)

	// Invalid limit should fall back to default limit of 50.
	req := authedRequest("GET", "/blobs?limit=invalid", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("LIST with invalid limit: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var results []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &results); err != nil {
		t.Fatalf("LIST decode: %v", err)
	}
}

// ---------------------------------------------------------------------------
// HEAD and GET with expired blobs (via controllable clock)
// ---------------------------------------------------------------------------

func TestHandleHeadExpiredBlob(t *testing.T) {
	_, handler, _, clock := setupTestPlugin(t)

	// PUT with a short TTL.
	body := "expiring"
	req := authedRequest("PUT", "/blobs/head-ttl?ttl=1s", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// HEAD before expiry should succeed.
	req = authedRequest("HEAD", "/blobs/head-ttl", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD before TTL: expected 200, got %d", rec.Code)
	}

	// Advance clock past TTL.
	clock.Advance(2 * time.Second)

	// HEAD after TTL should return 404.
	req = authedRequest("HEAD", "/blobs/head-ttl", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("HEAD after TTL: expected 404, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// setupSelectiveErrorDB helper
// ---------------------------------------------------------------------------

// setupSelectiveErrorDB is like setupTestPlugin but with selective error
// injection on the DB connection. Auth middleware still works because the
// tenant lookup query does not match the configured failure patterns.
func setupSelectiveErrorDB(t *testing.T, failExecPatterns, failQueryPatterns []string) (*Plugin, http.Handler, *fakeDBStore, *fakeClock) {
	t.Helper()

	clock := newFakeClock()
	store := newFakeDBStore()
	store.now = clock.Now

	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	errConn := &selectiveErrorConnector{
		store:             store,
		failExecPatterns:  failExecPatterns,
		failQueryPatterns: failQueryPatterns,
	}
	errDB := sql.OpenDB(errConn)
	t.Cleanup(func() { errDB.Close() })

	p := &Plugin{
		db:      &engine.SQLDBAdapter{DB: errDB},
		backend: newTestMemBackend(),
		logger:  slog.Default(),
		config:  Config{Backend: "memory"},
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	handler := auth.Middleware(engine.NewPostgresStore(errDB), false)(mux)
	return p, handler, store, clock
}
