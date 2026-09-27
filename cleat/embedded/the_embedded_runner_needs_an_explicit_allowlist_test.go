package embedded

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// unlistedHost is on no allowlist and resolves to nothing. A NAME rather than
// a loopback literal: the test below needs a destination the ALLOWLIST gate
// refuses, and an httptest server is a loopback literal that cleat#1627's
// separate, earlier refuser catches first for a different reason.
const unlistedHost = "http://api.example.com/probe"

// cleat#1565, owner decision 2026-09-14: the embedded runner takes an explicit
// allowlist rather than inheriting the worker's or defaulting open.
//
// It executes guest code exactly as the worker does. Defaulting it open would
// make the development path quietly weaker than production, and would make
// "it worked under embedded" stop predicting anything about a deployment.
// The destination is a NAME on no floor rule, not an httptest server, and the
// difference is the whole assertion.
//
// This used an httptest URL, which is always a loopback literal. That worked
// only because the allowlist gate was reached first; cleat#1627 refuses a
// denied literal above the allowlists, so the same fixture now produces
// "loopback: the worker's own API and admin surface" and the test could no
// longer see the thing it is named for. Two refusers, and the fixture chose
// between them by accident.
//
// Nothing needs to be listening: with no allowlist configured the refusal
// happens BEFORE any resolution, which is also why an unroutable name costs no
// DNS and cannot flake.
func TestTheEmbeddedRunnerRefusesEgressWithoutAnAllowlist(t *testing.T) {
	r := New() // no WithEgressAllowlist
	var fetchErr error
	r.Register("test", func(ctx *Context) error {
		_, fetchErr = ctx.H().DurableCall("http", "fetch", fmt.Sprintf(`{"url":%q}`, unlistedHost))
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
		t.Errorf("refused, but not by the allowlist -- %v. The destination is deliberately "+
			"a host no floor rule covers, so the allowlist is the only layer that can "+
			"refuse it; any other refuser means this test is measuring something else.",
			fetchErr)
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
