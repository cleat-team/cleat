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
	var haveTable bool

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

	// The table this test needs is admin.tenant_api_keys -- that is what
	// PostgresStore.ResolveTenantFromAPIKey queries (engine/store_deployment.go).
	// REQUIRE it rather than creating it, for two reasons.
	//
	// First, the 401 has to be attributable. If the relation is missing the
	// query errors, the middleware refuses the request, and all three subtests
	// pass against a database carrying none of the schema they claim to
	// exercise. Measured 2026-09-04 against an empty database: three of three
	// PASS. A test that cannot fail for its own reason is not a test.
	//
	// Second, this used to do `CREATE TABLE IF NOT EXISTS tenant_api_keys`,
	// UNQUALIFIED -- so it never satisfied the query it was added for (wrong
	// schema) and it manufactured a decoy. Unqualified names resolve through
	// search_path ("$user", public), so the new table then shadowed
	// admin.tenant_api_keys for every other unqualified reference in that
	// database. engine/testutil's cleanup list carried one, and its DELETE hit
	// this decoy instead of the real table for as long as both existed. Running
	// this one test was enough to recreate the decoy minutes after it was
	// dropped by hand.
	if err := db.QueryRow(
		`SELECT to_regclass('admin.tenant_api_keys') IS NOT NULL`).Scan(&haveTable); err != nil {
		fatalf("cannot check for admin.tenant_api_keys: %v", err)
		return
	}
	if !haveTable {
		fatalf("admin.tenant_api_keys does not exist in %s -- run the migrations. "+
			"Without it the middleware refuses every request because the lookup "+
			"errors, and this test passes without exercising the key check at all.", dsn)
		return
	}

	// requireAuth=FALSE, deliberately, and this comment used to say the opposite.
	// It claimed to mirror "what main() does when --require-auth=true" while
	// passing false -- so a reader checking whether the supported default is
	// covered found a test that said yes and tested the other mode.
	//
	// What this file is actually for is the INVALID-key path, which behaves the
	// same either way: a key that does not resolve is refused whether or not
	// auth is required. The supported default's own property -- a request with
	// NO key is refused rather than defaulted -- is covered by
	// tenant_isolation_db_test.go's "unauthenticated request is refused, not
	// defaulted", which builds the server with requireAuth: true.
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

		// This is the requireAuth=false behaviour, and the reason given here used
		// to be wrong twice over: it said the pass-through exists "so that
		// unauthenticated endpoints (healthz, metrics) continue to work", but
		// those are exempted BY PATH at auth/middleware.go:66, before the
		// requireAuth branch is reached -- and the path exercised below is
		// /api/workflows/my-wf/start, which is not one of them.
		//
		// So what this pins is narrow and worth stating exactly: with auth NOT
		// required, a missing key is not itself an error. Under the supported
		// default it is a 401, asserted in tenant_isolation_db_test.go.
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
