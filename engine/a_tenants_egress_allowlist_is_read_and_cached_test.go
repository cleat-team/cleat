package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#1565. The store is consulted on every dial, including every redirect
// hop, so the cache is what makes a per-tenant policy affordable where it has
// to be enforced. These pin the two properties that matter: it caches, and a
// failure to read is never a grant.
func TestTheAllowlistCacheHonoursItsTTL(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	clk := time.Unix(1_700_000_000, 0)
	s := &TenantEgressStore{
		DB:      db,
		Dialect: DialectPostgres,
		TTL:     time.Minute,
		now:     func() time.Time { return clk },
	}
	const tenant = "00000000-0000-0000-0000-000000000000"
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`DELETE FROM admin.tenant_egress_allow WHERE tenant_id = $1`, tenant); err != nil {
		t.Fatalf("clear: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM admin.tenant_egress_allow WHERE tenant_id = $1`, tenant)
	})

	first, err := s.For(ctx, tenant)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if first.Permits("api.example.com") {
		t.Fatal("an empty allowlist permitted a host")
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO admin.tenant_egress_allow (tenant_id, host) VALUES ($1, 'api.example.com')`,
		tenant); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Within the TTL the decision is the cached one. Asserted rather than
	// assumed, because "it caches" and "it re-reads every time and happens to
	// be fast" are indistinguishable from the outside.
	cached, err := s.For(ctx, tenant)
	if err != nil {
		t.Fatalf("For (cached): %v", err)
	}
	if cached.Permits("api.example.com") {
		t.Error("the new row was visible inside the TTL; the cache is not caching")
	}

	clk = clk.Add(2 * time.Minute)
	fresh, err := s.For(ctx, tenant)
	if err != nil {
		t.Fatalf("For (expired): %v", err)
	}
	if !fresh.Permits("api.example.com") {
		t.Error("the row is still invisible after the TTL elapsed; the cache never expires")
	}
}

// A tenant that is absent from the context is NOT an empty allowlist. Both
// deny, but only one of them says something an operator can act on.
func TestAMissingTenantIsAnErrorNotAnEmptyList(t *testing.T) {
	s := &TenantEgressStore{Dialect: DialectPostgres}
	list, err := s.For(context.Background(), "")
	if err == nil {
		t.Fatal("an empty tenant id returned a list rather than an error")
	}
	if list != nil {
		t.Error("a list was returned alongside the error; a caller might use it")
	}
	// The message is what an operator reads, so it has to name the actual
	// problem rather than blaming the list.
	if !strings.Contains(err.Error(), "no tenant") {
		t.Errorf("the error is %q; it should say there is no tenant, not imply the "+
			"allowlist is wrong", err)
	}
}
