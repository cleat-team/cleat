package auditlog

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

func TestInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "audit-log" {
		t.Errorf("expected Name 'audit-log', got %q", info.Name)
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
		Config: []byte(`{"retention_days": 30}`),
	}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.config.RetentionDays != 30 {
		t.Errorf("expected RetentionDays 30, got %d", p.config.RetentionDays)
	}
}

func TestInitDefaults(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.config.RetentionDays != 90 {
		t.Errorf("expected default RetentionDays 90, got %d", p.config.RetentionDays)
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

func TestMiddlewareCapturesStatusCode(t *testing.T) {
	p := &Plugin{}
	err := p.Init(context.Background(), &plugin.Environment{})
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}

	// Handler that returns a specific status code.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte{})
	})

	wrapped := p.Middleware(handler)

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()

	// The middleware runs the audit insert in a goroutine; we just verify
	// the response writer wrapper captured the status code by checking
	// the response.
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Errorf("expected status 418, got %d", rec.Code)
	}
}

func TestMiddlewareDefaultsTo200(t *testing.T) {
	p := &Plugin{}
	err := p.Init(context.Background(), &plugin.Environment{})
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}

	// Handler that does not call WriteHeader (defaults to 200).
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	wrapped := p.Middleware(handler)

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()

	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestRegisterRoutes(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	err := p.RegisterRoutes(mux)
	if err != nil {
		t.Fatalf("RegisterRoutes() returned error: %v", err)
	}

	req := httptest.NewRequest("GET", "/audit/events", nil)
	_, pattern := mux.Handler(req)
	if pattern == "" {
		t.Error("no handler matched GET /audit/events")
	}
}

func TestRegisterRoutesNilMux(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterRoutes(nil)
	if err == nil {
		t.Fatal("expected error for nil mux")
	}
}

func TestPluginRegistration(t *testing.T) {
	plugins, err := plugin.Discover()
	if err != nil {
		t.Fatalf("Discover() returned error: %v", err)
	}
	found := false
	for _, lp := range plugins {
		if lp.Plugin.Info().Name == "audit-log" {
			found = true
			break
		}
	}
	if !found {
		t.Error("audit-log plugin not found after Discover")
	}
}

func TestMiddlewareDBDisabled(t *testing.T) {
	p := &Plugin{
		logger: slog.New(slog.NewTextHandler(nil, nil)),
	}
	handler := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}
