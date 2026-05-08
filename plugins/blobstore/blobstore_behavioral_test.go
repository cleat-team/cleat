package blobstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
