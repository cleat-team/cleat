package main

// cleat#889: A/B version routing gets a way in.
//
// PickVersionByRouting has run on EVERY workflow start since routing was added
// and logs "A/B routing applied" when a rule fires. With no way to create a
// rule it always returned 0, so the branch was dead and the log line
// unreachable. SetRoutingRule, GetRoutingRules and RemoveRoutingRule all
// existed and none had a production caller.
//
// The read path is untouched by these tests. What is covered is the way in.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

func TestARoutingRuleCanBeCreated(t *testing.T) {
	var gotName string
	var gotVersion int
	var gotWeight float64
	ms := &mockStore{
		validateVersionFn: func(_ context.Context, _ string, _ int) (bool, error) { return true, nil },
		setRoutingRuleFn: func(_ context.Context, name string, version int, weight float64) error {
			gotName, gotVersion, gotWeight = name, version, weight
			return nil
		},
	}
	api := newTestAPIServer(ms)
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, httptest.NewRequest(http.MethodPost,
		"/api/workflows/checkout/routing", strings.NewReader(`{"target_version":2,"weight":0.25}`)))

	if rec.Code != 201 {
		t.Fatalf("creating a routing rule answered %d: %s", rec.Code, rec.Body.String())
	}
	if gotName != "checkout" || gotVersion != 2 || gotWeight != 0.25 {
		t.Errorf("store received (%q, %d, %v), want (checkout, 2, 0.25)",
			gotName, gotVersion, gotWeight)
	}
}

// TestAnOmittedWeightMeansAllTraffic pins the pointer, not a default. An
// omitted weight and an explicit 0 must not be the same thing: 0 is a
// legitimate way to park a rule without deleting it, and if omission collapsed
// to 0 every rule created without a weight would silently never fire -- which
// is the same "configured and inert" shape this whole issue is about.
func TestAnOmittedWeightMeansAllTrafficNotNone(t *testing.T) {
	var gotWeight float64 = -1
	ms := &mockStore{
		validateVersionFn: func(_ context.Context, _ string, _ int) (bool, error) { return true, nil },
		setRoutingRuleFn: func(_ context.Context, _ string, _ int, weight float64) error {
			gotWeight = weight
			return nil
		},
	}
	api := newTestAPIServer(ms)
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, httptest.NewRequest(http.MethodPost,
		"/api/workflows/checkout/routing", strings.NewReader(`{"target_version":2}`)))

	if rec.Code != 201 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if gotWeight != 1.0 {
		t.Errorf("an omitted weight reached the store as %v, want 1.0", gotWeight)
	}
}

func TestAnExplicitZeroWeightIsPreserved(t *testing.T) {
	gotWeight := -1.0
	ms := &mockStore{
		validateVersionFn: func(_ context.Context, _ string, _ int) (bool, error) { return true, nil },
		setRoutingRuleFn: func(_ context.Context, _ string, _ int, weight float64) error {
			gotWeight = weight
			return nil
		},
	}
	api := newTestAPIServer(ms)
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, httptest.NewRequest(http.MethodPost,
		"/api/workflows/checkout/routing", strings.NewReader(`{"target_version":2,"weight":0}`)))

	if rec.Code != 201 || gotWeight != 0 {
		t.Errorf("an explicit weight of 0 reached the store as %v (status %d); it must "+
			"survive, since 0 is how a rule is parked without deleting it", gotWeight, rec.Code)
	}
}

// TestRoutingToADeprecatedVersionIsRefused is the interesting refusal. The
// table's foreign key catches a version that does not exist, but as a 500 from
// a constraint violation rather than an answer -- and it cannot see deprecation
// at all. Routing live traffic to a version an operator has just deprecated is
// exactly the mistake worth refusing.
func TestRoutingToADeprecatedVersionIsRefused(t *testing.T) {
	called := false
	ms := &mockStore{
		validateVersionFn: func(_ context.Context, _ string, _ int) (bool, error) { return false, nil },
		setRoutingRuleFn: func(_ context.Context, _ string, _ int, _ float64) error {
			called = true
			return nil
		},
	}
	api := newTestAPIServer(ms)
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, httptest.NewRequest(http.MethodPost,
		"/api/workflows/checkout/routing", strings.NewReader(`{"target_version":9,"weight":1}`)))

	if rec.Code != 409 {
		t.Errorf("routing to a deprecated version answered %d, want 409: %s",
			rec.Code, rec.Body.String())
	}
	if called {
		t.Error("the rule was written anyway; the refusal must come before the store call")
	}
}

// TestRoutingRulesAreListableSoTheyCanBeRemoved is why the list endpoint is not
// optional. RemoveRoutingRule takes a rule ID, and nothing else in the API
// returns one -- so without this, a rule once created could never be deleted.
func TestRoutingRulesAreListableSoTheyCanBeRemoved(t *testing.T) {
	ms := &mockStore{
		getRoutingRulesFn: func(_ context.Context, name string) ([]engine.RoutingRule, error) {
			return []engine.RoutingRule{
				{ID: "rule-1", WorkflowName: name, TargetVersion: 2, Weight: 0.25},
			}, nil
		},
	}
	api := newTestAPIServer(ms)
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, httptest.NewRequest(http.MethodGet,
		"/api/workflows/checkout/routing", nil))

	if rec.Code != 200 {
		t.Fatalf("listing answered %d: %s", rec.Code, rec.Body.String())
	}
	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("listing did not return JSON: %v (%s)", err, rec.Body.String())
	}
	if len(out) != 1 || out[0]["id"] != "rule-1" {
		t.Fatalf("listing did not carry the rule id, so the rule cannot be removed: %s",
			rec.Body.String())
	}
}

// TestAnEmptyRoutingTableIsAnArrayNotNull — the normal state is no rules, and a
// caller iterating the response should not have to special-case null.
func TestAnEmptyRoutingTableIsAnArrayNotNull(t *testing.T) {
	api := newTestAPIServer(&mockStore{})
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, httptest.NewRequest(http.MethodGet,
		"/api/workflows/checkout/routing", nil))

	if body := strings.TrimSpace(rec.Body.String()); body != "[]" {
		t.Errorf("an empty routing table returned %q, want []", body)
	}
}

func TestARoutingRuleCanBeRemovedByID(t *testing.T) {
	var removed string
	ms := &mockStore{
		removeRoutingRuleFn: func(_ context.Context, ruleID string) error {
			removed = ruleID
			return nil
		},
	}
	api := newTestAPIServer(ms)
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, httptest.NewRequest(http.MethodDelete,
		"/api/workflows/checkout/routing/rule-1", nil))

	if rec.Code != 200 {
		t.Fatalf("removing answered %d: %s", rec.Code, rec.Body.String())
	}
	if removed != "rule-1" {
		t.Errorf("store was asked to remove %q, want rule-1", removed)
	}
}
