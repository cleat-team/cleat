package slacknotify

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rcownie/cleat/internal/auth"
	"github.com/rcownie/cleat/internal/plugin"
)

func TestInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "slack-notify" {
		t.Errorf("expected Name 'slack-notify', got %q", info.Name)
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
		{"POST", "/slack/configs"},
		{"GET", "/slack/configs"},
		{"GET", "/slack/configs/11111111-1111-1111-1111-111111111111"},
		{"PUT", "/slack/configs/11111111-1111-1111-1111-111111111111"},
		{"DELETE", "/slack/configs/11111111-1111-1111-1111-111111111111"},
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
		if lp.Plugin.Info().Name == "slack-notify" {
			found = true
			break
		}
	}
	if !found {
		t.Error("slack-notify plugin not found after Discover")
	}
}

func TestRegisterHostFunctionsNilScope(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterHostFunctions(nil)
	if err == nil {
		t.Fatal("expected error for nil scope")
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
	req := httptest.NewRequest("GET", "/slack/configs/not-a-uuid", nil).WithContext(ctx)
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
	req := httptest.NewRequest("DELETE", "/slack/configs/not-a-uuid", nil).WithContext(ctx)
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
	req := httptest.NewRequest("PUT", "/slack/configs/not-a-uuid", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for invalid config id, got %d", rec.Code)
	}
}

// TestHandleCreateConfigMissingTenant verifies that a POST without tenant returns 401.
func TestHandleCreateConfigMissingTenant(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/slack/configs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("expected 401 for missing tenant, got %d", rec.Code)
	}
}

// TestHandleListConfigsMissingTenant verifies that a GET without tenant returns 401.
func TestHandleListConfigsMissingTenant(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/slack/configs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("expected 401 for missing tenant, got %d", rec.Code)
	}
}

// TestHandleGetConfigMissingTenant verifies that a GET by ID without tenant returns 401.
func TestHandleGetConfigMissingTenant(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/slack/configs/11111111-1111-1111-1111-111111111111", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("expected 401 for missing tenant, got %d", rec.Code)
	}
}

// TestHandleUpdateConfigMissingTenant verifies that a PUT without tenant returns 401.
func TestHandleUpdateConfigMissingTenant(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("PUT", "/slack/configs/11111111-1111-1111-1111-111111111111", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("expected 401 for missing tenant, got %d", rec.Code)
	}
}

// TestHandleDeleteConfigMissingTenant verifies that a DELETE without tenant returns 401.
func TestHandleDeleteConfigMissingTenant(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("DELETE", "/slack/configs/11111111-1111-1111-1111-111111111111", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("expected 401 for missing tenant, got %d", rec.Code)
	}
}

// TestHandleCreateConfigInvalidBody verifies that invalid JSON returns 400.
func TestHandleCreateConfigInvalidBody(t *testing.T) {
	p := &Plugin{
		logger: slog.New(slog.NewTextHandler(nil, nil)),
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	body := strings.NewReader(`not json`)
	req := httptest.NewRequest("POST", "/slack/configs", body).WithContext(
		auth.WithTenantID(context.Background(), uuid.New()))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for invalid body, got %d", rec.Code)
	}
}

// TestHandleCreateConfigMissingName verifies that a POST without name returns 400.
func TestHandleCreateConfigMissingName(t *testing.T) {
	p := &Plugin{
		logger: slog.New(slog.NewTextHandler(nil, nil)),
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	body := strings.NewReader(`{}`)
	req := httptest.NewRequest("POST", "/slack/configs", body).WithContext(
		auth.WithTenantID(context.Background(), uuid.New()))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for missing name, got %d", rec.Code)
	}
}
