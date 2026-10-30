package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/internal/tenantdrop"
)

// cleat#3552, B3b-1b. GET /api/admin/tenants/{tenant}/drop-plan, through composeAuthChain with a fake
// provisioner and a store that counts nothing real: this file is about WHO may ask and what the answer
// leaks. What the counts are is a_drop_plan_counts_a_tenants_rows_test.go, on real databases.

const planTenantID = "aaaaaaaa-0000-4000-8000-000000000001"

// planStore is a mock store that is also a TenantRowCounter, so the handler can run past the point where
// it needs a real database. Every count is 3 and the unsettled count is 5, so a response that mixes the
// two up, or reports a number nothing counted, shows.
type planStore struct {
	*mockStore
	asked int
}

func (p *planStore) CountTenantRows(_ context.Context, tables []tenantdrop.Table) ([]int64, error) {
	p.asked++
	out := make([]int64, len(tables))
	for i, t := range tables {
		out[i] = 3
		if t.Label == "unsettled runs" {
			out[i] = 5
		}
	}
	return out, nil
}

type planFactory struct{ store *planStore }

func (f planFactory) OpenStore(context.Context, string, ...string) (engine.WorkflowStore, io.Closer, error) {
	return f.store, io.NopCloser(strings.NewReader("")), nil
}
func (planFactory) DriverName() string      { return "postgres" }
func (planFactory) Dialect() engine.Dialect { return engine.DialectPostgres }

func planFixture(t *testing.T) (*provisionHTTP, *fakeProvisioner, *planStore) {
	t.Helper()
	f, fp := buildProvisionHTTP(t, nil)
	ps := &planStore{mockStore: &mockStore{}}
	f.api.factory = planFactory{store: ps}
	fp.planHeader = auth.DropPlanHeader{Name: "acme", Suspended: true, CreatedBy: "the-provisioner"}
	fp.planFacts = auth.DropPlanFacts{AdminTables: []tenantdrop.RowCount{{Table: "admin.tenants", Rows: 1}}}
	return f, fp, ps
}

func TestADropPlanAnswersAnAdminKeyForAnyTenantAndItsCreatorOnlyForItsOwn(t *testing.T) {
	path := "/api/admin/tenants/" + planTenantID + "/drop-plan"

	f, fp, ps := planFixture(t)

	w := doBody(f.chainFixture, http.MethodGet, path, f.adminKey, ``)
	if w.Code != http.StatusOK {
		t.Fatalf("admin = %d %s; want 200", w.Code, w.Body.String())
	}
	var plan tenantdrop.Plan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if plan.TenantID != planTenantID || plan.Name != "acme" || !plan.Suspended {
		t.Errorf("plan identity = %+v", plan)
	}
	if plan.UnsettledRuns != 5 {
		t.Errorf("unsettled_runs = %d; want 5 (the unsettled statement's count, not a table's 3)", plan.UnsettledRuns)
	}
	if n, ok := rowsOf(plan.Tables, "admin.tenants"); !ok || n != 1 {
		t.Errorf("admin.tenants = %d (listed=%v); want the admin-connection count of 1", n, ok)
	}
	if n, ok := rowsOf(plan.Tables, "workflow_instances"); !ok || n != 3 {
		t.Errorf("workflow_instances = %d (listed=%v); want the tenant-scope count of 3", n, ok)
	}
	if ps.asked != 1 {
		t.Errorf("the tenant store was asked to count %d times; want once (no plugin tables registered)", ps.asked)
	}

	// A provision key reads the plan of a tenant its OWN principal created (the fixture mints it as
	// "clew-service"), and gets the same plan. This is the case that makes the refusals below mean
	// something: without it they would pass against a route that refuses every provision key.
	*fp = fakeProvisioner{planHeader: auth.DropPlanHeader{Name: "acme", Suspended: true, CreatedBy: "clew-service"}}
	psBefore := ps.asked
	own := doBody(f.chainFixture, http.MethodGet, path, f.provisionKey, ``)
	if own.Code != http.StatusOK {
		t.Fatalf("a provision key for the tenant it created = %d %s; want 200", own.Code, own.Body.String())
	}
	if ps.asked != psBefore+1 {
		t.Errorf("the provision key's plan did not reach the tenant store")
	}

	// A provision key, for a tenant it did not create: the same 404 as a tenant that does not exist.
	*fp = fakeProvisioner{planHeader: auth.DropPlanHeader{Name: "acme", CreatedBy: "someone-else"}}
	other := doBody(f.chainFixture, http.MethodGet, path, f.provisionKey, ``)
	*fp = fakeProvisioner{planHeaderErr: auth.ErrTenantNotFound}
	missing := doBody(f.chainFixture, http.MethodGet, path, f.provisionKey, ``)
	*fp = fakeProvisioner{planHeader: auth.DropPlanHeader{Name: "acme", CreatedBy: ""}}
	offline := doBody(f.chainFixture, http.MethodGet, path, f.provisionKey, ``)
	for name, got := range map[string]*struct {
		code int
		body string
	}{
		"not its own":  {other.Code, strings.TrimSpace(other.Body.String())},
		"missing":      {missing.Code, strings.TrimSpace(missing.Body.String())},
		"made offline": {offline.Code, strings.TrimSpace(offline.Body.String())},
	} {
		if got.code != http.StatusNotFound || got.body != `{"error":"not found"}` {
			t.Errorf("%s = %d %s; want the one 404 body", name, got.code, got.body)
		}
	}
}

func TestOnlyAnOperatorCredentialCanReadADropPlan(t *testing.T) {
	f, fp, ps := planFixture(t)
	path := "/api/admin/tenants/" + planTenantID + "/drop-plan"
	for _, tc := range []struct {
		name, method, path, key string
		want                    int
	}{
		{"a tenant key", http.MethodGet, path, f.tenantAKey, http.StatusForbidden},
		{"an unknown tenant key", http.MethodGet, path, f.unknownTenant, http.StatusUnauthorized},
		{"no credential", http.MethodGet, path, "", http.StatusUnauthorized},
		{"a forged operator key", http.MethodGet, path, f.forgedOpKey, http.StatusUnauthorized},
		// The scope gate: the method belongs to the route.
		{"a provision key, POST to the plan", http.MethodPost, path, f.provisionKey, http.StatusForbidden},
		{"a provision key, GET to suspend", http.MethodGet, "/api/admin/tenants/" + planTenantID + "/suspend", f.provisionKey, http.StatusForbidden},
		{"a provision key, GET to confirm", http.MethodGet, "/api/admin/tenants/" + planTenantID + "/confirm", f.provisionKey, http.StatusForbidden},
		{"a provision key, upper-case id", http.MethodGet, "/api/admin/tenants/" + strings.ToUpper(planTenantID) + "/drop-plan", f.provisionKey, http.StatusForbidden},
		{"a provision key, a tail", http.MethodGet, path + "/x", f.provisionKey, http.StatusForbidden},
		{"a provision key, the collection", http.MethodGet, "/api/admin/tenants", f.provisionKey, http.StatusForbidden},
		// Past the gate: wrong method and bad input are the handler's.
		{"an admin key, POST", http.MethodPost, path, f.adminKey, http.StatusMethodNotAllowed},
		{"an admin key, a name that is no id", http.MethodGet, "/api/admin/tenants/acme/drop-plan", f.adminKey, http.StatusBadRequest},
		{"an admin key, the default tenant", http.MethodGet, "/api/admin/tenants/" + engine.DefaultTenantUUID + "/drop-plan", f.adminKey, http.StatusBadRequest},
	} {
		w := doBody(f.chainFixture, tc.method, tc.path, tc.key, ``)
		if w.Code != tc.want {
			t.Errorf("%s = %d %s, want %d", tc.name, w.Code, strings.TrimSpace(w.Body.String()), tc.want)
		}
	}
	if fp.headerCalls != 0 || fp.factsCalls != 0 || ps.asked != 0 {
		t.Errorf("a refused request reached the header %d times, the counts %d times and the tenant store %d times",
			fp.headerCalls, fp.factsCalls, ps.asked)
	}
}

func TestADropPlanThatCannotReadItsFactsIsAnErrorNotAnEmptyPlan(t *testing.T) {
	f, fp, ps := planFixture(t)
	fp.planFactsErr = errors.New("admin connection lost: password=hunter2")
	w := doBody(f.chainFixture, http.MethodGet, "/api/admin/tenants/"+planTenantID+"/drop-plan", f.adminKey, ``)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("= %d %s; want 500", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "hunter2") {
		t.Errorf("the error body leaks the driver's message: %s", w.Body.String())
	}
	if ps.asked != 0 {
		t.Errorf("the tenant store was asked to count %d times although the facts could not be read", ps.asked)
	}
}

// A caller with no claim to a tenant must not be able to tell it exists. The creator check therefore runs on
// the cheap header read, BEFORE any counting: reading the counts first answered 500 for a non-creator
// whenever a count failed, where an absent tenant answered 404, and cost four-plus queries where an absent
// tenant cost one (cleat-review, cleat#3601). The counts half is made to fail here, so a handler that
// reads it first shows as a 500 or as a counts call, and a handler that checks first shows as neither.
func TestADropPlanRefusesAnUnrelatedCallerBeforeItCountsAnything(t *testing.T) {
	path := "/api/admin/tenants/" + planTenantID + "/drop-plan"
	for name, header := range map[string]auth.DropPlanHeader{
		"a tenant it did not create": {Name: "acme", CreatedBy: "someone-else"},
		"a tenant made offline":      {Name: "acme", CreatedBy: ""},
	} {
		f, fp, ps := planFixture(t)
		fp.planHeader = header
		fp.planFactsErr = errors.New("the counts would fail")
		w := doBody(f.chainFixture, http.MethodGet, path, f.provisionKey, ``)
		if w.Code != http.StatusNotFound || strings.TrimSpace(w.Body.String()) != `{"error":"not found"}` {
			t.Errorf("%s = %d %s; want the one 404", name, w.Code, strings.TrimSpace(w.Body.String()))
		}
		if fp.factsCalls != 0 || ps.asked != 0 {
			t.Errorf("%s: the counts half ran (admin counts %d, tenant store %d) before the creator check", name, fp.factsCalls, ps.asked)
		}
	}

	// CONTROL: the same failing counts half, asked by the tenant's own creator, IS an error. Without it the
	// 404s above would pass against a route that never reached the counts for anyone.
	f, fp, _ := planFixture(t)
	fp.planHeader = auth.DropPlanHeader{Name: "acme", CreatedBy: "clew-service"}
	fp.planFactsErr = errors.New("the counts would fail")
	if w := doBody(f.chainFixture, http.MethodGet, path, f.provisionKey, ``); w.Code != http.StatusInternalServerError || fp.factsCalls != 1 {
		t.Errorf("the creator with failing counts = %d (counts calls %d); want 500 and 1", w.Code, fp.factsCalls)
	}
}
