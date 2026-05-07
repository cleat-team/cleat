package oauthprovider

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
	if info.Name != "oauth-provider" {
		t.Errorf("expected Name 'oauth-provider', got %q", info.Name)
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
		{"GET", "/oauth/google/login"},
		{"GET", "/oauth/github/login"},
		{"GET", "/oauth/okta/login"},
		{"GET", "/oauth/google/callback"},
		{"GET", "/oauth/github/callback"},
		{"GET", "/oauth/okta/callback"},
		{"GET", "/oauth/sessions"},
		{"DELETE", "/oauth/sessions/550e8400-e29b-41d4-a716-446655440000"},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Errorf("no handler matched %s %s", tt.method, tt.path)
		}
	}
}
