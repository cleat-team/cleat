package main

// cleat#889: deployment-channel tags get a writer.
//
// A tag maps (workflow_name, tag) -> version. THE READER IS ALREADY LIVE:
// engine/children.go:43, :75 and :90 resolve tags when a child workflow starts,
// including a default "stable" lookup. SetWorkflowTag had no production caller,
// so no tag could ever be created and that resolution always missed.
//
// #889 described tags as "inert in both directions ... no live reader implying
// a live writer". That is wrong on the reader, and it made this look like the
// mildest of the four when it is the same shape as A/B routing.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestATagCanBePointedAtAVersion(t *testing.T) {
	var gotName, gotTag string
	var gotVersion int
	ms := &mockStore{
		validateVersionFn: func(_ context.Context, _ string, _ int) (bool, error) { return true, nil },
		setWorkflowTagFn: func(_ context.Context, name string, version int, tag string) error {
			gotName, gotVersion, gotTag = name, version, tag
			return nil
		},
	}
	api := newTestAPIServer(ms)
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, httptest.NewRequest(http.MethodPut,
		"/api/workflows/checkout/tags", strings.NewReader(`{"tag":"stable","version":3}`)))

	if rec.Code != 200 {
		t.Fatalf("setting a tag answered %d: %s", rec.Code, rec.Body.String())
	}
	if gotName != "checkout" || gotTag != "stable" || gotVersion != 3 {
		t.Errorf("store received (%q, %d, %q), want (checkout, 3, stable)",
			gotName, gotVersion, gotTag)
	}
}

// TestPointingATagAtADeprecatedVersionIsRefused matters more here than for A/B
// routing, and the reason is worth stating. A routing rule only affects starts
// that match it; a tag affects the children of every workflow whose binding
// policy is "stable" -- which `cleat build` selects on the author's behalf
// whenever --db or CLEAT_DATABASE_URL is set (cmd/cleat/main.go:134). So a
// wrong version here redirects runs whose authors never asked for a tag.
func TestPointingATagAtADeprecatedVersionIsRefused(t *testing.T) {
	called := false
	ms := &mockStore{
		validateVersionFn: func(_ context.Context, _ string, _ int) (bool, error) { return false, nil },
		setWorkflowTagFn: func(_ context.Context, _ string, _ int, _ string) error {
			called = true
			return nil
		},
	}
	api := newTestAPIServer(ms)
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, httptest.NewRequest(http.MethodPut,
		"/api/workflows/checkout/tags", strings.NewReader(`{"tag":"stable","version":9}`)))

	if rec.Code != 409 {
		t.Errorf("pointing a tag at a deprecated version answered %d, want 409: %s",
			rec.Code, rec.Body.String())
	}
	if called {
		t.Error("the tag was written anyway; the refusal must come before the store call")
	}
}

func TestATagRequiresBothANameAndAVersion(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no tag", `{"version":3}`},
		{"empty tag", `{"tag":"","version":3}`},
		{"no version", `{"tag":"stable"}`},
		{"zero version", `{"tag":"stable","version":0}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newTestAPIServer(&mockStore{})
			rec := httptest.NewRecorder()
			api.handleWorkflows(rec, httptest.NewRequest(http.MethodPut,
				"/api/workflows/checkout/tags", strings.NewReader(tc.body)))
			if rec.Code != 400 {
				t.Errorf("%s answered %d, want 400: %s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestTagsAreListableSoAnOperatorCanSeeWhereStablePoints is the read that makes
// the writer usable. Without it an operator cannot tell what "stable" currently
// resolves to, which is the one question they will have.
func TestTagsAreListableSoAnOperatorCanSeeWhereStablePoints(t *testing.T) {
	ms := &mockStore{
		listWorkflowDefsFn: deployedDef("checkout"),
		getWorkflowTagsFn: func(_ context.Context, _ string) (map[string]int, error) {
			return map[string]int{"stable": 3, "canary": 4}, nil
		},
	}
	api := newTestAPIServer(ms)
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, httptest.NewRequest(http.MethodGet,
		"/api/workflows/checkout/tags", nil))

	if rec.Code != 200 {
		t.Fatalf("listing answered %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("listing did not return a JSON object: %v (%s)", err, rec.Body.String())
	}
	if out["stable"] != 3 || out["canary"] != 4 {
		t.Errorf("listing did not carry the mappings: %s", rec.Body.String())
	}
}

// TestNoTagsIsAnObjectNotNull — most definitions have none, and a caller
// reading the response should not have to special-case null.
//
// The CONTROL for cleat#942 on the tags path; see the routing one for why an
// empty result for a DEPLOYED name must stay 200.
func TestNoTagsIsAnObjectNotNull(t *testing.T) {
	api := newTestAPIServer(&mockStore{listWorkflowDefsFn: deployedDef("checkout")})
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, httptest.NewRequest(http.MethodGet,
		"/api/workflows/checkout/tags", nil))

	if body := strings.TrimSpace(rec.Body.String()); body != "{}" {
		t.Errorf("a definition with no tags returned %q, want {}", body)
	}
}

func TestATagCanBeRemoved(t *testing.T) {
	var removedName, removedTag string
	ms := &mockStore{
		removeWorkflowTagFn: func(_ context.Context, name, tag string) error {
			removedName, removedTag = name, tag
			return nil
		},
	}
	api := newTestAPIServer(ms)
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, httptest.NewRequest(http.MethodDelete,
		"/api/workflows/checkout/tags/canary", nil))

	if rec.Code != 200 {
		t.Fatalf("removing answered %d: %s", rec.Code, rec.Body.String())
	}
	if removedName != "checkout" || removedTag != "canary" {
		t.Errorf("store was asked to remove (%q, %q), want (checkout, canary)",
			removedName, removedTag)
	}
}
