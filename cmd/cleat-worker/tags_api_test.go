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

// TestDeletingATagOnAnUnknownDefinitionAnswers200 pins the one path cleat#942's
// enumeration named and cleat#945 deliberately did not change.
//
//	DELETE /api/workflows/<never-deployed>/tags/stable  ->  200
//
// THIS TEST DECIDES NOTHING. It records which convention is in force so that
// changing it is a decision someone makes rather than one that happens.
//
// The case FOR leaving it: DELETE is conventionally idempotent, and removing a
// tag that is not there is a no-op success. A 404 changes that.
//
// The case AGAINST: the thing that does not exist is the DEFINITION, not the
// tag. "The tag was removed" and "there is no such workflow" share one
// response, so a caller who typos the workflow name is told the deletion
// succeeded. #945 has already decided that an unknown definition is a 404 on
// the name-scoped reads, and the writes answer 409. This is the last path where
// an unknown definition is silently fine.
//
// WHY IT LIVES HERE AND NOT ONLY IN THE PORT. cleat-ports pins this too, in
// ports/samples-go/tests/identifier_test.go, and that pin is where the question
// was first written down. But the port does not gate merges in this repo --
// docs/promotion-checklist.md is explicit that a finding is protected only once
// it is a hermetic test in cleat-team/cleat. Without this, someone finishing
// #945's job for consistency changes handleRemoveWorkflowTag, every test here
// passes, and the port tells them later and elsewhere. That is a live risk
// rather than a hypothetical one: making the reads 404 is exactly the change
// that invites making this one 404 too.
//
// It FAILS on 404 rather than accepting either answer. A test that passed on
// both would pin nothing -- it could not fail, which is the shape this repo
// keeps finding. If you are reading this because it went red, the convention
// has been decided the other way: assert 404 here, update the port's pin, and
// record the decision in that repo's ports/samples-go/ISSUES.md #8.
func TestDeletingATagOnAnUnknownDefinitionAnswers200(t *testing.T) {
	var asked bool
	ms := &mockStore{
		listWorkflowDefsFn: deployedDef("checkout"), // "unknown" is any other name
		removeWorkflowTagFn: func(_ context.Context, name, tag string) error {
			asked = true
			return nil
		},
	}
	api := newTestAPIServer(ms)
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, httptest.NewRequest(http.MethodDelete,
		"/api/workflows/never-deployed/tags/stable", nil))

	switch rec.Code {
	case 200:
		// Current behaviour. The store was still asked, which is the part that
		// makes this idempotent rather than merely quiet.
		if !asked {
			t.Error("answered 200 without asking the store to remove anything, so the " +
				"idempotency is a short-circuit rather than a real no-op delete")
		}
	case 404:
		t.Errorf("DELETE on an unknown definition now answers 404, so the idempotency " +
			"question has been decided the other way.\n\n" +
			"That may well be right -- see the two cases above. Update this test to " +
			"assert 404, update the pin in cleat-ports " +
			"(ports/samples-go/tests/identifier_test.go), and record the decision in " +
			"that repo's ports/samples-go/ISSUES.md #8.")
	default:
		t.Errorf("DELETE on an unknown definition answered %d: %s -- neither convention",
			rec.Code, rec.Body.String())
	}
}
