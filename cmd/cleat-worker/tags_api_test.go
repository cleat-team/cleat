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
		listWorkflowDefsFn: deployedDef("checkout"),
		validateVersionFn:  func(_ context.Context, _ string, _ int) (bool, error) { return true, nil },
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
		listWorkflowDefsFn: deployedDef("checkout"),
		validateVersionFn:  func(_ context.Context, _ string, _ int) (bool, error) { return false, nil },
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
			// The definition must exist, or the request is refused 404 by the
			// existence check before the body is read and every case below
			// would pass for the wrong reason (cleat#3003).
			api := newTestAPIServer(&mockStore{listWorkflowDefsFn: deployedDef("checkout")})
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
		listWorkflowDefsFn: deployedDef("checkout"),
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

// TestDeletingATagOnAnUnknownDefinitionIs404 records the decision this path
// carried an open question about until cleat#3003, and pins the answer.
//
//	DELETE /api/workflows/<never-deployed>/tags/stable  ->  404
//
// This test used to pin the OTHER answer -- 200, on the idempotency argument --
// and its own comment listed the two cases and said that changing it would be a
// decision someone makes rather than one that happens. This is that decision
// being made, so the comment is kept as the record rather than deleted.
//
// What decided it. The thing that does not exist is the DEFINITION, not the
// tag, and "the tag was removed" and "there is no such workflow" sharing one
// response means a caller who typos the workflow name is told the deletion
// succeeded. #945 had already made the reads 404 and the writes 409, and this
// was the last path where an unknown definition was silently fine.
//
// The idempotency argument is real and is not what it looks like here: it is an
// argument about repeating a DELETE on a definition that EXISTS, and that case
// is unaffected -- handleRemoveWorkflowTag still answers 200 for a tag that is
// not there on a definition that is.
//
// # Why 404 and not 409
//
// The writes answered 409 for an unknown name until cleat#3003, and this path
// inherited that framing. 409 was wrong for it: a name that was never deployed
// is an identifier that resolves to nothing, which is 404 everywhere else in
// this API, and it is also the observable difference from an `internal` name's
// 404. A caller could tell "something is here and it is refused" from "nothing
// is here" by the status alone, which is exactly what `internal` answering 404
// instead of 403 exists to prevent. See
// an_internal_definition_is_not_reachable_over_http_test.go.
//
// # The pin in cleat-ports still says 200
//
// ports/samples-go/tests/identifier_test.go is where this question was first
// written down, and it is a different repository: it was not updated in the
// same change and needs a follow-up (filed as an issue when cleat#3003 landed).
// A reader who finds that pin disagreeing with this test should treat this one
// as authoritative -- docs/promotion-checklist.md is explicit that a finding is
// protected only once it is a hermetic test in cleat-team/cleat.
func TestDeletingATagOnAnUnknownDefinitionIs404(t *testing.T) {
	asked := false
	ms := &mockStore{
		listWorkflowDefsFn: deployedDef("checkout"), // "never-deployed" is any other name
		removeWorkflowTagFn: func(_ context.Context, _, _ string) error {
			asked = true
			return nil
		},
	}
	api := newTestAPIServer(ms)
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, httptest.NewRequest(http.MethodDelete,
		"/api/workflows/never-deployed/tags/stable", nil))

	if rec.Code != 404 {
		t.Fatalf("DELETE on an unknown definition answered %d, want 404: %s\n\n"+
			"A 200 here is the last path on which an unknown definition is silently "+
			"fine, and it is distinguishable from an internal one's 404.",
			rec.Code, rec.Body.String())
	}
	if asked {
		t.Error("the store was asked to remove a tag from a definition that does not " +
			"exist, so the refusal did not come before the write")
	}
}
