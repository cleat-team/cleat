package kvstore

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
	if info.Name != "kvstore" {
		t.Errorf("expected Name 'kvstore', got %q", info.Name)
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
		Config: []byte(`{"max_value_size": 2048}`),
	}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.config.MaxValueSize != 2048 {
		t.Errorf("expected MaxValueSize 2048, got %d", p.config.MaxValueSize)
	}
}

func TestInitDefaults(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.config.MaxValueSize != 1_048_576 {
		t.Errorf("expected default MaxValueSize 1048576, got %d", p.config.MaxValueSize)
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
	// We test that the mux handles path patterns rather than actual handler
	// execution (which requires a running server).
	tests := []struct {
		method string
		path   string
	}{
		{"GET", "/kv/mykey"},
		{"PUT", "/kv/mykey"},
		{"DELETE", "/kv/mykey"},
		{"GET", "/kv"},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Errorf("no handler matched %s %s", tt.method, tt.path)
		}
	}
}
