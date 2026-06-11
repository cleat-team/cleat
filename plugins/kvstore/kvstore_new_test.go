package kvstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
)

// ---------------------------------------------------------------------------
// DB error path tests
// ---------------------------------------------------------------------------

// setupErrorPlugin creates a Plugin wired to a database where kv_store queries
// fail but tenant API key lookups still work.
func setupErrorPlugin(t *testing.T) http.Handler {
	t.Helper()

	store := newFakeKVStore()
	keyHash := sha256.Sum256([]byte(testAPIKey))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantID.String()

	db := sql.OpenDB(&fakeConnector{store: store, failPattern: "kv_store"})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{
		db:     &engine.SQLDBAdapter{DB: db},
		mux:    http.NewServeMux(),
		logger: slog.Default(),
		config: Config{MaxValueSize: 1_048_576},
	}
	if err := p.RegisterRoutes(p.mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	return auth.Middleware(engine.NewPostgresStore(db), false)(p.mux)
}

// TestGetDBError verifies that handleGet returns 500 when the DB query fails.
func TestGetDBError(t *testing.T) {
	handler := setupErrorPlugin(t)
	req := authedRequest("GET", "/kv/some-key", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPutDBError verifies that handlePut returns 500 when the DB query fails.
func TestPutDBError(t *testing.T) {
	handler := setupErrorPlugin(t)
	req := authedRequest("PUT", "/kv/some-key", bytes.NewReader([]byte(`"hello"`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestDeleteDBError verifies that handleDelete returns 500 when the DB query fails.
func TestDeleteDBError(t *testing.T) {
	handler := setupErrorPlugin(t)
	req := authedRequest("DELETE", "/kv/some-key", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestListDBError verifies that handleList returns 500 when the DB query fails.
func TestListDBError(t *testing.T) {
	handler := setupErrorPlugin(t)
	req := authedRequest("GET", "/kv", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Additional edge case tests
// ---------------------------------------------------------------------------

// TestInitWithNilLogger covers the slog.Default() fallback path.
func TestInitWithNilLogger(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Logger: nil,
	}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() with nil logger returned error: %v", err)
	}
	if p.logger == nil {
		t.Error("expected logger to be set after Init with nil logger")
	}
}

// TestPutWithIfMatchMySQL verifies optimistic concurrency with MySQL dialect.
func TestPutWithIfMatchMySQL(t *testing.T) {
	_, _, store := setupTestPlugin(t)

	// Insert initial value.
	store.data[kvStoreKey(testTenantID, "mysql-key")] = &kvRow{
		tenantID:  testTenantID,
		key:       "mysql-key",
		value:     []byte(`"v1"`),
		version:   1,
		createdAt: time.Now(),
		updatedAt: time.Now(),
	}

	// Build a handler with MySQL dialect.
	p := &Plugin{
		db:      &engine.SQLDBAdapter{DB: sql.OpenDB(&fakeConnector{store: store})},
		mux:     http.NewServeMux(),
		logger:  slog.Default(),
		config:  Config{MaxValueSize: 1_048_576},
		dialect: plugin.DialectMySQL,
	}
	if err := p.RegisterRoutes(p.mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	mysqlHandler := auth.Middleware(engine.NewPostgresStore(sql.OpenDB(&fakeConnector{store: store})), false)(p.mux)

	// Update with correct If-Match.
	req := authedRequest("PUT", "/kv/mysql-key", bytes.NewReader([]byte(`"v2"`)))
	req.Header.Set("If-Match", "1")
	rec := httptest.NewRecorder()
	mysqlHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT with If-Match (MySQL): expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify version incremented.
	row := store.data[kvStoreKey(testTenantID, "mysql-key")]
	if row == nil {
		t.Fatal("expected row to exist after MySQL If-Match update")
	}
	if row.version != 2 {
		t.Errorf("expected version 2, got %d", row.version)
	}
}

// TestPutUpsertMySQL verifies the upsert path with MySQL dialect.
func TestPutUpsertMySQL(t *testing.T) {
	store := newFakeKVStore()
	keyHash := sha256.Sum256([]byte(testAPIKey))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantID.String()

	p := &Plugin{
		db:      &engine.SQLDBAdapter{DB: sql.OpenDB(&fakeConnector{store: store})},
		mux:     http.NewServeMux(),
		logger:  slog.Default(),
		config:  Config{MaxValueSize: 1_048_576},
		dialect: plugin.DialectMySQL,
	}
	if err := p.RegisterRoutes(p.mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	mysqlHandler := auth.Middleware(engine.NewPostgresStore(sql.OpenDB(&fakeConnector{store: store})), false)(p.mux)

	// Upsert a new key with MySQL dialect.
	req := authedRequest("PUT", "/kv/new-key", bytes.NewReader([]byte(`"v1"`)))
	rec := httptest.NewRecorder()
	mysqlHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT upsert (MySQL): expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	row := store.data[kvStoreKey(testTenantID, "new-key")]
	if row == nil {
		t.Fatal("expected row to exist after MySQL upsert")
	}
	if row.version != 1 {
		t.Errorf("expected version 1, got %d", row.version)
	}
}

// TestListWithLimit verifies the list endpoint respects the limit parameter.
func TestListWithLimit(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	for i := 0; i < 10; i++ {
		k := fmt.Sprintf("lim-key-%d", i)
		store.data[kvStoreKey(testTenantID, k)] = &kvRow{
			tenantID:  testTenantID,
			key:       k,
			value:     []byte(`"v"`),
			version:   1,
			createdAt: time.Now(),
			updatedAt: time.Now(),
		}
	}

	req := authedRequest("GET", "/kv?limit=3", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("LIST with limit: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var results []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &results); err != nil {
		t.Fatalf("LIST: failed to decode: %v", err)
	}
	if len(results) != 3 {
		t.Errorf("expected 3 results with limit=3, got %d", len(results))
	}
}

// TestListWithInvalidLimit verifies that an invalid limit parameter is ignored.
func TestListWithInvalidLimit(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	for i := 0; i < 5; i++ {
		k := fmt.Sprintf("invlim-key-%d", i)
		store.data[kvStoreKey(testTenantID, k)] = &kvRow{
			tenantID:  testTenantID,
			key:       k,
			value:     []byte(`"v"`),
			version:   1,
			createdAt: time.Now(),
			updatedAt: time.Now(),
		}
	}

	req := authedRequest("GET", "/kv?limit=-1", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("LIST with invalid limit: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var results []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &results); err != nil {
		t.Fatalf("LIST: failed to decode: %v", err)
	}
	if len(results) != 5 {
		t.Errorf("expected all 5 results with invalid limit, got %d", len(results))
	}
}
