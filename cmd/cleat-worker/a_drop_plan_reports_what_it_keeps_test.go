package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/internal/tenantdrop"
)

// The drop plan says what a drop KEEPS as well as what it removes (cleat#3552, B3b-2), and a kept table is
// not counted into what is removed.

// derivedStore is a store that derives its table list from a catalogue, as SQL Server's does, and whose
// catalogue lists a retained table among the tenant's tables.
type derivedStore struct {
	*planStore
	derived []tenantdrop.Table
}

func (d *derivedStore) DerivedTenantTables(context.Context) ([]tenantdrop.Table, error) {
	return d.derived, nil
}

func TestAPlanNamesWhatTheDropKeepsAndDoesNotCountItAsRemoved(t *testing.T) {
	f, fp, _ := planFixture(t)
	path := "/api/admin/tenants/" + planTenantID + "/drop-plan"

	w := doBody(f.chainFixture, http.MethodGet, path, f.adminKey, ``)
	if w.Code != http.StatusOK {
		t.Fatalf("plan = %d %s", w.Code, w.Body.String())
	}
	var plan tenantdrop.Plan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(plan.Retained) == 0 || plan.Retained[0].Table != "admin.tenant_tombstones" || plan.Retained[0].Reason == "" {
		t.Errorf("retained = %+v; want the tombstone and why it is kept", plan.Retained)
	}
	for _, rc := range plan.Tables {
		if tenantdrop.IsRetained(rc.Table) {
			t.Errorf("%s is listed as removed and as retained", rc.Table)
		}
	}
	_ = fp

	// The field is present and an array even when a caller cannot see an entry, never absent: a reader of
	// "retained: []" and one of no field at all are told different things.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if string(raw["retained"]) == "" || string(raw["retained"]) == "null" {
		t.Errorf("retained = %s; want an array", raw["retained"])
	}
}

// A catalogue sweep that would list a retained table must not make the plan say the drop removes it.
func TestAPlanDoesNotListACatalogueTableThatIsRetainedAsRemoved(t *testing.T) {
	_, _, ps := planFixture(t)
	ds := &derivedStore{planStore: ps, derived: []tenantdrop.Table{
		{Label: "dbo.workflow_instances", Query: "SELECT 1"},
		{Label: "admin.tenant_tombstones", Query: "SELECT 1"},
	}}
	plan, err := buildDropPlan(context.Background(), ds, ds, planTenantID,
		auth.DropPlanHeader{Name: "acme", Suspended: true}, auth.DropPlanFacts{})
	if err != nil {
		t.Fatalf("buildDropPlan: %v", err)
	}
	var sawInstances bool
	for _, rc := range plan.Tables {
		if rc.Table == "admin.tenant_tombstones" {
			t.Errorf("a retained table was counted as removed: %+v", rc)
		}
		if rc.Table == "dbo.workflow_instances" {
			sawInstances = true
		}
	}
	// CONTROL: the ordinary derived table is still counted, so the filter is not dropping everything.
	if !sawInstances {
		t.Errorf("the ordinary catalogue table vanished from the plan: %+v", plan.Tables)
	}
	if len(plan.Retained) == 0 {
		t.Errorf("the plan names nothing as retained")
	}
}
