// Package kvstore tests the key-value store plugin with an in-memory fake
// database, avoiding any need for PostgreSQL.
package kvstore

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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/cleat/internal/auth"
	"github.com/rcownie/cleat/internal/plugin"
)

// ---------------------------------------------------------------------------
// In-memory KV store (replaces PostgreSQL entirely for testing)
// ---------------------------------------------------------------------------

type kvRow struct {
	tenantID  uuid.UUID
	key       string
	value     []byte
	version   int
	createdAt time.Time
	updatedAt time.Time
}

type fakeKVStore struct {
	mu      sync.RWMutex
	data    map[string]*kvRow // "tenantID:key" -> row
	apiKeys map[string]string // key_hash_hex -> tenant_id string
}

func newFakeKVStore() *fakeKVStore {
	return &fakeKVStore{
		data:    make(map[string]*kvRow),
		apiKeys: make(map[string]string),
	}
}

func kvStoreKey(tid uuid.UUID, key string) string {
	return tid.String() + ":" + key
}

// ---------------------------------------------------------------------------
// Fake SQL driver (replaces PostgreSQL entirely for testing)
// ---------------------------------------------------------------------------

type fakeConnector struct {
	store *fakeKVStore
}

func (c *fakeConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &fakeConn{store: c.store}, nil
}

func (c *fakeConnector) Driver() driver.Driver {
	return &fakeDrv{}
}

type fakeDrv struct{}

func (*fakeDrv) Open(_ string) (driver.Conn, error) {
	return nil, fmt.Errorf("fakeDriver: use sql.OpenDB")
}

type fakeConn struct {
	store *fakeKVStore
}

func (*fakeConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, fmt.Errorf("fakeConn: unexpected Prepare call")
}

func (*fakeConn) Close() error { return nil }
func (*fakeConn) Begin() (driver.Tx, error) { return &fakeTx{}, nil }

type fakeTx struct{}

func (*fakeTx) Commit() error   { return nil }
func (*fakeTx) Rollback() error { return nil }

// --- ExecContext ---

func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()

	switch {
	case strings.Contains(query, "DELETE FROM kv_store"):
		return c.execDelete(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Exec query: %s", query)
	}
}

// --- QueryContext ---

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "INSERT INTO kv_store"):
		c.store.mu.Lock()
		defer c.store.mu.Unlock()
		return c.queryUpsert(args)
	case strings.Contains(query, "UPDATE kv_store"):
		c.store.mu.Lock()
		defer c.store.mu.Unlock()
		return c.queryIfMatch(args)
	case strings.Contains(query, "tenant_api_keys"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryTenantLookup(args)
	case strings.Contains(query, "AND key = $2"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryByKey(args)
	case strings.Contains(query, "AND key LIKE"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryListWithPrefix(args)
	case strings.Contains(query, "ORDER BY key"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryList(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Query query: %s", query)
	}
}

// --- Exec implementations ---

func (c *fakeConn) execDelete(args []driver.NamedValue) (driver.Result, error) {
	tidStr, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := uuid.Parse(tidStr)
	if err != nil {
		return nil, err
	}
	key, err := argString(args, 2)
	if err != nil {
		return nil, err
	}

	sk := kvStoreKey(tid, key)
	if _, ok := c.store.data[sk]; !ok {
		return &fakeResult{rowsAffected: 0}, nil
	}
	delete(c.store.data, sk)
	return &fakeResult{rowsAffected: 1}, nil
}

// --- Query implementations ---

func (c *fakeConn) queryUpsert(args []driver.NamedValue) (driver.Rows, error) {
	tidStr, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := uuid.Parse(tidStr)
	if err != nil {
		return nil, err
	}
	key, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	value, err := argBytes(args, 3)
	if err != nil {
		return nil, err
	}

	sk := kvStoreKey(tid, key)
	now := time.Now().UTC()

	existing, ok := c.store.data[sk]
	if ok {
		existing.value = value
		existing.version++
		existing.updatedAt = now
		return &fakeRows{
			columns: []string{"version"},
			data:    [][]driver.Value{{int64(existing.version)}},
		}, nil
	}

	c.store.data[sk] = &kvRow{
		tenantID:  tid,
		key:       key,
		value:     value,
		version:   1,
		createdAt: now,
		updatedAt: now,
	}
	return &fakeRows{
		columns: []string{"version"},
		data:    [][]driver.Value{{int64(1)}},
	}, nil
}

func (c *fakeConn) queryIfMatch(args []driver.NamedValue) (driver.Rows, error) {
	value, err := argBytes(args, 1)
	if err != nil {
		return nil, err
	}
	tidStr, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	tid, err := uuid.Parse(tidStr)
	if err != nil {
		return nil, err
	}
	key, err := argString(args, 3)
	if err != nil {
		return nil, err
	}
	expectedVersion, err := argInt64(args, 4)
	if err != nil {
		return nil, err
	}

	sk := kvStoreKey(tid, key)
	existing, ok := c.store.data[sk]
	if !ok || int64(existing.version) != expectedVersion {
		return &fakeRows{columns: []string{"version"}}, nil // no rows -> ErrNoRows
	}

	existing.value = value
	existing.version++
	existing.updatedAt = time.Now().UTC()
	return &fakeRows{
		columns: []string{"version"},
		data:    [][]driver.Value{{int64(existing.version)}},
	}, nil
}

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

func (c *fakeConn) queryByKey(args []driver.NamedValue) (driver.Rows, error) {
	tidStr, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := uuid.Parse(tidStr)
	if err != nil {
		return nil, err
	}
	key, err := argString(args, 2)
	if err != nil {
		return nil, err
	}

	sk := kvStoreKey(tid, key)
	row, ok := c.store.data[sk]
	if !ok {
		return &fakeRows{columns: []string{"value", "version", "created_at", "updated_at"}}, nil
	}

	return &fakeRows{
		columns: []string{"value", "version", "created_at", "updated_at"},
		data: [][]driver.Value{
			{
				row.value,
				int64(row.version),
				row.createdAt,
				row.updatedAt,
			},
		},
	}, nil
}

func (c *fakeConn) queryListWithPrefix(args []driver.NamedValue) (driver.Rows, error) {
	tidStr, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := uuid.Parse(tidStr)
	if err != nil {
		return nil, err
	}
	prefixPattern, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimSuffix(prefixPattern, "%")

	var results []*kvRow
	for _, row := range c.store.data {
		if row.tenantID != tid {
			continue
		}
		if !strings.HasPrefix(row.key, prefix) {
			continue
		}
		results = append(results, row)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].key < results[j].key
	})

	return c.buildListRows(results)
}

func (c *fakeConn) queryList(args []driver.NamedValue) (driver.Rows, error) {
	tidStr, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := uuid.Parse(tidStr)
	if err != nil {
		return nil, err
	}

	var results []*kvRow
	for _, row := range c.store.data {
		if row.tenantID == tid {
			results = append(results, row)
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].key < results[j].key
	})

	return c.buildListRows(results)
}

func (c *fakeConn) buildListRows(rows []*kvRow) (driver.Rows, error) {
	columns := []string{"key", "value", "version", "created_at", "updated_at"}
	var data [][]driver.Value
	for _, row := range rows {
		data = append(data, []driver.Value{
			row.key,
			row.value,
			int64(row.version),
			row.createdAt,
			row.updatedAt,
		})
	}
	return &fakeRows{columns: columns, data: data}, nil
}

// ---------------------------------------------------------------------------
// Argument extractors
// ---------------------------------------------------------------------------

func argString(args []driver.NamedValue, ordinal int) (string, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			switch v := a.Value.(type) {
			case string:
				return v, nil
			case []byte:
				return string(v), nil
			default:
				return "", fmt.Errorf("arg %d: want string or []byte, got %T", ordinal, a.Value)
			}
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

// ---------------------------------------------------------------------------
// driver.Result and driver.Rows stubs
// ---------------------------------------------------------------------------

type fakeResult struct {
	rowsAffected int64
}

func (r *fakeResult) LastInsertId() (int64, error) { return 0, nil }
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
// Test setup helpers
// ---------------------------------------------------------------------------

var testTenantID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

const testAPIKey = "test-api-key"

// setupTestPlugin creates a Plugin wired to an in-memory fake database.
// The returned http.Handler includes the auth middleware so that requests
// carrying "Authorization: Bearer test-api-key" are authenticated.
func setupTestPlugin(t *testing.T) (*Plugin, http.Handler, *fakeKVStore) {
	t.Helper()

	store := newFakeKVStore()

	// Pre-populate tenant API key so auth middleware succeeds.
	keyHash := sha256.Sum256([]byte(testAPIKey))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantID.String()

	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{
		db:     db,
		mux:    http.NewServeMux(),
		logger: slog.Default(),
		config: Config{MaxValueSize: 1_048_576},
	}

	if err := p.RegisterRoutes(p.mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	handler := auth.Middleware(db)(p.mux)
	return p, handler, store
}

// authedRequest creates a request authenticated with the test API key.
func authedRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	return req
}

// ---------------------------------------------------------------------------
// Existing tests
// ---------------------------------------------------------------------------

func TestInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "kvstore" {
		t.Errorf("expected Name 'kvstore', got %q", info.Name)
	}
	if info.Version != "0.1.0" {
		t.Errorf("expected Version '0.1.0', got %q", info.Version)
	}
	if info.Description == "" {
		t.Error("expected non-empty Description")
	}
}

func TestInit(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte(`{"max_value_size": 2048}`),
	}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.config.MaxValueSize != 2048 {
		t.Errorf("expected MaxValueSize 2048, got %d", p.config.MaxValueSize)
	}
}

func TestInitDefaults(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.config.MaxValueSize != 1_048_576 {
		t.Errorf("expected default MaxValueSize 1048576, got %d", p.config.MaxValueSize)
	}
}

func TestInitInvalidConfig(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte(`not valid json`),
	}
	err := p.Init(context.Background(), env)
	if err == nil {
		t.Fatal("expected error for invalid config, got nil")
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
		{"GET", "/kv/mykey"},
		{"PUT", "/kv/mykey"},
		{"DELETE", "/kv/mykey"},
		{"GET", "/kv"},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Errorf("no handler matched %s %s", tt.method, tt.path)
		}
	}
}

// ---------------------------------------------------------------------------
// Behavioral tests
// ---------------------------------------------------------------------------

// TestPutGetDelete exercises the full PUT -> GET -> DELETE -> GET cycle.
func TestPutGetDelete(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)
	value := `{"hello":"world"}`

	// 1. PUT /kv/test-key
	req := authedRequest("PUT", "/kv/test-key", bytes.NewReader([]byte(value)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var putResp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &putResp); err != nil {
		t.Fatalf("PUT: failed to decode response: %v", err)
	}
	if putResp["key"] != "test-key" {
		t.Errorf("PUT: expected key 'test-key', got %q", putResp["key"])
	}
	version := int(putResp["version"].(float64))
	if version != 1 {
		t.Errorf("PUT: expected version 1, got %d", version)
	}

	// 2. GET /kv/test-key -> 200, value matches
	req = authedRequest("GET", "/kv/test-key", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var getResp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &getResp); err != nil {
		t.Fatalf("GET: failed to decode response: %v", err)
	}
	if getResp["key"] != "test-key" {
		t.Errorf("GET: expected key 'test-key', got %q", getResp["key"])
	}
	// The value is a nested JSON object within RawMessage, so compare as strings.
	gotValue, _ := json.Marshal(getResp["value"])
	if string(gotValue) != `{"hello":"world"}` {
		t.Errorf("GET: expected value %q, got %q", `{"hello":"world"}`, string(gotValue))
	}

	// 3. DELETE /kv/test-key -> 204
	req = authedRequest("DELETE", "/kv/test-key", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: expected 204, got %d", rec.Code)
	}

	// 4. GET /kv/test-key -> 404
	req = authedRequest("GET", "/kv/test-key", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET after delete: expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPutVersionIncrement verifies that PUTting the same key increments the
// version number and returns 200 (not 201) on subsequent puts.
func TestPutVersionIncrement(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	// First PUT -> 201, version 1
	req := authedRequest("PUT", "/kv/ver-key", bytes.NewReader([]byte(`"v1"`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT 1: expected 201, got %d", rec.Code)
	}
	resp1 := map[string]interface{}{}
	json.Unmarshal(rec.Body.Bytes(), &resp1)
	v1 := int(resp1["version"].(float64))

	// Second PUT -> 200, version 2
	req = authedRequest("PUT", "/kv/ver-key", bytes.NewReader([]byte(`"v2"`)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT 2: expected 200 (updated), got %d", rec.Code)
	}
	resp2 := map[string]interface{}{}
	json.Unmarshal(rec.Body.Bytes(), &resp2)
	v2 := int(resp2["version"].(float64))

	if v2 != v1+1 {
		t.Errorf("expected version %d, got %d", v1+1, v2)
	}
}

// TestListAll verifies listing all keys.
func TestListAll(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	keys := []string{"alpha", "beta", "gamma"}
	for _, k := range keys {
		req := authedRequest("PUT", "/kv/"+k, bytes.NewReader([]byte(`"`+k+`"`)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s: expected 201, got %d", k, rec.Code)
		}
	}

	// GET /kv -> should return all 3 in alphabetical order
	req := authedRequest("GET", "/kv", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("LIST: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var results []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &results); err != nil {
		t.Fatalf("LIST: failed to decode: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("LIST: expected 3 results, got %d: %+v", len(results), results)
	}
	if results[0]["key"] != "alpha" {
		t.Errorf("LIST: expected first key 'alpha', got %q", results[0]["key"])
	}
	if results[1]["key"] != "beta" {
		t.Errorf("LIST: expected second key 'beta', got %q", results[1]["key"])
	}
	if results[2]["key"] != "gamma" {
		t.Errorf("LIST: expected third key 'gamma', got %q", results[2]["key"])
	}
}

// TestListWithPrefix verifies prefix-based key listing. Note: keys cannot
// contain "/" because the mux pattern uses {key} which matches a single
// path segment.
func TestListWithPrefix(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	// PUT keys with various prefixes.
	entries := map[string]string{
		"test-alpha": `{"group":"test"}`,
		"test-beta":  `{"group":"test"}`,
		"other":      `{"group":"other"}`,
	}
	for k, v := range entries {
		req := authedRequest("PUT", "/kv/"+k, bytes.NewReader([]byte(v)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s: expected 201, got %d: %s", k, rec.Code, rec.Body.String())
		}
	}

	// GET /kv?prefix=test -> should return test-alpha and test-beta
	req := authedRequest("GET", "/kv?prefix=test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("LIST with prefix: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var results []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &results); err != nil {
		t.Fatalf("LIST: failed to decode: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("LIST: expected 2 results for prefix 'test', got %d: %+v", len(results), results)
	}

	keys := make(map[string]bool)
	for _, r := range results {
		k, _ := r["key"].(string)
		keys[k] = true
	}
	if !keys["test-alpha"] || !keys["test-beta"] {
		t.Errorf("LIST: expected keys [test-alpha, test-beta], got %v", keys)
	}

	// GET /kv?prefix=nonexistent -> should return empty list
	req = authedRequest("GET", "/kv?prefix=nonexistent", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("LIST with nonexistent prefix: expected 200, got %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &results); err != nil {
		t.Fatalf("LIST: failed to decode: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("LIST: expected empty list for nonexistent prefix, got %d results", len(results))
	}
}

// TestPutExceedsMaxSize verifies that PUTting a value larger than MaxValueSize
// returns 413.
func TestPutExceedsMaxSize(t *testing.T) {
	p, handler, _ := setupTestPlugin(t)
	p.config.MaxValueSize = 10 // tiny limit for testing

	// Body larger than 10 bytes should be rejected.
	largeBody := `"this is more than 10 bytes of json string"`
	if len(largeBody) <= p.config.MaxValueSize {
		t.Fatal("test body must be larger than MaxValueSize for this test")
	}

	req := authedRequest("PUT", "/kv/small-key", bytes.NewReader([]byte(largeBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("PUT oversize: expected 413, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestGetNonExistent verifies that GET for a non-existent key returns 404.
func TestGetNonExistent(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := authedRequest("GET", "/kv/nonexistent", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET nonexistent: expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestDeleteNonExistent verifies that DELETE for a non-existent key returns 404.
func TestDeleteNonExistent(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := authedRequest("DELETE", "/kv/nonexistent", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE nonexistent: expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPutWithIfMatch verifies optimistic concurrency: PUT with If-Match header
// matching the current version should succeed and increment the version.
func TestPutWithIfMatch(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	// Create initial value, get its version.
	req := authedRequest("PUT", "/kv/concurrent", bytes.NewReader([]byte(`"v1"`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var putResp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &putResp)
	version := int(putResp["version"].(float64))

	// Update with correct If-Match.
	req = authedRequest("PUT", "/kv/concurrent", bytes.NewReader([]byte(`"v2"`)))
	req.Header.Set("If-Match", fmt.Sprintf("%d", version))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT with correct If-Match: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	json.Unmarshal(rec.Body.Bytes(), &putResp)
	newVersion := int(putResp["version"].(float64))
	if newVersion != version+1 {
		t.Errorf("expected version %d, got %d", version+1, newVersion)
	}
}

// TestPutIfMatchConflict verifies that PUT with a stale If-Match header
// returns 409 Conflict.
func TestPutIfMatchConflict(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	// Create initial value.
	req := authedRequest("PUT", "/kv/stale", bytes.NewReader([]byte(`"v1"`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d", rec.Code)
	}

	// Update with wrong version.
	req = authedRequest("PUT", "/kv/stale", bytes.NewReader([]byte(`"v2"`)))
	req.Header.Set("If-Match", "99")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("PUT with stale If-Match: expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestUnauthenticated verifies that requests without credentials get 401.
func TestUnauthenticated(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := httptest.NewRequest("PUT", "/kv/some-key", bytes.NewReader([]byte(`"data"`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated request, got %d", rec.Code)
	}
}

// TestEmptyBody verifies that PUT with an empty body returns 400.
func TestEmptyBody(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := authedRequest("PUT", "/kv/empty", bytes.NewReader([]byte{}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT empty body: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestInvalidJSON verifies that PUT with invalid JSON returns 400.
func TestInvalidJSON(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := authedRequest("PUT", "/kv/bad-json", bytes.NewReader([]byte(`not json`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT invalid JSON: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}
