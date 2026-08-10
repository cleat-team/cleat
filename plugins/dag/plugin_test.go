package dag

import (
	"context"
	"log/slog"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

func TestInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "dag" {
		t.Errorf("expected Name 'dag', got %q", info.Name)
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
	if p.logger == nil {
		t.Error("expected logger to be set")
	}
}

func TestInitWithLogger(t *testing.T) {
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
