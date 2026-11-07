package main

// cleat#3669 (b) and (c). The 201 from POST /api/admin/tenants/{tenant}/recovery-key tells the operator,
// in its `next` field, and docs/operations/admin-api.md tells them again: use the recovery key to
// POST /api/keys for a permanent key, then DELETE /api/keys/{key_id} the recovery one. Until now that was
// asserted as TEXT (an_operator_recovers_a_locked_out_tenant_over_http_test.go checks that the string
// names the routes), over a fake provisioner, so nothing showed that a key the recovery route mints is
// accepted by the tenant routes it points at.
//
// Here the tenant routes and the recovery route sit behind one REAL store, on PostgreSQL and SQL Server,
// and the tenant's keys are resolved by that store (a chain built the way main.go builds it), so a
// recovery key authenticates only if the store says so.
//
// The second test measures the confirm route against a recovery key, which the docs now describe.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine/testutil"
)

// recoveryStore is a locked-out tenant: created through the provisioning routes by the provision
// principal, confirmed, and then stripped of its only key with ?force=true, which is the act the recovery
// route exists for. h is a handler whose tenant keys resolve against the real store.
type recoveryStore struct {
	h            http.Handler
	f            chainFixture
	db           *sql.DB
	dialect      string
	tid          uuid.UUID
	lostKeyID    string
	lostSecret   string
	adminKey     string
	provisionKey string
}

func (r *recoveryStore) do(t *testing.T, method, path, key, body string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	return w
}

func newLockedOutTenant(t *testing.T, dialect string) *recoveryStore {
	t.Helper()
	db := testutil.TestDB(t, testutil.Dialect(dialect))
	ts, err := auth.NewTenantStoreForDialect(db, dialect)
	if err != nil {
		t.Fatal(err)
	}
	f := buildChain(t, false)
	f.api.provisioner = ts
	f.api.keys = ts
	var asked bool
	mux := http.NewServeMux()
	registerRoutes(mux, f.api)
	r := &recoveryStore{
		f: f, db: db, dialect: dialect, tid: uuid.New(), adminKey: f.adminKey, provisionKey: f.provisionKey,
		// The tenant layer is the REAL store; operators are the fixture's, as on a worker whose operator
		// keys are configured rather than stored.
		h: composeRequestChain(mux, mux, &authChain{
			Tenants: ts, Operators: f.keys, Domains: noDomains{asked: &asked},
		}, "single-tenant"),
	}

	ph := func(n int) string {
		if dialect == auth.DialectMSSQL {
			return "@p" + string(rune('0'+n))
		}
		return "$" + string(rune('0'+n))
	}
	orgName := "3669-" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM admin.tenant_api_keys WHERE tenant_id = `+ph(1), r.tid.String())
		_, _ = db.Exec(`DELETE FROM admin.tenants WHERE tenant_id = `+ph(1), r.tid.String())
		_, _ = db.Exec(`DELETE FROM admin.orgs WHERE name = `+ph(1), orgName)
	})

	w := r.do(t, http.MethodPost, "/api/admin/tenants", r.provisionKey,
		`{"tenant_id":"`+r.tid.String()+`","display_name":"Recoverable","org_name":"`+orgName+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("provision on %s = %d %s", dialect, w.Code, w.Body.String())
	}
	var created provisionTenantResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.Key == nil {
		t.Fatalf("provision reply %s (%v)", w.Body.String(), err)
	}
	if w := r.do(t, http.MethodPost, "/api/admin/tenants/"+r.tid.String()+"/confirm", r.provisionKey,
		`{"key_id":"`+created.Key.KeyID+`"}`); w.Code != http.StatusOK {
		t.Fatalf("confirm = %d %s", w.Code, w.Body.String())
	}
	r.lostKeyID, r.lostSecret = created.Key.KeyID, created.Key.Secret

	// THE KNOWN POSITIVE for the whole test: the key works before the lock-out and stops working after
	// it, so a 401 below means the tenant is locked out and not that the chain refuses everything.
	if w := r.do(t, http.MethodGet, "/api/keys", r.lostSecret, ""); w.Code != http.StatusOK {
		t.Fatalf("the tenant's confirmed key on GET /api/keys = %d %s, want 200", w.Code, w.Body.String())
	}
	if w := r.do(t, http.MethodDelete, "/api/keys/"+r.lostKeyID+"?force=true", r.lostSecret, ""); w.Code != http.StatusOK {
		t.Fatalf("force-revoking the last key = %d %s", w.Code, w.Body.String())
	}
	if w := r.do(t, http.MethodGet, "/api/keys", r.lostSecret, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("the revoked key on GET /api/keys = %d, want 401: the tenant is not locked out", w.Code)
	}
	return r
}

type recoveredKey struct {
	KeyID  string `json:"key_id"`
	Secret string `json:"secret"`
}

func (r *recoveryStore) recover(t *testing.T) recoveredKey {
	t.Helper()
	w := r.do(t, http.MethodPost, "/api/admin/tenants/"+r.tid.String()+"/recovery-key", r.adminKey,
		`{"reason":"cleat#3669 test: the last key was revoked with force=true"}`, "X-Confirm", r.tid.String())
	if w.Code != http.StatusCreated {
		t.Fatalf("recovery-key = %d %s, want 201", w.Code, w.Body.String())
	}
	var out recoveredKey
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.KeyID == "" || out.Secret == "" {
		t.Fatalf("recovery reply %s (%v)", w.Body.String(), err)
	}
	return out
}

type listedKey struct {
	KeyID       string  `json:"key_id"`
	Status      string  `json:"status"`
	Provisional bool    `json:"provisional"`
	ExpiresAt   *string `json:"expires_at"`
}

func (r *recoveryStore) list(t *testing.T, key string) []listedKey {
	t.Helper()
	w := r.do(t, http.MethodGet, "/api/keys", key, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/keys = %d %s, want 200", w.Code, w.Body.String())
	}
	var out struct {
		Keys []listedKey `json:"keys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Keys
}

// The path the 201 documents, walked rather than quoted: the recovery key mints a permanent key, the new
// key revokes the recovery one, and the tenant is left holding exactly the key it made.
func TestTheDocumentedRecoveryPathWorksAgainstARealStore(t *testing.T) {
	for _, dialect := range []string{auth.DialectPostgres, auth.DialectMSSQL} {
		dialect := dialect
		t.Run(dialect, func(t *testing.T) {
			r := newLockedOutTenant(t, dialect)
			rec := r.recover(t)

			// Step 1 of `next`: POST /api/keys with the recovery key.
			w := r.do(t, http.MethodPost, "/api/keys", rec.Secret, `{"description":"permanent, minted with the recovery key"}`)
			if w.Code != http.StatusCreated {
				t.Fatalf("POST /api/keys with the recovery key = %d %s, want 201", w.Code, w.Body.String())
			}
			var minted struct {
				Key struct {
					KeyID       string `json:"key_id"`
					Secret      string `json:"secret"`
					Provisional bool   `json:"provisional"`
					Status      string `json:"status"`
				} `json:"key"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &minted); err != nil || minted.Key.Secret == "" {
				t.Fatalf("mint reply %s (%v)", w.Body.String(), err)
			}
			if minted.Key.Provisional || minted.Key.Status != "active" {
				t.Errorf("the key the recovery key minted is provisional=%v status=%q, want a permanent active key",
					minted.Key.Provisional, minted.Key.Status)
			}

			// Step 2 of `next`: DELETE /api/keys/{recovery key id}. Revoked with the NEW key, so the tenant
			// is never left depending on the recovery one.
			if w := r.do(t, http.MethodDelete, "/api/keys/"+rec.KeyID, minted.Key.Secret, ""); w.Code != http.StatusOK {
				t.Fatalf("DELETE the recovery key = %d %s, want 200", w.Code, w.Body.String())
			}

			// The recovery key stops authenticating at once; the new key is the only live key.
			if w := r.do(t, http.MethodGet, "/api/keys", rec.Secret, ""); w.Code != http.StatusUnauthorized {
				t.Errorf("the revoked recovery key on GET /api/keys = %d, want 401", w.Code)
			}
			live := 0
			for _, k := range r.list(t, minted.Key.Secret) {
				if k.Status == "active" || k.Status == "provisional" {
					live++
					if k.KeyID != minted.Key.KeyID {
						t.Errorf("live key %s is not the one the tenant minted (%s)", k.KeyID, minted.Key.KeyID)
					}
				}
			}
			if live != 1 {
				t.Errorf("the tenant holds %d live keys after the documented path, want exactly 1", live)
			}

			// And the route's guard is real on the same store: with a live key the tenant is not
			// recoverable, so a second call mints nothing.
			w = r.do(t, http.MethodPost, "/api/admin/tenants/"+r.tid.String()+"/recovery-key", r.adminKey,
				`{"reason":"cleat#3669 test: a second call"}`, "X-Confirm", r.tid.String())
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "tenant_has_live_keys") {
				t.Errorf("a second recovery of a tenant with a live key = %d %s, want 409 tenant_has_live_keys", w.Code, w.Body.String())
			}
		})
	}
}

// The confirm route and a recovery key. The recovery key is provisional, and /confirm makes a named
// provisional key of the tenant permanent, so the principal that CREATED the tenant can make the recovery
// key outlive its 15 minutes by naming its key_id. An admin operator is not that principal and cannot.
// docs/operations/admin-api.md says both; this is what it was written from.
func TestOnlyTheTenantsCreatorCanConfirmARecoveryKey(t *testing.T) {
	for _, dialect := range []string{auth.DialectPostgres, auth.DialectMSSQL} {
		dialect := dialect
		t.Run(dialect, func(t *testing.T) {
			r := newLockedOutTenant(t, dialect)
			rec := r.recover(t)
			confirm := "/api/admin/tenants/" + r.tid.String() + "/confirm"
			body := `{"key_id":"` + rec.KeyID + `"}`

			provisional := func() bool {
				for _, k := range r.list(t, rec.Secret) {
					if k.KeyID == rec.KeyID {
						return k.Provisional
					}
				}
				t.Fatalf("the recovery key %s is not in the tenant's key list", rec.KeyID)
				return false
			}
			if !provisional() {
				t.Fatal("a freshly minted recovery key is not provisional")
			}

			// The admin operator that minted it is not the tenant's creator: refused, and the key is
			// still provisional afterwards (the status alone would pass with the key made permanent).
			if w := r.do(t, http.MethodPost, confirm, r.adminKey, body); w.Code != http.StatusNotFound {
				t.Errorf("an admin operator confirming a recovery key = %d %s, want 404", w.Code, w.Body.String())
			}
			if !provisional() {
				t.Error("an admin operator's refused confirm made the recovery key permanent")
			}

			// The creator (the provision-scoped principal that created the tenant) can.
			if w := r.do(t, http.MethodPost, confirm, r.provisionKey, body); w.Code != http.StatusOK {
				t.Fatalf("the creator confirming a recovery key = %d %s, want 200", w.Code, w.Body.String())
			}
			if provisional() {
				t.Error("the creator's confirm left the recovery key provisional")
			}
			for _, k := range r.list(t, rec.Secret) {
				if k.KeyID == rec.KeyID && (k.Status != "active" || k.ExpiresAt != nil) {
					t.Errorf("the confirmed recovery key is status=%q expires_at=%v, want active with no expiry", k.Status, k.ExpiresAt)
				}
			}
		})
	}
}
