package datadogexport

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/rcownie/cleat/internal/auth"
	"github.com/rcownie/cleat/internal/plugin"
)

func TestInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "datadog-export" {
		t.Errorf("expected Name 'datadog-export', got %q", info.Name)
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
	env := &plugin.Environment{}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.httpClient == nil {
		t.Error("expected httpClient to be set")
	}
	if p.logger == nil {
		t.Error("expected logger to be set")
	}
}

func TestInitWithConfig(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte(`{}`),
	}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
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

	// Verify routes are registered by making requests.
	tests := []struct {
		method string
		path   string
	}{
		{"POST", "/datadog/configs"},
		{"GET", "/datadog/configs"},
		{"GET", "/datadog/configs/11111111-1111-1111-1111-111111111111"},
		{"PUT", "/datadog/configs/11111111-1111-1111-1111-111111111111"},
		{"DELETE", "/datadog/configs/11111111-1111-1111-1111-111111111111"},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Errorf("no handler matched %s %s", tt.method, tt.path)
		}
	}
}

func TestPluginRegistration(t *testing.T) {
	plugins, err := plugin.Discover()
	if err != nil {
		t.Fatalf("Discover() returned error: %v", err)
	}
	found := false
	for _, lp := range plugins {
		if lp.Plugin.Info().Name == "datadog-export" {
			found = true
			break
		}
	}
	if !found {
		t.Error("datadog-export plugin not found after Discover")
	}
}

func TestRegisterRoutesNilMux(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterRoutes(nil)
	if err == nil {
		t.Fatal("expected error for nil mux")
	}
}

// TestHandleGetConfigInvalidID verifies that an invalid UUID returns 400.
func TestHandleGetConfigInvalidID(t *testing.T) {
	p := &Plugin{
		logger: slog.New(slog.NewTextHandler(nil, nil)),
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	ctx := auth.WithTenantID(context.Background(), uuid.MustParse("00000000-0000-0000-0000-000000000001"))
	req := httptest.NewRequest("GET", "/datadog/configs/not-a-uuid", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for invalid config id, got %d", rec.Code)
	}
}

// TestHandleDeleteConfigInvalidID verifies that an invalid UUID returns 400.
func TestHandleDeleteConfigInvalidID(t *testing.T) {
	p := &Plugin{
		logger: slog.New(slog.NewTextHandler(nil, nil)),
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	ctx := auth.WithTenantID(context.Background(), uuid.MustParse("00000000-0000-0000-0000-000000000001"))
	req := httptest.NewRequest("DELETE", "/datadog/configs/not-a-uuid", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for invalid config id, got %d", rec.Code)
	}
}

// TestHandleUpdateConfigInvalidID verifies that an invalid UUID returns 400.
func TestHandleUpdateConfigInvalidID(t *testing.T) {
	p := &Plugin{
		logger: slog.New(slog.NewTextHandler(nil, nil)),
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	ctx := auth.WithTenantID(context.Background(), uuid.MustParse("00000000-0000-0000-0000-000000000001"))
	req := httptest.NewRequest("PUT", "/datadog/configs/not-a-uuid", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for invalid config id, got %d", rec.Code)
	}
}
