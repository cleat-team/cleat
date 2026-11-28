package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
)

// cleat#3552, B3b-2. DELETE /api/admin/tenants/{tenant}, through composeAuthChain with a fake provisioner.
// This file is about WHO may drop, what is checked before the store is touched, and what each refusal says.
// What the drop does to a database is a_tenant_is_dropped_test.go in auth, on real databases.

const dropTenantID = "aaaaaaaa-0000-4000-8000-000000000001"

// doDrop is a DELETE with an X-Confirm header, which doBody cannot send.
func doDrop(f chainFixture, path, key, confirm string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if confirm != "" {
		req.Header.Set("X-Confirm", confirm)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	return w
}

func droppedResult() auth.DropResult {
	seq, hash := int64(41), strings.Repeat("ab", 32)
	return auth.DropResult{Tombstone: auth.Tombstone{
		TenantID: uuid.MustParse(dropTenantID), Name: "acme", DroppedBy: "somebody",
		DroppedAt: time.Date(2026, 10, 30, 4, 0, 0, 0, time.UTC), AuditChainSeq: &seq, AuditChainHash: &hash,
	}}
}

// Nothing is read or changed until the caller has typed the id back, and a header that is not exactly the
// id is not typing it back. The control is the same request with the right header.
func TestADropNeedsTheTenantIdTypedBackBeforeAnythingElseRuns(t *testing.T) {
	f, fp := buildProvisionHTTP(t, nil)
	fp.dropRes = droppedResult()
	path := "/api/admin/tenants/" + dropTenantID

	for name, confirm := range map[string]string{
		"no header":        "",
		"another tenant":   "bbbbbbbb-0000-4000-8000-000000000002",
		"the word confirm": "confirm",
		"upper case":       strings.ToUpper(dropTenantID),
		"trailing space":   dropTenantID + " ",
	} {
		for _, key := range []string{f.adminKey, f.provisionKey} {
			w := doDrop(f.chainFixture, path, key, confirm)
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s: %d %s; want 400", name, w.Code, w.Body.String())
			}
		}
	}
	if fp.dropCalls != 0 {
		t.Fatalf("the store was asked to drop %d time(s) without a valid X-Confirm", fp.dropCalls)
	}

	// CONTROL. Without it every refusal above is satisfied by a route that refuses everything.
	if w := doDrop(f.chainFixture, path, f.adminKey, dropTenantID); w.Code != http.StatusOK {
		t.Fatalf("CONTROL FAILED: the right header = %d %s", w.Code, w.Body.String())
	}
	if fp.dropCalls != 1 {
		t.Errorf("the store was asked %d times; want 1", fp.dropCalls)
	}
}

// What claim the store is asked to honour decides who can drop what, and the handler is where it is set.
func TestADropAsksTheStoreForTheCallersClaimNotForMoreThanIt(t *testing.T) {
	f, fp := buildProvisionHTTP(t, nil)
	fp.dropRes = droppedResult()
	f.api.schema = "cleat_schema"
	path := "/api/admin/tenants/" + dropTenantID

	if w := doDrop(f.chainFixture, path, f.adminKey, dropTenantID); w.Code != http.StatusOK {
		t.Fatalf("admin = %d %s", w.Code, w.Body.String())
	}
	if fp.gotDrop.OnlyCreator != "" {
		t.Errorf("an admin key was limited to creator %q; it may drop any tenant", fp.gotDrop.OnlyCreator)
	}
	if !strings.HasPrefix(fp.gotDrop.DroppedBy, "operator:") {
		t.Errorf("DroppedBy = %q; an admin key is recorded as operator:<KeyID>", fp.gotDrop.DroppedBy)
	}
	if fp.gotDrop.Schema != "cleat_schema" {
		t.Errorf("Schema = %q; the worker's --schema must reach admin.drop_tenant", fp.gotDrop.Schema)
	}
	if fp.gotDrop.TenantID.String() != dropTenantID {
		t.Errorf("TenantID = %v", fp.gotDrop.TenantID)
	}

	if w := doDrop(f.chainFixture, path, f.provisionKey, dropTenantID); w.Code != http.StatusOK {
		t.Fatalf("provision = %d %s", w.Code, w.Body.String())
	}
	if fp.gotDrop.OnlyCreator != "clew-service" || fp.gotDrop.DroppedBy != "clew-service" {
		t.Errorf("a provision key asked for creator=%q dropped_by=%q; want its principal for both, so it can drop only what it made",
			fp.gotDrop.OnlyCreator, fp.gotDrop.DroppedBy)
	}
}

// Each answer, and that an unexplained error does not repeat itself.
func TestADropAnswersEachOutcomeWithItsOwnShape(t *testing.T) {
	f, fp := buildProvisionHTTP(t, nil)
	path := "/api/admin/tenants/" + dropTenantID
	drop := func() *httptest.ResponseRecorder { return doDrop(f.chainFixture, path, f.adminKey, dropTenantID) }
	decode := func(w *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatalf("decode %q: %v", w.Body.String(), err)
		}
		return m
	}

	fp.dropRes = droppedResult()
	m := decode(drop())
	if m["dropped"] != true || m["already_dropped"] != false || m["dropped_by"] != "somebody" {
		t.Errorf("a drop = %v", m)
	}
	chain, _ := m["audit_chain"].(map[string]any)
	if chain == nil || chain["seq"] != float64(41) || chain["hash"] != strings.Repeat("ab", 32) {
		t.Errorf("audit_chain = %v; want where the dropped chain ended", m["audit_chain"])
	}

	// A tenant with no chain says so, with null, and not with a zero that reads as "chain of length zero".
	noChain := droppedResult()
	noChain.Tombstone.AuditChainSeq, noChain.Tombstone.AuditChainHash = nil, nil
	fp.dropRes = noChain
	if m := decode(drop()); m["audit_chain"] != nil {
		t.Errorf("audit_chain = %v; want null for a tenant that had no chain", m["audit_chain"])
	}

	repeat := droppedResult()
	repeat.AlreadyDropped = true
	fp.dropRes = repeat
	if m := decode(drop()); m["already_dropped"] != true {
		t.Errorf("a repeat = %v; want already_dropped true", m)
	}

	fp.dropErr = &auth.DropRefusedError{Reason: auth.DropRefusedNotSuspended}
	w := drop()
	if m := decode(w); w.Code != http.StatusConflict || m["reason"] != "not_suspended" || m["unsettled_runs"] != nil {
		t.Errorf("not suspended = %d %v", w.Code, m)
	}
	fp.dropErr = &auth.DropRefusedError{Reason: auth.DropRefusedUnsettledRuns, Unsettled: 3}
	w = drop()
	if m := decode(w); w.Code != http.StatusConflict || m["reason"] != "unsettled_runs" || m["unsettled_runs"] != float64(3) {
		t.Errorf("unsettled = %d %v; want 409 naming the count", w.Code, m)
	}
	fp.dropErr = auth.ErrTenantNotFound
	if w := drop(); w.Code != http.StatusNotFound || strings.TrimSpace(w.Body.String()) != `{"error":"not found"}` {
		t.Errorf("not found = %d %s", w.Code, w.Body.String())
	}
	fp.dropErr = errors.New(`pq: deadlock detected on relation "tenants" (40P01)`)
	if w := drop(); w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "deadlock") || strings.Contains(w.Body.String(), "tenants") {
		t.Errorf("an unexplained error = %d %s; want a 500 that does not leak it", w.Code, w.Body.String())
	}
}

// The route is DELETE on a bare tenant id and nothing else, for the scope gate and for the mux.
func TestOnlyDeleteOnABareTenantIdIsADropAndOnlyACanonicalIdReachesItForAProvisionKey(t *testing.T) {
	f, fp := buildProvisionHTTP(t, nil)
	fp.dropRes = droppedResult()
	path := "/api/admin/tenants/" + dropTenantID

	// Another method on the same path is the 404 it always was, and never reaches the store.
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch} {
		w := doBody(f.chainFixture, method, path, f.adminKey, ``)
		if w.Code == http.StatusOK {
			t.Errorf("%s on a bare tenant id = 200", method)
		}
	}
	if fp.dropCalls != 0 {
		t.Fatalf("a non-DELETE reached the store %d time(s)", fp.dropCalls)
	}

	// A provision key reaches it only with the id in its one canonical spelling.
	if w := doDrop(f.chainFixture, "/api/admin/tenants/"+strings.ToUpper(dropTenantID), f.provisionKey, strings.ToUpper(dropTenantID)); w.Code != http.StatusForbidden {
		t.Errorf("an upper-case id with a provision key = %d; want 403 from the scope gate", w.Code)
	}
	// A tenant key and no key never reach it.
	if w := doDrop(f.chainFixture, path, f.tenantAKey, dropTenantID); w.Code == http.StatusOK {
		t.Errorf("a tenant key dropped a tenant: %d %s", w.Code, w.Body.String())
	}
	if w := doDrop(f.chainFixture, path, "", dropTenantID); w.Code == http.StatusOK {
		t.Errorf("no credential dropped a tenant: %d", w.Code)
	}
	if fp.dropCalls != 0 {
		t.Fatalf("a refused caller reached the store %d time(s)", fp.dropCalls)
	}

	// The default tenant is never droppable, by any key, whatever the header says.
	nilID := uuid.Nil.String()
	for name, key := range map[string]string{"admin": f.adminKey, "provision": f.provisionKey} {
		if w := doDrop(f.chainFixture, "/api/admin/tenants/"+nilID, key, nilID); w.Code != http.StatusBadRequest {
			t.Errorf("%s dropping the default tenant = %d %s; want 400", name, w.Code, w.Body.String())
		}
	}
	if fp.dropCalls != 0 {
		t.Fatalf("the default tenant reached the store")
	}
}

// A drop does NOT name the tenant it acted on. The audit log puts an operator's row on the chain of the
// tenant the handler names, and a dropped tenant's chain is deleted with it: naming it would put the record
// of the drop on a chain that no longer exists. With no target the row goes on the default tenant's chain.
//
// The control is a suspend through the same fixture, which does name its tenant: without it, "nothing was
// noted" is also what a slot that records nothing at all would show.
func TestADropLeavesItsAuditRowOnTheDefaultChain(t *testing.T) {
	f, fp := buildProvisionHTTP(t, slotAround)
	fp.dropRes = droppedResult()
	path := "/api/admin/tenants/" + dropTenantID

	if w := doBody(f.chainFixture, http.MethodPost, path+"/suspend", f.adminKey, ``); w.Code != http.StatusOK || !f.noted.set {
		t.Fatalf("CONTROL FAILED: a suspend = %d, noted=%v; the slot must record a handler that names its tenant", w.Code, f.noted.set)
	}

	for name, run := range map[string]func(){
		"a drop": func() { doDrop(f.chainFixture, path, f.adminKey, dropTenantID) },
		"a repeat": func() {
			r := droppedResult()
			r.AlreadyDropped = true
			fp.dropRes = r
			doDrop(f.chainFixture, path, f.adminKey, dropTenantID)
		},
		"a refused drop": func() {
			fp.dropErr = &auth.DropRefusedError{Reason: auth.DropRefusedNotSuspended}
			doDrop(f.chainFixture, path, f.adminKey, dropTenantID)
		},
	} {
		*f.noted = notedTarget{}
		run()
		if f.noted.set {
			t.Errorf("%s noted tenant %v: its audit row would land on a chain that is deleted with the tenant", name, f.noted.tenant)
		}
	}
}

func TestAWorkerThatCannotProvisionCannotDropEither(t *testing.T) {
	f, _ := buildProvisionHTTP(t, nil)
	f.api.provisioner = nil
	for name, key := range map[string]string{"admin": f.adminKey, "provision": f.provisionKey} {
		w := doDrop(f.chainFixture, "/api/admin/tenants/"+dropTenantID, key, dropTenantID)
		if w.Code != http.StatusNotImplemented || !strings.Contains(w.Body.String(), "PostgreSQL or SQL Server") {
			t.Errorf("%s = %d %s, want 501 naming what is supported", name, w.Code, w.Body.String())
		}
	}
}
