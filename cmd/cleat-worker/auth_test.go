package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	_ "github.com/lib/pq"

	"github.com/rcownie/cleat/internal/auth"
)

// TestAuthMiddlewareRejectsInvalidKey verifies that when auth middleware is
// wired in (the --require-auth=true default), requests with an invalid API key
// are rejected with 401 Unauthorized.
//
// This is an integration test requiring a running PostgreSQL. Set DURABLE_TEST_DB
// to the connection URL (e.g. "postgres://localhost:5432/cleat?sslmode=disable").
func TestAuthMiddlewareRejectsInvalidKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping auth integration test in short mode")
	}

	dsn := os.Getenv("DURABLE_TEST_DB")
	if dsn == "" {
		dsn = "postgres://localhost:5432/cleat?sslmode=disable"
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("cannot connect to database: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		t.Skipf("cannot ping database: %v", err)
	}

	// Ensure the tenant_api_keys table exists so that the query does not fail
	// with a "relation does not exist" error before reaching the row check.
	db.Exec(`CREATE TABLE IF NOT EXISTS tenant_api_keys (
		key_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		tenant_id UUID NOT NULL,
		key_hash BYTEA NOT NULL UNIQUE,
		description TEXT NOT NULL DEFAULT '',
		revoked_at TIMESTAMPTZ,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`)

	// Build a minimal handler chain that mirrors what main() does when
	// --require-auth=true: auth middleware wraps the route mux.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/workflows/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := auth.Middleware(db)(mux)

	t.Run("invalid_api_key_returns_401", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/workflows/my-wf/start", nil)
		req.Header.Set("Authorization", "Bearer cleat_sk_invalid_key_xxx")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected HTTP 401 for invalid API key, got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), "invalid or revoked API key") {
			t.Errorf("expected error message about invalid key, got %s", w.Body.String())
		}
	})

	t.Run("no_auth_header_passes_through", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/workflows/my-wf/start", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		// The middleware passes through requests without an API key so that
		// unauthenticated endpoints (healthz, metrics) continue to work.
		if w.Code != http.StatusOK {
			t.Errorf("expected HTTP 200 when no auth header is present, got %d", w.Code)
		}
	})

	t.Run("healthz_passes_through_without_key", func(t *testing.T) {
		mux2 := http.NewServeMux()
		mux2.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		handler2 := auth.Middleware(db)(mux2)

		req := httptest.NewRequest("GET", "/healthz", nil)
		w := httptest.NewRecorder()
		handler2.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected HTTP 200 for /healthz without key, got %d", w.Code)
		}
	})
}
