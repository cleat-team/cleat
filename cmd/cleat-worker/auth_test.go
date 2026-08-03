package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
)

// TestAuthMiddlewareRejectsInvalidKey verifies that when auth middleware is
// wired in (the --require-auth=true default), requests with an invalid API key
// are rejected with 401 Unauthorized.
//
// This is an integration test requiring a running PostgreSQL. Set CLEAT_TEST_DB
// to the connection URL (e.g. "postgres://localhost:5432/cleat?sslmode=disable").
//
// It read DURABLE_TEST_DB until now -- a name left behind by the incomplete
// rename recorded in plans/durable-to-cleat-rename.md, which lists
// DURABLE_TEST_DB -> CLEAT_TEST_DB. Nothing sets the old name any more, so this
// test fell through to a localhost DSN with no credentials, failed to
// authenticate as the CI runner's OS user, and skipped. It had not actually
// exercised the auth middleware in CI for as long as that rename has been
// pending.
func TestAuthMiddlewareRejectsInvalidKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping auth integration test in short mode")
	}

	dsn := os.Getenv("CLEAT_TEST_DB")
	if dsn == "" {
		dsn = os.Getenv("DURABLE_TEST_DB") // deprecated spelling
	}
	// Distinguish "no database configured" from "the configured database is
	// broken". Only the former is a legitimate skip; treating the latter as one
	// is how a test reports success while testing nothing.
	configured := dsn != ""
	if !configured {
		dsn = "postgres://localhost:5432/cleat?sslmode=disable"
	}

	fatalf := t.Skipf
	if configured {
		fatalf = t.Fatalf
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		fatalf("cannot connect to database: %v", err)
		return
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		fatalf("cannot ping database: %v", err)
		return
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
	handler := auth.Middleware(engine.NewPostgresStore(db), false)(mux)

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
		handler2 := auth.Middleware(engine.NewPostgresStore(db), false)(mux2)

		req := httptest.NewRequest("GET", "/healthz", nil)
		w := httptest.NewRecorder()
		handler2.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected HTTP 200 for /healthz without key, got %d", w.Code)
		}
	})
}
