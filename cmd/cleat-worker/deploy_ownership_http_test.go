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

// TestDeployOfANameAnotherTenantHoldsSucceedsOverHTTP is the HTTP half of
// IMPROVEMENT-PLAN 3.12.
//
// The store-level property is tested in engine (two tenants each hold their own
// definition of one name). This is the layer a customer actually meets, and the
// reason it needs its own test is the one §1.7 established: a correct store
// reached through a handler that reports the wrong thing is still a wrong
// product.
//
// **This test used to assert a 409**, and that was right while
// workflow_defs was keyed by (name, version): a name another tenant held was an
// ordinary client situation that the endpoint reported as a 500, which would
// have paged whoever owns the alerts and told the caller nothing actionable.
//
// D7 (IMPROVEMENT-PLAN 3.77) put the tenant in the key, so there is no conflict
// left to report. The assertion inverts rather than relaxes: B's deploy must
// SUCCEED, and both tenants must then read back their own bytes. The old
// "must not name the owning tenant" check is gone with it -- nothing is refused,
// so there is no refusal to leak through.
func TestDeployOfANameAnotherTenantHoldsSucceedsOverHTTP(t *testing.T) {
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

	if code, body := deploy(tenantB, defName, []byte{0x00, 0x61, 0x73, 0x6d, 0xBB}); code != http.StatusCreated {
		t.Errorf("tenant B deploying a name tenant A also uses: status = %d, want %d (201); body = %s",
			code, http.StatusCreated, body)
	}

	// And A's definition is untouched, which is the property the status code is
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
		t.Fatalf("tenant A's definition is gone after tenant B deployed the same name")
	}
	if len(defA.WASMBytes) == 0 || defA.WASMBytes[len(defA.WASMBytes)-1] != 0xAA {
		t.Errorf("tenant A's definition carries %#v, not A's own bytes", defA.WASMBytes)
	}

	// And the direction that only became assertable with D7: B has its own
	// definition, not a view of A's and not nothing.
	storeB, _, err := factory.OpenStore(ctx, tenantB)
	if err != nil {
		t.Fatalf("open store for tenant B: %v", err)
	}
	defB, err := storeB.GetWorkflowDef(ctx, defName, 1)
	if err != nil {
		t.Fatalf("GetWorkflowDef(B): %v", err)
	}
	if defB == nil {
		t.Fatalf("tenant B's own definition is missing after a 201")
	}
	if len(defB.WASMBytes) == 0 || defB.WASMBytes[len(defB.WASMBytes)-1] != 0xBB {
		t.Errorf("tenant B reads %#v, want its own bytes ending 0xBB", defB.WASMBytes)
	}
}
