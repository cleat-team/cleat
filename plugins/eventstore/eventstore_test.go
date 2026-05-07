package eventstore

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	"github.com/rcownie/cleat/internal/plugin"
)

func TestInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "eventstore" {
		t.Errorf("expected name 'eventstore', got %q", info.Name)
	}
	if info.Version != "0.1.0" {
		t.Errorf("expected version '0.1.0', got %q", info.Version)
	}
	if info.Description != "Append-only event streams with SSE" {
		t.Errorf("unexpected description: %q", info.Description)
	}
}

func TestInit(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		DB:     &sql.DB{},
		Mux:    http.NewServeMux(),
		Config: []byte(`{"max_event_size": 2097152}`),
	}

	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	if p.db == nil {
		t.Error("expected db to be set")
	}
	if p.mux == nil {
		t.Error("expected mux to be set")
	}
	if p.logger == nil {
		t.Error("expected logger to be set")
	}
	if p.config.MaxEventSize != 2097152 {
		t.Errorf("expected MaxEventSize 2097152, got %d", p.config.MaxEventSize)
	}
}

func TestInitWithDefaults(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		DB:  &sql.DB{},
		Mux: http.NewServeMux(),
	}

	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	if p.config.MaxEventSize != 1*1024*1024 {
		t.Errorf("expected default MaxEventSize %d, got %d", 1*1024*1024, p.config.MaxEventSize)
	}
}

func TestRegistered(t *testing.T) {
	infos := plugin.List()
	found := false
	for _, info := range infos {
		if info.Name == "eventstore" {
			found = true
			if info.Version != "0.1.0" {
				t.Errorf("expected version 0.1.0, got %q", info.Version)
			}
			break
		}
	}
	if !found {
		t.Error("eventstore plugin not found in registry")
	}
}
