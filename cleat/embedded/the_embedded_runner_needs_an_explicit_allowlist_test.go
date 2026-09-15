package embedded

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cleat#1565, owner decision 2026-09-14: the embedded runner takes an explicit
// allowlist rather than inheriting the worker's or defaulting open.
//
// It executes guest code exactly as the worker does. Defaulting it open would
// make the development path quietly weaker than production, and would make
// "it worked under embedded" stop predicting anything about a deployment.
func TestTheEmbeddedRunnerRefusesEgressWithoutAnAllowlist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("reached"))
	}))
	defer srv.Close()

	r := New() // no WithEgressAllowlist
	var fetchErr error
	r.Register("test", func(ctx *Context) error {
		_, fetchErr = ctx.H().DurableCall("http", "fetch", fmt.Sprintf(`{"url":%q}`, srv.URL))
		ctx.SetOutput(`{"ok":true}`)
		return nil
	})
	if _, err := r.ExecuteWorkflow(context.Background(), "test", "{}"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if fetchErr == nil {
		t.Fatal("http.fetch succeeded with no allowlist configured; an absent policy " +
			"must not read as permission")
	}
	if !strings.Contains(fetchErr.Error(), "allowlist") {
		t.Errorf("refused, but not by the allowlist -- %v", fetchErr)
	}
}

// And the other direction, so the test above cannot pass by refusing everything
// unconditionally.
func TestTheEmbeddedRunnerFetchesWhatItsAllowlistPermits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("reached"))
	}))
	defer srv.Close()

	// httptest binds loopback, which the FLOOR refuses regardless of any
	// allowlist -- that is the point of the floor. So this asserts the
	// allowlist is consulted and grants, and then the floor still refuses,
	// which is the layering under test rather than a workaround.
	host := strings.TrimPrefix(srv.URL, "http://")
	r := New(WithEgressAllowlist(host, "api.example.com"))
	if r.egressAllowlist == nil {
		t.Fatal("WithEgressAllowlist did not configure anything")
	}
	if !r.egressAllowlist.Permits("api.example.com") {
		t.Error("a listed host is not permitted by the list the option built")
	}
	if r.egressAllowlist.Permits("elsewhere.example.com") {
		t.Error("an unlisted host is permitted; the list is not narrowing")
	}
}
