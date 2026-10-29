package backendkit_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/cleat/backendkit"
)

// cleat#3552, B2d. One app, many tenants: each tenant has its own key and the worker 403s a request whose
// key belongs to another tenant, so the client that carries a tenant's calls must carry that tenant's key and
// nobody else's.

// authSeen records the Authorization header of every request the server receives.
type authSeen struct {
	mu   sync.Mutex
	auth []string
}

func (a *authSeen) handler(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.auth = append(a.auth, r.Header.Get("Authorization"))
	a.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (a *authSeen) last() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.auth) == 0 {
		return "<no request>"
	}
	return a.auth[len(a.auth)-1]
}

func TestForTenantCarriesTheTenantsKeyAndNeverTheBases(t *testing.T) {
	seen := &authSeen{}
	srv := httptest.NewServer(http.HandlerFunc(seen.handler))
	defer srv.Close()

	base := backendkit.New(srv.URL, backendkit.WithAPIKey("BASE-KEY"))
	base.TenantID = "base-tenant"
	tc := base.ForTenant("tenant-a", "key-a")

	if tc.TenantID != "tenant-a" || tc.APIKey != "key-a" || tc.BaseURL != base.BaseURL {
		t.Errorf("ForTenant = %+v", *tc)
	}
	if tc.HTTPClient != base.HTTPClient {
		t.Error("a tenant client must share the base's http.Client, or a hundred tenants are a hundred connection pools")
	}
	if base.APIKey != "BASE-KEY" || base.TenantID != "base-tenant" {
		t.Errorf("ForTenant changed the base client: %+v", *base)
	}

	if _, err := tc.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := seen.last(); got != "Bearer key-a" {
		t.Errorf("a tenant client sent Authorization %q, want %q", got, "Bearer key-a")
	}

	// An empty key is the caller's explicit input. It must NOT fall back to the base's key: that key belongs to
	// someone else (the provisioner, or another tenant), and a request that quietly used it would act as them.
	nokey := base.ForTenant("tenant-b", "")
	if _, err := nokey.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := seen.last(); got != "" {
		t.Errorf("a client made with no key sent Authorization %q, want none", got)
	}
}

func TestTenantClientsResolvesEachTenantsKeyOnEveryCall(t *testing.T) {
	seen := &authSeen{}
	srv := httptest.NewServer(http.HandlerFunc(seen.handler))
	defer srv.Close()

	keys := map[string]string{"tenant-a": "key-a-1", "tenant-b": "key-b"}
	calls := 0
	tcs := &backendkit.TenantClients{
		Base: backendkit.New(srv.URL, backendkit.WithAPIKey("BASE-KEY")),
		KeyResolver: func(_ context.Context, id string) (string, error) {
			calls++
			return keys[id], nil
		},
	}
	ctx := context.Background()
	call := func(tenant string) string {
		c, err := tcs.For(ctx, tenant)
		if err != nil {
			t.Fatalf("For(%s): %v", tenant, err)
		}
		if _, err := c.Health(ctx); err != nil {
			t.Fatal(err)
		}
		return seen.last()
	}

	if got := call("tenant-a"); got != "Bearer key-a-1" {
		t.Errorf("tenant-a sent %q", got)
	}
	if got := call("tenant-b"); got != "Bearer key-b" {
		t.Errorf("tenant-b sent %q: it must not carry tenant-a's key", got)
	}
	// Nothing is cached: a key the app rotated is used on the very next call.
	keys["tenant-a"] = "key-a-2"
	if got := call("tenant-a"); got != "Bearer key-a-2" {
		t.Errorf("after a rotation tenant-a sent %q, want the new key", got)
	}
	if calls != 3 {
		t.Errorf("the resolver was called %d times for 3 requests; the library must not cache keys", calls)
	}
}

func TestTenantClientsRefusesWhatItCannotServeSafely(t *testing.T) {
	base := backendkit.New("http://worker.invalid", backendkit.WithAPIKey("BASE-KEY"))
	ctx := context.Background()
	boom := errors.New("the vault is sealed")

	for _, tc := range []struct {
		name     string
		tcs      *backendkit.TenantClients
		tenant   string
		wantIs   error
		wantText string
	}{
		{"no key found", &backendkit.TenantClients{Base: base, KeyResolver: func(context.Context, string) (string, error) { return "", nil }}, "t1", backendkit.ErrNoTenantKey, "t1"},
		{"the resolver says there is none", &backendkit.TenantClients{Base: base, KeyResolver: func(context.Context, string) (string, error) { return "", backendkit.ErrNoTenantKey }}, "t1", backendkit.ErrNoTenantKey, ""},
		{"the resolver fails", &backendkit.TenantClients{Base: base, KeyResolver: func(context.Context, string) (string, error) { return "SECRET-NOT-RETURNED", boom }}, "t1", boom, "t1"},
		{"no tenant id", &backendkit.TenantClients{Base: base, KeyResolver: func(context.Context, string) (string, error) { return "k", nil }}, "", nil, "tenant id"},
		{"no resolver", &backendkit.TenantClients{Base: base}, "t1", nil, "KeyResolver"},
		{"no base", &backendkit.TenantClients{KeyResolver: func(context.Context, string) (string, error) { return "k", nil }}, "t1", nil, "Base"},
	} {
		c, err := tc.tcs.For(ctx, tc.tenant)
		if err == nil || c != nil {
			t.Errorf("%s: got client %v and error %v, want an error and no client", tc.name, c, err)
			continue
		}
		if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
			t.Errorf("%s: %v does not wrap %v", tc.name, err, tc.wantIs)
		}
		if tc.wantText != "" && !strings.Contains(err.Error(), tc.wantText) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.wantText)
		}
		if strings.Contains(err.Error(), "SECRET-NOT-RETURNED") || strings.Contains(err.Error(), "BASE-KEY") {
			t.Errorf("%s: a key reached the error text: %v", tc.name, err)
		}
	}
	var nilTCs *backendkit.TenantClients
	if _, err := nilTCs.For(ctx, "t1"); err == nil {
		t.Error("a nil *TenantClients returned a client")
	}
}
