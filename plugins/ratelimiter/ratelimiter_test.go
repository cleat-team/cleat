package ratelimiter

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/cleat-team/cleat/plugin"
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
