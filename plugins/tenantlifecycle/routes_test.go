// Behavioural tests for GET /api/tenant/lifecycle, run against real database
// backends (PostgreSQL always, MySQL and MSSQL when their CLEAT_TEST_* DSNs
// are set) via testutil.NewPluginTestBackends, the same harness
// background_multidb_test.go uses.
package tenantlifecycle

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

func newTestPlugin(t *testing.T, be testutil.PluginTestBackend) *Plugin {
	t.Helper()
	p := &Plugin{}
	ctx := context.Background()
	pluginDialect := plugin.Dialect(string(be.Dialect))

	loaded := []*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}
	if err := plugin.RunMigrations(ctx, be.DB, pluginDialect, nil, loaded); err != nil {
		t.Fatalf("RunMigrations for backend %q: %v", be.Name, err)
	}

	p.dialect = pluginDialect
	p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: pluginDialect}
	p.logger = slog.Default()
	p.env = &plugin.Environment{Dialect: pluginDialect}
	return p
}

func doGetLifecycle(p *Plugin, tenantID uuid.UUID) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/api/tenant/lifecycle", nil)
	r = r.WithContext(auth.WithTenantID(r.Context(), tenantID))
	w := httptest.NewRecorder()
	p.handleGetLifecycleStatus(w, r)
	return w
}

// TestGetLifecycleStatus_HasTrial covers the ordinary case: a tenant with a
// trial in flight reads back exactly its own expiry and handled state.
func TestGetLifecycleStatus_HasTrial(t *testing.T) {
	backends := testutil.NewPluginTestBackends(t)
	for _, be := range backends {
		t.Run(be.Name, func(t *testing.T) {
			t.Cleanup(be.Cleanup)
			p := newTestPlugin(t, be)

			tenantID := uuid.New()
			expiresAt := time.Now().Add(48 * time.Hour)
			insertTrial(t, p, tenantID, expiresAt)

			w := doGetLifecycle(p, tenantID)
			if w.Code != http.StatusOK {
				t.Fatalf("UNMEASURED: handler returned %d: %s", w.Code, w.Body.String())
			}

			var resp lifecycleStatusResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v\nbody: %s", err, w.Body.String())
			}
			if !resp.HasTrial {
				t.Fatalf("has_trial: got false, want true")
			}
			if resp.Handled {
				t.Fatalf("handled: got true, want false (fixture is unhandled)")
			}
			// Round-trip through JSON truncates sub-second precision on some
			// dialects (MySQL's DATETIME(6) keeps microseconds, but the JSON
			// wire format and time.Time's own comparison do not agree on
			// monotonic readings) -- compare with a generous tolerance rather
			// than requiring exact equality, per this repo's own lesson on
			// asserting round-trips rather than raw clock comparisons.
			if d := resp.ExpiresAt.Sub(expiresAt); d < -time.Second || d > time.Second {
				t.Fatalf("expires_at: got %v, want ~%v (delta %v)", resp.ExpiresAt, expiresAt, d)
			}
		})
	}
}

// TestGetLifecycleStatus_NoTrial covers the tenant with no tenant_trials row
// at all -- must be a 200 with has_trial:false, not a 404 or a 500. A tenant
// that has never had a trial set is an ordinary state, not an error.
func TestGetLifecycleStatus_NoTrial(t *testing.T) {
	backends := testutil.NewPluginTestBackends(t)
	for _, be := range backends {
		t.Run(be.Name, func(t *testing.T) {
			t.Cleanup(be.Cleanup)
			p := newTestPlugin(t, be)

			w := doGetLifecycle(p, uuid.New())
			if w.Code != http.StatusOK {
				t.Fatalf("UNMEASURED: handler returned %d: %s", w.Code, w.Body.String())
			}

			var resp lifecycleStatusResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v\nbody: %s", err, w.Body.String())
			}
			if resp.HasTrial {
				t.Fatalf("has_trial: got true, want false for a tenant with no trial row")
			}
		})
	}
}

// TestGetLifecycleStatus_NoTenant covers the unauthenticated case: no tenant
// resolved onto the request context at all (matching what a route with no
// valid API key actually sees, per auth.TenantIDFromRequest's own doc
// comment) must refuse with 401, not fall through to a nil-UUID query.
func TestGetLifecycleStatus_NoTenant(t *testing.T) {
	p := &Plugin{
		dialect: plugin.DialectPostgres,
		logger:  slog.Default(),
		env:     &plugin.Environment{Dialect: plugin.DialectPostgres},
	}
	r := httptest.NewRequest(http.MethodGet, "/api/tenant/lifecycle", nil)
	w := httptest.NewRecorder()
	p.handleGetLifecycleStatus(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 for a request with no tenant on its context", w.Code)
	}
}

// TestGetLifecycleStatus_NeverSeesAnotherTenantsTrial is the cross-tenant
// refusal test cleat#2534's Stage 3 scope requires, run on all three
// dialects. Two tenants, each with its own trial; tenant A's request must
// read only tenant A's row, never tenant B's, regardless of which one
// expires first or which was inserted first.
//
// FALSIFIED by widening queryOwnTrial's WHERE to `tenant_id = $1 OR 1=1`:
// PostgreSQL and MySQL both failed for the right reason ("tenant B got
// tenant A's trial"). MSSQL's own arm stayed green even under that
// mutation -- not a blind spot in this test, but a second, independent
// backstop: background.go documents that SQL Server's security policy has
// no owner-bypass (unlike PostgreSQL's, which does not constrain the table
// owner by default -- NewPluginTestBackends' connections are the owner on
// every dialect, so this test's PostgreSQL/MySQL arms exercise this
// handler's own WHERE clause, not RLS). Recorded here so a future reader
// re-falsifying this test does not mistake the MSSQL pass for the test
// failing to falsify.
func TestGetLifecycleStatus_NeverSeesAnotherTenantsTrial(t *testing.T) {
	backends := testutil.NewPluginTestBackends(t)
	for _, be := range backends {
		t.Run(be.Name, func(t *testing.T) {
			t.Cleanup(be.Cleanup)
			p := newTestPlugin(t, be)

			tenantA := uuid.New()
			tenantB := uuid.New()
			expiresA := time.Now().Add(24 * time.Hour)
			expiresB := time.Now().Add(72 * time.Hour)
			insertTrial(t, p, tenantA, expiresA)
			insertTrial(t, p, tenantB, expiresB)

			wA := doGetLifecycle(p, tenantA)
			if wA.Code != http.StatusOK {
				t.Fatalf("UNMEASURED: tenant A's request returned %d: %s", wA.Code, wA.Body.String())
			}
			var respA lifecycleStatusResponse
			if err := json.Unmarshal(wA.Body.Bytes(), &respA); err != nil {
				t.Fatalf("decode tenant A response: %v\nbody: %s", err, wA.Body.String())
			}
			if !respA.HasTrial {
				t.Fatalf("tenant A: has_trial got false, want true")
			}
			if d := respA.ExpiresAt.Sub(expiresA); d < -time.Second || d > time.Second {
				t.Fatalf("tenant A got tenant B's (or no) trial: expires_at %v, want ~%v (delta %v)",
					respA.ExpiresAt, expiresA, d)
			}

			wB := doGetLifecycle(p, tenantB)
			if wB.Code != http.StatusOK {
				t.Fatalf("UNMEASURED: tenant B's request returned %d: %s", wB.Code, wB.Body.String())
			}
			var respB lifecycleStatusResponse
			if err := json.Unmarshal(wB.Body.Bytes(), &respB); err != nil {
				t.Fatalf("decode tenant B response: %v\nbody: %s", err, wB.Body.String())
			}
			if !respB.HasTrial {
				t.Fatalf("tenant B: has_trial got false, want true")
			}
			if d := respB.ExpiresAt.Sub(expiresB); d < -time.Second || d > time.Second {
				t.Fatalf("tenant B got tenant A's trial: expires_at %v, want ~%v (delta %v)",
					respB.ExpiresAt, expiresB, d)
			}

			// A third identity that never had a trial set must see neither --
			// confirms the isolation is per-tenant-id, not "any authenticated
			// caller sees the first row found".
			wC := doGetLifecycle(p, uuid.New())
			if wC.Code != http.StatusOK {
				t.Fatalf("UNMEASURED: tenant C's request returned %d: %s", wC.Code, wC.Body.String())
			}
			var respC lifecycleStatusResponse
			if err := json.Unmarshal(wC.Body.Bytes(), &respC); err != nil {
				t.Fatalf("decode tenant C response: %v\nbody: %s", err, wC.Body.String())
			}
			if respC.HasTrial {
				t.Fatalf("tenant C (no trial ever set) got has_trial:true -- leaked another tenant's row")
			}
		})
	}
}
