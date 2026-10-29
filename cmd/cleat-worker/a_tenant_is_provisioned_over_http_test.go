package main

// cleat#3552, B2b-2. POST /api/admin/tenants and /api/admin/tenants/{id}/confirm, driven through
// composeAuthChain with the real mux, so the credential, the scope gate, the admin-API flag and the
// handler are all the ones a worker runs. The store behind them is a fake here -- what the store does is
// auth/a_tenant_is_provisioned_replayed_and_confirmed_test.go, on real databases -- except in the last
// test, which puts the real store behind the same HTTP and so measures the two halves together.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine/testutil"
)

type fakeProvisioner struct {
	provisionCalls, confirmCalls    int
	gotReq                          auth.ProvisionRequest
	gotConfirmTenant, gotConfirmKey uuid.UUID
	gotConfirmCreator               string
	result                          auth.ProvisionResult
	err                             error
	confirmResult                   auth.ConfirmResult
	confirmErr                      error

	anySuspendCalls, creatorSuspendCalls int
	gotSuspendTenant                     uuid.UUID
	gotSuspended                         bool
	gotSuspendCreator                    string
	suspendErr                           error
}

func (f *fakeProvisioner) ProvisionTenant(_ context.Context, r auth.ProvisionRequest) (auth.ProvisionResult, error) {
	f.provisionCalls++
	f.gotReq = r
	return f.result, f.err
}

// The suspension pair (cleat#3552, B3a): which of the two a request reached is the whole point, so each
// records its own calls.
func (f *fakeProvisioner) SetTenantSuspended(_ context.Context, id uuid.UUID, suspended bool) error {
	f.anySuspendCalls++
	f.gotSuspendTenant, f.gotSuspended = id, suspended
	return f.suspendErr
}

func (f *fakeProvisioner) SetTenantSuspendedByCreator(_ context.Context, id uuid.UUID, suspended bool, creator string) error {
	f.creatorSuspendCalls++
	f.gotSuspendTenant, f.gotSuspended, f.gotSuspendCreator = id, suspended, creator
	return f.suspendErr
}

func (f *fakeProvisioner) ConfirmProvisionedTenant(_ context.Context, id, keyID uuid.UUID, creator string) (auth.ConfirmResult, error) {
	f.confirmCalls++
	f.gotConfirmTenant, f.gotConfirmKey, f.gotConfirmCreator = id, keyID, creator
	return f.confirmResult, f.confirmErr
}

// provisionFixture is the HTTP chain plus the keys to drive it with. Operator keys are minted into
// the same key table buildChain uses; one carries the provision scope and a principal, one is admin.
type provisionHTTP struct {
	chainFixture
	provisionKey string
}

func buildProvisionHTTP(t *testing.T, wrap func(*notedTarget) func(http.Handler) http.Handler) (*provisionHTTP, *fakeProvisioner) {
	t.Helper()
	f := buildChainAround(t, false, wrap)
	fp := &fakeProvisioner{}
	f.api.provisioner = fp
	return &provisionHTTP{chainFixture: f, provisionKey: f.provisionKey}, fp
}

func postJSON(f chainFixture, path, key, body string) *httptest.ResponseRecorder {
	return f.do(http.MethodPost, path, key, "Content-Type", "application/json", "X-Body", body)
}

// doBody is f.do with a real body (do sends a fixed one).
func doBody(f chainFixture, method, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	return w
}

// confirmBody is the body a confirm of okResult's key carries.
const confirmBody = `{"key_id":"cccccccc-0000-4000-8000-000000000003"}`

func okResult(outcome auth.ProvisionOutcome, withKey bool) auth.ProvisionResult {
	tid := uuid.MustParse("aaaaaaaa-0000-4000-8000-000000000001")
	r := auth.ProvisionResult{Outcome: outcome, Tenant: auth.ProvisionedTenant{
		ID: tid, Name: tid.String(), DisplayName: "Acme", OrgID: uuid.MustParse("bbbbbbbb-0000-4000-8000-000000000002")}}
	if withKey {
		r.Key = &auth.ProvisionedKey{KeyID: uuid.MustParse("cccccccc-0000-4000-8000-000000000003"), Secret: "cleat_sk_the_secret", ValidFor: auth.ProvisionalKeyTTL}
	}
	return r
}

func TestProvisioningOverHTTPCreatesReplaysAndConfirms(t *testing.T) {
	f, fp := buildProvisionHTTP(t, nil)
	const create = "/api/admin/tenants"
	tid := "aaaaaaaa-0000-4000-8000-000000000001"

	// CREATE: 201, the key's secret once, and no caching.
	fp.result = okResult(auth.ProvisionCreated, true)
	w := doBody(f.chainFixture, http.MethodPost, create, f.provisionKey,
		`{"tenant_id":"`+strings.ToUpper(tid)+`","display_name":"Acme","org_name":" Acme Org ","key":{"description":"first"}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q: a response carrying a secret must not be cached", cc)
	}
	var out provisionTenantResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Outcome != "created" || out.Tenant.TenantID != tid || out.Key == nil || out.Key.Secret != "cleat_sk_the_secret" ||
		!out.Key.Provisional || out.Key.ValidForSeconds != 900 || !strings.Contains(out.Key.Confirm, "/api/admin/tenants/"+tid+"/confirm") || !strings.Contains(out.Key.Confirm, out.Key.KeyID) {
		t.Errorf("create body = %+v", out)
	}
	// What the store was asked: the id in canonical lower case, the org name trimmed, the creator the
	// key's PRINCIPAL (a provision key has one), and nothing the caller could have forged.
	if fp.gotReq.TenantID.String() != tid || fp.gotReq.OrgName != "Acme Org" || fp.gotReq.Creator != "clew-service" ||
		fp.gotReq.KeyDescription != "first" {
		t.Errorf("the store was asked %+v", fp.gotReq)
	}

	// REPLAY after the key was confirmed: 200, the tenant, NO key.
	fp.result = okResult(auth.ProvisionReplayed, false)
	w = doBody(f.chainFixture, http.MethodPost, create, f.provisionKey, `{"tenant_id":"`+tid+`","org_name":"Acme Org"}`)
	out = provisionTenantResponse{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != http.StatusOK || out.Outcome != "replayed" || out.Key != nil || strings.Contains(w.Body.String(), "secret") {
		t.Errorf("replay = %d %s, want 200 with no key", w.Code, w.Body.String())
	}

	// CONFIRM.
	fp.confirmResult = auth.ConfirmResult{Confirmed: 1}
	w = doBody(f.chainFixture, http.MethodPost, create+"/"+tid+"/confirm", f.provisionKey, confirmBody)
	if w.Code != http.StatusOK || fp.gotConfirmTenant.String() != tid || fp.gotConfirmCreator != "clew-service" ||
		fp.gotConfirmKey.String() != "cccccccc-0000-4000-8000-000000000003" {
		t.Errorf("confirm = %d %s (tenant %s, key %s, creator %q): the key named in the body must reach the store", w.Code, w.Body.String(), fp.gotConfirmTenant, fp.gotConfirmKey, fp.gotConfirmCreator)
	}

	// An ADMIN key has no principal, so it is its own creator: "operator:<KeyID>".
	fp.result = okResult(auth.ProvisionCreated, true)
	doBody(f.chainFixture, http.MethodPost, create, f.adminKey, `{"org_name":"x"}`)
	if !strings.HasPrefix(fp.gotReq.Creator, "operator:") {
		t.Errorf("an admin key's creator = %q, want operator:<KeyID>", fp.gotReq.Creator)
	}
}

// The credentials that must NOT provision. A tenant key reaches /api/admin/* when the flag is on, so
// the handler has to refuse it itself; the scope gate never sees it.
func TestOnlyAnOperatorCredentialCanProvision(t *testing.T) {
	f, fp := buildProvisionHTTP(t, nil)
	fp.result = okResult(auth.ProvisionCreated, true)
	tid := "aaaaaaaa-0000-4000-8000-000000000001"

	for _, tc := range []struct {
		name, method, path, key string
		want                    int
	}{
		{"a tenant key, create", http.MethodPost, "/api/admin/tenants", f.tenantAKey, http.StatusForbidden},
		{"a tenant key, confirm", http.MethodPost, "/api/admin/tenants/" + tid + "/confirm", f.tenantAKey, http.StatusForbidden},
		{"an unknown tenant key", http.MethodPost, "/api/admin/tenants", f.unknownTenant, http.StatusUnauthorized},
		{"no credential", http.MethodPost, "/api/admin/tenants", "", http.StatusUnauthorized},
		{"a forged operator key", http.MethodPost, "/api/admin/tenants", f.forgedOpKey, http.StatusUnauthorized},
	} {
		w := doBody(f.chainFixture, tc.method, tc.path, tc.key, `{"org_name":"x"}`)
		if w.Code != tc.want {
			t.Errorf("%s = %d %s, want %d", tc.name, w.Code, strings.TrimSpace(w.Body.String()), tc.want)
		}
	}
	if fp.provisionCalls+fp.confirmCalls != 0 {
		t.Errorf("a refused credential reached the store %d times", fp.provisionCalls+fp.confirmCalls)
	}

	// Wrong method, and the flag off: the route does not exist for anyone.
	if w := doBody(f.chainFixture, http.MethodGet, "/api/admin/tenants", f.provisionKey, ``); w.Code != http.StatusForbidden {
		t.Errorf("a provision key with GET = %d, want 403 from its scope", w.Code)
	}
	if w := doBody(f.chainFixture, http.MethodGet, "/api/admin/tenants", f.adminKey, ``); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("an admin key with GET = %d, want 405", w.Code)
	}
	off := false
	old := enableAdminAPI
	enableAdminAPI = &off
	t.Cleanup(func() { enableAdminAPI = old })
	if w := doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants", f.provisionKey, `{"org_name":"x"}`); w.Code != http.StatusNotFound {
		t.Errorf("with the admin API off = %d, want 404", w.Code)
	}
}

func TestAMalformedProvisionRequestNeverReachesTheStore(t *testing.T) {
	f, fp := buildProvisionHTTP(t, nil)
	fp.result = okResult(auth.ProvisionCreated, true)
	for name, body := range map[string]string{
		"not json":             `{`,
		"an unknown field":     `{"org_name":"x","creator":"someone-else"}`,
		"tenant_id not a uuid": `{"tenant_id":"acme","org_name":"x"}`,
		"the nil tenant id":    `{"tenant_id":"00000000-0000-0000-0000-000000000000","org_name":"x"}`,
		"org_id not a uuid":    `{"org_id":"acme"}`,
		"the nil org id":       `{"org_id":"00000000-0000-0000-0000-000000000000"}`,
		"both org fields":      `{"org_id":"bbbbbbbb-0000-4000-8000-000000000002","org_name":"x"}`,
		"neither org field":    `{"display_name":"x"}`,
		"a blank org name":     `{"org_name":"   "}`,
		"trailing garbage":     `{"org_name":"x"} {"org_name":"y"}`,
	} {
		w := doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants", f.provisionKey, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", name, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}
	// Past the limit is a 413 that names it, not a 400 that calls a body malformed that was never read.
	big := doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants", f.provisionKey, `{"org_name":"`+strings.Repeat("a", provisionBodyLimit)+`"}`)
	if big.Code != http.StatusRequestEntityTooLarge || !strings.Contains(big.Body.String(), "limit is") {
		t.Errorf("a body past the limit = %d %s, want 413 naming the limit", big.Code, strings.TrimSpace(big.Body.String()))
	}
	if fp.provisionCalls != 0 {
		t.Errorf("malformed requests reached the store %d times", fp.provisionCalls)
	}
}

// A confirm names its key (cleat-review, #3568). Without one there is no way to know which provisional
// key the creator holds, so the request is refused before the store is asked.
func TestAConfirmMustNameItsKey(t *testing.T) {
	f, fp := buildProvisionHTTP(t, nil)
	tid := "aaaaaaaa-0000-4000-8000-000000000001"
	for name, body := range map[string]string{
		"no body":              ``,
		"an empty object":      `{}`,
		"a key id that is not": `{"key_id":"k1"}`,
		"the nil key id":       `{"key_id":"00000000-0000-0000-0000-000000000000"}`,
		"an unknown field":     `{"key_id":"cccccccc-0000-4000-8000-000000000003","all":true}`,
		"trailing data":        `{"key_id":"cccccccc-0000-4000-8000-000000000003"} {}`,
	} {
		if w := doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants/"+tid+"/confirm", f.provisionKey, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", name, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}
	if fp.confirmCalls != 0 {
		t.Errorf("a confirm that named no key reached the store %d times", fp.confirmCalls)
	}
	// Upper case is the same key; the store receives the parsed value.
	if w := doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants/"+tid+"/confirm", f.provisionKey, `{"key_id":"CCCCCCCC-0000-4000-8000-000000000003"}`); w.Code != http.StatusOK ||
		fp.gotConfirmKey.String() != "cccccccc-0000-4000-8000-000000000003" {
		t.Errorf("an upper-case key id = %d, store saw %s", w.Code, fp.gotConfirmKey)
	}
}

func TestStoreErrorsBecomeTheDocumentedStatuses(t *testing.T) {
	f, fp := buildProvisionHTTP(t, nil)
	tid := "aaaaaaaa-0000-4000-8000-000000000001"
	for _, tc := range []struct {
		name   string
		err    error
		want   int
		reason string
		fields string
	}{
		{"name taken", &auth.ProvisionConflictError{Reason: auth.ConflictNameTaken}, 409, "name_taken", ""},
		{"a differing replay", &auth.ProvisionConflictError{Reason: auth.ConflictTenantDiffers, Fields: []string{"name", "org_id"}}, 409, "tenant_differs", "name,org_id"},
		{"another principal's tenant", &auth.ProvisionConflictError{Reason: auth.ConflictTenantExists}, 409, "tenant_exists", ""},
		{"an unknown org", auth.ErrOrgNotFound, 404, "", ""},
	} {
		fp.err = tc.err
		w := doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants", f.provisionKey, `{"org_name":"x"}`)
		if w.Code != tc.want {
			t.Errorf("%s = %d %s, want %d", tc.name, w.Code, w.Body.String(), tc.want)
			continue
		}
		var body struct {
			Reason string   `json:"reason"`
			Fields []string `json:"fields"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if body.Reason != tc.reason || strings.Join(body.Fields, ",") != tc.fields {
			t.Errorf("%s body = %s, want reason %q fields %q", tc.name, w.Body.String(), tc.reason, tc.fields)
		}
	}

	// An unexplained database error is a 500 that does NOT repeat the error: it can name a table,
	// a constraint or a value.
	fp.err = errors.New(`pq: duplicate key value violates unique constraint "tenants_pkey" (23505)`)
	w := doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants", f.provisionKey, `{"org_name":"x"}`)
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "tenants_pkey") || strings.Contains(w.Body.String(), "pq:") {
		t.Errorf("an unexplained error = %d %s, want a 500 that does not leak it", w.Code, w.Body.String())
	}
	fp.err = nil

	// Confirm.
	fp.confirmErr = auth.ErrTenantNotFound
	if w := doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants/"+tid+"/confirm", f.provisionKey, confirmBody); w.Code != http.StatusNotFound {
		t.Errorf("confirming a tenant that is not this principal's = %d, want 404", w.Code)
	}
	fp.confirmErr = auth.ErrNoProvisionalKey
	w = doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants/"+tid+"/confirm", f.provisionKey, confirmBody)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "no_provisional_key") {
		t.Errorf("confirming a lapsed key = %d %s, want 409 no_provisional_key", w.Code, w.Body.String())
	}
	fp.confirmErr = nil
	if w := doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants/acme/confirm", f.provisionKey, confirmBody); w.Code != http.StatusForbidden {
		// A name that is no id never gets past the scope gate (it is not canonical); an admin key reaches the handler.
		t.Errorf("a provision key confirming a non-id = %d, want 403 from the scope", w.Code)
	}
	if w := doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants/acme/confirm", f.adminKey, confirmBody); w.Code != http.StatusBadRequest {
		t.Errorf("an admin key confirming a non-id = %d, want 400", w.Code)
	}
}

// A worker that cannot provision says so, instead of failing inside a statement.
func TestAWorkerThatCannotProvisionAnswers501(t *testing.T) {
	f, _ := buildProvisionHTTP(t, nil)
	f.api.provisioner = nil
	for _, path := range []string{"/api/admin/tenants", "/api/admin/tenants/aaaaaaaa-0000-4000-8000-000000000001/confirm"} {
		w := doBody(f.chainFixture, http.MethodPost, path, f.provisionKey, `{"org_name":"x"}`)
		if w.Code != http.StatusNotImplemented || !strings.Contains(w.Body.String(), "PostgreSQL or SQL Server") {
			t.Errorf("%s = %d %s, want 501 naming what is supported", path, w.Code, w.Body.String())
		}
	}
	if tenantProvisionerFor(auth.DialectMySQL, nil, &auth.TenantStore{}) != nil {
		t.Error("MySQL was given a provisioner")
	}
	if tenantProvisionerFor(auth.DialectPostgres, nil, nil) != nil {
		t.Error("a nil resolver was given a provisioner")
	}
	if tenantProvisionerFor(auth.DialectPostgres, nil, &auth.TenantStore{}) == nil || tenantProvisionerFor(auth.DialectMSSQL, nil, &auth.TenantStore{}) == nil {
		t.Error("PostgreSQL and SQL Server were refused a provisioner")
	}
}

// The audit log puts the row on the tenant the handler says it acted on (cleat#3559), so the creation
// must say so -- and only once the tenant exists.
func TestProvisioningNamesTheTenantItActedOnForTheAuditLog(t *testing.T) {
	f, fp := buildProvisionHTTP(t, slotAround)
	tid := uuid.MustParse("aaaaaaaa-0000-4000-8000-000000000001")

	fp.result = okResult(auth.ProvisionCreated, true)
	doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants", f.provisionKey, `{"org_name":"x"}`)
	if !f.noted.set || f.noted.tenant != tid {
		t.Errorf("a create noted %v (set=%v), want the new tenant %v", f.noted.tenant, f.noted.set, tid)
	}

	*f.noted = notedTarget{}
	fp.confirmResult = auth.ConfirmResult{Confirmed: 1}
	doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants/"+tid.String()+"/confirm", f.provisionKey, confirmBody)
	if !f.noted.set || f.noted.tenant != tid {
		t.Errorf("a confirm noted %v (set=%v), want %v", f.noted.tenant, f.noted.set, tid)
	}

	for name, run := range map[string]func(){
		"a conflict": func() {
			fp.err = &auth.ProvisionConflictError{Reason: auth.ConflictNameTaken}
			doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants", f.provisionKey, `{"org_name":"x"}`)
		},
		"a malformed body": func() {
			doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants", f.provisionKey, `{`)
		},
		"a confirm that found nothing": func() {
			fp.confirmErr = auth.ErrTenantNotFound
			doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants/"+tid.String()+"/confirm", f.provisionKey, confirmBody)
		},
	} {
		*f.noted = notedTarget{}
		run()
		if f.noted.set {
			t.Errorf("%s noted tenant %v: the audit row would land on a chain for a tenant that was not created", name, f.noted.tenant)
		}
	}
}

// The secret appears in the create response and nowhere else.
func TestTheProvisionalSecretIsNotLogged(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	f, fp := buildProvisionHTTP(t, nil)
	fp.result = okResult(auth.ProvisionCreated, true)
	if w := doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants", f.provisionKey, `{"org_name":"x"}`); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	fp.err = errors.New("pq: boom")
	doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants", f.provisionKey, `{"org_name":"x"}`)
	if !strings.Contains(logs.String(), "admin: provision tenant") {
		t.Fatalf("UNMEASURED: the handler logged nothing, so the absence below proves nothing: %q", logs.String())
	}
	if strings.Contains(logs.String(), "cleat_sk_the_secret") || strings.Contains(logs.String(), f.provisionKey) {
		t.Errorf("a secret reached the log: %s", logs.String())
	}
}

// The whole thing against real databases: HTTP, scope gate, handler and the real store together. The
// secret a create returns authenticates as the new tenant, confirming it keeps it working, and a replay
// afterwards returns the tenant and no key.
func TestProvisioningOverHTTPAgainstRealDatabases(t *testing.T) {
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
			tid := uuid.New()
			orgName := "b2b2-http-" + uuid.NewString()
			t.Cleanup(func() {
				_, _ = db.Exec(`DELETE FROM admin.tenant_api_keys WHERE tenant_id = `+ph(1), tid.String())
				_, _ = db.Exec(`DELETE FROM admin.tenants WHERE tenant_id = `+ph(1), tid.String())
				_, _ = db.Exec(`DELETE FROM admin.orgs WHERE name = `+ph(1), orgName)
			})
			body := `{"tenant_id":"` + tid.String() + `","display_name":"Real","org_name":"` + orgName + `"}`

			w := doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants", f.provisionKey, body)
			if w.Code != http.StatusCreated {
				t.Fatalf("create on %s = %d %s", dialect, w.Code, w.Body.String())
			}
			var out provisionTenantResponse
			_ = json.Unmarshal(w.Body.Bytes(), &out)
			resolve := func() (uuid.UUID, error) {
				h := sha256.Sum256([]byte(out.Key.Secret))
				return ts.ResolveTenantFromAPIKey(context.Background(), h[:])
			}
			if got, err := resolve(); err != nil || got != tid {
				t.Fatalf("the returned secret resolves to %v, %v; want the new tenant %s", got, err, tid)
			}

			if w := doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants/"+tid.String()+"/confirm", f.provisionKey, `{"key_id":"`+out.Key.KeyID+`"}`); w.Code != http.StatusOK {
				t.Fatalf("confirm = %d %s", w.Code, w.Body.String())
			}
			if got, err := resolve(); err != nil || got != tid {
				t.Errorf("the confirmed secret resolves to %v, %v", got, err)
			}

			w = doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants", f.provisionKey, body)
			var again provisionTenantResponse
			_ = json.Unmarshal(w.Body.Bytes(), &again)
			if w.Code != http.StatusOK || again.Outcome != "replayed" || again.Key != nil {
				t.Errorf("replay after confirm = %d %s, want 200 replayed with no key", w.Code, w.Body.String())
			}

			// A different principal's key (the admin key) cannot take the tenant over.
			if w := doBody(f.chainFixture, http.MethodPost, "/api/admin/tenants", f.adminKey, body); w.Code != http.StatusConflict {
				t.Errorf("another principal replaying = %d %s, want 409", w.Code, w.Body.String())
			}
		})
	}
}

var _ = time.Second
