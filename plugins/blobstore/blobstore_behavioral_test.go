package blobstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
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

	if len(migrations) == 0 {
		t.Fatal("expected at least one migration")
	}

	for i, m := range migrations {
		t.Run(fmt.Sprintf("migration-%d", m.Version), func(t *testing.T) {
			if m.Version <= 0 {
				t.Errorf("migration[%d] has non-positive version %d", i, m.Version)
			}
			if strings.TrimSpace(m.Up) == "" {
				t.Errorf("migration[%d] (v%d) has empty Up SQL", i, m.Version)
			}
			if strings.TrimSpace(m.Down) == "" {
				t.Errorf("migration[%d] (v%d) has empty Down SQL", i, m.Version)
			}
		})
	}

	// Verify versions are sequential and strictly increasing.
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
		up := m.Up
		if !strings.Contains(up, "CREATE") && !strings.Contains(up, "ALTER") && !strings.Contains(up, "INSERT") {
			t.Errorf("migration v%d Up SQL does not contain CREATE, ALTER, or INSERT: %s", m.Version, up[:min(len(up), 80)])
		}
	}
}

func TestMigrationDownContainsSQL(t *testing.T) {
	p := &Plugin{}
	for _, m := range p.Migrations() {
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
	var results []map[string]interface{}
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

	var results []map[string]interface{}
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

	var resp map[string]interface{}
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

// selectiveErrorConn wraps a fakeConn and injects errors for SQL statements
// matching the configured patterns; all other queries pass through.
type selectiveErrorConn struct {
	*fakeConn
	failExecPatterns   []string
	failQueryPatterns  []string
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
	var results []map[string]interface{}
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
	var results []map[string]interface{}
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
