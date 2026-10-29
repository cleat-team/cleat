package main

// cleat#3552, B2c. POST/GET /api/keys and DELETE /api/keys/{key_id}, driven through composeAuthChain with
// the real mux, so the credential, the operator confinement and the handler are the ones a worker runs. The
// store is a fake except in the last test, which puts the real store behind the same HTTP and rotates a
// tenant's key end to end.

import (
	"bytes"
	"context"
	"encoding/json"
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

type fakeKeys struct {
	mintCalls, listCalls, revokeCalls int
	gotTenant                         uuid.UUID
	gotDesc                           string
	gotExpires                        *time.Time
	gotKey                            uuid.UUID
	gotForce                          bool
	mintErr, listErr, revokeErr       error
	listed                            []auth.TenantKey
	revoked                           auth.RevokeResult
}

var testKeyID = uuid.MustParse("dddddddd-0000-4000-8000-000000000004")

func (f *fakeKeys) MintKey(_ context.Context, tid uuid.UUID, desc string, exp *time.Time) (auth.TenantKey, string, error) {
	f.mintCalls++
	f.gotTenant, f.gotDesc, f.gotExpires = tid, desc, exp
	return auth.TenantKey{KeyID: testKeyID, Description: desc, CreatedAt: time.Now().UTC(), ExpiresAt: exp}, "cleat_sk_minted_secret", f.mintErr
}
func (f *fakeKeys) ListKeys(_ context.Context, tid uuid.UUID) ([]auth.TenantKey, error) {
	f.listCalls++
	f.gotTenant = tid
	return f.listed, f.listErr
}
func (f *fakeKeys) RevokeKey(_ context.Context, tid, kid uuid.UUID, force bool) (auth.RevokeResult, error) {
	f.revokeCalls++
	f.gotTenant, f.gotKey, f.gotForce = tid, kid, force
	return f.revoked, f.revokeErr
}

func buildKeysHTTP(t *testing.T) (chainFixture, *fakeKeys) {
	t.Helper()
	f := buildChain(t, false)
	fk := &fakeKeys{}
	f.api.keys = fk
	return f, fk
}

func TestATenantMintsListsAndRevokesItsKeysOverHTTP(t *testing.T) {
	f, fk := buildKeysHTTP(t)

	// MINT: 201, the secret once, not cached, for the AUTHENTICATED tenant.
	exp := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	w := doBody(f, http.MethodPost, "/api/keys?tenant_id="+tenantB, f.tenantAKey,
		`{"description":"ci","expires_at":"`+exp.Format(time.RFC3339)+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("mint = %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q on a response carrying a secret", w.Header().Get("Cache-Control"))
	}
	var minted struct{ Key tenantKeyJSON }
	_ = json.Unmarshal(w.Body.Bytes(), &minted)
	if minted.Key.Secret != "cleat_sk_minted_secret" || minted.Key.KeyID != testKeyID.String() || minted.Key.Status != "active" {
		t.Errorf("mint body = %+v", minted.Key)
	}
	if fk.gotTenant.String() != tenantA || fk.gotDesc != "ci" || fk.gotExpires == nil || !fk.gotExpires.Equal(exp) {
		t.Errorf("the store was asked for tenant %s desc %q expires %v; want tenant A (the caller, NOT the one a query string names), ci, %v", fk.gotTenant, fk.gotDesc, fk.gotExpires, exp)
	}

	// An empty body is a mint with nothing set.
	if w := doBody(f, http.MethodPost, "/api/keys", f.tenantAKey, ``); w.Code != http.StatusCreated || fk.gotExpires != nil || fk.gotDesc != "" {
		t.Errorf("an empty-body mint = %d (expires %v desc %q)", w.Code, fk.gotExpires, fk.gotDesc)
	}

	// LIST: no secret anywhere, every status named.
	past := time.Now().Add(-time.Hour)
	fk.listed = []auth.TenantKey{
		{KeyID: uuid.New(), Description: "live", CreatedAt: time.Now()},
		{KeyID: uuid.New(), Description: "gone", CreatedAt: time.Now(), DisabledAt: &past},
		{KeyID: uuid.New(), Description: "lapsed", CreatedAt: time.Now(), ExpiresAt: &past},
		{KeyID: uuid.New(), Description: "prov", CreatedAt: time.Now(), Provisional: true},
	}
	w = doBody(f, http.MethodGet, "/api/keys", f.tenantAKey, ``)
	var list struct{ Keys []tenantKeyJSON }
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	var statuses []string
	for _, k := range list.Keys {
		statuses = append(statuses, k.Status)
	}
	if w.Code != http.StatusOK || strings.Join(statuses, ",") != "active,revoked,expired,provisional" {
		t.Errorf("list = %d, statuses %v", w.Code, statuses)
	}
	if strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "key_hash") {
		t.Errorf("a listing mentions a secret or a hash: %s", w.Body.String())
	}

	// REVOKE.
	id := uuid.New()
	w = doBody(f, http.MethodDelete, "/api/keys/"+strings.ToUpper(id.String()), f.tenantAKey, ``)
	if w.Code != http.StatusOK || fk.gotKey != id || fk.gotForce || fk.gotTenant.String() != tenantA {
		t.Errorf("revoke = %d %s (key %s force %v tenant %s)", w.Code, w.Body.String(), fk.gotKey, fk.gotForce, fk.gotTenant)
	}
	doBody(f, http.MethodDelete, "/api/keys/"+id.String()+"?force=true", f.tenantAKey, ``)
	if !fk.gotForce {
		t.Error("?force=true did not reach the store")
	}
	fk.revoked = auth.RevokeResult{AlreadyRevoked: true}
	if w := doBody(f, http.MethodDelete, "/api/keys/"+id.String(), f.tenantAKey, ``); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"already_revoked":true`) {
		t.Errorf("a repeat revoke = %d %s", w.Code, w.Body.String())
	}
}

func TestOnlyATenantKeyReachesTheKeyRoutes(t *testing.T) {
	f, fk := buildKeysHTTP(t)
	for _, tc := range []struct {
		name, method, path, key string
		want                    int
	}{
		{"an admin operator key, mint", http.MethodPost, "/api/keys", f.adminKey, http.StatusForbidden},
		{"an admin operator key, list", http.MethodGet, "/api/keys", f.adminKey, http.StatusForbidden},
		{"an admin operator key, revoke", http.MethodDelete, "/api/keys/" + uuid.NewString(), f.adminKey, http.StatusForbidden},
		{"a provision operator key, mint", http.MethodPost, "/api/keys", f.provisionKey, http.StatusForbidden},
		{"a forged operator key", http.MethodGet, "/api/keys", f.forgedOpKey, http.StatusUnauthorized},
		{"an unknown tenant key", http.MethodGet, "/api/keys", f.unknownTenant, http.StatusUnauthorized},
		{"no credential", http.MethodGet, "/api/keys", "", http.StatusUnauthorized},
	} {
		if w := doBody(f, tc.method, tc.path, tc.key, `{}`); w.Code != tc.want {
			t.Errorf("%s = %d %s, want %d", tc.name, w.Code, strings.TrimSpace(w.Body.String()), tc.want)
		}
	}
	if fk.mintCalls+fk.listCalls+fk.revokeCalls != 0 {
		t.Errorf("a refused credential reached the store %d times", fk.mintCalls+fk.listCalls+fk.revokeCalls)
	}

	// Authentication off: the tenant in the context proves nothing, so the routes refuse.
	f.api.requireAuth = false
	if w := doBody(f, http.MethodGet, "/api/keys", f.tenantAKey, ``); w.Code != http.StatusForbidden || fk.listCalls != 0 {
		t.Errorf("with --require-auth off = %d (store calls %d), want 403 and none", w.Code, fk.listCalls)
	}
	f.api.requireAuth = true
	f.api.keys = nil
	if w := doBody(f, http.MethodGet, "/api/keys", f.tenantAKey, ``); w.Code != http.StatusNotImplemented {
		t.Errorf("with no key manager = %d, want 501", w.Code)
	}
}

func TestAMalformedKeyRequestNeverReachesTheStore(t *testing.T) {
	f, fk := buildKeysHTTP(t)
	for name, tc := range map[string]struct{ method, path, body string }{
		"a past expiry":                {"POST", "/api/keys", `{"expires_at":"2001-01-01T00:00:00Z"}`},
		"an expiry that is not a time": {"POST", "/api/keys", `{"expires_at":"tomorrow"}`},
		"a description too long":       {"POST", "/api/keys", `{"description":"` + strings.Repeat("a", maxKeyDescription+1) + `"}`},
		"an unknown field":             {"POST", "/api/keys", `{"tenant_id":"` + tenantB + `"}`},
		"trailing data":                {"POST", "/api/keys", `{} {}`},
		"not json":                     {"POST", "/api/keys", `{`},
		"a key id not a uuid":          {"DELETE", "/api/keys/k1", ``},
		"the nil key id":               {"DELETE", "/api/keys/00000000-0000-0000-0000-000000000000", ``},
		"a path below a key id":        {"DELETE", "/api/keys/" + uuid.NewString() + "/x", ``},
		"a force that is not a bool":   {"DELETE", "/api/keys/" + uuid.NewString() + "?force=maybe", ``},
	} {
		if w := doBody(f, tc.method, tc.path, f.tenantAKey, tc.body); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", name, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}
	if w := doBody(f, http.MethodPost, "/api/keys", f.tenantAKey, `{"description":"`+strings.Repeat("a", keyBodyLimit)+`"}`); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a body past the limit = %d, want 413", w.Code)
	}
	// Methods.
	for _, tc := range []struct{ method, path string }{{"PUT", "/api/keys"}, {"DELETE", "/api/keys"}, {"GET", "/api/keys/" + uuid.NewString()}, {"POST", "/api/keys/" + uuid.NewString()}} {
		if w := doBody(f, tc.method, tc.path, f.tenantAKey, ``); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", tc.method, tc.path, w.Code)
		}
	}
	if fk.mintCalls+fk.listCalls+fk.revokeCalls != 0 {
		t.Errorf("malformed requests reached the store %d times", fk.mintCalls+fk.listCalls+fk.revokeCalls)
	}
}

func TestKeyStoreErrorsBecomeTheDocumentedStatuses(t *testing.T) {
	f, fk := buildKeysHTTP(t)
	id := uuid.NewString()
	for _, tc := range []struct {
		name, method, path string
		set                func()
		want               int
		reason             string
	}{
		{"mint past the cap", "POST", "/api/keys", func() { fk.mintErr = auth.ErrKeyLimit }, 409, "key_limit"},
		{"mint for a tenant that vanished", "POST", "/api/keys", func() { fk.mintErr = auth.ErrTenantNotFound }, 404, ""},
		{"revoke a key that is not this tenant's", "DELETE", "/api/keys/" + id, func() { fk.revokeErr = auth.ErrKeyNotFound }, 404, ""},
		{"revoke the last key", "DELETE", "/api/keys/" + id, func() { fk.revokeErr = auth.ErrLastKey }, 409, "last_key"},
	} {
		fk.mintErr, fk.revokeErr = nil, nil
		tc.set()
		w := doBody(f, tc.method, tc.path, f.tenantAKey, ``)
		if w.Code != tc.want || (tc.reason != "" && !strings.Contains(w.Body.String(), `"reason":"`+tc.reason+`"`)) {
			t.Errorf("%s = %d %s, want %d %s", tc.name, w.Code, strings.TrimSpace(w.Body.String()), tc.want, tc.reason)
		}
	}
	// An unexplained database error does not leak.
	fk.mintErr, fk.revokeErr = nil, nil
	fk.listErr = context.DeadlineExceeded
	w := doBody(f, http.MethodGet, "/api/keys", f.tenantAKey, ``)
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "deadline") {
		t.Errorf("an unexplained error = %d %s", w.Code, w.Body.String())
	}
}

func TestAMintedSecretIsNotLogged(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	f, _ := buildKeysHTTP(t)
	doBody(f, http.MethodPost, "/api/keys", f.tenantAKey, `{}`)
	if !strings.Contains(logs.String(), "api keys: minted") {
		t.Fatalf("UNMEASURED: the handler logged nothing, so the absence below proves nothing: %q", logs.String())
	}
	if strings.Contains(logs.String(), "cleat_sk_minted_secret") || strings.Contains(logs.String(), f.tenantAKey) {
		t.Errorf("a secret reached the log: %s", logs.String())
	}
}

// A tenant rotates its key end to end, against real databases: the real store behind the real
// authentication. The new key is minted with the old one, works on its own, the old one is revoked and
// stops working at once, and the last key cannot be revoked by accident.
func TestATenantRotatesItsKeyEndToEndAgainstRealDatabases(t *testing.T) {
	for _, dialect := range []string{auth.DialectPostgres, auth.DialectMSSQL} {
		dialect := dialect
		t.Run(dialect, func(t *testing.T) {
			db := testutil.TestDB(t, testutil.Dialect(dialect))
			ts, err := auth.NewTenantStoreForDialect(db, dialect)
			if err != nil {
				t.Fatal(err)
			}
			enableAdminRoutes(t)
			api, _, _, _ := twoTenantServer(t, true)
			api.keys = ts
			mux := http.NewServeMux()
			registerRoutes(mux, api)
			handler := composeAuthChain(mux, mux, authChain{Tenants: ts, Operators: &keyTable{}, Domains: noDomains{asked: new(bool)}})
			call := func(method, path, key, body string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(method, path, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+key)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, req)
				return w
			}

			// A real tenant with a first key, made by the provisioning handshake.
			tid := uuid.New()
			orgName := "b2c-http-" + uuid.NewString()
			res, err := ts.ProvisionTenant(context.Background(), auth.ProvisionRequest{TenantID: tid, OrgName: orgName, Creator: "test"})
			if err != nil {
				t.Fatalf("provisioning the tenant: %v", err)
			}
			if _, err := ts.ConfirmProvisionedTenant(context.Background(), tid, res.Key.KeyID, "test"); err != nil {
				t.Fatal(err)
			}
			ph := func(n int) string {
				if dialect == auth.DialectMSSQL {
					return "@p" + string(rune('0'+n))
				}
				return "$" + string(rune('0'+n))
			}
			t.Cleanup(func() {
				_, _ = db.Exec(`DELETE FROM admin.tenant_api_keys WHERE tenant_id = `+ph(1), tid.String())
				_, _ = db.Exec(`DELETE FROM admin.tenants WHERE tenant_id = `+ph(1), tid.String())
				_, _ = db.Exec(`DELETE FROM admin.orgs WHERE name = `+ph(1), orgName)
			})
			old := res.Key.Secret
			oldID := res.Key.KeyID.String()

			// Mint the replacement WITH THE OLD KEY.
			w := call(http.MethodPost, "/api/keys", old, `{"description":"rotation"}`)
			if w.Code != http.StatusCreated {
				t.Fatalf("mint on %s = %d %s", dialect, w.Code, w.Body.String())
			}
			var minted struct{ Key tenantKeyJSON }
			_ = json.Unmarshal(w.Body.Bytes(), &minted)
			fresh := minted.Key.Secret
			// The new key works on its own, and sees both.
			w = call(http.MethodGet, "/api/keys", fresh, ``)
			var list struct{ Keys []tenantKeyJSON }
			_ = json.Unmarshal(w.Body.Bytes(), &list)
			if w.Code != http.StatusOK || len(list.Keys) != 2 || strings.Contains(w.Body.String(), fresh) {
				t.Fatalf("list with the new key = %d, %d keys, secret leaked=%v", w.Code, len(list.Keys), strings.Contains(w.Body.String(), fresh))
			}

			// Revoke the OLD key with the NEW one; the old one stops working at once.
			if w := call(http.MethodDelete, "/api/keys/"+oldID, fresh, ``); w.Code != http.StatusOK {
				t.Fatalf("revoking the old key = %d %s", w.Code, w.Body.String())
			}
			if w := call(http.MethodGet, "/api/keys", old, ``); w.Code != http.StatusUnauthorized {
				t.Errorf("the revoked key still authenticates: %d", w.Code)
			}

			// The new key is now the last. A plain revoke is refused; it keeps working.
			if w := call(http.MethodDelete, "/api/keys/"+minted.Key.KeyID, fresh, ``); w.Code != http.StatusConflict {
				t.Fatalf("revoking the last key = %d %s, want 409", w.Code, w.Body.String())
			}
			if w := call(http.MethodGet, "/api/keys", fresh, ``); w.Code != http.StatusOK {
				t.Errorf("the refused revoke locked the tenant out: %d", w.Code)
			}
			// Forced, it goes, and the tenant is locked out.
			if w := call(http.MethodDelete, "/api/keys/"+minted.Key.KeyID+"?force=true", fresh, ``); w.Code != http.StatusOK {
				t.Fatalf("forced revoke = %d %s", w.Code, w.Body.String())
			}
			if w := call(http.MethodGet, "/api/keys", fresh, ``); w.Code != http.StatusUnauthorized {
				t.Errorf("a force-revoked key still authenticates: %d", w.Code)
			}
		})
	}
}
