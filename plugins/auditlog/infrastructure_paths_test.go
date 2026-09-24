package auditlog

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The probes a kubelet, a load balancer and a scraper make every few seconds are not audit events
// (cleat#2007, owner decision 5A), and everything else is, including the authenticated health route.
func TestInfrastructurePathsAreNotAudited(t *testing.T) {
	p := &Plugin{buffer: make(chan queuedAuditEvent, 64)}
	handler := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	serve := func(method, path string) {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil))
	}

	for _, path := range []string{"/healthz", "/livez", "/readyz", "/metrics"} {
		serve(http.MethodGet, path)
		serve(http.MethodHead, path)
	}
	if got := p.q.sent.Load(); got != 0 || len(p.buffer) != 0 {
		t.Fatalf("the infrastructure paths produced %d audit events (%d queued), want none", got, len(p.buffer))
	}

	// The known-positive: paths that look like them but are not on the list, and the authenticated
	// health route, ARE recorded, so the zero above is not a handler that records nothing.
	for _, path := range []string{"/api/admin/health", "/api/workflows", "/healthz/", "/readyz/verbose", "/livezx"} {
		serve(http.MethodGet, path)
	}
	if got := p.q.sent.Load(); got != 5 || len(p.buffer) != 5 {
		t.Errorf("the other paths produced %d audit events (%d queued), want 5", got, len(p.buffer))
	}
}
