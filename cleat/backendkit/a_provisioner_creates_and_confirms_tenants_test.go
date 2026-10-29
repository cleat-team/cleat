package backendkit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/cleat/backendkit"
)

// cleat#3552, B2d. The Provisioner decodes JSON the worker writes. The files under testdata/provisioning are
// that JSON, captured from the real handlers by cmd/cleat-worker's
// TestTheProvisioningResponsesMatchWhatBackendkitDecodes, which fails if the worker's wording drifts from them.
// So these tests decode what the worker really says, not what this package imagines it says.

type wireFixture struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

func loadWire(t *testing.T, name string) wireFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "provisioning", name+".json"))
	if err != nil {
		t.Fatalf("no fixture %q: %v", name, err)
	}
	var f wireFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("fixture %q: %v", name, err)
	}
	return f
}

// provisionerAgainst serves one fixture to every request and records the last request it saw.
type seenRequest struct {
	method, path, auth string
	body               map[string]any
	count              int
}

func provisionerAgainst(t *testing.T, fixture string) (*backendkit.Provisioner, *seenRequest) {
	t.Helper()
	fx := loadWire(t, fixture)
	seen := &seenRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.count++
		seen.method, seen.path, seen.auth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		seen.body = nil
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &seen.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fx.Status)
		_, _ = w.Write(fx.Body)
	}))
	t.Cleanup(srv.Close)
	return backendkit.NewProvisioner(backendkit.New(srv.URL, backendkit.WithAPIKey("PROVISIONER-KEY"))), seen
}

const (
	tid = "aaaaaaaa-0000-4000-8000-000000000001"
	kid = "cccccccc-0000-4000-8000-000000000003"
)

var acme = backendkit.TenantRequest{TenantID: tid, OrgName: "Acme Org", KeyDescription: "first"}

func TestEnsureTenantSendsWhatTheWorkerExpectsAndDecodesTheFirstKey(t *testing.T) {
	p, seen := provisionerAgainst(t, "created")
	tenant, key, err := p.EnsureTenant(context.Background(), acme)
	if err != nil {
		t.Fatal(err)
	}
	if seen.method != http.MethodPost || seen.path != "/api/admin/tenants" || seen.auth != "Bearer PROVISIONER-KEY" {
		t.Errorf("request was %s %s with Authorization %q", seen.method, seen.path, seen.auth)
	}
	if seen.body["tenant_id"] != tid || seen.body["org_name"] != "Acme Org" {
		t.Errorf("body = %v", seen.body)
	}
	if k, _ := seen.body["key"].(map[string]any); k["description"] != "first" {
		t.Errorf("key.description missing from %v", seen.body)
	}
	for _, empty := range []string{"name", "display_name", "org_id"} {
		if _, sent := seen.body[empty]; sent {
			t.Errorf("an unset %s was sent as an empty string: the worker would read it as a value", empty)
		}
	}

	if tenant.ID != tid || tenant.Name != tid || tenant.DisplayName != "Acme" || tenant.OrgID != "bbbbbbbb-0000-4000-8000-000000000002" || !tenant.Created {
		t.Errorf("tenant = %+v", tenant)
	}
	if key == nil || key.KeyID != kid || key.Secret != "cleat_sk_the_secret" || key.ValidFor != 15*time.Minute {
		t.Errorf("key = %#v, want id %s, the secret, 15 minutes", key, kid)
	}
}

func TestARepeatedEnsureTenantReturnsTheTenantAndAKeyOnlyWhileUnconfirmed(t *testing.T) {
	p, _ := provisionerAgainst(t, "replayed_with_key")
	tenant, key, err := p.EnsureTenant(context.Background(), acme)
	if err != nil || tenant.Created || key == nil || key.Secret == "" {
		t.Errorf("an unconfirmed replay = %+v, %v, %v; want the tenant, Created false and a fresh key", tenant, key, err)
	}
	p, _ = provisionerAgainst(t, "replayed_no_key")
	tenant, key, err = p.EnsureTenant(context.Background(), acme)
	if err != nil || tenant.Created || tenant.ID != tid || key != nil {
		t.Errorf("a confirmed replay = %+v, %v, %v; want the tenant and NO key", tenant, key, err)
	}
}

func TestEnsureTenantNamesTheRefusalsTheWorkerDocuments(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		is      error
		status  int
	}{
		{"name_taken", backendkit.ErrNameTaken, 409},
		{"tenant_exists", backendkit.ErrTenantExists, 409},
		{"tenant_differs", backendkit.ErrTenantDiffers, 409},
		{"org_not_found", backendkit.ErrOrgNotFound, 404},
		{"unavailable", backendkit.ErrProvisioningUnavailable, 501},
	} {
		p, _ := provisionerAgainst(t, tc.fixture)
		req := acme
		_, key, err := p.EnsureTenant(context.Background(), req)
		if !errors.Is(err, tc.is) || key != nil {
			t.Errorf("%s: err = %v, key = %v; want %v and no key", tc.fixture, err, key, tc.is)
			continue
		}
		var up *backendkit.UpstreamStatusError
		if !errors.As(err, &up) || up.Status != tc.status {
			t.Errorf("%s: the status is not recoverable alongside the sentinel: %v", tc.fixture, err)
		}
		if strings.Contains(err.Error(), "cleat_sk_") {
			t.Errorf("%s: a secret reached the error text: %v", tc.fixture, err)
		}
		// Each refusal is exactly one of the sentinels: a caller that branches on one must not be sent down
		// another branch.
		for _, other := range []error{backendkit.ErrNameTaken, backendkit.ErrTenantExists, backendkit.ErrTenantDiffers, backendkit.ErrOrgNotFound, backendkit.ErrProvisioningUnavailable, backendkit.ErrNoProvisionalKey} {
			if other != tc.is && errors.Is(err, other) {
				t.Errorf("%s also matches %v", tc.fixture, other)
			}
		}
	}

	p, _ := provisionerAgainst(t, "tenant_differs")
	_, _, err := p.EnsureTenant(context.Background(), acme)
	var differs *backendkit.TenantDiffersError
	if !errors.As(err, &differs) || strings.Join(differs.Fields, ",") != "name,org_id" {
		t.Errorf("the differing fields were not recovered: %v", err)
	}
}

func TestEnsureTenantRefusesARequestThatCouldNotBeIdempotentBeforeSendingIt(t *testing.T) {
	p, seen := provisionerAgainst(t, "created")
	ctx := context.Background()
	for name, req := range map[string]backendkit.TenantRequest{
		"no tenant id: a retry would create a second tenant": {OrgName: "Acme Org"},
		"neither org": {TenantID: tid},
		"both orgs":   {TenantID: tid, OrgID: "bbbbbbbb-0000-4000-8000-000000000002", OrgName: "Acme Org"},
	} {
		if _, _, err := p.EnsureTenant(ctx, req); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if seen.count != 0 {
		t.Errorf("%d requests reached the worker for requests that were invalid on their face", seen.count)
	}
}

func TestConfirmKeyNamesTheKeyAndTreatsARepeatAsDone(t *testing.T) {
	for _, fixture := range []string{"confirm_ok", "confirm_already"} {
		p, seen := provisionerAgainst(t, fixture)
		if err := p.ConfirmKey(context.Background(), tid, kid); err != nil {
			t.Errorf("%s: %v", fixture, err)
		}
		if seen.method != http.MethodPost || seen.path != "/api/admin/tenants/"+tid+"/confirm" || seen.auth != "Bearer PROVISIONER-KEY" || seen.body["key_id"] != kid {
			t.Errorf("%s: request was %s %s auth %q body %v", fixture, seen.method, seen.path, seen.auth, seen.body)
		}
	}

	p, _ := provisionerAgainst(t, "confirm_no_provisional_key")
	if err := p.ConfirmKey(context.Background(), tid, kid); !errors.Is(err, backendkit.ErrNoProvisionalKey) {
		t.Errorf("a lapsed or replaced key = %v, want ErrNoProvisionalKey", err)
	}

	p, seen := provisionerAgainst(t, "confirm_ok")
	if p.ConfirmKey(context.Background(), "", kid) == nil || p.ConfirmKey(context.Background(), tid, "") == nil || seen.count != 0 {
		t.Errorf("a confirm missing its tenant or key id was sent (%d requests) or did not error", seen.count)
	}
}

// A confirm's 404 is the same bytes whether the tenant is missing, is someone else's, or the worker's admin API is
// off. Naming it "tenant not found" would send an app hunting for the wrong cause, so it is not named.
func TestAConfirm404IsNotGivenAMeaningTheWorkerDoesNotGiveIt(t *testing.T) {
	notFound, adminOff := loadWire(t, "confirm_not_found"), loadWire(t, "confirm_admin_api_off")
	if notFound.Status != adminOff.Status || !bytes.Equal(notFound.Body, adminOff.Body) {
		t.Fatalf("the premise is gone: the two 404s now differ (%s vs %s), so a sentinel for one may be possible", notFound.Body, adminOff.Body)
	}
	for _, fixture := range []string{"confirm_not_found", "confirm_admin_api_off"} {
		p, _ := provisionerAgainst(t, fixture)
		err := p.ConfirmKey(context.Background(), tid, kid)
		var up *backendkit.UpstreamStatusError
		if !errors.As(err, &up) || up.Status != http.StatusNotFound {
			t.Errorf("%s: err = %v, want an *UpstreamStatusError with status 404", fixture, err)
		}
		for _, s := range []error{backendkit.ErrNameTaken, backendkit.ErrTenantExists, backendkit.ErrTenantDiffers, backendkit.ErrOrgNotFound, backendkit.ErrProvisioningUnavailable, backendkit.ErrNoProvisionalKey} {
			if errors.Is(err, s) {
				t.Errorf("%s matches %v", fixture, s)
			}
		}
	}
}

func TestEveryFixtureIsDecodedByATest(t *testing.T) {
	used := map[string]bool{}
	// The fixtures the tests above load. A fixture added to testdata/ with no test that decodes it is a worker
	// response this client has never been shown.
	for _, n := range []string{"created", "replayed_with_key", "replayed_no_key", "name_taken", "tenant_exists", "tenant_differs",
		"org_not_found", "unavailable", "confirm_ok", "confirm_already", "confirm_no_provisional_key", "confirm_not_found", "confirm_admin_api_off"} {
		used[n+".json"] = true
	}
	entries, err := os.ReadDir(filepath.Join("testdata", "provisioning"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !used[e.Name()] {
			t.Errorf("testdata/provisioning/%s is decoded by no test", e.Name())
		}
		delete(used, e.Name())
	}
	for missing := range used {
		t.Errorf("a test loads %s, which does not exist", missing)
	}
}

func TestAFirstKeyNeverPrintsItsSecret(t *testing.T) {
	const secret = "cleat_sk_do_not_print_me"
	k := backendkit.FirstKey{KeyID: kid, Secret: secret, ValidFor: 15 * time.Minute}
	type holder struct {
		Tenant string
		Key    backendkit.FirstKey
		Ptr    *backendkit.FirstKey
	}
	h := holder{Tenant: tid, Key: k, Ptr: &k}

	for _, v := range []any{k, &k, h, &h, []backendkit.FirstKey{k}, map[string]backendkit.FirstKey{"a": k}} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
			if out := fmt.Sprintf(verb, v); strings.Contains(out, secret) || strings.Contains(out, fmt.Sprintf("%x", secret)) {
				t.Errorf("fmt %s of %T printed the secret: %s", verb, v, out)
			}
		}
	}
	if out := fmt.Sprint(k); !strings.Contains(out, kid) || !strings.Contains(out, "redacted") {
		t.Errorf("a redacted key should still say which key it is: %s", out)
	}

	var buf bytes.Buffer
	for _, h := range []slog.Handler{slog.NewJSONHandler(&buf, nil), slog.NewTextHandler(&buf, nil)} {
		slog.New(h).Info("provisioned", "key", k, "pointer", &k, slog.Any("holder", h))
	}
	if strings.Contains(buf.String(), secret) {
		t.Errorf("slog printed the secret: %s", buf.String())
	}
	if !strings.Contains(buf.String(), kid) {
		t.Errorf("slog lost the key id too: %s", buf.String())
	}

	// The field itself is still there to store.
	if k.Secret != secret {
		t.Error("redaction must not change the value")
	}
}
