package ratelimiter

import (
	"database/sql"
	"fmt"
	"github.com/cleat-team/cleat/engine"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

func TestInfo(t *testing.T) {
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

func TestInit(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.buckets == nil {
		t.Error("expected buckets map to be initialized")
	}
	if p.logger == nil {
		t.Error("expected logger to be set")
	}
}

func TestInitWithEnvLogger(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Logger: nil, // nil logger should result in slog.Default()
	}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
}

func TestInitWithLoggerSet(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Logger: slog.Default(),
	}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.logger == nil {
		t.Error("expected logger to be set after Init")
	}
}

func TestMiddlewareNoTenant(t *testing.T) {
	p := &Plugin{}
	p.Init(context.Background(), &plugin.Environment{})

	var called bool
	handler := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("expected handler to be called when no tenant in context")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestMiddlewareTenantNoLimit(t *testing.T) {
	p := &Plugin{}
	p.Init(context.Background(), &plugin.Environment{})

	var called bool
	handler := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	// Without tenant context, the request passes through.
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("expected handler to be called when tenant has no rate limit")
	}
}

func TestAllowWithRateLimit(t *testing.T) {
	p := &Plugin{}
	p.Init(context.Background(), &plugin.Environment{})

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001")

	// Configure a rate limit: 2 requests per 1 second.
	p.mu.Lock()
	p.buckets[tid.String()+"/__default__"] = newTokenBucket(2, 1)
	p.mu.Unlock()

	// First two should be allowed.
	if !p.allow(tid).allowed {
		t.Error("request 1: expected allow")
	}
	if !p.allow(tid).allowed {
		t.Error("request 2: expected allow")
	}
	// Third should be denied (bucket empty).
	if p.allow(tid).allowed {
		t.Error("request 3: expected deny")
	}
}

func TestAllowNoLimit(t *testing.T) {
	p := &Plugin{}
	p.Init(context.Background(), &plugin.Environment{})

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001")

	// No rate limits configured — all requests allowed.
	if !p.allow(tid).allowed {
		t.Error("expected allow when no rate limit configured")
	}
}

func TestAllowMultipleBuckets(t *testing.T) {
	p := &Plugin{}
	p.Init(context.Background(), &plugin.Environment{})

	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001")

	// Configure two rate limits: one generous (10/sec) and one strict (1/sec).
	p.mu.Lock()
	p.buckets[tid.String()+"/generous"] = newTokenBucket(10, 1)
	p.buckets[tid.String()+"/strict"] = newTokenBucket(1, 1)
	p.mu.Unlock()

	// First request: both have tokens, should pass.
	if !p.allow(tid).allowed {
		t.Error("request 1: expected allow (both buckets have tokens)")
	}
	// Second request: generous has 9 left, strict is empty — should be denied.
	if p.allow(tid).allowed {
		t.Error("request 2: expected deny (strict bucket empty)")
	}
}

func TestTokenBucketInit(t *testing.T) {
	tb := newTokenBucket(10, 60)
	if tb.maxTokens != 10.0 {
		t.Errorf("expected maxTokens 10.0, got %f", tb.maxTokens)
	}
	if tb.refillRate != 10.0/60.0 {
		t.Errorf("expected refillRate %f, got %f", 10.0/60.0, tb.refillRate)
	}
	if tb.tokens != 10.0 {
		t.Errorf("expected tokens 10.0, got %f", tb.tokens)
	}
}

func TestTokenBucketRefill(t *testing.T) {
	tb := newTokenBucket(5, 60) // 5 tokens per 60 seconds = 0.0833 tokens/sec

	// Consume down to 2 tokens, then wait 30 seconds.
	tb.tokens = 2.0
	tb.lastRefill = time.Now().Add(-30 * time.Second)

	tb.refill()

	// Expected tokens: 2 + (30 * 5/60) = 2 + 2.5 = 4.5
	if tb.tokens < 4.4 || tb.tokens > 4.6 {
		t.Errorf("expected ~4.5 tokens after 30s refill, got %f", tb.tokens)
	}

	// Refill again immediately (no meaningful time elapsed). In the
	// nanoseconds between calls the refill adds a negligible amount.
	tokensBefore := tb.tokens
	tb.refill()
	if tb.tokens > tokensBefore+0.001 {
		t.Errorf("expected tokens ~unchanged after immediate refill, got %f (was %f)", tb.tokens, tokensBefore)
	}
}

func TestTokenBucketMaxCap(t *testing.T) {
	tb := newTokenBucket(5, 60) // 5 tokens max

	// Simulate waiting 10 minutes — refill should cap at 5.
	tb.tokens = 0.0
	tb.lastRefill = time.Now().Add(-10 * time.Minute)

	tb.refill()

	if tb.tokens != 5.0 {
		t.Errorf("expected tokens capped at maxTokens 5.0, got %f", tb.tokens)
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
		{"GET", "/rate-limits"},
		{"PUT", "/rate-limits/mykey"},
		{"DELETE", "/rate-limits/mykey"},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Errorf("no handler matched %s %s", tt.method, tt.path)
		}
	}
}

func TestNew(t *testing.T) {
	p := New()
	if p == nil {
		t.Fatal("New() returned nil")
	}
}

func TestInit_InvalidConfig(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte("not-valid-json"),
	}
	err := p.Init(context.Background(), env)
	if err == nil {
		t.Fatal("expected error for invalid config")
	}
}

func TestInit_DBMode_NoDB(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte(`{"mode":"db"}`),
	}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() should succeed (fallback to memory), got: %v", err)
	}
	if p.mode != "memory" {
		t.Errorf("expected mode=memory (fallback), got %q", p.mode)
	}
}

func TestInit_ExplicitMemoryMode(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte(`{"mode":"memory"}`),
	}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() with memory mode: %v", err)
	}
	if p.mode != "memory" {
		t.Errorf("expected mode=memory, got %q", p.mode)
	}
}

func TestPruneRateCounters(t *testing.T) {
	// Use the fake DB connector to avoid a nil-dereference on p.db.
	store := &fakeDBStore{
		apiKeys:    make(map[string]string),
		rateLimits: make(map[string]fakeRateLimitRow),
	}
	fakeDB := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { fakeDB.Close() })

	p := &Plugin{}
	p.Init(context.Background(), &plugin.Environment{DB: &engine.SQLDBAdapter{DB: fakeDB}})
	p.buckets = make(map[string]*tokenBucket)
	p.buckets["tenant/key"] = newTokenBucket(10, 1)
	p.pruneRateCounters(context.Background())
}

func TestIsDuplicateKeyError(t *testing.T) {
	if isDuplicateKeyError(nil, plugin.DialectPostgres) {
		t.Error("nil error should not be duplicate")
	}
	if !isDuplicateKeyError(fmt.Errorf("duplicate key value violates"), plugin.DialectPostgres) {
		t.Error("Postgres duplicate key not detected")
	}
	if !isDuplicateKeyError(fmt.Errorf("23505"), plugin.DialectPostgres) {
		t.Error("Postgres 23505 not detected")
	}
	if !isDuplicateKeyError(fmt.Errorf("Duplicate entry"), plugin.DialectMySQL) {
		t.Error("MySQL duplicate entry not detected")
	}
	if !isDuplicateKeyError(fmt.Errorf("1062"), plugin.DialectMySQL) {
		t.Error("MySQL 1062 not detected")
	}
	if !isDuplicateKeyError(fmt.Errorf("PRIMARY KEY violation"), plugin.DialectMSSQL) {
		t.Error("MSSQL primary key not detected")
	}
	if !isDuplicateKeyError(fmt.Errorf("2627"), plugin.DialectMSSQL) {
		t.Error("MSSQL 2627 not detected")
	}
	// Unknown dialect falls back to "duplicate" substring.
	if !isDuplicateKeyError(fmt.Errorf("something duplicate here"), "unknown-dialect") {
		t.Error("unknown-dialect duplicate fallback not detected")
	}
}
