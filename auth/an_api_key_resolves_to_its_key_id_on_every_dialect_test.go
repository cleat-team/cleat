package auth

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/google/uuid"
)

// cleat#3623, against real databases. The fake driver answers by substring match, so it would serve a
// key id for a statement that selected none; only a real table can say whether each dialect's
// statement returns the id of the row it matched, and whether it still refuses a revoked or expired one.

// storedKeyID reads the key_id of the row seedAPIKey inserted, by the same hash the lookup uses, so the
// expected value comes from the table and not from the code under test.
func storedKeyID(t *testing.T, db *sql.DB, dialect string, hash []byte) uuid.UUID {
	t.Helper()
	var raw string
	var err error
	switch dialect {
	case DialectMSSQL:
		err = db.QueryRow(`SELECT LOWER(CONVERT(NVARCHAR(36), key_id)) FROM admin.tenant_api_keys WHERE key_hash = @p1`, hash).Scan(&raw)
	case DialectMySQL:
		err = db.QueryRow(`SELECT key_id FROM tenant_api_keys WHERE key_hash = ?`, hash).Scan(&raw)
	default:
		err = db.QueryRow(`SELECT key_id::text FROM admin.tenant_api_keys WHERE key_hash = $1`, hash).Scan(&raw)
	}
	if err != nil {
		t.Fatalf("read back key_id (%s): %v", dialect, err)
	}
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		t.Fatalf("stored key_id %q (%s) is not a usable uuid: %v", raw, dialect, err)
	}
	return id
}

func TestAnAPIKeyResolvesToItsOwnKeyIDOnEveryDialect(t *testing.T) {
	for _, dialect := range []string{DialectPostgres, DialectMySQL, DialectMSSQL} {
		t.Run(dialect, func(t *testing.T) {
			db := testutil.TestDB(t, testutil.Dialect(dialect))
			ts, err := NewTenantStoreForDialect(db, dialect)
			if err != nil {
				t.Fatalf("NewTenantStoreForDialect(%q): %v", dialect, err)
			}
			tenantID := seedTenant(t, db, dialect, "keyid-"+dialect)

			// Two live keys in one tenant: a statement that returned "the tenant's key" and not the
			// matched one would give both the same id.
			rawA, rawB := GenerateAPIKey(), GenerateAPIKey()
			hashA := seedAPIKey(t, db, dialect, tenantID, rawA, apiKeyFixture{})
			hashB := seedAPIKey(t, db, dialect, tenantID, rawB, apiKeyFixture{})
			wantA, wantB := storedKeyID(t, db, dialect, hashA), storedKeyID(t, db, dialect, hashB)
			if wantA == wantB {
				t.Fatalf("fixture: both keys have key_id %s", wantA)
			}

			now := time.Now()
			expiredRaw, revokedRaw := GenerateAPIKey(), GenerateAPIKey()
			expiredHash := seedAPIKey(t, db, dialect, tenantID, expiredRaw,
				apiKeyFixture{expiresAt: sql.NullTime{Time: now.Add(-time.Hour), Valid: true}})
			revokedHash := seedAPIKey(t, db, dialect, tenantID, revokedRaw,
				apiKeyFixture{disabledAt: sql.NullTime{Time: now, Valid: true}})

			var engineStore KeyResolver
			switch dialect {
			case DialectMySQL:
				engineStore = engine.NewMySQLStore(db)
			case DialectMSSQL:
				engineStore = engine.NewMSSQLStore(db)
			default:
				engineStore = engine.NewPostgresStore(db)
			}

			for name, r := range map[string]KeyResolver{"auth.TenantStore": ts, "engine store": engineStore} {
				t.Run(name, func(t *testing.T) {
					for _, c := range []struct {
						hash []byte
						want uuid.UUID
					}{{hashA, wantA}, {hashB, wantB}} {
						gotTenant, gotKey, err := r.ResolveAPIKey(context.Background(), c.hash)
						if err != nil {
							t.Fatalf("ResolveAPIKey: %v", err)
						}
						if gotTenant != tenantID || gotKey != c.want {
							t.Errorf("ResolveAPIKey = (%s, %s), want (%s, %s)", gotTenant, gotKey, tenantID, c.want)
						}
					}
					for label, h := range map[string][]byte{"expired": expiredHash, "revoked": revokedHash} {
						if _, k, err := r.ResolveAPIKey(context.Background(), h); err == nil {
							t.Errorf("%s key resolved (key id %s); want an error", label, k)
						}
					}
				})
			}

			// Through the middleware: the request carries the matched key's subject, and the two
			// keys are told apart.
			var subject string
			h := MiddlewareWithMux(ts, true, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				subject, _ = SubjectFromContext(r.Context())
			}))
			for raw, want := range map[string]uuid.UUID{rawA: wantA, rawB: wantB} {
				subject = ""
				req := httptest.NewRequest(http.MethodGet, "/api/anything", nil)
				req.Header.Set("Authorization", "Bearer "+raw)
				h.ServeHTTP(httptest.NewRecorder(), req)
				if subject != KeySubjectPrefix+want.String() {
					t.Errorf("subject = %q, want %q", subject, KeySubjectPrefix+want.String())
				}
				if strings.Contains(subject, raw) {
					t.Errorf("subject %q contains the secret", subject)
				}
			}
		})
	}
}
