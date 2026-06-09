// Package featureflags provides multi-backend behavioral tests that run the
// feature flag plugin against real database backends (PostgreSQL, MySQL,
// MSSQL). These tests complement the existing in-memory fake-driver tests in
// featureflags_behavioral_test.go and are kept in a separate file to keep
// them distinct.
//
// When a backend lacks dialect-specific migration SQL (e.g. UpMySQL or
// UpMSSQL is empty), the test skips with a descriptive message rather than
// failing. Currently the featureflags plugin only has a PostgreSQL migration,
// so only the postgres backend exercises real queries.
package featureflags

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// testBackendTenantID is the tenant UUID used for multi-backend test requests.
// It must match the tenant used by the existing behavioral tests.
var testBackendTenantID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

// TestFFBehavioral_MultiBackend runs the featureflags plugin's behavioural
// assertions against every available database backend (PostgreSQL always,
// MySQL and MSSQL when environment variables are set).
//
// Each backend is tested in its own subtest. Scenarios are executed
// sequentially and the feature_flags table is cleaned between scenarios.
func TestFFBehavioral_MultiBackend(t *testing.T) {
	backends := testutil.NewPluginTestBackends(t)
	for _, be := range backends {
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			p := &Plugin{}
			ctx := context.Background()

			// Check whether this dialect has migration SQL. The featureflags
			// plugin currently only provides a PostgreSQL migration (the Up
			// field). For MySQL and MSSQL the plugin has no dialect-specific
			// DDL, so RunMigrations skips it and the table is never created.
			if be.Dialect == testutil.DialectMySQL || be.Dialect == testutil.DialectMSSQL {
				if !ffHasDialectMigrations(p, be.Dialect) {
					t.Skipf("featureflags plugin does not have %s migrations yet", be.Name)
				}
			}

			// Run plugin migrations to create the feature_flags table.
			pluginDialect := plugin.Dialect(string(be.Dialect))
			loaded := []*plugin.LoadedPlugin{
				{Plugin: p, Healthy: true},
			}
			if err := plugin.RunMigrations(ctx, be.DB, pluginDialect, nil, loaded); err != nil {
				t.Fatalf(
					"WHAT: RunMigrations failed for backend %q\n"+
						"WHERE: plugin.RunMigrations(ctx, db, %s, nil, loadedPlugin)\n"+
						"WHY:   Without the feature_flags table, behavioral assertions cannot execute\n"+
						"HOW:   Verify the plugin's Migrations() method returns valid DDL for %s\n"+
						"       and that the database connection is healthy\n"+
						"CLARITY: %v",
					be.Name, pluginDialect, be.Name, err,
				)
			}

			// Initialise the plugin with the real database connection.
			p.db = &engine.SQLDBAdapter{DB: be.DB}
			p.mux = http.NewServeMux()
			p.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

			if err := p.RegisterRoutes(p.mux); err != nil {
				t.Fatalf("RegisterRoutes: %v", err)
			}

			// --- Scenarios (sequential, each scenario gets a clean slate) ---

			// Evaluate non-existent flag.
			t.Run("EvaluateNotFound", func(t *testing.T) {
				cleanupFeatureFlags(t, p)
				backendEvaluateNotFound(t, p.mux)
			})

			// List flags when none exist — expects [] not null.
			t.Run("ListEmpty", func(t *testing.T) {
				cleanupFeatureFlags(t, p)
				backendListEmpty(t, p.mux)
			})

			// Full CRUD lifecycle — create, list, get, update, delete, verify.
			t.Run("CRUDFullLifecycle", func(t *testing.T) {
				cleanupFeatureFlags(t, p)
				backendCRUDFullLifecycle(t, p.mux)
			})

			// List multiple flags.
			t.Run("ListMultiple", func(t *testing.T) {
				cleanupFeatureFlags(t, p)
				backendListMultiple(t, p.mux)
			})
		})
	}
}

// cleanupFeatureFlags removes all rows from feature_flags for the test tenant.
func cleanupFeatureFlags(t *testing.T, p *Plugin) {
	t.Helper()
	if p.db == nil {
		return
	}
	_, err := p.db.Exec(context.Background(),
		`DELETE FROM feature_flags WHERE tenant_id = $1`, testBackendTenantID)
	if err != nil {
		// Log rather than fail — the table may not exist on backends that
		// were not properly set up.
		t.Logf("cleanup: %v", err)
	}
}

// ffHasDialectMigrations returns true if the plugin has at least one
// migration with dialect-specific DDL (UpMySQL or UpMSSQL) for the given
// dialect. For PostgreSQL we always return true since the Up field is the
// default and is always populated.
func ffHasDialectMigrations(p *Plugin, dialect testutil.Dialect) bool {
	for _, m := range p.Migrations() {
		var ddl string
		switch dialect {
		case testutil.DialectMySQL:
			ddl = m.UpMySQL
		case testutil.DialectMSSQL:
			ddl = m.UpMSSQL
		default:
			return true // PostgreSQL always has the Up field
		}
		if ddl != "" {
			return true
		}
	}
	return false
}

// beRequest creates an HTTP request with the test tenant ID injected directly
// into the request context via auth.WithTenantID. This bypasses the need for
// the tenant_api_keys table and auth middleware, matching the pattern used by
// the existing featureflags behavioural tests.
func beRequest(method, path string, body []byte) *http.Request {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	return req.WithContext(auth.WithTenantID(context.Background(), testBackendTenantID))
}

// decodeJSON is a test helper that decodes the response body into v.
func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.NewDecoder(rec.Body).Decode(v); err != nil {
		t.Fatalf(
			"WHAT: Failed to decode JSON response body\n"+
				"WHERE: decodeJSON(t, httptest.ResponseRecorder, v)\n"+
				"WHY:   Cannot verify behavioral assertions without decoding the response\n"+
				"HOW:   Verify the handler is returning valid JSON matching the expected type\n"+
				"CLARITY: decode error: %v (body: %s)",
			err, rec.Body.String(),
		)
	}
}

// ---------------------------------------------------------------------------
// Behavioral test helpers
// ---------------------------------------------------------------------------

// stringify is a convenience for formatting map assertions.
func stringify(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// backendCRUDFullLifecycle exercises the full CRUD cycle: create, list, get
// by ID, update, delete, and verify deletion.
func backendCRUDFullLifecycle(t *testing.T, mux *http.ServeMux) {
	t.Helper()

	// Create flag.
	createBody := `{"key":"mdb-beta","name":"MDB Beta Feature","enabled":true,"rollout_percentage":50}`
	rec := httptest.NewRecorder()
	req := beRequest("POST", "/features/flags", []byte(createBody))
	mux.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf(
			"WHAT: Create flag returned unexpected status code\n"+
				"WHERE: POST /features/flags\n"+
				"WHY:   A well-formed flag creation request should return 201 Created\n"+
				"HOW:   Verify the INSERT query executes successfully\n"+
				"CLARITY: expected HTTP 201, got %d: %s",
			rec.Code, rec.Body.String(),
		)
	}
	var created flagJSON
	decodeJSON(t, rec, &created)
	if created.Key != "mdb-beta" {
		t.Errorf("create: expected key %q, got %q", "mdb-beta", created.Key)
	}
	if !created.Enabled {
		t.Error("create: expected enabled=true")
	}
	if created.RolloutPercentage != 50 {
		t.Errorf("create: expected rollout 50, got %d", created.RolloutPercentage)
	}

	// List flags — should include the newly created flag.
	rec = httptest.NewRecorder()
	req = beRequest("GET", "/features/flags", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var flags []flagJSON
	decodeJSON(t, rec, &flags)
	if len(flags) != 1 {
		t.Fatalf(
			"WHAT: List returned unexpected number of flags\n"+
				"WHERE: GET /features/flags (after creating one flag)\n"+
				"WHY:   Only one flag was created, so the list should contain exactly one\n"+
				"HOW:   Verify the SELECT query filters by tenant_id correctly\n"+
				"CLARITY: expected 1 flag, got %d: %s",
			len(flags), stringify(flags),
		)
	}
	if len(flags) > 0 && flags[0].Key != "mdb-beta" {
		t.Errorf("list: expected key %q, got %q", "mdb-beta", flags[0].Key)
	}

	// Get flag by ID.
	rec = httptest.NewRecorder()
	req = beRequest("GET", "/features/flags/"+created.ID.String(), nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("get: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got flagJSON
	decodeJSON(t, rec, &got)
	if got.Key != "mdb-beta" {
		t.Errorf("get: expected key %q, got %q", "mdb-beta", got.Key)
	}

	// Update flag.
	updateBody := `{"enabled":false,"rollout_percentage":100}`
	rec = httptest.NewRecorder()
	req = beRequest("PUT", "/features/flags/"+created.ID.String(), []byte(updateBody))
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf(
			"WHAT: Update flag returned unexpected status code\n"+
				"WHERE: PUT /features/flags/%s\n"+
				"WHY:   An existing flag should be updated successfully with 200\n"+
				"HOW:   Verify the UPDATE query uses the correct WHERE id AND tenant_id\n"+
				"CLARITY: expected HTTP 200, got %d: %s",
			created.ID.String(), rec.Code, rec.Body.String(),
		)
	}

	// Delete flag.
	rec = httptest.NewRecorder()
	req = beRequest("DELETE", "/features/flags/"+created.ID.String(), nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf(
			"WHAT: Delete flag returned unexpected status code\n"+
				"WHERE: DELETE /features/flags/%s\n"+
				"WHY:   An existing flag should be deleted successfully with 204\n"+
				"HOW:   Verify the DELETE query uses the correct WHERE clause\n"+
				"CLARITY: expected HTTP 204, got %d: %s",
			created.ID.String(), rec.Code, rec.Body.String(),
		)
	}

	// Verify deleted — list should be empty.
	rec = httptest.NewRecorder()
	req = beRequest("GET", "/features/flags", nil)
	mux.ServeHTTP(rec, req)
	var afterDelete []flagJSON
	decodeJSON(t, rec, &afterDelete)
	if len(afterDelete) != 0 {
		t.Errorf(
			"WHAT: List after delete still contains flags\n"+
				"WHERE: GET /features/flags (after DELETE)\n"+
				"WHY:   The flag was deleted, so the list should be empty\n"+
				"HOW:   Verify the DELETE query actually removes the row\n"+
				"CLARITY: expected 0 flags after delete, got %d: %s",
			len(afterDelete), stringify(afterDelete),
		)
	}
}

// backendListEmpty verifies that listing flags when none exist returns an
// empty JSON array (not null).
func backendListEmpty(t *testing.T, mux *http.ServeMux) {
	t.Helper()

	req := beRequest("GET", "/features/flags", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list empty: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	body := string(bytes.TrimSpace(rec.Body.Bytes()))
	if body != "[]" {
		t.Errorf(
			"WHAT: Empty list did not return JSON array\n"+
				"WHERE: GET /features/flags (no flags exist)\n"+
				"WHY:   Client code expects [] not null for empty collections\n"+
				"HOW:   Verify the handler initialises the slice with make([]flagJSON, 0) when rows are empty\n"+
				"CLARITY: expected body %q, got %q",
			"[]", body,
		)
	}
}

// backendListMultiple verifies that listing flags returns all created flags.
func backendListMultiple(t *testing.T, mux *http.ServeMux) {
	t.Helper()

	// Create three flags with a unique prefix to avoid collision in case
	// cleanup did not run.
	prefix := "mdb-multi-" + uuid.New().String()[:8]
	for i, letter := range []string{"a", "b", "c"} {
		body := fmt.Sprintf(`{"key":"%s-%s","enabled":true}`, prefix, letter)
		rec := httptest.NewRecorder()
		req := beRequest("POST", "/features/flags", []byte(body))
		mux.ServeHTTP(rec, req)
		if rec.Code != 201 {
			t.Fatalf("create %d: expected 201, got %d: %s", i, rec.Code, rec.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	req := beRequest("GET", "/features/flags", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list multiple: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var flags []flagJSON
	decodeJSON(t, rec, &flags)
	if len(flags) != 3 {
		t.Errorf(
			"WHAT: List returned unexpected number of flags\n"+
				"WHERE: GET /features/flags (after creating 3 flags)\n"+
				"WHY:   All three created flags should be visible in the list\n"+
				"HOW:   Verify the SELECT query has no incorrect WHERE filters\n"+
				"CLARITY: expected 3 flags, got %d",
			len(flags),
		)
	}
}

// backendEvaluateNotFound verifies that evaluating a non-existent flag returns
// 404.
func backendEvaluateNotFound(t *testing.T, mux *http.ServeMux) {
	t.Helper()

	req := beRequest("POST", "/features/evaluate", []byte(`{"key":"mdb-nonexistent"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Errorf(
			"WHAT: Evaluate non-existent flag did not return 404\n"+
				"WHERE: POST /features/evaluate with key %q\n"+
				"WHY:   A flag that was never created should result in ErrNoRows -> 404\n"+
				"HOW:   Verify the handler checks sql.ErrNoRows from QueryRow\n"+
				"CLARITY: expected HTTP 404, got %d: %s",
			"mdb-nonexistent", rec.Code, rec.Body.String(),
		)
	}
}
