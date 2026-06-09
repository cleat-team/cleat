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
	"math"
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
// Fake SQL driver — handles auth middleware tenant lookup AND rate_limits CRUD
// ---------------------------------------------------------------------------

// fakeRow is an in-memory rate_limits row.
type fakeRateLimitRow struct {
	tenantID    string
	limitKey    string
	maxRequests int
	windowSecs  int
	createdAt   time.Time
	updatedAt   time.Time
}

type fakeDBStore struct {
	mu              sync.RWMutex
	apiKeys         map[string]string           // sha256 hex -> tenant_id uuid string
	rateLimits      map[string]fakeRateLimitRow // key: "tenant_uuid/limit_key"
	corruptNextScan bool                        // next queryAllRateLimits returns corrupt data
}

type fakeConnector struct {
	store *fakeDBStore
}

func (c *fakeConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &fakeConn{store: c.store}, nil
}

func (c *fakeConnector) Driver() driver.Driver {
	return &fakeDriverStruct{}
}

type fakeDriverStruct struct{}

func (*fakeDriverStruct) Open(_ string) (driver.Conn, error) {
	return nil, fmt.Errorf("fakeDriver: use sql.OpenDB")
}

type fakeConn struct {
	store *fakeDBStore
}

func (*fakeConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, fmt.Errorf("fakeConn: unexpected Prepare call")
}

func (*fakeConn) Close() error { return nil }

func (*fakeConn) Begin() (driver.Tx, error) { return &fakeTxStruct{}, nil }

type fakeTxStruct struct{}

func (*fakeTxStruct) Commit() error   { return nil }
func (*fakeTxStruct) Rollback() error { return nil }

// ---- QueryerContext --------------------------------------------------------

var _ driver.QueryerContext = (*fakeConn)(nil)

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	// Check fault flags under write lock before acquiring read lock.
	c.store.mu.Lock()
	corrupt := c.store.corruptNextScan
	if corrupt {
		c.store.corruptNextScan = false
	}
	c.store.mu.Unlock()

	c.store.mu.RLock()
	defer c.store.mu.RUnlock()

	switch {
	case strings.Contains(query, "FROM tenant_api_keys"):
		return c.queryTenantLookup(args)
	case strings.Contains(query, "SELECT tenant_id, limit_key, max_requests, window_seconds"):
		// background.go reload() — no WHERE clause, returns all rows.
		return c.queryAllRateLimits(corrupt)
	case strings.Contains(query, "SELECT limit_key, max_requests, window_seconds, created_at, updated_at"):
		// routes.go handleList — filtered by tenant.
		return c.queryRateLimits(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected query: %s", query)
	}
}

// ---- ExecerContext ---------------------------------------------------------

var _ driver.ExecerContext = (*fakeConn)(nil)

func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()

	switch {
	case strings.Contains(query, "INSERT INTO rate_limits"):
		return c.execInsertRateLimit(args)
	case strings.Contains(query, "DELETE FROM rate_limits"):
		return c.execDeleteRateLimit(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected exec: %s", query)
	}
}

// ---- Auth: tenant lookup ---------------------------------------------------

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

// ---- Rate limits: INSERT ... ON CONFLICT DO UPDATE -------------------------

func (c *fakeConn) execInsertRateLimit(args []driver.NamedValue) (driver.Result, error) {
	tid, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	key, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	maxReq, err := argInt64(args, 3)
	if err != nil {
		return nil, err
	}
	winSec, err := argInt64(args, 4)
	if err != nil {
		return nil, err
	}

	storeKey := tid + "/" + key
	now := time.Now()

	if existing, ok := c.store.rateLimits[storeKey]; ok {
		existing.maxRequests = int(maxReq)
		existing.windowSecs = int(winSec)
		existing.updatedAt = now
		c.store.rateLimits[storeKey] = existing
	} else {
		c.store.rateLimits[storeKey] = fakeRateLimitRow{
			tenantID:    tid,
			limitKey:    key,
			maxRequests: int(maxReq),
			windowSecs:  int(winSec),
			createdAt:   now,
			updatedAt:   now,
		}
	}
	return &fakeResult{rowsAffected: 1}, nil
}

// ---- Rate limits: DELETE ---------------------------------------------------

func (c *fakeConn) execDeleteRateLimit(args []driver.NamedValue) (driver.Result, error) {
	tid, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	key, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	storeKey := tid + "/" + key
	if _, ok := c.store.rateLimits[storeKey]; ok {
		delete(c.store.rateLimits, storeKey)
		return &fakeResult{rowsAffected: 1}, nil
	}
	return &fakeResult{rowsAffected: 0}, nil
}

// ---- Rate limits: SELECT ---------------------------------------------------

// queryAllRateLimits returns every row in the rate_limits table (used by
// background.go's reload()).
func (c *fakeConn) queryAllRateLimits(corrupt bool) (driver.Rows, error) {
	var rows []fakeRateLimitRow
	for _, row := range c.store.rateLimits {
		rows = append(rows, row)
	}
	// Sort by tenant_id then limit_key for deterministic output.
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			if rows[i].tenantID+"/"+rows[i].limitKey < rows[j].tenantID+"/"+rows[j].limitKey {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	columns := []string{"tenant_id", "limit_key", "max_requests", "window_seconds"}
	data := make([][]driver.Value, len(rows))
	for i, row := range rows {
		if corrupt && i == 0 {
			// Put a string where int64 is expected for max_requests.
			data[i] = []driver.Value{
				row.tenantID,
				row.limitKey,
				"not-an-int",
				int64(row.windowSecs),
			}
		} else {
			data[i] = []driver.Value{
				row.tenantID,
				row.limitKey,
				int64(row.maxRequests),
				int64(row.windowSecs),
			}
		}
	}
	return &fakeRows{columns: columns, data: data}, nil
}

func (c *fakeConn) queryRateLimits(args []driver.NamedValue) (driver.Rows, error) {
	tid, err := argString(args, 1)
	if err != nil {
		return nil, err
	}

	var rows []fakeRateLimitRow
	for _, row := range c.store.rateLimits {
		if row.tenantID == tid {
			rows = append(rows, row)
		}
	}

	// Sort by limit_key (stable enough for small test data).
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			if rows[i].limitKey > rows[j].limitKey {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}

	columns := []string{"limit_key", "max_requests", "window_seconds", "created_at", "updated_at"}
	data := make([][]driver.Value, len(rows))
	for i, row := range rows {
		data[i] = []driver.Value{
			row.limitKey,
			int64(row.maxRequests),
			int64(row.windowSecs),
			row.createdAt,
			row.updatedAt,
		}
	}
	return &fakeRows{columns: columns, data: data}, nil
}

// ---- Shared stubs ----------------------------------------------------------

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

// ---- Argument extractors ---------------------------------------------------

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

func argInt64(args []driver.NamedValue, ordinal int) (int64, error) {
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

// ---------------------------------------------------------------------------
// Test fixtures
// ---------------------------------------------------------------------------

var (
	testTenantA = uuid.MustParse("00000000-0000-0000-0000-000000000001")
	testTenantB = uuid.MustParse("00000000-0000-0000-0000-000000000002")
	testAPIKey  = "enforcement-test-api-key"
)

// newTestPlugin creates a Plugin initialised without a database. The buckets
// map is ready but empty — tests populate it before exercising allow().
func newTestPlugin(t *testing.T) *Plugin {
	t.Helper()
	p := &Plugin{}
	if err := p.Init(context.Background(), &plugin.Environment{}); err != nil {
		t.Fatalf("Init(): %v", err)
	}
	return p
}

// newPluginWithDB creates a Plugin whose p.db field is wired to a fake SQL
// driver that supports both the auth middleware tenant lookup AND rate_limits
// CRUD operations.  It also pre-populates the API key so that
// "Authorization: Bearer "+testAPIKey maps to testTenantA.
func newPluginWithDB(t *testing.T) (*Plugin, *fakeDBStore) {
	t.Helper()
	store := &fakeDBStore{
		apiKeys:    make(map[string]string),
		rateLimits: make(map[string]fakeRateLimitRow),
	}
	keyHash := sha256.Sum256([]byte(testAPIKey))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantA.String()

	fakeDB := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { fakeDB.Close() })

	p := &Plugin{}
	if err := p.Init(context.Background(), &plugin.Environment{DB: &engine.SQLDBAdapter{DB: fakeDB}}); err != nil {
		t.Fatalf("Init(): %v", err)
	}
	return p, store
}

// authMiddlewareHandler builds an http.Handler that chains auth.Middleware
// (which sets the tenant context from a Bearer token) with the
// ratelimiter.Middleware and a final 200-OK handler. The tenant identified by
// testAPIKey is pre-wired to the given rate-limit bucket in the plugin.
func authMiddlewareHandler(t *testing.T, maxRequests, windowSeconds int) (*Plugin, http.Handler) {
	t.Helper()
	p := newTestPlugin(t)

	p.mu.Lock()
	p.buckets[testTenantA.String()+"/default"] = newTokenBucket(maxRequests, windowSeconds)
	p.mu.Unlock()

	store := &fakeDBStore{apiKeys: make(map[string]string)}
	keyHash := sha256.Sum256([]byte(testAPIKey))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantA.String()

	fakeDB := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { fakeDB.Close() })

	finalOK := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return p, auth.Middleware(engine.NewPostgresStore(fakeDB), false)(p.Middleware(finalOK))
}

// authedRequest builds a GET request carrying the test API key so that the
// auth middleware places testTenantA into the request context.
func authedRequest() *http.Request {
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	return req
}

// consumeTokens calls p.allow(tid) until it returns denied and returns the
// number of successful calls. It panics (via t.Fatal) if no limit is
// configured (infinite loop guard — bails after 100_000).
func consumeTokens(t *testing.T, p *Plugin, tid uuid.UUID) int {
	t.Helper()
	n := 0
	for i := 0; i < 100_000; i++ {
		if !p.allow(tid).allowed {
			return n
		}
		n++
	}
	t.Fatal("consumeTokens: no rate limit configured (loop would never terminate)")
	return 0
}

// ---------------------------------------------------------------------------
// Migrations
// ---------------------------------------------------------------------------

func TestMigrations(t *testing.T) {
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
			t.Errorf("migration[%d]: Up query must not be empty", i)
		}
		if m.Down == "" {
			t.Errorf("migration[%d]: Down query must not be empty", i)
		}
	}
	// Verify the migration SQL mentions the rate_limits table.
	if !strings.Contains(migs[0].Up, "rate_limits") {
		t.Error("migration Up should create the rate_limits table")
	}
}

// ---------------------------------------------------------------------------
// Rate-limit enforcement via allow()
// ---------------------------------------------------------------------------

func TestBurst(t *testing.T) {
	p := newTestPlugin(t)
	tid := testTenantA

	// 5 requests per 60 seconds — all 5 are available immediately.
	p.mu.Lock()
	bucket := newTokenBucket(5, 60)
	p.buckets[tid.String()+"/default"] = bucket
	p.mu.Unlock()

	for i := 0; i < 5; i++ {
		if !p.allow(tid).allowed {
			t.Errorf("burst request %d: expected allow", i+1)
		}
	}
	// Sixth request must be denied.
	if p.allow(tid).allowed {
		t.Error("post-burst request: expected deny")
	}
	// Bucket should be empty.
	p.mu.Lock()
	if bucket.tokens >= 1 {
		t.Errorf("expected 0 tokens after full burst, got %f", bucket.tokens)
	}
	p.mu.Unlock()
}

func TestWindowReset(t *testing.T) {
	p := newTestPlugin(t)
	tid := testTenantA

	// 2 requests per 2 seconds (1 token/sec).
	p.mu.Lock()
	bucket := newTokenBucket(2, 2)
	p.buckets[tid.String()+"/default"] = bucket
	p.mu.Unlock()

	// Exhaust the bucket.
	if !p.allow(tid).allowed {
		t.Fatal("request 1: expected allow")
	}
	if !p.allow(tid).allowed {
		t.Fatal("request 2: expected allow")
	}
	if p.allow(tid).allowed {
		t.Fatal("request 3: expected deny (bucket empty)")
	}

	// Winding lastRefill back by the window duration makes the bucket appear
	// fully refilled when the next allow() call runs refill().
	p.mu.Lock()
	bucket.lastRefill = time.Now().Add(-2 * time.Second)
	p.mu.Unlock()

	if !p.allow(tid).allowed {
		t.Error("after window reset: expected allow")
	}
	// And a second request is also allowed (2 tokens = full refill).
	if !p.allow(tid).allowed {
		t.Error("after window reset, request 2: expected allow")
	}
	// Third should be denied again.
	if p.allow(tid).allowed {
		t.Error("after window reset, request 3: expected deny")
	}
}

func TestPartialRefill(t *testing.T) {
	p := newTestPlugin(t)
	tid := testTenantA

	// 10 requests per 60 seconds (refillRate = 1/6 token/sec).
	p.mu.Lock()
	bucket := newTokenBucket(10, 60)
	p.buckets[tid.String()+"/default"] = bucket
	p.mu.Unlock()

	// Burn all 10 at T=0.
	n := consumeTokens(t, p, tid)
	if n != 10 {
		t.Fatalf("expected 10 tokens consumed, got %d", n)
	}

	// Simulate 30 seconds → 30 * (10/60) = 5 tokens refilled.
	p.mu.Lock()
	bucket.lastRefill = time.Now().Add(-30 * time.Second)
	p.mu.Unlock()

	for i := 0; i < 5; i++ {
		if !p.allow(tid).allowed {
			t.Errorf("partial refill request %d: expected allow", i+1)
		}
	}
	if p.allow(tid).allowed {
		t.Error("after partial refill + 5 requests: expected deny")
	}

	// Simulate a full 60 seconds — full refill back to 10.
	p.mu.Lock()
	bucket.lastRefill = time.Now().Add(-60 * time.Second)
	p.mu.Unlock()

	for i := 0; i < 10; i++ {
		if !p.allow(tid).allowed {
			t.Errorf("full refill request %d: expected allow", i+1)
		}
	}
	if p.allow(tid).allowed {
		t.Error("after full refill + burst: expected deny")
	}
}

// ---------------------------------------------------------------------------
// Per-tenant isolation
// ---------------------------------------------------------------------------

func TestTenantIsolation(t *testing.T) {
	p := newTestPlugin(t)

	// Both tenants have 2 req/60s each.
	p.mu.Lock()
	p.buckets[testTenantA.String()+"/default"] = newTokenBucket(2, 60)
	p.buckets[testTenantB.String()+"/default"] = newTokenBucket(2, 60)
	p.mu.Unlock()

	// Tenant A exhausts its quota.
	na := consumeTokens(t, p, testTenantA)
	if na != 2 {
		t.Fatalf("tenant A: expected 2 tokens, got %d", na)
	}
	if p.allow(testTenantA).allowed {
		t.Error("tenant A: expected deny after quota exhausted")
	}

	// Tenant B still has its full quota.
	nb := consumeTokens(t, p, testTenantB)
	if nb != 2 {
		t.Errorf("tenant B: expected 2 tokens (isolated from A), got %d", nb)
	}
}

func TestTenantIndependentReset(t *testing.T) {
	p := newTestPlugin(t)

	// Both tenants have 2 req/60s.
	p.mu.Lock()
	bucketA := newTokenBucket(2, 60)
	bucketB := newTokenBucket(2, 60)
	p.buckets[testTenantA.String()+"/default"] = bucketA
	p.buckets[testTenantB.String()+"/default"] = bucketB
	p.mu.Unlock()

	// Exhaust both.
	consumeTokens(t, p, testTenantA)
	consumeTokens(t, p, testTenantB)

	if p.allow(testTenantA).allowed || p.allow(testTenantB).allowed {
		t.Fatal("both tenants should be exhausted before independent reset test")
	}

	// Reset only tenant A.
	p.mu.Lock()
	bucketA.lastRefill = time.Now().Add(-60 * time.Second)
	p.mu.Unlock()

	if !p.allow(testTenantA).allowed {
		t.Error("tenant A: expected allow after independent reset")
	}
	if p.allow(testTenantB).allowed {
		t.Error("tenant B: expected deny (not reset)")
	}
}

func TestDifferentTenantLimits(t *testing.T) {
	p := newTestPlugin(t)

	// Tenant A: 10 req/60s, Tenant B: 1 req/60s.
	p.mu.Lock()
	p.buckets[testTenantA.String()+"/default"] = newTokenBucket(10, 60)
	p.buckets[testTenantB.String()+"/default"] = newTokenBucket(1, 60)
	p.mu.Unlock()

	na := consumeTokens(t, p, testTenantA)
	if na != 10 {
		t.Errorf("tenant A: expected 10 tokens, got %d", na)
	}

	nb := consumeTokens(t, p, testTenantB)
	if nb != 1 {
		t.Errorf("tenant B: expected 1 token, got %d", nb)
	}
}

func TestTenantNoLimit(t *testing.T) {
	p := newTestPlugin(t)

	// No buckets configured for any tenant — allow() always returns true.
	for i := 0; i < 100; i++ {
		if !p.allow(testTenantA).allowed {
			t.Errorf("request %d: expected allow (no rate limit configured)", i+1)
			break
		}
	}
}

// ---------------------------------------------------------------------------
// Middleware integration tests
// ---------------------------------------------------------------------------

func TestMiddlewareAllowsUnderLimit(t *testing.T) {
	p, handler := authMiddlewareHandler(t, 10, 60)

	// Bucket is full (10 tokens); first request passes.
	req := authedRequest()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", rec.Code)
	}
	// Rate-limit headers must be present when a limit is configured.
	if rec.Header().Get("X-RateLimit-Limit") == "" {
		t.Error("expected X-RateLimit-Limit header")
	}
	if rec.Header().Get("X-RateLimit-Remaining") == "" {
		t.Error("expected X-RateLimit-Remaining header")
	}
	// Verify the token was consumed.
	p.mu.Lock()
	bucket := p.buckets[testTenantA.String()+"/default"]
	p.mu.Unlock()
	if bucket.tokens != 9 {
		t.Errorf("expected 9 tokens remaining after one request, got %f", bucket.tokens)
	}
}

func TestMiddlewareBlocksOverLimit(t *testing.T) {
	p, handler := authMiddlewareHandler(t, 2, 300)

	// Burn the budget by calling allow() directly.
	consumeTokens(t, p, testTenantA)

	req := authedRequest()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 Too Many Requests, got %d", rec.Code)
	}
	// All headers should be present on a 429.
	if rec.Header().Get("X-RateLimit-Limit") == "" {
		t.Error("expected X-RateLimit-Limit header on 429")
	}
	if rec.Header().Get("X-RateLimit-Remaining") == "" {
		t.Error("expected X-RateLimit-Remaining header on 429")
	}
	if rec.Header().Get("X-RateLimit-Reset") == "" {
		t.Error("expected X-RateLimit-Reset header on 429")
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header on 429")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "rate limit exceeded") {
		t.Errorf("body should mention rate limit exceeded, got: %s", rec.Body.String())
	}
}

func TestMiddlewareRetryAfterIsPositive(t *testing.T) {
	_, handler := authMiddlewareHandler(t, 1, 60)

	// First request consumes the only token.
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, authedRequest())
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", rec1.Code)
	}

	// Second request is over limit.
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, authedRequest())
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: expected 429, got %d", rec2.Code)
	}

	ra := rec2.Header().Get("Retry-After")
	if ra == "" {
		t.Fatal("expected Retry-After header")
	}
	var secs int
	if _, err := fmt.Sscanf(ra, "%d", &secs); err != nil {
		t.Fatalf("Retry-After is not an integer: %q", ra)
	}
	if secs <= 0 {
		t.Errorf("expected positive Retry-After, got %d", secs)
	}
}

func TestMiddlewarePassThroughWithoutAuth(t *testing.T) {
	_, handler := authMiddlewareHandler(t, 1, 60)

	// No Authorization header -> auth middleware passes through ->
	// ratelimiter middleware sees no tenant context -> passes through.
	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK (pass-through), got %d", rec.Code)
	}
	// No rate-limit headers when tenant is absent.
	if h := rec.Header().Get("X-RateLimit-Limit"); h != "" {
		t.Error("expected no X-RateLimit-Limit header without tenant auth")
	}
}

// ---------------------------------------------------------------------------
// Edge cases
// ---------------------------------------------------------------------------

func TestZeroLimit(t *testing.T) {
	p := newTestPlugin(t)
	tid := testTenantA

	// max_requests = 0.
	p.mu.Lock()
	p.buckets[tid.String()+"/default"] = newTokenBucket(0, 60)
	p.mu.Unlock()

	// Every request is denied.
	for i := 0; i < 5; i++ {
		if p.allow(tid).allowed {
			t.Errorf("request %d: expected deny with zero limit", i+1)
		}
	}
	// Info fields reflect the zero limit.
	info := p.allow(tid)
	if info.limit != 0 {
		t.Errorf("expected limit 0, got %f", info.limit)
	}
	if info.remaining != 0 {
		t.Errorf("expected remaining 0, got %f", info.remaining)
	}
	// RefillRate is 0, so no tokens are ever added.
	p.mu.Lock()
	bucket := p.buckets[tid.String()+"/default"]
	bucket.lastRefill = time.Now().Add(-3600 * time.Second)
	p.mu.Unlock()

	if p.allow(tid).allowed {
		t.Error("expected deny even after simulated time passes (refillRate=0)")
	}
}

func TestHighLimit(t *testing.T) {
	p := newTestPlugin(t)
	tid := testTenantA

	// 1 million requests per second — effectively unlimited.
	p.mu.Lock()
	p.buckets[tid.String()+"/default"] = newTokenBucket(1_000_000, 1)
	p.mu.Unlock()

	const count = 2000
	for i := 0; i < count; i++ {
		if !p.allow(tid).allowed {
			t.Errorf("request %d: expected allow with high limit", i+1)
		}
	}
}

func TestMultipleBucketsMostConstrained(t *testing.T) {
	p := newTestPlugin(t)
	tid := testTenantA

	// Two rate limits with different constraints.
	// Most constrained bucket governs overall access.
	p.mu.Lock()
	p.buckets[tid.String()+"/generous"] = newTokenBucket(10, 1)
	p.buckets[tid.String()+"/strict"] = newTokenBucket(1, 1)
	p.mu.Unlock()

	// First request: both have tokens.
	if !p.allow(tid).allowed {
		t.Error("request 1: expected allow (both buckets have tokens)")
	}
	// Second request: generous has 9, strict is empty -> denied.
	if p.allow(tid).allowed {
		t.Error("request 2: expected deny (strict bucket empty)")
	}
}

func TestMultipleBucketsInfo(t *testing.T) {
	p := newTestPlugin(t)
	tid := testTenantA

	// Two limits — the info returned should reflect the most constrained.
	p.mu.Lock()
	p.buckets[tid.String()+"/low"] = newTokenBucket(2, 60)
	p.buckets[tid.String()+"/high"] = newTokenBucket(10, 60)
	p.mu.Unlock()

	info := p.allow(tid)
	if !info.allowed {
		t.Fatal("expected allow")
	}
	// The most constrained bucket (low, 2 max) now has 1 remaining after
	// consuming one token. So remaining should be 1.
	if math.Abs(info.remaining-1) > 0.001 {
		t.Errorf("expected remaining ~1 (most constrained bucket), got %f", info.remaining)
	}
	// Limit should reflect the most constrained bucket.
	if info.limit != 2 {
		t.Errorf("expected limit 2 (most constrained), got %f", info.limit)
	}
}

// ---------------------------------------------------------------------------
// HTTP CRUD route tests
// ---------------------------------------------------------------------------

func TestPutRateLimit(t *testing.T) {
	p, store := newPluginWithDB(t)

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes(): %v", err)
	}
	handler := auth.Middleware(engine.NewPostgresStore(p.db.(*engine.SQLDBAdapter).DB), false)(mux)

	body := `{"max_requests":100,"window_seconds":60}`
	req := httptest.NewRequest("PUT", "/rate-limits/myapi", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d. Body: %s", rec.Code, rec.Body.String())
	}

	// Verify the rate limit was stored.
	store.mu.RLock()
	row, ok := store.rateLimits[testTenantA.String()+"/myapi"]
	store.mu.RUnlock()
	if !ok {
		t.Fatal("rate limit not stored in fake DB")
	}
	if row.maxRequests != 100 {
		t.Errorf("expected max_requests=100, got %d", row.maxRequests)
	}
	if row.windowSecs != 60 {
		t.Errorf("expected window_seconds=60, got %d", row.windowSecs)
	}

	// Verify the in-memory bucket was created.
	p.mu.Lock()
	bucket, ok := p.buckets[testTenantA.String()+"/myapi"]
	p.mu.Unlock()
	if !ok {
		t.Fatal("in-memory bucket not created")
	}
	if bucket.maxTokens != 100 {
		t.Errorf("expected maxTokens=100, got %f", bucket.maxTokens)
	}
}

func TestPutRateLimitUpdate(t *testing.T) {
	p, _ := newPluginWithDB(t)

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes(): %v", err)
	}
	handler := auth.Middleware(engine.NewPostgresStore(p.db.(*engine.SQLDBAdapter).DB), false)(mux)

	// Create with 50 req / 30s.
	body1 := `{"max_requests":50,"window_seconds":30}`
	req1 := httptest.NewRequest("PUT", "/rate-limits/myapi", bytes.NewReader([]byte(body1)))
	req1.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("PUT (create): expected 200, got %d", rec1.Code)
	}

	// Update to 200 req / 120s.
	body2 := `{"max_requests":200,"window_seconds":120}`
	req2 := httptest.NewRequest("PUT", "/rate-limits/myapi", bytes.NewReader([]byte(body2)))
	req2.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("PUT (update): expected 200, got %d", rec2.Code)
	}

	// Verify the bucket was updated.
	p.mu.Lock()
	bucket := p.buckets[testTenantA.String()+"/myapi"]
	p.mu.Unlock()
	if bucket.maxTokens != 200 {
		t.Errorf("expected maxTokens=200 after update, got %f", bucket.maxTokens)
	}
	if bucket.refillRate != 200.0/120.0 {
		t.Errorf("expected refillRate=%.6f, got %.6f", 200.0/120.0, bucket.refillRate)
	}
}

func TestPutRateLimitInvalidBody(t *testing.T) {
	_, handler := routeHandler(t)

	tests := []struct {
		name string
		body string
		code int
	}{
		{"non-JSON", `not-json`, http.StatusBadRequest},
		{"negative max_requests", `{"max_requests":-1,"window_seconds":60}`, http.StatusBadRequest},
		{"zero max_requests", `{"max_requests":0,"window_seconds":60}`, http.StatusBadRequest},
		{"negative window", `{"max_requests":10,"window_seconds":-1}`, http.StatusBadRequest},
		{"zero window", `{"max_requests":10,"window_seconds":0}`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("PUT", "/rate-limits/myapi", bytes.NewReader([]byte(tt.body)))
			req.Header.Set("Authorization", "Bearer "+testAPIKey)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.code {
				t.Errorf("expected %d, got %d: %s", tt.code, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestPutRateLimitEmptyKey verifies that an empty {key} segment produces a
// 400 Bad Request.  We call handlePut directly (bypassing the mux) so that
// r.PathValue("key") returns "".
func TestPutRateLimitEmptyKey(t *testing.T) {
	p, _ := newPluginWithDB(t)

	handler := auth.Middleware(engine.NewPostgresStore(p.db.(*engine.SQLDBAdapter).DB), false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.handlePut(w, r)
	}))

	body := `{"max_requests":10,"window_seconds":60}`
	req := httptest.NewRequest("PUT", "/rate-limits/", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty key, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPutRateLimitUnauthenticated(t *testing.T) {
	_, handler := routeHandler(t)

	body := `{"max_requests":10,"window_seconds":60}`
	req := httptest.NewRequest("PUT", "/rate-limits/myapi", bytes.NewReader([]byte(body)))
	// No Authorization header.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized, got %d", rec.Code)
	}
}

func TestListRateLimits(t *testing.T) {
	p, handler := routeHandler(t)

	// Create two rate limits.
	put := func(key, body string) {
		req := httptest.NewRequest("PUT", "/rate-limits/"+key, bytes.NewReader([]byte(body)))
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT %s: expected 200, got %d", key, rec.Code)
		}
	}
	// Insert directly into the fake DB (bypass the mux) so the reload path
	// also works.  We go via the plugin's own PUT handler for realism.
	put("alpha", `{"max_requests":5,"window_seconds":30}`)
	put("beta", `{"max_requests":10,"window_seconds":60}`)

	// Reload from the database so buckets are repopulated from scratch.
	ctx := context.Background()
	n, err := p.reload(ctx)
	if err != nil {
		t.Fatalf("reload(): %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 configs reloaded, got %d", n)
	}

	// List via GET.
	req := httptest.NewRequest("GET", "/rate-limits", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var limits []rateLimitEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &limits); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(limits) != 2 {
		t.Fatalf("expected 2 limits, got %d: %+v", len(limits), limits)
	}
	if limits[0].LimitKey != "alpha" || limits[1].LimitKey != "beta" {
		t.Errorf("expected alpha,beta order, got %s,%s", limits[0].LimitKey, limits[1].LimitKey)
	}
	if limits[0].MaxRequests != 5 || limits[0].WindowSeconds != 30 {
		t.Errorf("alpha: expected 5/30, got %d/%d", limits[0].MaxRequests, limits[0].WindowSeconds)
	}
}

func TestListRateLimitsUnauthenticated(t *testing.T) {
	_, handler := routeHandler(t)

	req := httptest.NewRequest("GET", "/rate-limits", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized, got %d", rec.Code)
	}
}

func TestDeleteRateLimit(t *testing.T) {
	p, handler := routeHandler(t)

	// Create a rate limit via PUT.
	body := `{"max_requests":10,"window_seconds":60}`
	putReq := httptest.NewRequest("PUT", "/rate-limits/todelete", bytes.NewReader([]byte(body)))
	putReq.Header.Set("Authorization", "Bearer "+testAPIKey)
	putRec := httptest.NewRecorder()
	handler.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d", putRec.Code)
	}

	// Verify bucket exists.
	p.mu.Lock()
	_, ok := p.buckets[testTenantA.String()+"/todelete"]
	p.mu.Unlock()
	if !ok {
		t.Fatal("bucket should exist before delete")
	}

	// Delete via DELETE.
	delReq := httptest.NewRequest("DELETE", "/rate-limits/todelete", nil)
	delReq.Header.Set("Authorization", "Bearer "+testAPIKey)
	delRec := httptest.NewRecorder()
	handler.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: expected 204, got %d: %s", delRec.Code, delRec.Body.String())
	}

	// Bucket should be removed from memory.
	p.mu.Lock()
	_, ok = p.buckets[testTenantA.String()+"/todelete"]
	p.mu.Unlock()
	if ok {
		t.Error("bucket should be removed from memory after delete")
	}
}

func TestDeleteRateLimitNotFound(t *testing.T) {
	_, handler := routeHandler(t)

	req := httptest.NewRequest("DELETE", "/rate-limits/nonexistent", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 Not Found, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteRateLimitUnauthenticated(t *testing.T) {
	_, handler := routeHandler(t)

	req := httptest.NewRequest("DELETE", "/rate-limits/myapi", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized, got %d", rec.Code)
	}
}

func TestListAfterDelete(t *testing.T) {
	_, handler := routeHandler(t)

	// Create two limits.
	put := func(key string) {
		body := fmt.Sprintf(`{"max_requests":5,"window_seconds":30}`)
		req := httptest.NewRequest("PUT", "/rate-limits/"+key, bytes.NewReader([]byte(body)))
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT %s: expected 200, got %d", key, rec.Code)
		}
	}
	put("keep")
	put("remove")

	// Delete one.
	delReq := httptest.NewRequest("DELETE", "/rate-limits/remove", nil)
	delReq.Header.Set("Authorization", "Bearer "+testAPIKey)
	delRec := httptest.NewRecorder()
	handler.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: expected 204, got %d", delRec.Code)
	}

	// List — should only see "keep".
	req := httptest.NewRequest("GET", "/rate-limits", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var limits []rateLimitEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &limits); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(limits) != 1 {
		t.Fatalf("expected 1 limit after delete, got %d: %+v", len(limits), limits)
	}
	if limits[0].LimitKey != "keep" {
		t.Errorf("expected remaining key 'keep', got %q", limits[0].LimitKey)
	}
}

// routeHandler creates a full middleware chain (auth + rate-limiter CRUD).
func routeHandler(t *testing.T) (*Plugin, http.Handler) {
	t.Helper()
	p, _ := newPluginWithDB(t)
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes(): %v", err)
	}
	return p, auth.Middleware(engine.NewPostgresStore(p.db.(*engine.SQLDBAdapter).DB), false)(mux)
}

// ---------------------------------------------------------------------------
// Concurrent access (race-condition tests)
// ---------------------------------------------------------------------------

func TestConcurrentAccessUnderLimit(t *testing.T) {
	p := newTestPlugin(t)
	tid := testTenantA

	// Generous limit so all goroutines get tokens.
	p.mu.Lock()
	p.buckets[tid.String()+"/default"] = newTokenBucket(200, 60)
	p.mu.Unlock()

	const goroutines = 50
	var wg sync.WaitGroup
	results := make([]bool, goroutines)

	for i := range goroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = p.allow(tid).allowed
		}(i)
	}
	wg.Wait()

	allowed := 0
	for _, r := range results {
		if r {
			allowed++
		}
	}
	if allowed != goroutines {
		t.Errorf("expected %d allowed, got %d", goroutines, allowed)
	}
}

func TestConcurrentAccessExactLimit(t *testing.T) {
	p := newTestPlugin(t)
	tid := testTenantA

	// Exactly enough tokens for every goroutine.
	p.mu.Lock()
	p.buckets[tid.String()+"/default"] = newTokenBucket(50, 60)
	p.mu.Unlock()

	const goroutines = 50
	var wg sync.WaitGroup
	results := make([]bool, goroutines)

	for i := range goroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = p.allow(tid).allowed
		}(i)
	}
	wg.Wait()

	allowed := 0
	for _, r := range results {
		if r {
			allowed++
		}
	}
	if allowed != goroutines {
		t.Errorf("expected %d allowed (exact limit), got %d", goroutines, allowed)
	}

	// Next request must be denied — all tokens were consumed.
	if p.allow(tid).allowed {
		t.Error("expected deny after concurrent exhaustion")
	}
}

// ---------------------------------------------------------------------------
// Init with DB and background reload edge
// ---------------------------------------------------------------------------

func TestInitWithDB(t *testing.T) {
	p, store := newPluginWithDB(t)
	_ = store

	if p.db == nil {
		t.Error("expected p.db to be set after Init with DB")
	}
	if p.buckets == nil {
		t.Error("expected buckets map to be initialized")
	}
}

func TestRunWithoutDB(t *testing.T) {
	p := newTestPlugin(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately so Run returns promptly.

	err := p.Run(ctx)
	if err != nil {
		t.Errorf("Run() with cancelled context: expected nil, got %v", err)
	}
}

func TestRunWithDBInitialReloadSuccess(t *testing.T) {
	p, _ := newPluginWithDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := p.Run(ctx)
	if err != nil {
		t.Errorf("Run() with DB: expected nil, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Reload scan-error path
// ---------------------------------------------------------------------------

func TestReload_ScanError(t *testing.T) {
	p, store := newPluginWithDB(t)

	// Add a valid rate limit so there is data to scan.
	store.mu.Lock()
	store.rateLimits[testTenantA.String()+"/testkey"] = fakeRateLimitRow{
		tenantID: testTenantA.String(), limitKey: "testkey",
		maxRequests: 10, windowSecs: 60,
		createdAt: time.Now(), updatedAt: time.Now(),
	}
	store.mu.Unlock()

	// Set the corrupt flag before calling reload.
	store.mu.Lock()
	store.corruptNextScan = true
	store.mu.Unlock()

	// reload should handle the scan error gracefully (log and continue).
	n, err := p.reload(context.Background())
	if err != nil {
		t.Fatalf("reload() should not return error on scan error, got: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 configs reloaded (scan error skips the row), got %d", n)
	}
}

// ---------------------------------------------------------------------------
// Reload rows.Err() path using a custom driver
// ---------------------------------------------------------------------------

// rowsErrFakeRows wraps fakeRows and returns a non-EOF error from Next
// after exhausting data, which triggers Go's sql.Rows.Err().
type rowsErrFakeRows struct {
	columns []string
	data    [][]driver.Value
	pos     int
	failOn  int // fail on this Next() call
}

func (r *rowsErrFakeRows) Columns() []string { return r.columns }
func (r *rowsErrFakeRows) Close() error      { return nil }
func (r *rowsErrFakeRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) && r.pos >= r.failOn {
		return fmt.Errorf("simulated rows iteration error after EOF")
	}
	if r.pos >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.pos])
	r.pos++
	return nil
}

// rowsErrConnector wraps a fakeDBStore and returns a connection whose
// QueryContext returns rowsErrFakeRows for the reload query.
type rowsErrConnector struct {
	store *fakeDBStore
}

func (c *rowsErrConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &rowsErrConn{store: c.store}, nil
}

func (c *rowsErrConnector) Driver() driver.Driver { return &rowsErrDriver{} }

type rowsErrDriver struct{}

func (*rowsErrDriver) Open(_ string) (driver.Conn, error) {
	return nil, fmt.Errorf("rowsErrDriver: use sql.OpenDB")
}

type rowsErrConn struct {
	store *fakeDBStore
}

func (*rowsErrConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, fmt.Errorf("rowsErrConn: unexpected Prepare")
}
func (*rowsErrConn) Close() error              { return nil }
func (*rowsErrConn) Begin() (driver.Tx, error) { return nil, fmt.Errorf("rowsErrConn: no tx") }

var _ driver.QueryerContext = (*rowsErrConn)(nil)

func (c *rowsErrConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.store.mu.RLock()
	defer c.store.mu.RUnlock()

	if strings.Contains(query, "SELECT tenant_id, limit_key, max_requests, window_seconds") {
		var rows []fakeRateLimitRow
		for _, r := range c.store.rateLimits {
			rows = append(rows, r)
		}
		columns := []string{"tenant_id", "limit_key", "max_requests", "window_seconds"}
		data := make([][]driver.Value, len(rows))
		for i, r := range rows {
			data[i] = []driver.Value{r.tenantID, r.limitKey, int64(r.maxRequests), int64(r.windowSecs)}
		}
		return &rowsErrFakeRows{columns: columns, data: data, failOn: len(data)}, nil
	}
	if strings.Contains(query, "FROM tenant_api_keys") {
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
	return nil, fmt.Errorf("rowsErrConn: unexpected query: %s", query)
}

func TestReload_RowsErr(t *testing.T) {
	store := &fakeDBStore{
		apiKeys:    make(map[string]string),
		rateLimits: make(map[string]fakeRateLimitRow),
	}
	keyHash := sha256.Sum256([]byte(testAPIKey))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantA.String()

	store.rateLimits[testTenantA.String()+"/errkey"] = fakeRateLimitRow{
		tenantID: testTenantA.String(), limitKey: "errkey",
		maxRequests: 5, windowSecs: 30,
		createdAt: time.Now(), updatedAt: time.Now(),
	}

	fakeDB := sql.OpenDB(&rowsErrConnector{store: store})
	defer fakeDB.Close()

	p := &Plugin{}
	if err := p.Init(context.Background(), &plugin.Environment{DB: &engine.SQLDBAdapter{DB: fakeDB}}); err != nil {
		t.Fatalf("Init(): %v", err)
	}

	_, err := p.reload(context.Background())
	if err == nil {
		t.Fatal("expected error from rows.Err() in reload, got nil")
	}
	if !strings.Contains(err.Error(), "rows iteration") {
		t.Errorf("expected 'rows iteration' error, got: %v", err)
	}
}
