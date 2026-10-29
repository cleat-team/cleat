package main

// cleat#3552, B3a. POST /api/admin/tenants/{tenant}/suspend and /resume, through composeAuthChain with the real
// mux, so the credential, the scope gate, the admin-API flag and the handler are the ones a worker runs. The
// store is a fake here (what it does is auth/a_provisioner_suspends_only_its_own_tenants_test.go, on real
// databases) except in the last test, which puts the real store behind the same HTTP.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine/testutil"
)

const suspendTenantID = "aaaaaaaa-0000-4000-8000-000000000001"

func TestSuspendAndResumeReachTheStoreThatMatchesTheCredential(t *testing.T) {
	f, fp := buildProvisionHTTP(t, nil)
	tid := uuid.MustParse(suspendTenantID)

	// An admin key may act on any tenant: it reaches the unrestricted store call, never the creator one.
	for _, tc := range []struct {
		path string
		want bool
	}{{"/suspend", true}, {"/resume", false}} {
		*fp = fakeProvisioner{}
		w := doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants/"+suspendTenantID+tc.path, f.adminKey, ``)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), suspendTenantID) {
			t.Errorf("admin %s = %d %s", tc.path, w.Code, w.Body.String())
		}
		if fp.anySuspendCalls != 1 || fp.creatorSuspendCalls != 0 || fp.gotSuspendTenant != tid || fp.gotSuspended != tc.want {
			t.Errorf("admin %s reached any=%d creator=%d tenant=%v suspended=%v; want the unrestricted call once, suspended=%v",
				tc.path, fp.anySuspendCalls, fp.creatorSuspendCalls, fp.gotSuspendTenant, fp.gotSuspended, tc.want)
		}
	}
	// An admin key is not held to the canonical spelling: the scope already reaches everything.
	*fp = fakeProvisioner{}
	if w := doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants/"+strings.ToUpper(suspendTenantID)+"/suspend", f.adminKey, ``); w.Code != http.StatusOK || fp.gotSuspendTenant != tid {
		t.Errorf("admin, upper-case id = %d (tenant %v), want 200 on the canonical tenant", w.Code, fp.gotSuspendTenant)
	}

	// A provision key reaches only the creator-scoped call, naming its own principal, and only to SUSPEND.
	*fp = fakeProvisioner{}
	w := doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants/"+suspendTenantID+"/suspend", f.provisionKey, ``)
	if w.Code != http.StatusOK {
		t.Errorf("provision /suspend = %d %s", w.Code, w.Body.String())
	}
	if fp.creatorSuspendCalls != 1 || fp.anySuspendCalls != 0 || fp.gotSuspendTenant != tid || !fp.gotSuspended || fp.gotSuspendCreator == "" {
		t.Errorf("provision /suspend reached any=%d creator=%d tenant=%v suspended=%v creator=%q; want the creator call once with a creator",
			fp.anySuspendCalls, fp.creatorSuspendCalls, fp.gotSuspendTenant, fp.gotSuspended, fp.gotSuspendCreator)
	}

	// ...and it can NEVER resume: that would let a provisioner lift a suspension an operator made.
	*fp = fakeProvisioner{}
	w = doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants/"+suspendTenantID+"/resume", f.provisionKey, ``)
	if w.Code != http.StatusForbidden || fp.anySuspendCalls+fp.creatorSuspendCalls != 0 {
		t.Errorf("provision /resume = %d %s (store calls %d), want 403 and none", w.Code, w.Body.String(), fp.anySuspendCalls+fp.creatorSuspendCalls)
	}
}

func TestOnlyAnOperatorCredentialCanSuspendATenant(t *testing.T) {
	f, fp := buildProvisionHTTP(t, nil)
	path := "/api/admin/tenants/" + suspendTenantID + "/suspend"
	for _, tc := range []struct {
		name, method, path, key string
		want                    int
	}{
		{"a tenant key", http.MethodPost, path, f.tenantAKey, http.StatusForbidden},
		{"an unknown tenant key", http.MethodPost, path, f.unknownTenant, http.StatusUnauthorized},
		{"no credential", http.MethodPost, path, "", http.StatusUnauthorized},
		{"a forged operator key", http.MethodPost, path, f.forgedOpKey, http.StatusUnauthorized},
		// The scope gate: a provision key reaches only the canonical spelling, and only POST.
		{"a provision key, upper-case id", http.MethodPost, "/api/admin/tenants/" + strings.ToUpper(suspendTenantID) + "/suspend", f.provisionKey, http.StatusForbidden},
		{"a provision key, bare hex id", http.MethodPost, "/api/admin/tenants/" + strings.ReplaceAll(suspendTenantID, "-", "") + "/suspend", f.provisionKey, http.StatusForbidden},
		{"a provision key, resume", http.MethodPost, "/api/admin/tenants/" + suspendTenantID + "/resume", f.provisionKey, http.StatusForbidden},
		{"a provision key, GET", http.MethodGet, path, f.provisionKey, http.StatusForbidden},
		{"a provision key, DELETE", http.MethodDelete, path, f.provisionKey, http.StatusForbidden},
		{"a provision key, a tail", http.MethodPost, path + "/x", f.provisionKey, http.StatusForbidden},
		{"a provision key, the drop path (not B3a)", http.MethodPost, "/api/admin/tenants/" + suspendTenantID + "/drop", f.provisionKey, http.StatusForbidden},
		// Past the gate: wrong method is the handler's.
		{"an admin key, GET", http.MethodGet, path, f.adminKey, http.StatusMethodNotAllowed},
		{"an admin key, a name that is no id", http.MethodPost, "/api/admin/tenants/acme/suspend", f.adminKey, http.StatusBadRequest},
	} {
		w := doBody(f.chainFixture, tc.method, tc.path, tc.key, ``)
		if w.Code != tc.want {
			t.Errorf("%s = %d %s, want %d", tc.name, w.Code, strings.TrimSpace(w.Body.String()), tc.want)
		}
	}
	if fp.anySuspendCalls+fp.creatorSuspendCalls != 0 {
		t.Errorf("a refused request reached the store %d times", fp.anySuspendCalls+fp.creatorSuspendCalls)
	}

	// The flag off: the route does not exist for anyone.
	off := false
	old := enableAdminAPI
	enableAdminAPI = &off
	t.Cleanup(func() { enableAdminAPI = old })
	if w := doBody(f.chainFixture, http.MethodPost, path, f.adminKey, ``); w.Code != http.StatusNotFound {
		t.Errorf("with the admin API off = %d, want 404", w.Code)
	}
}

func TestASuspendThatFindsNothingIsOneAnswerWhoeverAsked(t *testing.T) {
	f, fp := buildProvisionHTTP(t, nil)
	path := "/api/admin/tenants/" + suspendTenantID + "/suspend"

	fp.suspendErr = auth.ErrTenantNotFound
	bodies := map[string]string{}
	for name, key := range map[string]string{"admin": f.adminKey, "provision": f.provisionKey} {
		w := doBody(f.chainFixture, http.MethodPost, path, key, ``)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s with nothing to suspend = %d, want 404", name, w.Code)
		}
		bodies[name] = w.Body.String()
	}
	if bodies["admin"] != bodies["provision"] {
		t.Errorf("the two 404 bodies differ (%q vs %q): the difference says whether the tenant exists", bodies["admin"], bodies["provision"])
	}

	// An unexplained error is a 500 that does not repeat itself.
	fp.suspendErr = errors.New(`pq: deadlock detected on relation "tenants" (40P01)`)
	w := doBody(f.chainFixture, http.MethodPost, path, f.provisionKey, ``)
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "deadlock") || strings.Contains(w.Body.String(), "tenants") {
		t.Errorf("an unexplained error = %d %s, want a 500 that does not leak it", w.Code, w.Body.String())
	}
}

func TestAWorkerThatCannotProvisionCannotSuspendByCreatorEither(t *testing.T) {
	f, _ := buildProvisionHTTP(t, nil)
	f.api.provisioner = nil
	for name, key := range map[string]string{"admin": f.adminKey, "provision": f.provisionKey} {
		w := doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants/"+suspendTenantID+"/suspend", key, ``)
		if w.Code != http.StatusNotImplemented || !strings.Contains(w.Body.String(), "PostgreSQL or SQL Server") {
			t.Errorf("%s = %d %s, want 501 naming what is supported", name, w.Code, w.Body.String())
		}
	}
}

// A suspend names the tenant it acted on, so the audit row lands on that tenant's chain, and only once the
// tenant is known to exist: otherwise a refusal would create a chain for a tenant that is not there.
func TestSuspendNamesTheTenantItActedOnOnlyOnSuccess(t *testing.T) {
	f, fp := buildProvisionHTTP(t, slotAround)
	tid := uuid.MustParse(suspendTenantID)
	path := "/api/admin/tenants/" + suspendTenantID

	for _, key := range []string{f.adminKey, f.provisionKey} {
		*f.noted = notedTarget{}
		doBody(f.chainFixture, http.MethodPost, path+"/suspend", key, ``)
		if !f.noted.set || f.noted.tenant != tid {
			t.Errorf("a suspend noted %v (set=%v), want %v", f.noted.tenant, f.noted.set, tid)
		}
	}
	fp.suspendErr = auth.ErrTenantNotFound
	for _, key := range []string{f.adminKey, f.provisionKey} {
		*f.noted = notedTarget{}
		doBody(f.chainFixture, http.MethodPost, path+"/suspend", key, ``)
		if f.noted.set {
			t.Errorf("a suspend that found nothing noted %v: the audit row would land on a chain for a tenant that may not exist", f.noted.tenant)
		}
	}
}

// The real store behind the same HTTP: the question is whether admin.tenants.suspended changed.
func TestSuspendingOverHTTPAgainstRealDatabases(t *testing.T) {
	for _, dialect := range []string{auth.DialectPostgres, auth.DialectMSSQL} {
		dialect := dialect
		t.Run(dialect, func(t *testing.T) {
			db := testutil.TestDB(t, testutil.Dialect(dialect))
			ts, err := auth.NewTenantStoreForDialect(db, dialect)
			if err != nil {
				t.Fatal(err)
			}
			f, _ := buildProvisionHTTP(t, nil)
			f.api.provisioner = ts
			ph := func(n int) string {
				if dialect == auth.DialectMSSQL {
					return "@p" + string(rune('0'+n))
				}
				return "$" + string(rune('0'+n))
			}
			var tenants []uuid.UUID
			orgPrefix := "b3a-http-" + uuid.NewString()
			t.Cleanup(func() {
				for _, id := range tenants {
					_, _ = db.Exec(`DELETE FROM admin.tenant_api_keys WHERE tenant_id = `+ph(1), id.String())
					_, _ = db.Exec(`DELETE FROM admin.tenants WHERE tenant_id = `+ph(1), id.String())
				}
				_, _ = db.Exec(`DELETE FROM admin.orgs WHERE name LIKE `+ph(1), orgPrefix+"%")
			})
			suspended := func(id uuid.UUID) bool {
				t.Helper()
				var v bool
				if err := db.QueryRow(`SELECT suspended FROM admin.tenants WHERE tenant_id = `+ph(1), id.String()).Scan(&v); err != nil {
					t.Fatal(err)
				}
				return v
			}

			// One tenant created BY THE PROVISION KEY over HTTP, so its creator is exactly what the handler
			// will compute for that key; one created by someone else through the store.
			mine := uuid.New()
			w := doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants", f.provisionKey, `{"tenant_id":"`+mine.String()+`","org_name":"`+orgPrefix+`-mine"}`)
			if w.Code != http.StatusCreated {
				t.Fatalf("create on %s = %d %s", dialect, w.Code, w.Body.String())
			}
			tenants = append(tenants, mine)
			theirs := uuid.New()
			if _, err := ts.ProvisionTenant(context.Background(), auth.ProvisionRequest{TenantID: theirs, Name: orgPrefix + "-theirs", OrgName: orgPrefix + "-theirs-org", Creator: "someone-else"}); err != nil {
				t.Fatal(err)
			}
			tenants = append(tenants, theirs)
			if suspended(mine) || suspended(theirs) {
				t.Fatal("UNMEASURED: a fresh tenant starts suspended")
			}
			url := func(id uuid.UUID, verb string) string { return "/api/admin/tenants/" + id.String() + "/" + verb }

			if w := doBody(f.chainFixture, http.MethodPost, url(mine, "suspend"), f.provisionKey, ``); w.Code != http.StatusOK || !suspended(mine) {
				t.Errorf("the provision key suspending its own tenant = %d %s, suspended=%v", w.Code, w.Body.String(), suspended(mine))
			}
			// A repeat is a success that changes nothing.
			if w := doBody(f.chainFixture, http.MethodPost, url(mine, "suspend"), f.provisionKey, ``); w.Code != http.StatusOK || !suspended(mine) {
				t.Errorf("a repeated suspend = %d %s, suspended=%v", w.Code, w.Body.String(), suspended(mine))
			}
			// THE REVIEWER'S KNOWN POSITIVE (cleat#3582): the provision key cannot undo even a suspension it made
			// itself, because the flag does not record who set it and an admin's would look identical.
			if w := doBody(f.chainFixture, http.MethodPost, url(mine, "resume"), f.provisionKey, ``); w.Code != http.StatusForbidden || !suspended(mine) {
				t.Errorf("the provision key resuming = %d %s, suspended=%v; want 403 and still suspended", w.Code, w.Body.String(), suspended(mine))
			}
			if w := doBody(f.chainFixture, http.MethodPost, url(mine, "resume"), f.adminKey, ``); w.Code != http.StatusOK || suspended(mine) {
				t.Errorf("the admin key resuming = %d %s, suspended=%v", w.Code, w.Body.String(), suspended(mine))
			}
			// An ADMIN's suspension survives the provision key's attempts to lift it.
			if w := doBody(f.chainFixture, http.MethodPost, url(mine, "suspend"), f.adminKey, ``); w.Code != http.StatusOK || !suspended(mine) {
				t.Fatalf("the admin key suspending = %d %s, suspended=%v", w.Code, w.Body.String(), suspended(mine))
			}
			for _, attempt := range []string{"resume", "suspend"} {
				doBody(f.chainFixture, http.MethodPost, url(mine, attempt), f.provisionKey, ``)
				if !suspended(mine) {
					t.Errorf("after the provision key's %s, an admin's suspension was lifted", attempt)
				}
			}
			if w := doBody(f.chainFixture, http.MethodPost, url(theirs, "suspend"), f.provisionKey, ``); w.Code != http.StatusNotFound || suspended(theirs) {
				t.Errorf("the provision key suspending someone else's tenant = %d %s, suspended=%v; want 404 and unchanged", w.Code, w.Body.String(), suspended(theirs))
			}
			// The admin key may act on any tenant, in both directions.
			if w := doBody(f.chainFixture, http.MethodPost, url(theirs, "suspend"), f.adminKey, ``); w.Code != http.StatusOK || !suspended(theirs) {
				t.Errorf("the admin key suspending any tenant = %d %s, suspended=%v", w.Code, w.Body.String(), suspended(theirs))
			}
			if w := doBody(f.chainFixture, http.MethodPost, url(theirs, "resume"), f.adminKey, ``); w.Code != http.StatusOK || suspended(theirs) {
				t.Errorf("the admin key resuming any tenant = %d %s, suspended=%v", w.Code, w.Body.String(), suspended(theirs))
			}
			// A tenant that does not exist is the same 404 for both.
			ghost := uuid.New()
			for name, key := range map[string]string{"admin": f.adminKey, "provision": f.provisionKey} {
				if w := doBody(f.chainFixture, http.MethodPost, url(ghost, "suspend"), key, ``); w.Code != http.StatusNotFound {
					t.Errorf("%s suspending a tenant that does not exist = %d, want 404", name, w.Code)
				}
			}
		})
	}
}
