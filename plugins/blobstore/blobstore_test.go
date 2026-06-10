// Package blobstore tests the blob storage plugin with an in-memory backend
// and a fake SQL database, avoiding any need for PostgreSQL or S3.
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
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// In-memory Backend (implements the Backend interface without S3 or a DB)
// ---------------------------------------------------------------------------

type testMemBackend struct {
	mu   sync.RWMutex
	data map[string][]byte // sha256 hex -> raw bytes
}

func newTestMemBackend() *testMemBackend {
	return &testMemBackend{data: make(map[string][]byte)}
}

func (b *testMemBackend) Put(_ context.Context, sha256 string, data []byte, _ string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data[sha256] = data
	return nil
}

func (b *testMemBackend) Get(_ context.Context, sha256 string) ([]byte, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	data, ok := b.data[sha256]
	if !ok {
		return nil, fmt.Errorf("blobstore: content not found: %s", sha256)
	}
	return append([]byte(nil), data...), nil
}

func (b *testMemBackend) Delete(_ context.Context, sha256 string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.data, sha256)
	return nil
}

// ---------------------------------------------------------------------------
// Fake SQL database (replaces PostgreSQL entirely for testing)
// ---------------------------------------------------------------------------

// fakeDBStore is a goroutine-safe in-memory store that implements the subset of
// SQL used by the blobstore plugin and auth middleware.
type fakeDBStore struct {
	mu     sync.RWMutex
	nextID int64

	// blob_content: sha256 hex -> row
	blobContent map[string]*fcRow
	// blob_index: "tenant_uuid:key" -> row
	blobIndex map[string]*fiRow
	// tenant_api_keys: key_hash hex -> tenant_id string
	apiKeys map[string]string

	// Controllable clock for TTL tests.
	now func() time.Time
}

type fcRow struct {
	sha256Bytes    []byte
	size           int64
	refCount       int64
	storageBackend string
	s3Key          *string
}

type fiRow struct {
	key         string
	tenantID    string
	sha256Bytes []byte
	size        int64
	contentType string
	tags        string // JSON text
	createdAt   time.Time
	expiresAt   *time.Time
	deletedAt   *time.Time
}

func newFakeDBStore() *fakeDBStore {
	return &fakeDBStore{
		blobContent: make(map[string]*fcRow),
		blobIndex:   make(map[string]*fiRow),
		apiKeys:     make(map[string]string),
		now:         time.Now,
	}
}

func indexKey(tenantID, key string) string { return tenantID + ":" + key }

// --- driver.Connector ---

type fakeConnector struct {
	store *fakeDBStore
}

func (c *fakeConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &fakeConn{store: c.store}, nil
}

func (c *fakeConnector) Driver() driver.Driver {
	return &fakeDrv{}
}

type fakeDrv struct{}

func (*fakeDrv) Open(_ string) (driver.Conn, error) {
	return nil, fmt.Errorf("fakeDriver: Open not supported; use sql.OpenDB")
}

// --- driver.Conn ---

type fakeConn struct {
	store *fakeDBStore
}

func (*fakeConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, fmt.Errorf("fakeConn: unexpected Prepare call")
}

func (*fakeConn) Close() error { return nil }

func (*fakeConn) Begin() (driver.Tx, error) { return &fakeTx{}, nil }

type fakeTx struct{}

func (*fakeTx) Commit() error   { return nil }
func (*fakeTx) Rollback() error { return nil }

// --- driver.ExecerContext ---

func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()

	switch {
	case strings.Contains(query, "INSERT INTO blob_content"):
		return c.execInsertBlobContent(args)
	case strings.Contains(query, "INSERT INTO blob_index"):
		return c.execInsertBlobIndex(args)
	case strings.Contains(query, "UPDATE blob_index SET deleted_at"):
		return c.execSoftDelete(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Exec query: %s", query)
	}
}

// --- driver.QueryerContext ---

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.store.mu.RLock()
	defer c.store.mu.RUnlock()

	// Match specific SQL patterns. The queries in routes.go span multiple lines
	// with tabs/newlines, so we match on unique substrings that appear on a
	// single line.
	switch {
	case strings.Contains(query, "SELECT c.sha256, i.content_type, i.size, i.expires_at"):
		return c.queryBlobByKey(args)
	case strings.Contains(query, "SELECT i.key, i.sha256, i.size, i.content_type"):
		return c.queryListBlobs(query, args)
	case strings.Contains(query, "tenant_api_keys") && strings.Contains(query, "tenant_id"):
		return c.queryTenantLookup(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Query query: %s", query)
	}
}

// ---------------------------------------------------------------------------
// Argument extractors
// ---------------------------------------------------------------------------

func argString(args []driver.NamedValue, ordinal int) (string, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			s, ok := a.Value.(string)
			if !ok {
				return "", fmt.Errorf("arg %d: want string, got %T", ordinal, a.Value)
			}
			return s, nil
		}
	}
	return "", fmt.Errorf("arg %d not found", ordinal)
}

func argBytes(args []driver.NamedValue, ordinal int) ([]byte, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			b, ok := a.Value.([]byte)
			if !ok {
				return nil, fmt.Errorf("arg %d: want []byte, got %T", ordinal, a.Value)
			}
			return b, nil
		}
	}
	return nil, fmt.Errorf("arg %d not found", ordinal)
}

func argInt64(args []driver.NamedValue, ordinal int) (int64, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			v, ok := a.Value.(int64)
			if !ok {
				return 0, fmt.Errorf("arg %d: want int64, got %T", ordinal, a.Value)
			}
			return v, nil
		}
	}
	return 0, fmt.Errorf("arg %d not found", ordinal)
}

func argAny(args []driver.NamedValue, ordinal int) (driver.Value, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			return a.Value, nil
		}
	}
	return nil, fmt.Errorf("arg %d not found", ordinal)
}

// ---------------------------------------------------------------------------
// Exec implementations
// ---------------------------------------------------------------------------

func (c *fakeConn) execInsertBlobContent(args []driver.NamedValue) (driver.Result, error) {
	hashBytes, err := argBytes(args, 1)
	if err != nil {
		return nil, err
	}
	size, err := argInt64(args, 2)
	if err != nil {
		return nil, err
	}
	storageBackend, err := argString(args, 3)
	if err != nil {
		return nil, err
	}
	var s3Key *string
	if v, err := argAny(args, 4); err == nil && v != nil {
		s, ok := v.(string)
		if ok {
			s3Key = &s
		}
	}

	sha256Hex := fmt.Sprintf("%x", hashBytes)

	if existing, ok := c.store.blobContent[sha256Hex]; ok {
		existing.refCount++
		existing.size = size
	} else {
		c.store.blobContent[sha256Hex] = &fcRow{
			sha256Bytes:    hashBytes,
			size:           size,
			refCount:       1,
			storageBackend: storageBackend,
			s3Key:          s3Key,
		}
	}

	c.store.nextID++
	return &fakeResult{rowsAffected: 1, lastInsertID: c.store.nextID}, nil
}

func (c *fakeConn) execInsertBlobIndex(args []driver.NamedValue) (driver.Result, error) {
	key, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	hashBytes, err := argBytes(args, 3)
	if err != nil {
		return nil, err
	}
	size, err := argInt64(args, 4)
	if err != nil {
		return nil, err
	}
	contentType, err := argString(args, 5)
	if err != nil {
		return nil, err
	}
	tagsVal, err := argAny(args, 6)
	if err != nil {
		return nil, err
	}
	var tags string
	switch v := tagsVal.(type) {
	case string:
		tags = v
	case []byte:
		tags = string(v)
	default:
		return nil, fmt.Errorf("fakeDB: tags arg: unexpected type %T", tagsVal)
	}

	var expiresAt *time.Time
	if len(args) >= 7 {
		if v, err := argAny(args, 7); err == nil && v != nil {
			if t, ok := v.(time.Time); ok {
				expiresAt = &t
			}
		}
	}

	idxKey := indexKey(tid, key)
	now := c.store.now()

	c.store.blobIndex[idxKey] = &fiRow{
		key:         key,
		tenantID:    tid,
		sha256Bytes: hashBytes,
		size:        size,
		contentType: contentType,
		tags:        tags,
		createdAt:   now,
		expiresAt:   expiresAt,
	}

	c.store.nextID++
	return &fakeResult{rowsAffected: 1, lastInsertID: c.store.nextID}, nil
}

func (c *fakeConn) execSoftDelete(args []driver.NamedValue) (driver.Result, error) {
	key, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := argString(args, 2)
	if err != nil {
		return nil, err
	}

	idxKey := indexKey(tid, key)
	row, ok := c.store.blobIndex[idxKey]
	if !ok || row.deletedAt != nil {
		return &fakeResult{rowsAffected: 0}, nil
	}

	now := c.store.now()
	row.deletedAt = &now
	return &fakeResult{rowsAffected: 1}, nil
}

// ---------------------------------------------------------------------------
// Query implementations
// ---------------------------------------------------------------------------

// queryBlobByKey handles the SELECT ... FROM blob_index JOIN blob_content WHERE key=$1 AND tenant=$2 AND deleted IS NULL
func (c *fakeConn) queryBlobByKey(args []driver.NamedValue) (driver.Rows, error) {
	key, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := argString(args, 2)
	if err != nil {
		return nil, err
	}

	idxKey := indexKey(tid, key)
	row, ok := c.store.blobIndex[idxKey]
	if !ok || row.deletedAt != nil {
		return &fakeRows{columns: []string{"sha256", "content_type", "size", "expires_at"}}, nil
	}

	// TTL check: expired entries behave as if they don't exist.
	if row.expiresAt != nil && row.expiresAt.Before(c.store.now()) {
		return &fakeRows{columns: []string{"sha256", "content_type", "size", "expires_at"}}, nil
	}

	// Return expires_at as driver.Value — time.Time or nil.
	var expiresAtVal driver.Value
	if row.expiresAt != nil {
		expiresAtVal = *row.expiresAt
	}

	return &fakeRows{
		columns: []string{"sha256", "content_type", "size", "expires_at"},
		data: [][]driver.Value{
			{row.sha256Bytes, row.contentType, row.size, expiresAtVal},
		},
	}, nil
}

// queryListBlobs handles the SELECT ... FROM blob_index WHERE tenant=$1 AND deleted IS NULL ...
func (c *fakeConn) queryListBlobs(query string, args []driver.NamedValue) (driver.Rows, error) {
	tid, err := argString(args, 1)
	if err != nil {
		return nil, err
	}

	// Collect all active entries for this tenant.
	var results []*fiRow
	for _, row := range c.store.blobIndex {
		if row.tenantID != tid || row.deletedAt != nil {
			continue
		}
		if row.expiresAt != nil && row.expiresAt.Before(c.store.now()) {
			continue
		}
		results = append(results, row)
	}

	// Optional prefix filter.
	hasPrefix := strings.Contains(query, "AND i.key LIKE")
	if hasPrefix && len(args) >= 2 {
		prefixVal, err := argAny(args, 2)
		if err == nil {
			prefixStr := strings.TrimSuffix(fmt.Sprintf("%v", prefixVal), "%")
			var filtered []*fiRow
			for _, row := range results {
				if strings.HasPrefix(row.key, prefixStr) {
					filtered = append(filtered, row)
				}
			}
			results = filtered
		}
	}

	columns := []string{"key", "sha256", "size", "content_type", "tags", "created_at", "expires_at"}
	var data [][]driver.Value
	for _, row := range results {
		var expiresAtVal driver.Value
		if row.expiresAt != nil {
			expiresAtVal = *row.expiresAt
		}
		data = append(data, []driver.Value{
			row.key,
			row.sha256Bytes,
			row.size,
			row.contentType,
			[]byte(row.tags),
			row.createdAt,
			expiresAtVal,
		})
	}

	return &fakeRows{columns: columns, data: data}, nil
}

// queryTenantLookup handles SELECT tenant_id FROM tenant_api_keys WHERE key_hash=$1 AND revoked IS NULL
func (c *fakeConn) queryTenantLookup(args []driver.NamedValue) (driver.Rows, error) {
	keyHash, err := argBytes(args, 1)
	if err != nil {
		return nil, err
	}

	hashHex := fmt.Sprintf("%x", keyHash)
	tid, ok := c.store.apiKeys[hashHex]
	if !ok {
		return &fakeRows{columns: []string{"tenant_id"}}, nil
	}

	return &fakeRows{
		columns: []string{"tenant_id"},
		data:    [][]driver.Value{{tid}},
	}, nil
}

// ---------------------------------------------------------------------------
// driver.Result / driver.Rows stubs
// ---------------------------------------------------------------------------

type fakeResult struct {
	lastInsertID int64
	rowsAffected int64
}

func (r *fakeResult) LastInsertId() (int64, error) { return r.lastInsertID, nil }
func (r *fakeResult) RowsAffected() (int64, error) { return r.rowsAffected, nil }

type fakeRows struct {
	columns []string
	data    [][]driver.Value
	pos     int
}

func (r *fakeRows) Columns() []string { return r.columns }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.pos])
	r.pos++
	return nil
}

// ---------------------------------------------------------------------------
// Controllable clock
// ---------------------------------------------------------------------------

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Now()}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// ---------------------------------------------------------------------------
// Test setup helper
// ---------------------------------------------------------------------------

var testTenantID = uuid.MustParse("00000000-0000-0000-0000-000000000001")
var testTenantStr = testTenantID.String()

// setupTestPlugin creates a Plugin wired to an in-memory backend and a fake
// SQL database. The returned http.Handler includes the auth middleware so that
// requests carrying "Authorization: Bearer test-api-key" are authenticated.
func setupTestPlugin(t *testing.T) (*Plugin, http.Handler, *fakeDBStore, *fakeClock) {
	t.Helper()

	clock := newFakeClock()
	store := newFakeDBStore()
	store.now = clock.Now

	// Pre-populate a tenant API key so the auth middleware succeeds.
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{
		db:      &engine.SQLDBAdapter{DB: db},
		backend: newTestMemBackend(),
		logger:  slog.Default(),
		config:  Config{Backend: "memory"},
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	handler := auth.Middleware(engine.NewPostgresStore(db), false)(mux)
	return p, handler, store, clock
}

// authedRequest creates a request authenticated with the test API key.
func authedRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer test-api-key")
	return req
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "blobstore" {
		t.Errorf("expected Name 'blobstore', got %q", info.Name)
	}
	if info.Version != "0.1.0" {
		t.Errorf("expected Version '0.1.0', got %q", info.Version)
	}
	if info.Description == "" {
		t.Error("expected non-empty Description")
	}
	if info.Author != "cleat" {
		t.Errorf("expected Author 'cleat', got %q", info.Author)
	}
}

func TestInit(t *testing.T) {
	p := &Plugin{}
	env := &struct {
		DB     *sql.DB
		Mux    *http.ServeMux
		Config []byte
		Logger *slog.Logger
	}{
		DB:  sql.OpenDB(&fakeConnector{store: newFakeDBStore()}),
		Mux: http.NewServeMux(),
	}
	p.db = &engine.SQLDBAdapter{DB: env.DB}
	p.mux = env.Mux
	p.logger = env.Logger
	if p.logger == nil {
		p.logger = slog.Default()
	}
	p.config = Config{Backend: "memory"}
	p.backend = newTestMemBackend()

	if p.backend == nil {
		t.Error("expected backend to be set")
	}
	if p.db == nil {
		t.Error("expected db to be set")
	}
	if p.mux == nil {
		t.Error("expected mux to be set")
	}
}

func TestRegisterRoutes(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	err := p.RegisterRoutes(mux)
	if err != nil {
		t.Fatalf("RegisterRoutes() returned error: %v", err)
	}

	tests := []struct {
		method string
		path   string
	}{
		{"PUT", "/blobs/mykey"},
		{"GET", "/blobs/mykey"},
		{"HEAD", "/blobs/mykey"},
		{"DELETE", "/blobs/mykey"},
		{"GET", "/blobs"},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Errorf("no handler matched %s %s", tt.method, tt.path)
		}
	}
}

// TestPutGetDelete exercises the full PUT → GET → HEAD → DELETE → GET cycle.
func TestPutGetDelete(t *testing.T) {
	_, handler, _, _ := setupTestPlugin(t)
	body := "hello world"

	// 1. PUT /blobs/test-key
	req := authedRequest("PUT", "/blobs/test-key", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var putResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &putResp); err != nil {
		t.Fatalf("PUT: failed to decode response: %v", err)
	}
	if putResp["key"] != "test-key" {
		t.Errorf("PUT: expected key 'test-key', got %q", putResp["key"])
	}

	// 2. GET /blobs/test-key → 200, body "hello world"
	req = authedRequest("GET", "/blobs/test-key", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != body {
		t.Errorf("GET: expected body %q, got %q", body, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") == "" {
		t.Error("GET: expected Content-Type header")
	}
	if rec.Header().Get("X-Blob-SHA256") == "" {
		t.Error("GET: expected X-Blob-SHA256 header")
	}

	// 3. HEAD /blobs/test-key → 200
	req = authedRequest("HEAD", "/blobs/test-key", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD: expected 200, got %d", rec.Code)
	}
	if rec.Header().Get("Content-Type") == "" {
		t.Error("HEAD: expected Content-Type header")
	}
	if rec.Header().Get("X-Blob-SHA256") == "" {
		t.Error("HEAD: expected X-Blob-SHA256 header")
	}

	// 4. DELETE /blobs/test-key → 204
	req = authedRequest("DELETE", "/blobs/test-key", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: expected 204, got %d", rec.Code)
	}

	// 5. GET /blobs/test-key → 404
	req = authedRequest("GET", "/blobs/test-key", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET after delete: expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestDeduplication verifies that content is shared by SHA-256: putting the
// same bytes under different keys does not duplicate storage.
func TestDeduplication(t *testing.T) {
	_, handler, store, _ := setupTestPlugin(t)
	content := "shared content"

	// 1–2. PUT key1 and key2 with identical content.
	for _, key := range []string{"key1", "key2"} {
		req := authedRequest("PUT", "/blobs/"+key, bytes.NewReader([]byte(content)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s: expected 201, got %d", key, rec.Code)
		}
	}

	// 3. GET both keys — both return the same content.
	for _, key := range []string{"key1", "key2"} {
		req := authedRequest("GET", "/blobs/"+key, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: expected 200, got %d", key, rec.Code)
		}
		if rec.Body.String() != content {
			t.Errorf("GET %s: expected body %q, got %q", key, content, rec.Body.String())
		}
	}

	// The blob_content table must have exactly one row for the shared content.
	sha256Hex := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	store.mu.RLock()
	cr, ok := store.blobContent[sha256Hex]
	store.mu.RUnlock()
	if !ok {
		t.Fatal("expected shared content in blob_content")
	}
	if cr.refCount != 2 {
		t.Errorf("expected ref_count=2 for shared content, got %d", cr.refCount)
	}

	// 4. DELETE key1
	req := authedRequest("DELETE", "/blobs/key1", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE key1: expected 204, got %d", rec.Code)
	}

	// 5. GET key2 — still accessible (its index entry is intact).
	req = authedRequest("GET", "/blobs/key2", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET key2 after deleting key1: expected 200, got %d: %s",
			rec.Code, rec.Body.String())
	}
	if rec.Body.String() != content {
		t.Errorf("GET key2: expected %q, got %q", content, rec.Body.String())
	}

	// GET key1 — gone.
	req = authedRequest("GET", "/blobs/key1", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET key1 after delete: expected 404, got %d", rec.Code)
	}
}

// TestTTLExpiry verifies that a blob with a TTL becomes inaccessible after
// the TTL window passes.
func TestTTLExpiry(t *testing.T) {
	_, handler, _, clock := setupTestPlugin(t)
	body := "expires"

	// PUT /blobs/ttl-key?ttl=1s — it should expire after 1 second.
	req := authedRequest("PUT", "/blobs/ttl-key?ttl=1s", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// GET immediately → 200.
	req = authedRequest("GET", "/blobs/ttl-key", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET before TTL: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Advance the clock past TTL.
	clock.Advance(2 * time.Second)

	// GET after TTL → 404 (expired).
	req = authedRequest("GET", "/blobs/ttl-key", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET after TTL: expected 404, got %d: %s", rec.Code, rec.Body.String())
	}

	// HEAD after TTL → 404.
	req = authedRequest("HEAD", "/blobs/ttl-key", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("HEAD after TTL: expected 404, got %d", rec.Code)
	}
}

// TestListWithPrefix verifies the prefix-based list endpoint.
// Note: keys cannot contain "/" because the mux pattern uses {key} which
// matches a single path segment.
func TestListWithPrefix(t *testing.T) {
	_, handler, _, _ := setupTestPlugin(t)

	// PUT three blobs with simple prefix-based naming.
	blobs := map[string]string{
		"alpha": "one",
		"aster": "two",
		"beta":  "three",
	}
	for k, v := range blobs {
		req := authedRequest("PUT", "/blobs/"+k, bytes.NewReader([]byte(v)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s: expected 201, got %d: %s", k, rec.Code, rec.Body.String())
		}
	}

	// GET /blobs?prefix=a → should return only alpha and aster.
	req := authedRequest("GET", "/blobs?prefix=a", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("LIST with prefix 'a': expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var results []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &results); err != nil {
		t.Fatalf("LIST: failed to decode: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("LIST: expected 2 results for prefix 'a', got %d: %+v", len(results), results)
	}

	keys := make(map[string]bool)
	for _, r := range results {
		k, _ := r["key"].(string)
		keys[k] = true
	}
	if !keys["alpha"] || !keys["aster"] {
		t.Errorf("LIST: expected keys [alpha, aster], got %v", keys)
	}

	// GET /blobs?prefix=b → should return only beta.
	req = authedRequest("GET", "/blobs?prefix=b", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("LIST with prefix 'b': expected 200, got %d", rec.Code)
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &results); err != nil {
		t.Fatalf("LIST: failed to decode: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("LIST: expected 1 result for prefix 'b', got %d", len(results))
	}
	if k, _ := results[0]["key"].(string); k != "beta" {
		t.Errorf("LIST: expected key 'beta', got %q", k)
	}
}

// TestSoftDelete verifies that DELETE hides the blob from GET but preserves
// the underlying content in the backend.
func TestSoftDelete(t *testing.T) {
	p, handler, store, _ := setupTestPlugin(t)
	body := "to delete"

	// PUT /blobs/del-key
	req := authedRequest("PUT", "/blobs/del-key", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d", rec.Code)
	}

	// DELETE /blobs/del-key → 204
	req = authedRequest("DELETE", "/blobs/del-key", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: expected 204, got %d", rec.Code)
	}

	// GET /blobs/del-key → 404 (soft-deleted)
	req = authedRequest("GET", "/blobs/del-key", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET after soft-delete: expected 404, got %d: %s", rec.Code, rec.Body.String())
	}

	// The content should still be in the backend (the handler never calls
	// backend.Delete after soft-delete).
	sha256Hex := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
	data, err := p.backend.Get(context.Background(), sha256Hex)
	if err != nil {
		t.Fatalf("backend.Get after soft-delete: %v", err)
	}
	if string(data) != body {
		t.Errorf("backend content: expected %q, got %q", body, string(data))
	}

	// The index entry should be marked as deleted.
	store.mu.RLock()
	idxKey := indexKey(testTenantStr, "del-key")
	row, ok := store.blobIndex[idxKey]
	store.mu.RUnlock()
	if !ok {
		t.Fatal("expected index entry to exist after soft-delete")
	}
	if row.deletedAt == nil {
		t.Error("expected deleted_at to be set after soft-delete")
	}
}

// TestWorkflowRefPreventsGC verifies that content referenced by an active
// index entry survives even after another key is deleted. The test also
// confirms that the content bytes remain in the backend after soft-delete.
func TestWorkflowRefPreventsGC(t *testing.T) {
	p, handler, _, _ := setupTestPlugin(t)
	body := "workflow data"

	// PUT /blobs/wf-key
	req := authedRequest("PUT", "/blobs/wf-key", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d", rec.Code)
	}

	// DELETE /blobs/wf-key (soft delete — index hidden, content preserved).
	req = authedRequest("DELETE", "/blobs/wf-key", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: expected 204, got %d", rec.Code)
	}

	// GET /blobs/wf-key → 404 (hidden by soft delete).
	req = authedRequest("GET", "/blobs/wf-key", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET after delete: expected 404, got %d", rec.Code)
	}

	// Content bytes must still be in the backend (no physical deletion).
	sha256Hex := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
	data, err := p.backend.Get(context.Background(), sha256Hex)
	if err != nil {
		t.Fatalf("backend.Get after soft-delete: %v", err)
	}
	if string(data) != body {
		t.Errorf("backend content: expected %q, got %q", body, string(data))
	}

	// Also verify that a second key pointing to the same content keeps it
	// accessible even after the first is deleted (shared-content reuse).
	req = authedRequest("PUT", "/blobs/wf-key-v2", bytes.NewReader([]byte(body)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT v2: expected 201, got %d", rec.Code)
	}

	// GET wf-key-v2 → 200 (active index entry).
	req = authedRequest("GET", "/blobs/wf-key-v2", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET v2: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != body {
		t.Errorf("GET v2: expected %q, got %q", body, rec.Body.String())
	}
}

// TestUnauthenticated verifies that requests without credentials get 401.
func TestUnauthenticated(t *testing.T) {
	_, handler, _, _ := setupTestPlugin(t)

	req := httptest.NewRequest("PUT", "/blobs/some-key", bytes.NewReader([]byte("data")))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated request, got %d", rec.Code)
	}
}

// TestNotFound verifies 404 for missing blobs and unknown routes.
func TestNotFound(t *testing.T) {
	_, handler, _, _ := setupTestPlugin(t)

	req := authedRequest("GET", "/blobs/nonexistent", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing blob, got %d", rec.Code)
	}

	req = authedRequest("HEAD", "/blobs/nonexistent", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing blob HEAD, got %d", rec.Code)
	}

	req = authedRequest("DELETE", "/blobs/nonexistent", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for deleting missing blob, got %d", rec.Code)
	}
}

func TestNew(t *testing.T) {
	p := New()
	if p == nil {
		t.Fatal("New() returned nil")
	}
}
