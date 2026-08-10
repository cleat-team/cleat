// Package kvstore provides multi-backend behavioral tests that run the
// key-value store plugin against real database backends (PostgreSQL, MySQL,
// MSSQL). These tests complement the existing in-memory fake-driver tests
// in kvstore_test.go and are kept in a separate file to keep them distinct.
//
// migrations.go provides full UpMySQL and UpMSSQL DDL alongside the
// PostgreSQL Up field, so every backend testutil.NewPluginTestBackends
// returns exercises real queries -- there is no dialect this suite still
// needs to skip.
package kvstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestKVStoreBehavioral_MultiBackend runs the kvstore plugin's behavioral
// assertions against every available database backend (PostgreSQL always,
// MySQL and MSSQL when environment variables are set).
//
// Each backend is tested in its own subtest. Within a backend, scenarios are
// executed sequentially and the kv_store table is cleaned between scenarios
// so that list- and count-based assertions start from a known state.
func TestKVStoreBehavioral_MultiBackend(t *testing.T) {
	backends := testutil.NewPluginTestBackends(t)
	for _, be := range backends {
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			p := &Plugin{}
			ctx := context.Background()

			// Run plugin migrations to create the kv_store table.
			pluginDialect := plugin.Dialect(string(be.Dialect))
			loaded := []*plugin.LoadedPlugin{
				{Plugin: p, Healthy: true},
			}
			if err := plugin.RunMigrations(ctx, be.DB, pluginDialect, nil, loaded); err != nil {
				t.Fatalf(
					"WHAT: RunMigrations failed for backend %q\n"+
						"WHERE: plugin.RunMigrations(ctx, db, %s, nil, loadedPlugin)\n"+
						"WHY:   Without the plugin's database tables, behavioral assertions cannot execute\n"+
						"HOW:   Verify the plugin's Migrations() method returns valid DDL for %s\n"+
						"       and that the database connection is healthy\n"+
						"CLARITY: %v",
					be.Name, pluginDialect, be.Name, err,
				)
			}

			// Initialise the plugin with the real database connection.
			//
			// p.dialect is load-bearing and was previously left unset. The
			// plugin builds its SQL with plugin.Rebind(query, p.dialect), and
			// Rebind passes a query through unchanged for a dialect it does
			// not recognise -- so with the zero value the plugin sent
			// PostgreSQL $1 placeholders to MySQL and SQL Server. Every
			// backend subtest then failed against an object no deployment
			// produces: Init sets this field, and the test built the plugin
			// field by field instead.
			p.dialect = pluginDialect
			p.db = &engine.SQLDBAdapter{DB: be.DB}
			p.mux = http.NewServeMux()
			p.logger = slog.Default()
			p.config = Config{MaxValueSize: 1_048_576}

			if err := p.RegisterRoutes(p.mux); err != nil {
				t.Fatalf("RegisterRoutes: %v", err)
			}

			// --- Scenarios (sequential, each scenario gets a clean slate) ---

			// Error paths — no prior data needed.
			t.Run("GetNonExistent", func(t *testing.T) {
				cleanupKVStore(t, p)
				backendGetNonExistent(t, p.mux)
			})
			t.Run("DeleteNonExistent", func(t *testing.T) {
				cleanupKVStore(t, p)
				backendDeleteNonExistent(t, p.mux)
			})

			// Full lifecycle — PUT / GET / DELETE / GET.
			t.Run("PutGetDelete", func(t *testing.T) {
				cleanupKVStore(t, p)
				backendPutGetDelete(t, p.mux)
			})

			// Version semantics — two PUTs on the same key.
			t.Run("VersionIncrement", func(t *testing.T) {
				cleanupKVStore(t, p)
				backendVersionIncrement(t, p.mux)
			})

			// List scenarios — need a known dataset, so clean first.
			t.Run("ListAll", func(t *testing.T) {
				cleanupKVStore(t, p)
				backendListAll(t, p.mux)
			})
			t.Run("ListWithPrefix", func(t *testing.T) {
				cleanupKVStore(t, p)
				backendListWithPrefix(t, p.mux)
			})
		})
	}
}

// cleanupKVStore removes all rows from kv_store for the test tenant.
func cleanupKVStore(t *testing.T, p *Plugin) {
	t.Helper()
	if p.db == nil {
		return
	}
	// Rebound like every other query the plugin issues: an unrebound $1 is
	// an unknown column on MySQL and a money literal on SQL Server.
	_, err := p.db.Exec(context.Background(),
		plugin.Rebind(`DELETE FROM kv_store WHERE tenant_id = $1`, p.dialect), testTenantID)
	if err != nil {
		// Not a log: every scenario below counts rows, so a cleanup that did
		// not happen silently invalidates the assertions that follow it.
		t.Fatalf("cleanup: clearing kv_store for tenant %s failed: %v", testTenantID, err)
	}
}

// beReq creates an HTTP request authenticated with the test tenant ID. The
// tenant ID is injected directly into the request context via
// auth.WithTenantID, bypassing the need for an API key lookup table. This
// matches the pattern used by the featureflags behavioral tests.
func beReq(method, target string, body []byte) *http.Request {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	return req.WithContext(auth.WithTenantID(context.Background(), testTenantID))
}

// ---------------------------------------------------------------------------
// Behavioural test helpers
// ---------------------------------------------------------------------------

// stringify is a convenience for formatting map assertions.
func stringify(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// backendPutGetDelete exercises the full PUT -> GET -> DELETE -> GET cycle.
func backendPutGetDelete(t *testing.T, mux *http.ServeMux) {
	t.Helper()

	value := `{"hello":"world"}`

	// 1. PUT /kv/mdb-test-key
	req := beReq("PUT", "/kv/mdb-test-key", []byte(value))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf(
			"WHAT: PUT returned unexpected status code\n"+
				"WHERE: PUT /kv/mdb-test-key\n"+
				"WHY:   First insert should return 201 Created\n"+
				"HOW:   Check the kv_store table exists and the INSERT query is valid\n"+
				"CLARITY: expected HTTP 201, got %d: %s",
			rec.Code, rec.Body.String(),
		)
	}

	var putResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &putResp); err != nil {
		t.Fatalf("decode PUT response: %v", err)
	}
	if key, _ := putResp["key"].(string); key != "mdb-test-key" {
		t.Errorf("PUT key: expected %q, got %q", "mdb-test-key", key)
	}

	// 2. GET /kv/mdb-test-key -> 200, value matches
	req = beReq("GET", "/kv/mdb-test-key", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf(
			"WHAT: GET after PUT returned unexpected status code\n"+
				"WHERE: GET /kv/mdb-test-key\n"+
				"WHY:   The key was just created, so GET should find it\n"+
				"HOW:   Verify the SELECT query matches the INSERT that PUT performed\n"+
				"CLARITY: expected HTTP 200, got %d: %s",
			rec.Code, rec.Body.String(),
		)
	}

	var getResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &getResp); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if key, _ := getResp["key"].(string); key != "mdb-test-key" {
		t.Errorf("GET key: expected %q, got %q", "mdb-test-key", key)
	}
	gotValue, _ := json.Marshal(getResp["value"])
	if string(gotValue) != `{"hello":"world"}` {
		t.Errorf("GET value: expected %q, got %q", `{"hello":"world"}`, string(gotValue))
	}

	// 3. DELETE /kv/mdb-test-key -> 204
	req = beReq("DELETE", "/kv/mdb-test-key", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf(
			"WHAT: DELETE returned unexpected status code\n"+
				"WHERE: DELETE /kv/mdb-test-key\n"+
				"WHY:   The key exists, so DELETE should succeed with 204\n"+
				"HOW:   Verify the DELETE query uses the correct WHERE clause\n"+
				"CLARITY: expected HTTP 204, got %d",
			rec.Code,
		)
	}

	// 4. GET /kv/mdb-test-key -> 404 (key was deleted)
	req = beReq("GET", "/kv/mdb-test-key", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf(
			"WHAT: GET after DELETE returned unexpected status code\n"+
				"WHERE: GET /kv/mdb-test-key (after delete)\n"+
				"WHY:   The key was deleted, so GET should return 404\n"+
				"HOW:   Verify the DELETE query actually removes the row\n"+
				"CLARITY: expected HTTP 404, got %d: %s",
			rec.Code, rec.Body.String(),
		)
	}
}

// backendVersionIncrement verifies that PUTting the same key increments the
// version and returns 200 (not 201) on subsequent puts.
func backendVersionIncrement(t *testing.T, mux *http.ServeMux) {
	t.Helper()

	// First PUT -> 201, version 1
	req := beReq("PUT", "/kv/mdb-ver-key", []byte(`"v1"`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT 1: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp1 map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp1)
	v1 := int(resp1["version"].(float64))

	// Second PUT -> 200, version 2
	req = beReq("PUT", "/kv/mdb-ver-key", []byte(`"v2"`))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT 2: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp2 map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp2)
	v2 := int(resp2["version"].(float64))

	if v2 != v1+1 {
		t.Errorf(
			"WHAT: Version did not increment correctly\n"+
				"WHERE: PUT /kv/mdb-ver-key\n"+
				"WHY:   The upsert should increment version by 1 on each update\n"+
				"HOW:   Verify the ON CONFLICT DO UPDATE SET version = kv_store.version + 1 logic\n"+
				"CLARITY: expected version %d, got %d",
			v1+1, v2,
		)
	}
}

// backendListAll verifies listing all keys returns them in alphabetical order.
func backendListAll(t *testing.T, mux *http.ServeMux) {
	t.Helper()

	keys := []string{"mdb-alpha", "mdb-beta", "mdb-gamma"}
	for _, k := range keys {
		req := beReq("PUT", "/kv/"+k, []byte(fmt.Sprintf(`"%s"`, k)))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s: expected 201, got %d: %s", k, rec.Code, rec.Body.String())
		}
	}

	req := beReq("GET", "/kv", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("LIST: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var results []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &results); err != nil {
		t.Fatalf("LIST decode: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf(
			"WHAT: Unexpected number of results in list\n"+
				"WHERE: GET /kv (after inserting 3 keys)\n"+
				"WHY:   All inserted keys should appear in the listing\n"+
				"HOW:   Verify the SELECT query has no extra WHERE filters\n"+
				"CLARITY: expected 3 results, got %d: %s",
			len(results), stringify(results),
		)
	}
	expectedOrder := []string{"mdb-alpha", "mdb-beta", "mdb-gamma"}
	for i, k := range expectedOrder {
		if key, _ := results[i]["key"].(string); key != k {
			t.Errorf("LIST position %d: expected key %q, got %q", i, k, key)
		}
	}
}

// backendListWithPrefix verifies prefix-based filtering of keys.
func backendListWithPrefix(t *testing.T, mux *http.ServeMux) {
	t.Helper()

	entries := map[string]string{
		"mdb-test-alpha": `{"group":"test"}`,
		"mdb-test-beta":  `{"group":"test"}`,
		"mdb-other":      `{"group":"other"}`,
	}
	for k, v := range entries {
		req := beReq("PUT", "/kv/"+k, []byte(v))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s: expected 201, got %d: %s", k, rec.Code, rec.Body.String())
		}
	}

	// GET /kv?prefix=mdb-test -> should return mdb-test-alpha and mdb-test-beta
	req := beReq("GET", "/kv?prefix=mdb-test", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("LIST with prefix: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var results []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &results); err != nil {
		t.Fatalf("LIST decode: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf(
			"WHAT: Unexpected number of results for prefix filter\n"+
				"WHERE: GET /kv?prefix=mdb-test\n"+
				"WHY:   Only 2 of 3 keys match the prefix, so the filtered list should have 2\n"+
				"HOW:   Verify the key LIKE $N clause is correctly parameterized\n"+
				"CLARITY: expected 2 results for prefix %q, got %d: %s",
			"mdb-test", len(results), stringify(results),
		)
	}

	keys := make(map[string]bool)
	for _, r := range results {
		k, _ := r["key"].(string)
		keys[k] = true
	}
	if !keys["mdb-test-alpha"] || !keys["mdb-test-beta"] {
		t.Errorf("expected keys [mdb-test-alpha, mdb-test-beta], got %v", keys)
	}

	// GET /kv?prefix=mdb-nonexistent -> empty list
	req = beReq("GET", "/kv?prefix=mdb-nonexistent", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("LIST with nonexistent prefix: expected 200, got %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &results); err != nil {
		t.Fatalf("LIST decode: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected empty list for nonexistent prefix, got %d results", len(results))
	}
}

// backendGetNonExistent verifies that GET for a non-existent key returns 404.
func backendGetNonExistent(t *testing.T, mux *http.ServeMux) {
	t.Helper()

	req := beReq("GET", "/kv/mdb-nonexistent", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf(
			"WHAT: GET for missing key did not return 404\n"+
				"WHERE: GET /kv/mdb-nonexistent\n"+
				"WHY:   A key that was never inserted should result in ErrNoRows -> 404\n"+
				"HOW:   Verify the handler checks sql.ErrNoRows from QueryRow\n"+
				"CLARITY: expected HTTP 404, got %d: %s",
			rec.Code, rec.Body.String(),
		)
	}
}

// backendDeleteNonExistent verifies that DELETE for a non-existent key returns
// 404.
func backendDeleteNonExistent(t *testing.T, mux *http.ServeMux) {
	t.Helper()

	req := beReq("DELETE", "/kv/mdb-nonexistent-del", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf(
			"WHAT: DELETE for missing key did not return 404\n"+
				"WHERE: DELETE /kv/mdb-nonexistent-del\n"+
				"WHY:   A key that was never inserted should result in 0 RowsAffected -> 404\n"+
				"HOW:   Verify the handler checks RowsAffected from Exec\n"+
				"CLARITY: expected HTTP 404, got %d: %s",
			rec.Code, rec.Body.String(),
		)
	}
}
