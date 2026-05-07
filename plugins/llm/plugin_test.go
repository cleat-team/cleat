package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rcownie/cleat/internal/plugin"
)

func TestInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "llm" {
		t.Errorf("expected Name 'llm', got %q", info.Name)
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
	cfg := `{"providers": {"openai": {"api_key": "sk-test", "enabled": true}}}`
	env := &plugin.Environment{Config: []byte(cfg)}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.httpClient == nil {
		t.Error("expected httpClient to be set")
	}
	openaiCfg, ok := p.config.Providers["openai"]
	if !ok {
		t.Fatal("expected openai provider config")
	}
	if !openaiCfg.Enabled {
		t.Error("expected openai to be enabled")
	}
	if openaiCfg.APIKey != "sk-test" {
		t.Errorf("expected APIKey 'sk-test', got %q", openaiCfg.APIKey)
	}
}

func TestInitNoConfig(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() with no config returned error: %v", err)
	}
	if p.httpClient == nil {
		t.Error("expected httpClient to be set")
	}
}

func TestInitInvalidConfig(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{Config: []byte(`not valid json`)}
	err := p.Init(context.Background(), env)
	if err == nil {
		t.Fatal("expected error for invalid config, got nil")
	}
}

func TestRegisterRoutes(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes() returned error: %v", err)
	}

	tests := []struct {
		method string
		path   string
	}{
		{"GET", "/api/llm/health"},
		{"GET", "/api/llm/models"},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Errorf("no handler matched %s %s", tt.method, tt.path)
		}
	}
}

func TestRegisterHostFunctions(t *testing.T) {
	p := &Plugin{}
	scope := &testFuncRegistry{funcs: make(map[string]plugin.FuncOptions)}
	if err := p.RegisterHostFunctions(scope); err != nil {
		t.Fatalf("RegisterHostFunctions() returned error: %v", err)
	}
	for _, name := range []string{"chat", "embed", "list_models"} {
		if _, ok := scope.funcs[name]; !ok {
			t.Errorf("expected function %q to be registered", name)
		}
	}
}

func TestRegisterHostFunctionsNilScope(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterHostFunctions(nil)
	if err == nil {
		t.Fatal("expected error for nil scope, got nil")
	}
}

// testFuncRegistry is a minimal in-memory registry for testing.
type testFuncRegistry struct {
	funcs map[string]plugin.FuncOptions
}

func (r *testFuncRegistry) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	r.funcs[opts.Name] = opts
	return nil
}
