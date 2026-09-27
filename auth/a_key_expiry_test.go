package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/google/uuid"
)

// cleat#2352, a separable piece of #2340 (OAuth login). These are real-DB
// tests, not the fake-driver suite the rest of this package uses: the fake
// driver in fake_driver_test.go dispatches on a query-string substring match
// (ExecContext / QueryContext), not real SQL predicate evaluation, so it
// cannot tell "expires_at > now()" apart from any other WHERE clause it
// happens to also match. It would pass whether resolveAPIKeyStmt's new
// clause were correct, absent, or inverted -- CLAUDE.md's "a fake more
// correct than production" trap, reached from the read side this time
// instead of the write side. Only a real database can fail this the way a
// wrong query actually fails.
//
// Falsified by hand while writing this: reverting resolveAPIKeyStmt's three
// dialect statements to drop the "AND (expires_at IS NULL OR ...)" clause
// turns the expired-key assertions in TestExpiredKeyCannotAuthenticate red
// on all three dialects (the key resolves and the request gets 200 instead
// of 401), and the live/revoked assertions stay green -- confirming the test
// is checking the clause this migration added, not something else nearby.

// seedTenant ensures a tenant row exists for seedAPIKey to reference, and
// returns its id. PostgreSQL and SQL Server allow many tenants, so it inserts
// a fresh one directly (bypassing TenantStore.CreateTenant, which
// TestCreateTenantAndRevokeAPIKeyRefuseNonPostgres establishes refuses on
// MySQL and SQL Server). MySQL is single-tenant (tiers.yaml's decision,
// enforced by the singleton unique constraint migration 038 adds): a second
// INSERT fails, so it returns the one default tenant every dialect's
// migrations backfill -- engine.DefaultTenantUUID, the all-zeros UUID --
// matching engine/queue_store_test.go's createQueueTestTenant.
func seedTenant(t *testing.T, db *sql.DB, dialect string, name string) uuid.UUID {
	t.Helper()
	if dialect == DialectMySQL {
		return uuid.MustParse(engine.DefaultTenantUUID)
	}
	tenantID := uuid.New()
	var stmt string
	switch dialect {
	case DialectMSSQL:
		stmt = `INSERT INTO admin.tenants (tenant_id, name) VALUES (@p1, @p2)`
	default:
		stmt = `INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`
	}
	if _, err := db.Exec(stmt, tenantID.String(), name+"-"+tenantID.String()); err != nil {
		t.Fatalf("seed tenant (%s): %v", dialect, err)
	}
	return tenantID
}

// apiKeyFixture is what seedAPIKey needs beyond the tenant and the raw key:
// which of expires_at/disabled_at to set, if either. Both default to SQL
// NULL, matching every key created through the ordinary CreateAPIKey path.
type apiKeyFixture struct {
	expiresAt  sql.NullTime
	disabledAt sql.NullTime
}

// seedAPIKey inserts an API key directly -- CreateAPIKey has no expires_at
// parameter (cleat#2352 deliberately did not extend it; see the migration's
// header on oauth_identity being unwired here by design, for the same
// reason) -- and returns its sha256 hash for ResolveTenantFromAPIKey.
func seedAPIKey(t *testing.T, db *sql.DB, dialect string, tenantID uuid.UUID, rawKey string, f apiKeyFixture) []byte {
	t.Helper()
	hash := sha256.Sum256([]byte(rawKey))

	var stmt string
	var args []any
	switch dialect {
	case DialectMySQL:
		stmt = `INSERT INTO tenant_api_keys (key_id, tenant_id, key_hash, description, expires_at, disabled_at) VALUES (?, ?, ?, ?, ?, ?)`
		args = []any{uuid.New().String(), tenantID.String(), hash[:], rawKey, f.expiresAt, f.disabledAt}
	case DialectMSSQL:
		stmt = `INSERT INTO admin.tenant_api_keys (tenant_id, key_hash, description, expires_at, disabled_at) VALUES (@p1, @p2, @p3, @p4, @p5)`
		args = []any{tenantID.String(), hash[:], rawKey, f.expiresAt, f.disabledAt}
	default:
		stmt = `INSERT INTO admin.tenant_api_keys (tenant_id, key_hash, description, expires_at, disabled_at) VALUES ($1, $2, $3, $4, $5)`
		args = []any{tenantID.String(), hash[:], rawKey, f.expiresAt, f.disabledAt}
	}
	if _, err := db.Exec(stmt, args...); err != nil {
		t.Fatalf("seed api key %q (%s): %v", rawKey, dialect, err)
	}
	return hash[:]
}

// TestExpiredKeyCannotAuthenticate is the design's "expired -> 401,
// future -> 200, revoked -> 401" on all three dialects: expiry is enforced
// by auth.TenantStore.ResolveTenantFromAPIKey regardless of which dialect
// minted the key, even though OAuth login itself (the feature that mints
// short-lived keys) is Postgres-only in 0.3.0. Checked at both the layer
// resolveAPIKeyStmt lives at and, since the issue's own framing is HTTP
// status codes, through auth.MiddlewareWithMux with a real *http.Request.
func TestExpiredKeyCannotAuthenticate(t *testing.T) {
	for _, dialect := range []string{DialectPostgres, DialectMySQL, DialectMSSQL} {
		t.Run(dialect, func(t *testing.T) {
			db := testutil.TestDB(t, testutil.Dialect(dialect))
			ts, err := NewTenantStoreForDialect(db, dialect)
			if err != nil {
				t.Fatalf("NewTenantStoreForDialect(%q): %v", dialect, err)
			}

			tenantID := seedTenant(t, db, dialect, "expiry-"+dialect)

			now := time.Now()
			// GenerateAPIKey() rather than fixed literals: sha256 of a fixed
			// string is a fixed hash, and TestDB runs against a shared,
			// persistent database (SetupMinimalSchema does not wipe rows), so
			// a previous run's key with the same hash would shadow the
			// freshly-seeded one and ResolveTenantFromAPIKey would return the
			// stale row's tenant, failing the tenant assertion below. A
			// random key per run gets a fresh hash every time.
			expiredRaw := GenerateAPIKey()
			liveRaw := GenerateAPIKey()
			revokedRaw := GenerateAPIKey()
			expiredHash := seedAPIKey(t, db, dialect, tenantID, expiredRaw,
				apiKeyFixture{expiresAt: sql.NullTime{Time: now.Add(-time.Hour), Valid: true}})
			liveHash := seedAPIKey(t, db, dialect, tenantID, liveRaw,
				apiKeyFixture{expiresAt: sql.NullTime{Time: now.Add(time.Hour), Valid: true}})
			revokedHash := seedAPIKey(t, db, dialect, tenantID, revokedRaw,
				apiKeyFixture{disabledAt: sql.NullTime{Time: now, Valid: true}})

			// Store level, directly.
			if _, err := ts.ResolveTenantFromAPIKey(context.Background(), expiredHash); err == nil {
				t.Error("expired key: ResolveTenantFromAPIKey returned no error, want one")
			}
			gotTenant, err := ts.ResolveTenantFromAPIKey(context.Background(), liveHash)
			if err != nil {
				t.Errorf("future-expiry key: ResolveTenantFromAPIKey: %v, want success", err)
			} else if gotTenant != tenantID {
				t.Errorf("future-expiry key resolved tenant %s, want %s", gotTenant, tenantID)
			}
			if _, err := ts.ResolveTenantFromAPIKey(context.Background(), revokedHash); err == nil {
				t.Error("revoked key: ResolveTenantFromAPIKey returned no error, want one")
			}

			// HTTP level, through the same middleware cmd/cleat-worker wires up.
			handler := MiddlewareWithMux(ts, true, nil)(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

			checkStatus := func(label, rawKey string, want int) {
				t.Helper()
				req := httptest.NewRequest(http.MethodGet, "/api/anything", nil)
				req.Header.Set("Authorization", "Bearer "+rawKey)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != want {
					t.Errorf("%s key: status = %d, want %d (body: %s)", label, rec.Code, want, rec.Body.String())
				}
			}
			checkStatus("expired", expiredRaw, http.StatusUnauthorized)
			checkStatus("future-expiry", liveRaw, http.StatusOK)
			checkStatus("revoked", revokedRaw, http.StatusUnauthorized)
		})
	}
}

// TestRevokeAPIKeyByHash_RevokedKeyCannotAuthenticate, TestRevokeAPIKeyByHash_
// NotFoundIsNotError and TestRevokeAPIKeyByHash_DoubleRevokeIsIdempotent
// mirror the three RevokeAPIKey tests of the same shape in
// tenant_store_test.go, against a real Postgres database rather than the
// fake driver -- RevokeAPIKeyByHash's UPDATE has the same
// "UPDATE admin.tenant_api_keys SET disabled_at" prefix RevokeAPIKey's does,
// so the fake driver's substring dispatch (fake_driver_test.go) would route
// it into execRevokeAPIKey, which reads args[1] as a key_id STRING
// (argString) -- but RevokeAPIKeyByHash passes a []byte hash in that
// position. Extending the fake to tell the two apart would mean teaching it
// to parse the WHERE clause, at which point it is reimplementing the SQL it
// exists to stand in for. A real database needs no such extension.
func TestRevokeAPIKeyByHash_RevokedKeyCannotAuthenticate(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	ts, err := NewTenantStoreForDialect(db, DialectPostgres)
	if err != nil {
		t.Fatal(err)
	}

	tenantID := seedTenant(t, db, DialectPostgres, "revoke-by-hash")

	rawKey := GenerateAPIKey()
	hash := seedAPIKey(t, db, DialectPostgres, tenantID, rawKey, apiKeyFixture{})

	if _, err := ts.ResolveTenantFromAPIKey(context.Background(), hash); err != nil {
		t.Fatalf("expected key to work before revoke: %v", err)
	}

	if err := ts.RevokeAPIKeyByHash(context.Background(), hash); err != nil {
		t.Fatalf("RevokeAPIKeyByHash: %v", err)
	}

	if _, err := ts.ResolveTenantFromAPIKey(context.Background(), hash); err == nil {
		t.Error("expected error for revoked key in ResolveTenantFromAPIKey")
	}
}

func TestRevokeAPIKeyByHash_NotFoundIsNotError(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	ts, err := NewTenantStoreForDialect(db, DialectPostgres)
	if err != nil {
		t.Fatal(err)
	}

	nonexistentHash := sha256.Sum256([]byte("cleat_sk_never_issued"))
	if err := ts.RevokeAPIKeyByHash(context.Background(), nonexistentHash[:]); err != nil {
		t.Fatalf("RevokeAPIKeyByHash on non-existent hash: %v (expected nil)", err)
	}
}

func TestRevokeAPIKeyByHash_DoubleRevokeIsIdempotent(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	ts, err := NewTenantStoreForDialect(db, DialectPostgres)
	if err != nil {
		t.Fatal(err)
	}

	tenantID := seedTenant(t, db, DialectPostgres, "double-revoke-by-hash")
	rawKey := GenerateAPIKey()
	hash := seedAPIKey(t, db, DialectPostgres, tenantID, rawKey, apiKeyFixture{})

	if err := ts.RevokeAPIKeyByHash(context.Background(), hash); err != nil {
		t.Fatalf("first RevokeAPIKeyByHash: %v", err)
	}
	if err := ts.RevokeAPIKeyByHash(context.Background(), hash); err != nil {
		t.Fatalf("second RevokeAPIKeyByHash should be idempotent, got: %v", err)
	}
}
