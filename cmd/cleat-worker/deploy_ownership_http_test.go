package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// TestDeployOverAnotherTenantsNameIsRefusedOverHTTP is the HTTP half of
// IMPROVEMENT-PLAN 3.12.
//
// The store-level property is tested in engine (a deploy must not replace a
// definition owned by another tenant). This is the layer a customer actually
// meets, and the reason it needs its own test is the same one §1.7 established:
// a correct store reached through a handler that reports the wrong thing is
// still a wrong product. Specifically, the endpoint returned 500 for every
// error, so a name another tenant holds -- an ordinary, expected client
// situation -- would have read as a server fault, paged whoever owns the
// alerts, and told the caller nothing actionable.
//
// It also pins the part that must NOT leak: the response says the name is
// taken, never who holds it.
func TestDeployOverAnotherTenantsNameIsRefusedOverHTTP(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping database-backed deploy ownership test")
	}

	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	// Non-superuser, for the reason spelled out in
	// TestTenantIsolationOverHTTP_Postgres: on a superuser connection RLS is
	// bypassed and this test would pass without proving anything.
	rlsDB := testutil.OpenPostgresRLSTestDB(t, db)
	factory := engine.NewPostgresStoreFactory(rlsDB, "public")

	const (
		tenantA = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
		tenantB = "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb"
	)
	ctx := context.Background()

	defaultStore, _, err := factory.OpenStore(ctx, engine.DefaultTenantUUID)
	if err != nil {
		t.Fatalf("open default store: %v", err)
	}
	api := &apiServer{
		store:       defaultStore,
		worker:      newTestWorker(&mockStore{}),
		maxBodySize: 1 << 20,
		factory:     factory,
		requireAuth: true,
	}
	mux := http.NewServeMux()
	registerRoutes(mux, api)
	// POST /api/definitions is wired inline in main.go rather than in
	// registerRoutes, so a mux built the usual way for tests does not have it
	// and every request 404s. Registered here explicitly; the split is worth
	// noticing, because it means the route this test covers is invisible to
	// every other registerRoutes-based test in this package.
	mux.HandleFunc("POST /api/definitions", api.handleCreateDefinition)

	deploy := func(tenant, name string, wasm []byte) (int, string) {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"name":              name,
			"version":           1,
			"wasm_bytes_base64": base64.StdEncoding.EncodeToString(wasm),
		})
		if err != nil {
			t.Fatalf("marshal deploy request: %v", err)
		}
		req := asTenant(httptest.NewRequest(http.MethodPost, "/api/definitions", strings.NewReader(string(body))), tenant)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}

	const defName = "http-owned-def"
	if code, body := deploy(tenantA, defName, []byte{0x00, 0x61, 0x73, 0x6d, 0xAA}); code != http.StatusCreated {
		t.Fatalf("tenant A's own deploy: status = %d, body = %s", code, body)
	}

	code, body := deploy(tenantB, defName, []byte{0x00, 0x61, 0x73, 0x6d, 0xBB})
	if code != http.StatusConflict {
		t.Errorf("tenant B deploying over tenant A's name: status = %d, want %d (409); body = %s",
			code, http.StatusConflict, body)
	}
	if strings.Contains(body, tenantA) {
		t.Errorf("the refusal names the owning tenant %s, which the caller has no business learning: %s",
			tenantA, body)
	}

	// And A's definition is intact, which is the property the status code is
	// only reporting on.
	storeA, _, err := factory.OpenStore(ctx, tenantA)
	if err != nil {
		t.Fatalf("open store for tenant A: %v", err)
	}
	defA, err := storeA.GetWorkflowDef(ctx, defName, 1)
	if err != nil {
		t.Fatalf("GetWorkflowDef(A): %v", err)
	}
	if defA == nil {
		t.Fatalf("tenant A's definition is gone after tenant B's refused deploy")
	}
	if len(defA.WASMBytes) == 0 || defA.WASMBytes[len(defA.WASMBytes)-1] != 0xAA {
		t.Errorf("tenant A's definition carries %#v, not A's own bytes", defA.WASMBytes)
	}
}
