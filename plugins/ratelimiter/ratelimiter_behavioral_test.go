package ratelimiter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Fault-injection fake SQL driver for route handler error-path tests.
// Names are prefixed with "beh" to avoid conflicts with ratelimiter_enforcement_test.go.
// ---------------------------------------------------------------------------

type behRow struct {
	tenantID    string
	limitKey    string
	maxRequests int
	windowSecs  int
	createdAt   time.Time
	updatedAt   time.Time
}

type behStore struct {
	mu            sync.RWMutex
	apiKeys       map[string]string
	rateLimits    map[string]behRow
	failNextQuery bool
	failNextExec  bool
}

func newBehStore() *behStore {
	return &behStore{
		apiKeys:    make(map[string]string),
		rateLimits: make(map[string]behRow),
	}
}

type behConnector struct {
	store *behStore
}

func (c *behConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &behConn{store: c.store}, nil
}

func (c *behConnector) Driver() driver.Driver { return &behDriver{} }

type behDriver struct{}

func (*behDriver) Open(_ string) (driver.Conn, error) {
	return nil, fmt.Errorf("behDriver: Open not supported")
}

type behConn struct {
	store *behStore
}

func (*behConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, fmt.Errorf("behConn: unexpected Prepare")
}

func (*behConn) Close() error { return nil }

func (*behConn) Begin() (driver.Tx, error) { return &behTx{}, nil }

type behTx struct{}

func (*behTx) Commit() error   { return nil }
func (*behTx) Rollback() error { return nil }

// ---- ExecerContext -------------------------------------------------------

var _ driver.ExecerContext = (*behConn)(nil)

func (c *behConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()

	if c.store.failNextExec {
		c.store.failNextExec = false
		return nil, fmt.Errorf("simulated exec error")
	}

	if strings.Contains(query, "INSERT INTO rate_limits") {
		return c.execInsert(args)
	}
	if strings.Contains(query, "DELETE FROM rate_limits") {
		return c.execDelete(args)
	}
	return nil, fmt.Errorf("behConn: unexpected exec: %s", query[:minLen(len(query), 80)])
}

func (c *behConn) execInsert(args []driver.NamedValue) (driver.Result, error) {
	tid, _ := behArgStr(args, 1)
	key, _ := behArgStr(args, 2)
	maxReq, _ := behArgInt(args, 3)
	winSec, _ := behArgInt(args, 4)

	storeKey := tid + "/" + key
	now := time.Now()
	if existing, ok := c.store.rateLimits[storeKey]; ok {
		existing.maxRequests = int(maxReq)
		existing.windowSecs = int(winSec)
		existing.updatedAt = now
		c.store.rateLimits[storeKey] = existing
	} else {
		c.store.rateLimits[storeKey] = behRow{
			tenantID: tid, limitKey: key,
			maxRequests: int(maxReq), windowSecs: int(winSec),
			createdAt: now, updatedAt: now,
		}
	}
	return &behResult{1}, nil
}

func (c *behConn) execDelete(args []driver.NamedValue) (driver.Result, error) {
	tid, _ := behArgStr(args, 1)
	key, _ := behArgStr(args, 2)
	storeKey := tid + "/" + key
	if _, ok := c.store.rateLimits[storeKey]; ok {
		delete(c.store.rateLimits, storeKey)
		return &behResult{1}, nil
	}
	return &behResult{0}, nil
}

// ---- QueryerContext ------------------------------------------------------

var _ driver.QueryerContext = (*behConn)(nil)

func (c *behConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	// Check fail flag under write lock before proceeding.
	c.store.mu.Lock()
	shouldFail := c.store.failNextQuery
	if shouldFail {
		c.store.failNextQuery = false
	}
	c.store.mu.Unlock()

	if shouldFail {
		return nil, fmt.Errorf("simulated query error")
	}

	c.store.mu.RLock()
	defer c.store.mu.RUnlock()

	switch {
	case strings.Contains(query, "tenant_api_keys") && strings.Contains(query, "tenant_id"):
		return c.queryTenant(args)
	case strings.Contains(query, "FROM rate_limits") && !strings.Contains(query, "WHERE"):
		return c.queryAllLimits()
	case strings.Contains(query, "FROM rate_limits"):
		return c.queryTenantLimits(args)
	default:
		return nil, fmt.Errorf("behConn: unexpected query: %s", query[:minLen(len(query), 80)])
	}
}

func (c *behConn) queryTenant(args []driver.NamedValue) (driver.Rows, error) {
	keyHash, _ := behArgBytes(args, 1)
	hashHex := fmt.Sprintf("%x", keyHash)
	tid, ok := c.store.apiKeys[hashHex]
	if !ok {
		return &behRows{columns: []string{"tenant_id"}}, nil
	}
	return &behRows{
		columns: []string{"tenant_id"},
		data:    [][]driver.Value{{tid}},
	}, nil
}

func (c *behConn) queryAllLimits() (driver.Rows, error) {
	var rows []behRow
	for _, r := range c.store.rateLimits {
		rows = append(rows, r)
	}
	cols := []string{"tenant_id", "limit_key", "max_requests", "window_seconds"}
	data := make([][]driver.Value, len(rows))
	for i, r := range rows {
		data[i] = []driver.Value{r.tenantID, r.limitKey, int64(r.maxRequests), int64(r.windowSecs)}
	}
	return &behRows{columns: cols, data: data}, nil
}

func (c *behConn) queryTenantLimits(args []driver.NamedValue) (driver.Rows, error) {
	tid, _ := behArgStr(args, 1)
	var rows []behRow
	for _, r := range c.store.rateLimits {
		if r.tenantID == tid {
			rows = append(rows, r)
		}
	}
	cols := []string{"limit_key", "max_requests", "window_seconds", "created_at", "updated_at"}
	data := make([][]driver.Value, len(rows))
	for i, r := range rows {
		data[i] = []driver.Value{r.limitKey, int64(r.maxRequests), int64(r.windowSecs), r.createdAt, r.updatedAt}
	}
	return &behRows{columns: cols, data: data}, nil
}

// ---- Stubs ---------------------------------------------------------------

type behResult struct {
	rowsAffected int64
}

func (r *behResult) LastInsertId() (int64, error) { return 0, nil }
func (r *behResult) RowsAffected() (int64, error) { return r.rowsAffected, nil }

type behRows struct {
	columns []string
	data    [][]driver.Value
	pos     int
}

func (r *behRows) Columns() []string { return r.columns }
func (r *behRows) Close() error      { return nil }
func (r *behRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.pos])
	r.pos++
	return nil
}

// ---- Argument extractors -------------------------------------------------

func behArgBytes(args []driver.NamedValue, ordinal int) ([]byte, error) {
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

func behArgStr(args []driver.NamedValue, ordinal int) (string, error) {
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

func behArgInt(args []driver.NamedValue, ordinal int) (int64, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			i, ok := a.Value.(int64)
			if !ok {
				return 0, fmt.Errorf("arg %d: want int64, got %T", ordinal, a.Value)
			}
			return i, nil
		}
	}
	return 0, fmt.Errorf("arg %d not found", ordinal)
}

func minLen(i, j int) int {
	if i < j {
		return i
	}
	return j
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

var (
	behTenantID = uuid.MustParse("00000000-0000-0000-0000-000000000001")
	behAPIKey   = "beh-test-api-key"
)

func newBehPlugin(t *testing.T) (*Plugin, *behStore) {
	t.Helper()
	store := newBehStore()
	keyHash := sha256.Sum256([]byte(behAPIKey))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = behTenantID.String()

	fakeDB := sql.OpenDB(&behConnector{store: store})
	t.Cleanup(func() { fakeDB.Close() })

	p := &Plugin{}
	if err := p.Init(context.Background(), &plugin.Environment{DB: &engine.SQLDBAdapter{DB: fakeDB}}); err != nil {
		t.Fatalf("Init(): %v", err)
	}
	return p, store
}

// behAuthedReq creates a request with the behavioral-test API key.
func behAuthedReq(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer "+behAPIKey)
	return req
}

// behRouteHandler creates a full auth middleware chain for route tests.
func behRouteHandler(t *testing.T) (*Plugin, http.Handler) {
	t.Helper()
	p, _ := newBehPlugin(t)
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes(): %v", err)
	}
	return p, auth.Middleware(engine.NewPostgresStore(p.db.(*engine.SQLDBAdapter).DB), false)(mux)
}

// ---------------------------------------------------------------------------
// RegisterRoutes -- nil mux test
// ---------------------------------------------------------------------------

func TestRegisterRoutes_NilMux(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterRoutes(nil)
	// ratelimiter.RegisterRoutes returns nil for nil mux.
	if err != nil {
		t.Fatalf("RegisterRoutes(nil) returned error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Route handler error-path tests (DB errors)
// ---------------------------------------------------------------------------

func TestHandleList_DBError(t *testing.T) {
	p, store := newBehPlugin(t)
	store.failNextQuery = true

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes(): %v", err)
	}
	// Inject tenant context before the mux, bypass auth middleware query.
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(auth.WithTenantID(r.Context(), behTenantID))
		mux.ServeHTTP(w, r)
	})

	req := httptest.NewRequest("GET", "/rate-limits", nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for DB error, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "failed to list") {
		t.Errorf("expected 'failed to list' error, got: %s", rec.Body.String())
	}
}

func TestHandlePut_DBError(t *testing.T) {
	p, store := newBehPlugin(t)
	store.failNextExec = true

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes(): %v", err)
	}
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(auth.WithTenantID(r.Context(), behTenantID))
		mux.ServeHTTP(w, r)
	})

	body := `{"max_requests":10,"window_seconds":60}`
	req := httptest.NewRequest("PUT", "/rate-limits/mykey", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for DB error, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "failed to set") {
		t.Errorf("expected 'failed to set' error, got: %s", rec.Body.String())
	}
}

func TestHandleDelete_DBError(t *testing.T) {
	p, store := newBehPlugin(t)
	store.failNextExec = true

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes(): %v", err)
	}
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(auth.WithTenantID(r.Context(), behTenantID))
		mux.ServeHTTP(w, r)
	})

	req := httptest.NewRequest("DELETE", "/rate-limits/mykey", nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for DB error, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "failed to delete") {
		t.Errorf("expected 'failed to delete' error, got: %s", rec.Body.String())
	}
}

func TestHandleDelete_NotFound(t *testing.T) {
	_, handler := behRouteHandler(t)

	req := behAuthedReq("DELETE", "/rate-limits/nonexistent", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Route handler validation tests
// ---------------------------------------------------------------------------

func TestHandlePut_InvalidJSON(t *testing.T) {
	_, handler := behRouteHandler(t)

	req := behAuthedReq("PUT", "/rate-limits/mykey", bytes.NewReader([]byte(`not json`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlePut_NegativeMaxRequests(t *testing.T) {
	_, handler := behRouteHandler(t)

	body := `{"max_requests":-1,"window_seconds":60}`
	req := behAuthedReq("PUT", "/rate-limits/mykey", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlePut_ZeroWindow(t *testing.T) {
	_, handler := behRouteHandler(t)

	body := `{"max_requests":10,"window_seconds":0}`
	req := behAuthedReq("PUT", "/rate-limits/mykey", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlePut_EmptyKey(t *testing.T) {
	p, _ := newBehPlugin(t)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.handlePut(w, r)
	})
	authHandler := auth.Middleware(engine.NewPostgresStore(p.db.(*engine.SQLDBAdapter).DB), false)(handler)

	body := `{"max_requests":10,"window_seconds":60}`
	req := behAuthedReq("PUT", "/rate-limits/", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	authHandler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty key, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleDelete_EmptyKey(t *testing.T) {
	p, _ := newBehPlugin(t)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.handleDelete(w, r)
	})
	authHandler := auth.Middleware(engine.NewPostgresStore(p.db.(*engine.SQLDBAdapter).DB), false)(handler)

	req := behAuthedReq("DELETE", "/rate-limits/", nil)
	rec := httptest.NewRecorder()
	authHandler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty key, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Route handler missing tenant tests
// ---------------------------------------------------------------------------

func TestHandleList_NoTenant(t *testing.T) {
	p := &Plugin{}
	p.Init(context.Background(), &plugin.Environment{})

	req := httptest.NewRequest("GET", "/rate-limits", nil)
	rec := httptest.NewRecorder()
	p.handleList(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlePut_NoTenant(t *testing.T) {
	p := &Plugin{}
	p.Init(context.Background(), &plugin.Environment{})

	body := `{"max_requests":10,"window_seconds":60}`
	req := httptest.NewRequest("PUT", "/rate-limits/mykey", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	p.handlePut(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleDelete_NoTenant(t *testing.T) {
	p := &Plugin{}
	p.Init(context.Background(), &plugin.Environment{})

	req := httptest.NewRequest("DELETE", "/rate-limits/mykey", nil)
	rec := httptest.NewRecorder()
	p.handleDelete(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Plugin metadata test
// ---------------------------------------------------------------------------

func TestBehavioralPluginInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "rate-limiter" {
		t.Errorf("expected Name 'rate-limiter', got %q", info.Name)
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

// ---------------------------------------------------------------------------
// Migrations test
// ---------------------------------------------------------------------------

func TestBehavioralMigrations(t *testing.T) {
	p := &Plugin{}
	migs := p.Migrations()
	if len(migs) == 0 {
		t.Fatal("expected at least one migration")
	}
	for i, m := range migs {
		if m.Version == 0 {
			t.Errorf("migration[%d]: Version must be > 0", i)
		}
		if m.Up == "" {
			t.Errorf("migration[%d]: Up must not be empty", i)
		}
		if m.Down == "" {
			t.Errorf("migration[%d]: Down must not be empty", i)
		}
	}
	if !strings.Contains(migs[0].Up, "rate_limits") {
		t.Error("migration Up should create rate_limits table")
	}
	if !strings.Contains(migs[0].Up, "PRIMARY KEY") {
		t.Error("migration should define a primary key")
	}
}

// ---------------------------------------------------------------------------
// Reload error paths
// ---------------------------------------------------------------------------

func TestReload_QueryError(t *testing.T) {
	p, store := newBehPlugin(t)
	store.failNextQuery = true

	_, err := p.reload(context.Background())
	if err == nil {
		t.Fatal("expected error from reload with failing query")
	}
	if !strings.Contains(err.Error(), "simulated query error") {
		t.Errorf("expected simulated query error, got: %v", err)
	}
}

func TestReload_Success(t *testing.T) {
	p, store := newBehPlugin(t)

	store.mu.Lock()
	store.rateLimits[behTenantID.String()+"/key1"] = behRow{
		tenantID: behTenantID.String(), limitKey: "key1",
		maxRequests: 10, windowSecs: 60,
		createdAt: time.Now(), updatedAt: time.Now(),
	}
	store.rateLimits[behTenantID.String()+"/key2"] = behRow{
		tenantID: behTenantID.String(), limitKey: "key2",
		maxRequests: 5, windowSecs: 30,
		createdAt: time.Now(), updatedAt: time.Now(),
	}
	store.mu.Unlock()

	n, err := p.reload(context.Background())
	if err != nil {
		t.Fatalf("reload() returned error: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 configs reloaded, got %d", n)
	}

	p.mu.Lock()
	b1, ok1 := p.buckets[behTenantID.String()+"/key1"]
	b2, ok2 := p.buckets[behTenantID.String()+"/key2"]
	p.mu.Unlock()

	if !ok1 {
		t.Error("expected bucket for key1")
	} else if b1.maxTokens != 10 {
		t.Errorf("expected maxTokens 10 for key1, got %f", b1.maxTokens)
	}
	if !ok2 {
		t.Error("expected bucket for key2")
	} else if b2.maxTokens != 5 {
		t.Errorf("expected maxTokens 5 for key2, got %f", b2.maxTokens)
	}
}

func TestReload_EmptyTable(t *testing.T) {
	p, _ := newBehPlugin(t)

	n, err := p.reload(context.Background())
	if err != nil {
		t.Fatalf("reload() returned error: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 configs for empty table, got %d", n)
	}

	p.mu.Lock()
	count := len(p.buckets)
	p.mu.Unlock()
	if count != 0 {
		t.Errorf("expected 0 buckets after reloading empty table, got %d", count)
	}
}

// ---------------------------------------------------------------------------
// Run with cancelled context
// ---------------------------------------------------------------------------

func TestBehavioralRun_CancelledContext(t *testing.T) {
	p := &Plugin{}
	p.Init(context.Background(), &plugin.Environment{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := p.Run(ctx)
	if err != nil {
		t.Errorf("Run() with cancelled context: expected nil, got %v", err)
	}
}

func TestBehavioralRun_CancelledContextWithDB(t *testing.T) {
	p, _ := newBehPlugin(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := p.Run(ctx)
	if err != nil {
		t.Errorf("Run() with cancelled context: expected nil, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Init with DB verification
// ---------------------------------------------------------------------------

func TestBehavioralInitWithDB(t *testing.T) {
	p, _ := newBehPlugin(t)

	if p.db == nil {
		t.Error("expected p.db to be set")
	}
	if p.buckets == nil {
		t.Error("expected buckets map to be initialized")
	}
}

// ---------------------------------------------------------------------------
// Full round-trip: PUT then GET
// ---------------------------------------------------------------------------

func TestPutAndGetRateLimit(t *testing.T) {
	_, handler := behRouteHandler(t)

	putBody := `{"max_requests":50,"window_seconds":120}`
	putReq := behAuthedReq("PUT", "/rate-limits/testkey", bytes.NewReader([]byte(putBody)))
	putRec := httptest.NewRecorder()
	handler.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d: %s", putRec.Code, putRec.Body.String())
	}

	var putResp map[string]any
	if err := json.Unmarshal(putRec.Body.Bytes(), &putResp); err != nil {
		t.Fatalf("unmarshal PUT response: %v", err)
	}
	if putResp["limit_key"] != "testkey" {
		t.Errorf("expected limit_key 'testkey', got %q", putResp["limit_key"])
	}

	getReq := behAuthedReq("GET", "/rate-limits", nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", getRec.Code, getRec.Body.String())
	}

	var limits []rateLimitEntry
	if err := json.Unmarshal(getRec.Body.Bytes(), &limits); err != nil {
		t.Fatalf("unmarshal GET response: %v", err)
	}
	if len(limits) != 1 {
		t.Fatalf("expected 1 rate limit, got %d", len(limits))
	}
	if limits[0].LimitKey != "testkey" {
		t.Errorf("expected 'testkey', got %q", limits[0].LimitKey)
	}
	if limits[0].MaxRequests != 50 {
		t.Errorf("expected MaxRequests 50, got %d", limits[0].MaxRequests)
	}
	if limits[0].WindowSeconds != 120 {
		t.Errorf("expected WindowSeconds 120, got %d", limits[0].WindowSeconds)
	}
}

// ---------------------------------------------------------------------------
// RegisterHostFunctions -- ratelimiter does not implement HasHostFunctions.
// There is no RegisterHostFunctions method to test.
// ---------------------------------------------------------------------------
