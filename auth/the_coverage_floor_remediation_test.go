package auth

// cleat#2487. The functions below had no direct test, only indirect coverage
// through callers that stub or fake the interface: TenantForHost,
// CountTenantDomains, allTenantIDs and scopedRead (host_binding.go) are
// exercised today only through fakeDomains in host_binding_test.go, which
// never calls the real, DB-backed TenantStore methods; CreateOAuthAPIKey and
// RevokeOAuthAPIKeys/RevokeExpiredOAuthAPIKeys are exercised in
// oauthallow_test.go only via a raw INSERT/UPDATE fixture, never through the
// TenantStore methods themselves; CreateOrg and SetTenantSuspended had no
// caller in any test at all; and TenantIDFromRequest/WithSubject/
// SubjectFromContext are pure context helpers nothing called directly.

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

func TestCreateOrg(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	ts := NewTenantStore(db)

	// admin.orgs.name is unique, so the name carries a fresh uuid the same
	// way seedTenant's does -- a literal name would collide with a prior run
	// against a database this test does not get to recreate.
	name := "coverage-floor-remediation-org-" + uuid.New().String()
	oid, err := ts.CreateOrg(context.Background(), name)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if oid == uuid.Nil {
		t.Error("CreateOrg returned the nil uuid")
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM admin.orgs WHERE org_id = $1`, oid)
	})
}

func TestCreateOrg_NotImplementedOffPostgres(t *testing.T) {
	ts, err := NewTenantStoreForDialect(&sql.DB{}, DialectMySQL)
	if err != nil {
		t.Fatalf("NewTenantStoreForDialect: %v", err)
	}
	if _, err := ts.CreateOrg(context.Background(), "x"); err == nil {
		t.Error("CreateOrg on MySQL returned nil error, want a not-implemented refusal")
	}
}

func seedTenantDomain(t *testing.T, db *sql.DB, hostname string, tenantID uuid.UUID) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO tenant_domains (hostname, tenant_id) VALUES ($1, $2)`,
		hostname, tenantID.String()); err != nil {
		t.Fatalf("seed tenant_domains row for %q: %v", hostname, err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tenant_domains WHERE hostname = $1`, hostname)
	})
}

func TestTenantForHost(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	ts := NewTenantStore(db)
	tenantA := seedTenant(t, db, DialectPostgres, "host-binding-a")
	tenantB := seedTenant(t, db, DialectPostgres, "host-binding-b")
	seedTenantDomain(t, db, "owned-by-a.example.com", tenantA)

	ok, err := ts.TenantForHost(context.Background(), "owned-by-a.example.com", tenantA)
	if err != nil {
		t.Fatalf("TenantForHost(own host): %v", err)
	}
	if !ok {
		t.Error("TenantForHost(own host) = false, want true")
	}

	ok, err = ts.TenantForHost(context.Background(), "owned-by-a.example.com", tenantB)
	if err != nil {
		t.Fatalf("TenantForHost(another tenant's host): %v", err)
	}
	if ok {
		t.Error("TenantForHost(another tenant's host) = true, want false")
	}

	ok, err = ts.TenantForHost(context.Background(), "nobody-owns-this.example.com", tenantA)
	if err != nil {
		t.Fatalf("TenantForHost(unregistered host): %v", err)
	}
	if ok {
		t.Error("TenantForHost(unregistered host) = true, want false")
	}
}

func TestCountTenantDomains(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	ts := NewTenantStore(db)
	tenantA := seedTenant(t, db, DialectPostgres, "count-domains-a")

	n, err := ts.CountTenantDomains(context.Background())
	if err != nil {
		t.Fatalf("CountTenantDomains(none seeded by this test): %v", err)
	}
	_ = n // other tests in this package may have left rows; only the next assertion needs an exact count

	seedTenantDomain(t, db, "count-domains.example.com", tenantA)

	n, err = ts.CountTenantDomains(context.Background())
	if err != nil {
		t.Fatalf("CountTenantDomains(after seeding one): %v", err)
	}
	if n < 1 {
		t.Errorf("CountTenantDomains() = %d after seeding one row, want >= 1", n)
	}
}

func TestAllTenantIDs(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	ts := NewTenantStore(db)
	tenantA := seedTenant(t, db, DialectPostgres, "all-tenant-ids-a")

	ids, err := ts.allTenantIDs(context.Background())
	if err != nil {
		t.Fatalf("allTenantIDs: %v", err)
	}
	found := false
	for _, id := range ids {
		if id == tenantA {
			found = true
		}
	}
	if !found {
		t.Errorf("allTenantIDs() = %v, want it to include seeded tenant %s", ids, tenantA)
	}
}

func TestCreateOAuthAPIKeyAndRevoke(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	ts := NewTenantStore(db)
	tenantID := seedTenant(t, db, DialectPostgres, "oauth-key-lifecycle")
	const identity = "coverage-floor-remediation:oauth-identity"
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM admin.tenant_api_keys WHERE oauth_identity = $1`, identity)
	})

	if err := ts.CreateOAuthAPIKey(context.Background(), tenantID,
		"coverage-floor-remediation", "raw-key-material", time.Now().Add(time.Hour), identity); err != nil {
		t.Fatalf("CreateOAuthAPIKey: %v", err)
	}

	n, err := ts.RevokeOAuthAPIKeys(context.Background(), identity)
	if err != nil {
		t.Fatalf("RevokeOAuthAPIKeys: %v", err)
	}
	if n != 1 {
		t.Errorf("RevokeOAuthAPIKeys(%q) revoked %d rows, want 1", identity, n)
	}

	// A second revoke finds nothing left to disable: disabled_at IS NULL
	// already excludes the row this test just revoked.
	n, err = ts.RevokeOAuthAPIKeys(context.Background(), identity)
	if err != nil {
		t.Fatalf("RevokeOAuthAPIKeys (second call): %v", err)
	}
	if n != 0 {
		t.Errorf("RevokeOAuthAPIKeys(%q) a second time revoked %d rows, want 0", identity, n)
	}
}

func TestRevokeOAuthAPIKeys_EmptyIdentityIsRefused(t *testing.T) {
	ts := NewTenantStore(&sql.DB{})
	if _, err := ts.RevokeOAuthAPIKeys(context.Background(), ""); err == nil {
		t.Error("RevokeOAuthAPIKeys(\"\") returned nil error, want a refusal")
	}
}

func TestRevokeOAuthAPIKeys_NotImplementedOffPostgres(t *testing.T) {
	ts, err := NewTenantStoreForDialect(&sql.DB{}, DialectMSSQL)
	if err != nil {
		t.Fatalf("NewTenantStoreForDialect: %v", err)
	}
	if _, err := ts.RevokeOAuthAPIKeys(context.Background(), "x"); err == nil {
		t.Error("RevokeOAuthAPIKeys on MSSQL returned nil error, want a not-implemented refusal")
	}
}

func TestRevokeExpiredOAuthAPIKeys(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	ts := NewTenantStore(db)
	tenantID := seedTenant(t, db, DialectPostgres, "oauth-expiry-sweep")
	const expiredIdentity = "coverage-floor-remediation:expired"
	const liveIdentity = "coverage-floor-remediation:live"
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM admin.tenant_api_keys WHERE oauth_identity IN ($1, $2)`,
			expiredIdentity, liveIdentity)
	})

	if err := ts.CreateOAuthAPIKey(context.Background(), tenantID,
		"expired", "raw-key-expired", time.Now().Add(-time.Hour), expiredIdentity); err != nil {
		t.Fatalf("CreateOAuthAPIKey(expired): %v", err)
	}
	if err := ts.CreateOAuthAPIKey(context.Background(), tenantID,
		"live", "raw-key-live", time.Now().Add(time.Hour), liveIdentity); err != nil {
		t.Fatalf("CreateOAuthAPIKey(live): %v", err)
	}

	n, err := ts.RevokeExpiredOAuthAPIKeys(context.Background())
	if err != nil {
		t.Fatalf("RevokeExpiredOAuthAPIKeys: %v", err)
	}
	if n < 1 {
		t.Errorf("RevokeExpiredOAuthAPIKeys() = %d, want at least the one expired key this test seeded", n)
	}

	var liveDisabled sql.NullTime
	if err := db.QueryRow(`SELECT disabled_at FROM admin.tenant_api_keys WHERE oauth_identity = $1`,
		liveIdentity).Scan(&liveDisabled); err != nil {
		t.Fatalf("read back the live key: %v", err)
	}
	if liveDisabled.Valid {
		t.Error("RevokeExpiredOAuthAPIKeys disabled a key that has not expired yet")
	}
}

func TestRevokeExpiredOAuthAPIKeys_NotImplementedOffPostgres(t *testing.T) {
	ts, err := NewTenantStoreForDialect(&sql.DB{}, DialectMySQL)
	if err != nil {
		t.Fatalf("NewTenantStoreForDialect: %v", err)
	}
	if _, err := ts.RevokeExpiredOAuthAPIKeys(context.Background()); err == nil {
		t.Error("RevokeExpiredOAuthAPIKeys on MySQL returned nil error, want a not-implemented refusal")
	}
}

func TestSetTenantSuspended(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	ts := NewTenantStore(db)
	tenantID := seedTenant(t, db, DialectPostgres, "set-suspended")

	if err := ts.SetTenantSuspended(context.Background(), tenantID, true); err != nil {
		t.Fatalf("SetTenantSuspended(true): %v", err)
	}
	var suspended bool
	if err := db.QueryRow(`SELECT suspended FROM admin.tenants WHERE tenant_id = $1`, tenantID).
		Scan(&suspended); err != nil {
		t.Fatalf("read back suspended: %v", err)
	}
	if !suspended {
		t.Error("SetTenantSuspended(true) did not set the column")
	}

	if err := ts.SetTenantSuspended(context.Background(), tenantID, false); err != nil {
		t.Fatalf("SetTenantSuspended(false): %v", err)
	}
	if err := db.QueryRow(`SELECT suspended FROM admin.tenants WHERE tenant_id = $1`, tenantID).
		Scan(&suspended); err != nil {
		t.Fatalf("read back suspended: %v", err)
	}
	if suspended {
		t.Error("SetTenantSuspended(false) did not clear the column")
	}
}

func TestSetTenantSuspended_NoSuchTenant(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	ts := NewTenantStore(db)

	err := ts.SetTenantSuspended(context.Background(), uuid.New(), true)
	if !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("SetTenantSuspended(unknown tenant) = %v, want ErrTenantNotFound", err)
	}
}

func TestTenantIDFromRequest(t *testing.T) {
	tenantID := uuid.New()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(WithTenantID(r.Context(), tenantID))

	got, ok := TenantIDFromRequest(r)
	if !ok {
		t.Fatal("TenantIDFromRequest: ok = false, want true")
	}
	if got != tenantID {
		t.Errorf("TenantIDFromRequest = %s, want %s", got, tenantID)
	}

	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, ok := TenantIDFromRequest(r2); ok {
		t.Error("TenantIDFromRequest on a request with no tenant set: ok = true, want false")
	}
}

func TestWithSubjectAndSubjectFromContext(t *testing.T) {
	ctx := WithSubject(context.Background(), "someone@example.com")
	got, ok := SubjectFromContext(ctx)
	if !ok {
		t.Fatal("SubjectFromContext: ok = false, want true")
	}
	if got != "someone@example.com" {
		t.Errorf("SubjectFromContext = %q, want %q", got, "someone@example.com")
	}

	if _, ok := SubjectFromContext(context.Background()); ok {
		t.Error("SubjectFromContext on a context with no subject set: ok = true, want false")
	}
}
