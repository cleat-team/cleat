package kafkaconnect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rcownie/durable/internal/plugin"
)

func TestInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "kafka-connect" {
		t.Errorf("expected Name 'kafka-connect', got %q", info.Name)
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

func TestInitWithRestProxyConfig(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte(`{"rest_proxy_url": "http://localhost:8082"}`),
	}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.config.RestProxyURL != "http://localhost:8082" {
		t.Errorf("expected RestProxyURL 'http://localhost:8082', got %q", p.config.RestProxyURL)
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
	env := &plugin.Environment{}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}

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
		{"POST", "/kafka/configs"},
		{"GET", "/kafka/configs"},
		{"DELETE", "/kafka/configs/11111111-1111-1111-1111-111111111111"},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Errorf("no handler matched %s %s", tt.method, tt.path)
		}
	}
}

func TestRegisterRoutesNilMux(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterRoutes(nil)
	if err == nil {
		t.Fatal("expected error for nil mux, got nil")
	}
}

func TestRegisterHostFunctions(t *testing.T) {
	p := &Plugin{}
	scope := &mockRegistry{}
	err := p.RegisterHostFunctions(scope)
	if err != nil {
		t.Fatalf("RegisterHostFunctions() returned error: %v", err)
	}
	if scope.name != "produce" {
		t.Errorf("expected function name 'produce', got %q", scope.name)
	}
	if scope.fn == nil {
		t.Error("expected function to be registered")
	}
}

func TestRegisterHostFunctionsNilScope(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterHostFunctions(nil)
	if err == nil {
		t.Fatal("expected error for nil scope, got nil")
	}
}

func TestProduceValidateInput(t *testing.T) {
	p := &Plugin{}
	_, err := p.produce(context.Background(), `{}`)
	if err == nil {
		t.Fatal("expected error for empty config_id, got nil")
	}
}

func TestMigrations(t *testing.T) {
	p := &Plugin{}
	migrations := p.Migrations()
	if len(migrations) != 2 {
		t.Fatalf("expected 2 migrations, got %d", len(migrations))
	}
	if migrations[0].Version != 1 {
		t.Errorf("expected Version 1, got %d", migrations[0].Version)
	}
	if migrations[0].Up == "" {
		t.Error("expected non-empty Up SQL for migration 1")
	}
	if migrations[0].Down == "" {
		t.Error("expected non-empty Down SQL for migration 1")
	}
	if migrations[1].Version != 2 {
		t.Errorf("expected Version 2, got %d", migrations[1].Version)
	}
	if migrations[1].Up == "" {
		t.Error("expected non-empty Up SQL for migration 2")
	}
	if migrations[1].Down == "" {
		t.Error("expected non-empty Down SQL for migration 2")
	}
}

// mockRegistry implements plugin.FuncRegistry for testing.
type mockRegistry struct {
	name string
	fn   plugin.PluginFunc
}

func (m *mockRegistry) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	m.name = opts.Name
	m.fn = fn
	return nil
}
