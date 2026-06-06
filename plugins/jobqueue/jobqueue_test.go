package jobqueue

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
)

func TestInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "jobqueue" {
		t.Errorf("expected Name 'jobqueue', got %q", info.Name)
	}
	if info.Version != "0.1.0" {
		t.Errorf("expected Version '0.1.0', got %q", info.Version)
	}
	if info.Description != "Standalone job queue" {
		t.Errorf("expected Description 'Standalone job queue', got %q", info.Description)
	}
	if info.Author != "cleat" {
		t.Errorf("expected Author 'cleat', got %q", info.Author)
	}
}

func TestInit(t *testing.T) {
	p := &Plugin{}
	err := p.Init(context.Background(), &plugin.Environment{})
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.logger == nil {
		t.Error("expected logger to be set after Init")
	}
}

func TestInitPreservesLogger(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.logger == nil {
		t.Error("expected logger to be set after Init")
	}
}

// ---- Route handler error path tests (pre-DB) ----

func TestHandleEnqueueMissingTenant(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/jobqueue/testqueue/jobs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("expected 401 for missing tenant, got %d", rec.Code)
	}
}

func TestHandleEnqueueInvalidBody(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	body := strings.NewReader(`not json`)
	req := httptest.NewRequest("POST", "/jobqueue/testqueue/jobs", body).WithContext(
		auth.WithTenantID(context.Background(), uuid.New()))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for invalid body, got %d", rec.Code)
	}
}

func TestHandleListJobsMissingTenant(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/jobqueue/testqueue/jobs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("expected 401 for missing tenant, got %d", rec.Code)
	}
}

func TestHandleGetJobMissingTenant(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/jobqueue/testqueue/jobs/11111111-1111-1111-1111-111111111111", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("expected 401 for missing tenant, got %d", rec.Code)
	}
}

func TestHandleGetJobInvalidJobID(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/jobqueue/testqueue/jobs/not-a-uuid", nil).WithContext(
		auth.WithTenantID(context.Background(), uuid.New()))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for invalid job id, got %d", rec.Code)
	}
}

func TestHandleCancelJobMissingTenant(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("DELETE", "/jobqueue/testqueue/jobs/11111111-1111-1111-1111-111111111111", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("expected 401 for missing tenant, got %d", rec.Code)
	}
}

func TestHandleCancelJobInvalidJobID(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("DELETE", "/jobqueue/testqueue/jobs/not-a-uuid", nil).WithContext(
		auth.WithTenantID(context.Background(), uuid.New()))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for invalid job id, got %d", rec.Code)
	}
}

// ---- RegisterCommands tests ----

func TestRegisterCommandsMissingArgs(t *testing.T) {
	p := &Plugin{}
	cmds := p.RegisterCommands()
	if len(cmds) != 1 {
		t.Fatalf("expected 1 command, got %d", len(cmds))
	}
	err := cmds[0].Run([]string{})
	if err == nil {
		t.Fatal("expected error for missing tenant and queue arguments")
	}
}

func TestRegisterCommandsCommandMetadata(t *testing.T) {
	p := &Plugin{}
	cmds := p.RegisterCommands()
	if len(cmds) != 1 {
		t.Fatalf("expected 1 command, got %d", len(cmds))
	}
	if cmds[0].Name != "jobqueue-enqueue" {
		t.Errorf("expected name 'jobqueue-enqueue', got %q", cmds[0].Name)
	}
	if cmds[0].Description == "" {
		t.Error("expected non-empty description")
	}
}
