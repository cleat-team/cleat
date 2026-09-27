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
	"io"
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
		listWorkflowDefsFn: deployedDef("checkout"),
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
//
// This is the CONTROL for cleat#942, not an incidental shape assertion. An
// unknown name now answers 404, and the over-broad version of that change --
// 404 whenever the collection is empty -- passes the unknown-name test and
// breaks every caller polling a definition that simply has no routing yet.
// The definition is deployed here so the empty result means "deployed, no
// rules" and nothing else.
func TestAnEmptyRoutingTableIsAnArrayNotNull(t *testing.T) {
	api := newTestAPIServer(&mockStore{listWorkflowDefsFn: deployedDef("checkout")})
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

// ---- cleat#942: the name-scoped reads and the writes on the same path
// disagreed about an unknown definition name ----

// deployedDef makes ListWorkflowDefs answer as though one version of name has
// been deployed, and nothing for any other name. The name is checked rather
// than ignored, because the whole defect is a handler not checking it: a stub
// that answers "exists" for every input would make defExists untestable and
// every test below vacuous.
func deployedDef(name string) func(context.Context, string) ([]engine.WorkflowDef, error) {
	return func(_ context.Context, got string) ([]engine.WorkflowDef, error) {
		if got != name {
			return nil, nil
		}
		return []engine.WorkflowDef{{Name: name, Version: 1}}, nil
	}
}

func TestListingRoutingForANameThatWasNeverDeployedIs404(t *testing.T) {
	api := newTestAPIServer(&mockStore{listWorkflowDefsFn: deployedDef("checkout")})
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, httptest.NewRequest(http.MethodGet,
		"/api/workflows/never-deployed/routing", nil))

	if rec.Code != 404 {
		t.Fatalf("listing routing for an undeployed name answered %d, want 404: %s",
			rec.Code, rec.Body.String())
	}
}

func TestListingTagsForANameThatWasNeverDeployedIs404(t *testing.T) {
	api := newTestAPIServer(&mockStore{listWorkflowDefsFn: deployedDef("checkout")})
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, httptest.NewRequest(http.MethodGet,
		"/api/workflows/never-deployed/tags", nil))

	if rec.Code != 404 {
		t.Fatalf("listing tags for an undeployed name answered %d, want 404: %s",
			rec.Code, rec.Body.String())
	}
}

// TestARunIDInTheNamePositionIsNotFound is the case the samples-go port
// tripped over, and the reason this is worth a status code rather than a
// documentation note.
//
// /api/workflows/{id}/... and /api/workflows/{name}/... share a path segment
// and are told apart only by the suffix. The two namespaces never overlap, so
// a caller that builds the URL from the wrong variable is always wrong -- and
// used to get 200 with an empty collection, byte-identical to a deployed
// definition with no rules.
func TestARunIDInTheNamePositionIsNotFound(t *testing.T) {
	const runID = "00000000-0000-0000-0000-000000000000"
	api := newTestAPIServer(&mockStore{listWorkflowDefsFn: deployedDef("checkout")})

	for _, suffix := range []string{"routing", "tags"} {
		rec := httptest.NewRecorder()
		api.handleWorkflows(rec, httptest.NewRequest(http.MethodGet,
			"/api/workflows/"+runID+"/"+suffix, nil))
		if rec.Code != 404 {
			t.Errorf("GET /%s/%s answered %d, want 404: %s",
				runID, suffix, rec.Code, rec.Body.String())
		}
	}
}

// TestTheNameScopedReadsAndWritesAgreeOnAnUnknownName is the point of the
// change: before it, the write refused the input the read accepted.
//
// The codes differ on purpose and that is not an inconsistency left behind.
// The writers answer 409 because they are refusing a (name, version) pair --
// ValidateVersion cannot distinguish "no such name" from "that version is
// deprecated", and both are conflicts with deployment state. The readers name
// no version, so the only thing they can be refusing is the name itself, and
// 404 is what the rest of this API says for an identifier that resolves to
// nothing. What matters is that neither answers success.
func TestTheNameScopedReadsAndWritesAgreeOnAnUnknownName(t *testing.T) {
	ms := &mockStore{
		listWorkflowDefsFn: deployedDef("checkout"),
		validateVersionFn: func(_ context.Context, name string, _ int) (bool, error) {
			return name == "checkout", nil
		},
	}
	api := newTestAPIServer(ms)

	cases := []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/workflows/never-deployed/routing", ""},
		{http.MethodGet, "/api/workflows/never-deployed/tags", ""},
		{http.MethodPost, "/api/workflows/never-deployed/routing", `{"target_version":1}`},
		{http.MethodPut, "/api/workflows/never-deployed/tags", `{"version":1,"tag":"stable"}`},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		var body io.Reader
		if c.body != "" {
			body = strings.NewReader(c.body)
		}
		api.handleWorkflows(rec, httptest.NewRequest(c.method, c.path, body))
		if rec.Code < 400 {
			t.Errorf("%s %s answered %d, want a refusal: %s",
				c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}
