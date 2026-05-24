package testfixture

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/cleat-team/cleat/pluginapi"
	cservice "github.com/cleat-team/cleat/plugins/clewservice"
)

// Fixture holds a running clew-service test environment.
type Fixture struct {
	URL    string           // e.g. "http://127.0.0.1:54321"
	server *httptest.Server // nil after Close()
}

// Close shuts down the HTTP server. Safe to call multiple times.
func (f *Fixture) Close() {
	if f.server != nil {
		f.server.Close()
		f.server = nil
	}
}

// New boots a clew-service HTTP server with the clew-side plugin routes.
// The server runs in-process via httptest.NewServer — no external binaries,
// no PostgreSQL, no auth middleware. Starts in <1s.
//
// The returned fixture has all registered plugin routes.
func New(t testing.TB) *Fixture {
	t.Helper()

	root := t.TempDir()

	// Directory structure matching what the plugin handlers expect.
	taskState := filepath.Join(root, "task_state")
	os.MkdirAll(filepath.Join(taskState, "lessons_learned"), 0755)
	os.MkdirAll(filepath.Join(root, "projects"), 0755)

	// Seed an empty tasks.json so readTasksJSON doesn't ENOENT on startup.
	tasks := cservice.TasksJSON{
		Version: "1",
		Updated: cservice.Timestamp(),
		Tasks:   map[string]cservice.TaskEntry{},
	}
	data, _ := json.MarshalIndent(tasks, "", "  ")
	data = append(data, '\n')
	os.WriteFile(filepath.Join(taskState, "tasks.json"), data, 0644)

	// Construct the plugin directly (bypass pluginapi.Register factory).
	p := &cservice.Plugin{}
	env := &pluginapi.Environment{
		Config:   json.RawMessage(`{"project_root":"` + root + `"}`),
		TenantID: "test-tenant",
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("testfixture: plugin init: %v", err)
	}

	// Register all plugin routes.
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("testfixture: register routes: %v", err)
	}

	srv := httptest.NewServer(mux)

	// Health check: GET /healthz must return 200.
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		srv.Close()
		t.Fatalf("testfixture: health check: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		srv.Close()
		t.Fatalf("testfixture: health check returned %d, want 200", resp.StatusCode)
	}

	return &Fixture{URL: srv.URL, server: srv}
}
