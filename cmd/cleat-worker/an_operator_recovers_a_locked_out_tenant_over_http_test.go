package main

// cleat#3578: POST /api/admin/tenants/{tenant}/recovery-key, through the real authentication chain.
//
// The store decides whether a tenant is locked out (auth/a_recovery_key_is_minted_only_for_a_tenant_with_no_live_key_test.go,
// on real databases). What is under test here is everything the route decides itself, and the order it
// decides it in: WHO may call it, that the tenant id is typed back, that a reason is required and what a
// valid one is, what lands in the audit row, and above all where the SECRET goes. The store is a fake that
// records what it was asked, because "the store was not reached" is half of every refusal below.
//
// THE SECRET is searched for, by value, in the places it must not be: the audit document the handler
// noted, the log, and every refusal body. A test that only checked the happy path's reply would pass a
// handler that also wrote the secret to the log.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
)

// recoveryProvisioner is the fake provisioner plus the one method the route asserts.
type recoveryProvisioner struct {
	*fakeProvisioner
	calls     int
	gotTenant uuid.UUID
	gotDesc   string
	key       auth.RecoveryKey
	err       error
}

func (p *recoveryProvisioner) MintRecoveryKey(_ context.Context, tid uuid.UUID, desc string) (auth.RecoveryKey, error) {
	p.calls++
	p.gotTenant, p.gotDesc = tid, desc
	return p.key, p.err
}

const recoveryTenant = "aaaaaaaa-0000-4000-8000-000000000001"

// recoverySecret is what GenerateAPIKey makes: the prefix and 64 lower-case hex digits. It is built at run
// time because the literal of that shape is a gitleaks generic-api-key finding (cleat#3663), and the
// scanner's allowlist is by exact value, which this should not need.
var recoverySecret = "cleat_sk_" + strings.Repeat("0123456789abcdef", 4)

type recoveryHTTP struct {
	*provisionHTTP
	rp   *recoveryProvisioner
	meta *string
	logs *bytes.Buffer
}

func buildRecoveryHTTP(t *testing.T) *recoveryHTTP {
	t.Helper()
	var meta string
	wrap := func(noted *notedTarget) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx, slot := auth.WithAuditTarget(r.Context())
				next.ServeHTTP(w, r.WithContext(ctx))
				noted.tenant, noted.set = slot.Tenant()
				meta = slot.Metadata()
			})
		}
	}
	f, fp := buildProvisionHTTP(t, wrap)
	rp := &recoveryProvisioner{fakeProvisioner: fp, key: auth.RecoveryKey{
		KeyID: uuid.MustParse("cccccccc-0000-4000-8000-0000000000aa"), Secret: recoverySecret, ValidFor: auth.ProvisionalKeyTTL}}
	f.api.provisioner = rp
	logs := &bytes.Buffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &recoveryHTTP{provisionHTTP: f, rp: rp, meta: &meta, logs: logs}
}

// recover sends one request. confirm "" means "do not send X-Confirm".
func (h *recoveryHTTP) recoverTenant(method, tenant, key, confirm, body string) *httptest.ResponseRecorder {
	*h.meta = ""
	h.noted.set = false
	req := httptest.NewRequest(method, "/api/admin/tenants/"+tenant+"/recovery-key", strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if confirm != "" {
		req.Header.Set("X-Confirm", confirm)
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	return w
}

func okBody(reason string) string {
	b, _ := json.Marshal(map[string]string{"reason": reason})
	return string(b)
}

// neverTheSecret fails if the secret is anywhere it must not be.
func (h *recoveryHTTP) neverTheSecret(t *testing.T, where string, w *httptest.ResponseRecorder) {
	t.Helper()
	if strings.Contains(w.Body.String(), recoverySecret) {
		t.Errorf("%s: the response body carries the secret", where)
	}
	if strings.Contains(*h.meta, recoverySecret) {
		t.Errorf("%s: the audit document carries the secret: %s", where, *h.meta)
	}
	if strings.Contains(h.logs.String(), recoverySecret) {
		t.Errorf("%s: the log carries the secret:\n%s", where, h.logs.String())
	}
}

func TestAnAdminKeyMintsARecoveryKeyAndTheSecretGoesNowhereElse(t *testing.T) {
	h := buildRecoveryHTTP(t)
	const reason = "locked out: the last key was revoked with force=true on 2026-10-30"
	w := h.recoverTenant(http.MethodPost, recoveryTenant, h.adminKey, recoveryTenant, okBody("  "+reason+"  "))
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q: a response carrying a secret must not be cached", cc)
	}
	var out struct {
		TenantID  string `json:"tenant_id"`
		KeyID     string `json:"key_id"`
		Secret    string `json:"secret"`
		Provision bool   `json:"provisional"`
		ExpiresAt string `json:"expires_at"`
		ValidFor  int    `json:"valid_for_seconds"`
		Next      string `json:"next"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.TenantID != recoveryTenant || out.KeyID != h.rp.key.KeyID.String() || out.Secret != recoverySecret || !out.Provision || out.ValidFor != 900 || out.ExpiresAt == "" {
		t.Errorf("body = %+v", out)
	}
	if !strings.Contains(out.Next, "POST /api/keys") || !strings.Contains(out.Next, "DELETE /api/keys/"+out.KeyID) || strings.Contains(out.Next, "/confirm") {
		t.Errorf("next = %q: the documented path is POST /api/keys then revoke the recovery key, and it must not rely on the confirm route", out.Next)
	}
	// What the store was asked: the canonical tenant, once, and a description that is NOT the reason.
	if h.rp.calls != 1 || h.rp.gotTenant.String() != recoveryTenant {
		t.Errorf("store asked %d time(s) for %s", h.rp.calls, h.rp.gotTenant)
	}
	if strings.Contains(h.rp.gotDesc, "locked out") {
		t.Errorf("the key's description %q holds the reason; that is the audit row's job", h.rp.gotDesc)
	}
	// The audit row: on the TARGET tenant's chain, with the TRIMMED reason and the key id, not the secret.
	if !h.noted.set || h.noted.tenant.String() != recoveryTenant {
		t.Errorf("the audit target is %v (set=%v), want the recovered tenant", h.noted.tenant, h.noted.set)
	}
	var doc map[string]string
	if err := json.Unmarshal([]byte(*h.meta), &doc); err != nil || doc["reason"] != reason || doc["key_id"] != out.KeyID {
		t.Errorf("audit document = %q (%v), want the trimmed reason and the key id", *h.meta, err)
	}
	// The secret is in the reply and nowhere else.
	if strings.Contains(*h.meta, recoverySecret) || strings.Contains(h.logs.String(), recoverySecret) {
		t.Errorf("the secret reached the audit document or the log\n%s\n%s", *h.meta, h.logs.String())
	}
	if !strings.Contains(h.logs.String(), "minted a recovery key") || !strings.Contains(h.logs.String(), out.KeyID) {
		t.Errorf("no log line records the mint and its key id:\n%s", h.logs.String())
	}
}

func TestOnlyAnAdminOperatorKeyReachesTheRecoveryRoute(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  func(*recoveryHTTP) string
		want int
	}{
		{"a provision-scoped operator key: refused by the scope gate", func(h *recoveryHTTP) string { return h.provisionKey }, http.StatusForbidden},
		{"a tenant's own key: it can reach /api/admin when the admin API is on, and must not recover", func(h *recoveryHTTP) string { return h.tenantAKey }, http.StatusForbidden},
		{"a key in no table", func(h *recoveryHTTP) string { return h.unknownTenant }, http.StatusUnauthorized},
		{"an operator-shaped key in no table", func(h *recoveryHTTP) string { return h.forgedOpKey }, http.StatusUnauthorized},
		{"no key at all", func(h *recoveryHTTP) string { return "" }, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := buildRecoveryHTTP(t)
			w := h.recoverTenant(http.MethodPost, recoveryTenant, tc.key(h), recoveryTenant, okBody("a good reason"))
			if w.Code != tc.want {
				t.Errorf("status %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if h.rp.calls != 0 {
				t.Errorf("the store was reached %d time(s) by a caller who may not recover a tenant", h.rp.calls)
			}
			h.neverTheSecret(t, tc.name, w)
		})
	}
	// The control: the same request with the admin key reaches the store, so the refusals above are about the key.
	h := buildRecoveryHTTP(t)
	if w := h.recoverTenant(http.MethodPost, recoveryTenant, h.adminKey, recoveryTenant, okBody("a good reason")); w.Code != http.StatusCreated || h.rp.calls != 1 {
		t.Errorf("control: admin key got %d, store calls %d", w.Code, h.rp.calls)
	}
}

func TestTheRecoveryRouteChecksTheRequestBeforeItReadsAnything(t *testing.T) {
	for _, tc := range []struct {
		name, tenant, confirm, body string
		want                        int
	}{
		{"no X-Confirm", recoveryTenant, "", okBody("r"), 400},
		{"X-Confirm of another tenant", recoveryTenant, "bbbbbbbb-0000-4000-8000-000000000002", okBody("r"), 400},
		{"X-Confirm in upper case is not the canonical id", recoveryTenant, strings.ToUpper(recoveryTenant), okBody("r"), 400},
		{"the default tenant", uuid.Nil.String(), uuid.Nil.String(), okBody("r"), 400},
		{"a tenant that is not a UUID", "not-a-uuid", "not-a-uuid", okBody("r"), 400},
		{"a body that is not JSON", recoveryTenant, recoveryTenant, `not json`, 400},
		{"an unknown field", recoveryTenant, recoveryTenant, `{"reason":"r","force":true}`, 400},
		{"two objects", recoveryTenant, recoveryTenant, okBody("r") + okBody("r"), 400},
		{"no reason field", recoveryTenant, recoveryTenant, `{}`, 400},
		{"a body over the limit", recoveryTenant, recoveryTenant, okBody(strings.Repeat("x", 5000)), 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := buildRecoveryHTTP(t)
			w := h.recoverTenant(http.MethodPost, tc.tenant, h.adminKey, tc.confirm, tc.body)
			if w.Code != tc.want {
				t.Errorf("status %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if h.rp.calls != 0 {
				t.Errorf("the store was reached %d time(s)", h.rp.calls)
			}
			if *h.meta != "" || h.noted.set {
				t.Errorf("a refused request recorded audit metadata %q / target %v", *h.meta, h.noted.set)
			}
		})
	}
}

func TestARecoveryReasonIsRequiredBoundedAndNeverKeyShaped(t *testing.T) {
	long := strings.Repeat("é", maxRecoveryReason+1) // runes, not bytes
	for _, tc := range []struct {
		name, reason string
	}{
		{"empty", ""},
		{"only spaces", "   \t "},
		{"over the length in characters", long},
		{"a newline in it", "line one\nline two"},
		{"a NUL in it", "bad\x00reason"},
		{"an escape in it", "bad\x1b[31mreason"},
		// THE COORDINATOR'S CASE. A reason holding the shape of a key cannot be stored: a credential
		// pasted into a free-text field must not reach the audit chain, which is append-only and hashed.
		{"the exact shape of the key the store is about to mint", "see " + recoverySecret},
		{"a tenant key prefix", "cleat_sk_anything"},
		{"a tenant key prefix in upper case", "CLEAT_SK_ANYTHING"},
		{"an operator key prefix", "pasted cleat_op_0123456789abcdef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := buildRecoveryHTTP(t)
			w := h.recoverTenant(http.MethodPost, recoveryTenant, h.adminKey, recoveryTenant, okBody(tc.reason))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", w.Code, w.Body.String())
			}
			if h.rp.calls != 0 {
				t.Errorf("the store was reached with a reason that is not acceptable")
			}
			// The refusal never repeats the reason, the field most likely to hold something it should not.
			if tc.reason != "" && strings.TrimSpace(tc.reason) != "" && strings.Contains(w.Body.String(), strings.TrimSpace(tc.reason)) {
				t.Errorf("the 400 body repeats the reason: %s", w.Body.String())
			}
			if strings.Contains(h.logs.String(), "cleat_sk_") || strings.Contains(h.logs.String(), "cleat_op_") {
				t.Errorf("a refused key-shaped reason reached the log:\n%s", h.logs.String())
			}
			if *h.meta != "" {
				t.Errorf("a refused reason reached the audit document: %q", *h.meta)
			}
		})
	}
	t.Run("a long but acceptable reason, and non-ASCII text, are accepted", func(t *testing.T) {
		h := buildRecoveryHTTP(t)
		ok := strings.Repeat("é", maxRecoveryReason) // exactly the limit, in characters
		if w := h.recoverTenant(http.MethodPost, recoveryTenant, h.adminKey, recoveryTenant, okBody(ok)); w.Code != http.StatusCreated {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		var doc map[string]string
		if err := json.Unmarshal([]byte(*h.meta), &doc); err != nil || doc["reason"] != ok {
			t.Errorf("the stored reason is not the one given (%v)", err)
		}
	})
	t.Run("a reason with quotes and braces cannot change the shape of the audit document", func(t *testing.T) {
		h := buildRecoveryHTTP(t)
		nasty := `x"} , "key_id":"forged", {"y":"`
		if w := h.recoverTenant(http.MethodPost, recoveryTenant, h.adminKey, recoveryTenant, okBody(nasty)); w.Code != http.StatusCreated {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		var doc map[string]string
		if err := json.Unmarshal([]byte(*h.meta), &doc); err != nil || doc["reason"] != nasty || doc["key_id"] != h.rp.key.KeyID.String() {
			t.Errorf("audit document %q (%v): the reason broke out of its string", *h.meta, err)
		}
	})
}

func TestTheRecoveryRouteAnswersWhateverTheStoreRefusesAndLeaksNothing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		want       int
		wantTarget bool   // the tenant exists, so the attempt is on ITS chain
		wantMeta   string // substring of the audit document
	}{
		{"a tenant that does not exist", auth.ErrTenantNotFound, 404, false, ""},
		{"a tenant that still has a live key", &auth.RecoveryRefusedError{Reason: auth.RecoveryRefusedLiveKeys, LiveKeys: 2}, 409, true, `"refused":"tenant_has_live_keys"`},
		{"a suspended tenant", &auth.RecoveryRefusedError{Reason: auth.RecoveryRefusedSuspended}, 409, true, `"refused":"tenant_suspended"`},
		{"a database failure", errors.New("pq: relation admin.tenant_api_keys is broken"), 500, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := buildRecoveryHTTP(t)
			h.rp.err = tc.err
			h.rp.key = auth.RecoveryKey{} // a refusal has no key
			w := h.recoverTenant(http.MethodPost, recoveryTenant, h.adminKey, recoveryTenant, okBody("a good reason"))
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if w.Header().Get("Cache-Control") == "no-store" && tc.want != 201 {
				// not wrong, but a refusal has no secret to protect; pin that none is present.
				if strings.Contains(w.Body.String(), "secret") {
					t.Errorf("a refusal carries a secret field: %s", w.Body.String())
				}
			}
			if h.noted.set != tc.wantTarget {
				t.Errorf("audit target set=%v, want %v", h.noted.set, tc.wantTarget)
			}
			if tc.wantMeta != "" && !strings.Contains(*h.meta, tc.wantMeta) {
				t.Errorf("audit document %q does not record what refused it (%s)", *h.meta, tc.wantMeta)
			}
			if tc.wantMeta != "" && !strings.Contains(*h.meta, `"reason":"a good reason"`) {
				t.Errorf("a refused attempt must still record the reason: %q", *h.meta)
			}
			if tc.want == 500 && strings.Contains(w.Body.String(), "broken") {
				t.Errorf("the 500 body leaks the database error: %s", w.Body.String())
			}
			if tc.want == 404 && strings.TrimSpace(w.Body.String()) != `{"error":"not found"}` {
				t.Errorf("404 body = %s", w.Body.String())
			}
			h.neverTheSecret(t, tc.name, w)
		})
	}
}

func TestTheRecoveryRouteIsPOSTOnlyAndNeedsAProvisionerThatCanMint(t *testing.T) {
	h := buildRecoveryHTTP(t)
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		if w := h.recoverTenant(m, recoveryTenant, h.adminKey, recoveryTenant, ""); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s answered %d, want 405", m, w.Code)
		}
	}
	if h.rp.calls != 0 {
		t.Errorf("a non-POST reached the store")
	}
	// A provisioner without the method (the plain fake) answers 501 rather than panicking or minting.
	f, _ := buildProvisionHTTP(t, nil)
	w := doRecovery(f, f.adminKey, recoveryTenant, recoveryTenant, okBody("a good reason"))
	if w.Code != http.StatusNotImplemented {
		t.Errorf("a provisioner that cannot mint answered %d: %s", w.Code, w.Body.String())
	}
}

func doRecovery(f *provisionHTTP, key, tenant, confirm, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/admin/tenants/"+tenant+"/recovery-key", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Confirm", confirm)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	return w
}

// The handler's OWN admin check, with the scope gate out of the way. Through the chain a provision key is
// refused by the gate before the handler runs, so a test through the chain stays green with this check
// deleted: the layer holding it up is not the layer it names. Calling the handler directly with a
// provision-scoped operator in the context is the only way to see the second layer.
func TestTheRecoveryHandlerRefusesANonAdminOperatorOnItsOwn(t *testing.T) {
	h := buildRecoveryHTTP(t)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/tenants/"+recoveryTenant+"/recovery-key", strings.NewReader(okBody("a good reason")))
	req.Header.Set("X-Confirm", recoveryTenant)
	req = req.WithContext(auth.WithOperator(req.Context(), auth.Operator{
		KeyID: "1c1b7f3e-5f43-4a8e-bf3a-0d6a1a2b3c4d", Scopes: auth.ScopeProvision, Principal: "clew-service"}))
	w := httptest.NewRecorder()
	h.api.handleRecoveryKey(w, req, recoveryTenant)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a provision-scoped operator reached the handler and got %d: %s", w.Code, w.Body.String())
	}
	if h.rp.calls != 0 {
		t.Errorf("the store was reached by a provision-scoped operator")
	}
}
