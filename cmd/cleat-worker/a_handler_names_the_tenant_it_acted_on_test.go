package main

// cleat#3559. The audit log puts an operator's row on the chain of the tenant the HANDLER says it
// acted on (auth.NoteAuditTarget), so the real admin handlers have to say it, and say it only when
// there is a tenant that was acted on. This drives them through composeAuthChain with a stand-in
// for the audit middleware that allocates the slot and reads it back.
//
// Why the handler and not the path: cleat-review measured both ways the path was wrong. The id-only
// routes (/api/admin/instances/<id>/...) act on the tenant the header: resolver put in the context
// and name none in their path, so the row moved off that tenant's chain; and a path naming a UUID
// that is no tenant would have started a chain for it.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
)

func slotAround(noted *notedTarget) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, slot := auth.WithAuditTarget(r.Context())
			next.ServeHTTP(w, r.WithContext(ctx))
			noted.tenant, noted.set = slot.Tenant()
		})
	}
}

func TestAnAdminHandlerNamesTheTenantItActedOn(t *testing.T) {
	tenantBID := uuid.MustParse(tenantB)
	nobody := uuid.MustParse("55555555-5555-5555-5555-555555555555") // a tenant no factory knows

	// withHeaderTenant is what the header: tenant resolver does: it puts a client-chosen tenant in
	// the context of every request, outside the auth chain.
	withHeaderTenant := func(h http.Handler, tid uuid.UUID) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(w, r.WithContext(auth.WithTenantID(r.Context(), tid)))
		})
	}

	t.Run("a tenant-named route, workflow found: that tenant", func(t *testing.T) {
		f := buildChainAround(t, false, slotAround)
		w := f.do(http.MethodPost, adminTenantPath(tenantB, "wf-b", "force-complete"), f.adminKey, "X-Confirm", "force-complete")
		if w.Code != http.StatusOK {
			t.Fatalf("status %d, want 200: %s", w.Code, w.Body.String())
		}
		if !f.noted.set || f.noted.tenant != tenantBID {
			t.Errorf("the handler noted %v (set=%v), want tenant B %v", f.noted.tenant, f.noted.set, tenantBID)
		}
	})

	t.Run("a tenant-named route, workflow NOT found: nothing is noted", func(t *testing.T) {
		f := buildChainAround(t, false, slotAround)
		f.storeB.getWorkflowByIDFn = func(_ context.Context, _ string) (*engine.WorkflowInstance, error) { return nil, nil }
		w := f.do(http.MethodPost, adminTenantPath(tenantB, "wf-missing", "force-complete"), f.adminKey, "X-Confirm", "force-complete")
		if w.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", w.Code)
		}
		if f.noted.set {
			t.Errorf("a 404 noted tenant %v: the audit log would record on a tenant whose workflow was never found", f.noted.tenant)
		}
	})

	t.Run("a tenant UUID that is no tenant: nothing is noted, so no chain is started for it", func(t *testing.T) {
		f := buildChainAround(t, false, slotAround)
		w := f.do(http.MethodPost, adminTenantPath(nobody.String(), "wf-1", "force-complete"), f.adminKey, "X-Confirm", "force-complete")
		if w.Code == http.StatusOK {
			t.Fatalf("a tenant nobody knows got 200")
		}
		if f.noted.set {
			t.Errorf("an unknown tenant %v was noted as the target", f.noted.tenant)
		}
	})

	t.Run("the id-only route with a header tenant: that tenant, as before the operator was attributed", func(t *testing.T) {
		f := buildChainAround(t, false, slotAround)
		f.storeB.getWorkflowByIDFn = func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
			return &engine.WorkflowInstance{ID: id, TenantID: tenantB}, nil
		}
		handler := withHeaderTenant(f.handler, tenantBID)
		req := httptest.NewRequest(http.MethodPost, "/api/admin/instances/wf-b/force-complete", strings.NewReader(`{"generation":3,"result":"{}"}`))
		req.Header.Set("Authorization", "Bearer "+f.adminKey)
		req.Header.Set("X-Confirm", "force-complete")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d, want 200 (reviewer measured the same): %s", w.Code, w.Body.String())
		}
		if !f.noted.set || f.noted.tenant != tenantBID {
			t.Errorf("the handler noted %v (set=%v), want the header tenant B %v", f.noted.tenant, f.noted.set, tenantBID)
		}
	})

	t.Run("the id-only route with no tenant at all: refused, nothing noted", func(t *testing.T) {
		f := buildChainAround(t, false, slotAround)
		w := f.do(http.MethodPost, "/api/admin/instances/wf-b/force-complete", f.adminKey, "X-Confirm", "force-complete")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status %d, want 401", w.Code)
		}
		if f.noted.set {
			t.Errorf("a refused request noted tenant %v", f.noted.tenant)
		}
	})

	t.Run("a route that names no tenant: nothing is noted", func(t *testing.T) {
		f := buildChainAround(t, false, slotAround)
		if w := f.do(http.MethodGet, "/api/admin/health", f.adminKey); w.Code != http.StatusOK {
			t.Fatalf("status %d, want 200", w.Code)
		}
		if f.noted.set {
			t.Errorf("/api/admin/health noted tenant %v", f.noted.tenant)
		}
	})

}
