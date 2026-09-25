package plugintest

import (
	"context"
	"fmt"
	"sync"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
)

// FakeSecrets is an in-memory (tenant, name) -> value store implementing
// plugin.Secrets, standing in for engine.SecretStore in a plugin's own tests
// -- the same role fakeDBStore-style fakes play for a plugin's SQL. cleat#1992.
//
// WHY SHARED RATHER THAN WRITTEN PER PLUGIN. datadogexport and pagerdutyalert
// both needed one for their handleCreate/handleUpdate/handleDelete and
// background-loop tests the moment their api_key/routing_key columns moved
// into tenant secrets; a third copy was one PR away (oauthprovider). Same
// reasoning as AssertMigrationsDoSomething above: a fake this small still
// drifts if it is retyped per package, and the drift is invisible until two
// copies disagree about something like retire-then-get semantics.
//
// Zero value is not usable; construct with NewFakeSecrets.
type FakeSecrets struct {
	mu      sync.RWMutex
	values  map[string]map[string]string
	retired map[string]map[string]bool
}

// NewFakeSecrets returns an empty FakeSecrets.
func NewFakeSecrets() *FakeSecrets {
	return &FakeSecrets{
		values:  make(map[string]map[string]string),
		retired: make(map[string]map[string]bool),
	}
}

// Seed sets a value directly, bypassing ctx/tenant resolution -- for test
// setup that wants a secret to already exist before a handler or background
// call is exercised, the same role a direct append to a fake DB store's slice
// plays for SQL-backed fakes.
func (s *FakeSecrets) Seed(tenantID, name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setLocked(tenantID, name, value)
}

func (s *FakeSecrets) setLocked(tenantID, name, value string) {
	if s.values[tenantID] == nil {
		s.values[tenantID] = make(map[string]string)
	}
	s.values[tenantID][name] = value
	if s.retired[tenantID] != nil {
		delete(s.retired[tenantID], name)
	}
}

func (s *FakeSecrets) getLocked(tenantID, name string) (string, error) {
	if s.retired[tenantID][name] {
		return "", fmt.Errorf("plugintest.FakeSecrets: %s/%s: %w", tenantID, name, plugin.ErrSecretNotFound)
	}
	v, ok := s.values[tenantID][name]
	if !ok {
		return "", fmt.Errorf("plugintest.FakeSecrets: %s/%s: %w", tenantID, name, plugin.ErrSecretNotFound)
	}
	return v, nil
}

func (s *FakeSecrets) retireLocked(tenantID, name string) (int64, error) {
	if _, ok := s.values[tenantID][name]; !ok {
		return 0, nil
	}
	if s.retired[tenantID] == nil {
		s.retired[tenantID] = make(map[string]bool)
	}
	if s.retired[tenantID][name] {
		return 0, nil
	}
	s.retired[tenantID][name] = true
	return 1, nil
}

// tenantIDFromContext resolves the same value production code does:
// auth.WithTenantID / the real auth.Middleware and auth.TenantIDFromContext
// share one underlying mechanism (both delegate to internal/tenantctx), so a
// test context built either way resolves here exactly as it would against a
// real engine.SecretStore.
func tenantIDFromContext(ctx context.Context) (string, error) {
	tid, ok := auth.TenantIDFromContext(ctx)
	if !ok {
		return "", fmt.Errorf("plugintest.FakeSecrets: no tenant in context")
	}
	return tid.String(), nil
}

func (s *FakeSecrets) Get(ctx context.Context, name string) (string, error) {
	tid, err := tenantIDFromContext(ctx)
	if err != nil {
		return "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getLocked(tid, name)
}

func (s *FakeSecrets) Put(ctx context.Context, name, value string) error {
	tid, err := tenantIDFromContext(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setLocked(tid, name, value)
	return nil
}

func (s *FakeSecrets) Retire(ctx context.Context, name string) (int64, error) {
	tid, err := tenantIDFromContext(ctx)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retireLocked(tid, name)
}

func (s *FakeSecrets) ForTenant(tenantID string) plugin.TenantSecrets {
	return &fakeTenantSecrets{store: s, tenantID: tenantID}
}

type fakeTenantSecrets struct {
	store    *FakeSecrets
	tenantID string
}

func (t *fakeTenantSecrets) Get(_ context.Context, name string) (string, error) {
	t.store.mu.RLock()
	defer t.store.mu.RUnlock()
	return t.store.getLocked(t.tenantID, name)
}

func (t *fakeTenantSecrets) Put(_ context.Context, name, value string) error {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	t.store.setLocked(t.tenantID, name, value)
	return nil
}

func (t *fakeTenantSecrets) Retire(_ context.Context, name string) (int64, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	return t.store.retireLocked(t.tenantID, name)
}

var _ plugin.Secrets = (*FakeSecrets)(nil)
