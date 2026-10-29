package auth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3552, B2b-2a. ProvisionTenant is a handshake (create with a provisional key, confirm, replay
// safely) and each of its guarantees is something a plausible implementation gets wrong in a way that
// still passes the first call. So the rows below are the second, third and fourth calls.
//
// Two layers, as in the B2a test: the statement test needs no database; the database test is the one
// that can be wrong about SQL Server.

func TestTheProvisionStatementsAreShapedPerDialect(t *testing.T) {
	for _, tc := range []struct{ dialect, placeholder string }{{DialectPostgres, "$1"}, {DialectMSSQL, "@p1"}} {
		st, err := provisionStatements(tc.dialect)
		if err != nil {
			t.Fatalf("%s: %v", tc.dialect, err)
		}
		for name, stmt := range map[string]string{
			"tenantByID": st.tenantByID, "tenantByName": st.tenantByName, "orgByName": st.orgByName,
			"insertOrg": st.insertOrg, "insertTenant": st.insertTenant, "insertKey": st.insertKey,
			"lockTenant": st.lockTenant, "liveKeys": st.liveKeys,
			"revokeProvisional": st.revokeProvisional, "confirmProvisional": st.confirmProvisional,
		} {
			up := strings.ToUpper(stmt)
			if strings.Contains(up, "RETURNING") || strings.Contains(up, "OUTPUT") {
				t.Errorf("%s %s asks the database for a value: %s", tc.dialect, name, stmt)
			}
			if !strings.Contains(stmt, tc.placeholder) {
				t.Errorf("%s %s does not use %s: %s", tc.dialect, name, tc.placeholder, stmt)
			}
		}
		// A key's expiry is computed by the database from its own clock.
		if !strings.Contains(st.insertKey, "now()") && !strings.Contains(st.insertKey, "SYSUTCDATETIME()") {
			t.Errorf("%s insertKey does not take its expiry from the database clock: %s", tc.dialect, st.insertKey)
		}
	}
	if _, err := provisionStatements(DialectMySQL); err == nil || !strings.Contains(err.Error(), "mysql") {
		t.Errorf("provisionStatements(mysql) = %v, want a refusal naming the dialect", err)
	}
}

type provisionFixture struct {
	db      *sql.DB
	ts      *TenantStore
	ph      func(int) string
	tenants []uuid.UUID
	orgs    []uuid.UUID
}

func newProvisionFixture(t *testing.T, dialect string) *provisionFixture {
	t.Helper()
	db := testutil.TestDB(t, testutil.Dialect(dialect))
	ts, err := NewTenantStoreForDialect(db, dialect)
	if err != nil {
		t.Fatal(err)
	}
	f := &provisionFixture{db: db, ts: ts, ph: func(n int) string {
		if dialect == DialectMSSQL {
			return "@p" + string(rune('0'+n))
		}
		return "$" + string(rune('0'+n))
	}}
	t.Cleanup(func() {
		for _, tid := range f.tenants {
			_, _ = db.Exec(`DELETE FROM admin.tenant_api_keys WHERE tenant_id = `+f.ph(1), tid.String())
			_, _ = db.Exec(`DELETE FROM admin.tenants WHERE tenant_id = `+f.ph(1), tid.String())
		}
		for _, oid := range f.orgs {
			_, _ = db.Exec(`DELETE FROM admin.orgs WHERE org_id = `+f.ph(1), oid.String())
		}
	})
	return f
}

// provision calls ProvisionTenant and registers what it made for cleanup.
func (f *provisionFixture) provision(t *testing.T, req ProvisionRequest) (ProvisionResult, error) {
	t.Helper()
	res, err := f.ts.ProvisionTenant(context.Background(), req)
	if err == nil {
		f.tenants = append(f.tenants, res.Tenant.ID)
		f.orgs = append(f.orgs, res.Tenant.OrgID)
	}
	return res, err
}

// keyState reads one key's provisional flag and whether it has an expiry, straight from the table.
func (f *provisionFixture) keyState(t *testing.T, keyID uuid.UUID) (provisional, hasExpiry, disabled bool) {
	t.Helper()
	var p bool
	var exp, dis sql.NullTime
	err := f.db.QueryRow(`SELECT provisional, expires_at, disabled_at FROM admin.tenant_api_keys WHERE key_id = `+f.ph(1), keyID.String()).Scan(&p, &exp, &dis)
	if err != nil {
		t.Fatalf("reading key %s: %v", keyID, err)
	}
	return p, exp.Valid, dis.Valid
}

func (f *provisionFixture) resolves(secret string) (uuid.UUID, bool) {
	tid, err := f.ts.ResolveTenantFromAPIKey(context.Background(), sha256Hash(secret))
	return tid, err == nil
}

func TestATenantIsProvisionedReplayedAndConfirmedOnPostgresAndSQLServer(t *testing.T) {
	for _, dialect := range []string{DialectPostgres, DialectMSSQL} {
		dialect := dialect
		t.Run(dialect, func(t *testing.T) {
			f := newProvisionFixture(t, dialect)
			ctx := context.Background()
			sfx := uuid.New().String()
			tid := uuid.New()
			req := ProvisionRequest{TenantID: tid, DisplayName: "Acme", OrgName: "b2b2a-org-" + sfx, KeyDescription: "first", Creator: "clew-service"}

			// CREATE. The known positive for everything below.
			created, err := f.provision(t, req)
			if err != nil {
				t.Fatalf("create on %s: %v", dialect, err)
			}
			if created.Outcome != ProvisionCreated || created.Key == nil {
				t.Fatalf("create = %+v, want created with a key", created)
			}
			if created.Tenant.ID != tid || created.Tenant.Name != tid.String() {
				t.Errorf("tenant %+v: want the caller's id, and the id as the default name", created.Tenant)
			}
			if got, ok := f.resolves(created.Key.Secret); !ok || got != tid {
				t.Fatalf("the provisional key resolves to %v, %v; want tenant %s: it must authenticate before it is confirmed", got, ok, tid)
			}
			if p, exp, dis := f.keyState(t, created.Key.KeyID); !p || !exp || dis {
				t.Errorf("fresh key: provisional=%v expires=%v disabled=%v, want true true false", p, exp, dis)
			}
			var by string
			if err := f.db.QueryRow(`SELECT created_by FROM admin.tenants WHERE tenant_id = `+f.ph(1), tid.String()).Scan(&by); err != nil || by != "clew-service" {
				t.Errorf("created_by = %q, %v; want clew-service", by, err)
			}

			// An org given by name is found, not duplicated: a second tenant under it shares the org.
			other, err := f.provision(t, ProvisionRequest{DisplayName: "Two", OrgName: req.OrgName, Creator: "clew-service"})
			if err != nil {
				t.Fatalf("second tenant under the same org name: %v", err)
			}
			if other.Tenant.OrgID != created.Tenant.OrgID {
				t.Errorf("org name resolved to two orgs: %s and %s", created.Tenant.OrgID, other.Tenant.OrgID)
			}

			// REPLAY while the key is only provisional: a fresh key, and the old one is dead.
			replay1, err := f.provision(t, req)
			if err != nil || replay1.Outcome != ProvisionReplayed || replay1.Key == nil {
				t.Fatalf("replay before confirm = %+v, %v; want replayed with a fresh key", replay1, err)
			}
			if _, ok := f.resolves(created.Key.Secret); ok {
				t.Error("the first provisional key still authenticates after a replay reissued it")
			}
			if got, ok := f.resolves(replay1.Key.Secret); !ok || got != tid {
				t.Error("the reissued key does not authenticate")
			}
			if _, _, dis := f.keyState(t, created.Key.KeyID); !dis {
				t.Error("the first provisional key was not revoked")
			}

			// A different principal cannot replay it, and learns only that the id is taken.
			_, err = f.ts.ProvisionTenant(ctx, ProvisionRequest{TenantID: tid, OrgName: req.OrgName, Creator: "someone-else"})
			var conflict *ProvisionConflictError
			if !errors.As(err, &conflict) || conflict.Reason != ConflictTenantExists || len(conflict.Fields) != 0 {
				t.Errorf("another principal's replay = %v, want tenant_exists with no fields", err)
			}
			if _, err := f.ts.ConfirmProvisionedTenant(ctx, tid, "someone-else"); !errors.Is(err, ErrTenantNotFound) {
				t.Errorf("another principal's confirm = %v, want ErrTenantNotFound", err)
			}

			// CONFIRM. The surviving key becomes permanent.
			c1, err := f.ts.ConfirmProvisionedTenant(ctx, tid, "clew-service")
			if err != nil || c1.Confirmed != 1 || c1.AlreadyConfirmed {
				t.Fatalf("confirm = %+v, %v; want 1 key confirmed", c1, err)
			}
			if p, exp, _ := f.keyState(t, replay1.Key.KeyID); p || exp {
				t.Errorf("confirmed key: provisional=%v expires=%v, want false false", p, exp)
			}
			c2, err := f.ts.ConfirmProvisionedTenant(ctx, tid, "clew-service")
			if err != nil || c2.Confirmed != 0 || !c2.AlreadyConfirmed {
				t.Errorf("a repeated confirm = %+v, %v; want already confirmed", c2, err)
			}

			// REPLAY after confirm: the tenant, and NO secret. A replay must never yield a second key.
			replay2, err := f.provision(t, req)
			if err != nil || replay2.Outcome != ProvisionReplayed || replay2.Key != nil {
				t.Fatalf("replay after confirm = %+v, %v; want replayed with no key", replay2, err)
			}
			if got, ok := f.resolves(replay1.Key.Secret); !ok || got != tid {
				t.Error("the confirmed key stopped authenticating")
			}
			var live int
			if err := f.db.QueryRow(`SELECT count(*) FROM admin.tenant_api_keys WHERE tenant_id = `+f.ph(1)+` AND disabled_at IS NULL`, tid.String()).Scan(&live); err != nil || live != 1 {
				t.Errorf("live keys = %d, %v; want exactly 1 after create, replay, confirm, replay", live, err)
			}

			// A replay that differs says which fields.
			otherOrg, err := f.provision(t, ProvisionRequest{DisplayName: "x", OrgName: "b2b2a-other-" + sfx, Creator: "clew-service"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.ts.ProvisionTenant(ctx, ProvisionRequest{TenantID: tid, Name: "renamed-" + sfx, OrgID: otherOrg.Tenant.OrgID, Creator: "clew-service"})
			if !errors.As(err, &conflict) || conflict.Reason != ConflictTenantDiffers || strings.Join(conflict.Fields, ",") != "name,org_id" {
				t.Errorf("a differing replay = %v, want tenant_differs naming name and org_id", err)
			}

			// A name held by another tenant is a conflict of its own.
			_, err = f.ts.ProvisionTenant(ctx, ProvisionRequest{Name: other.Tenant.Name, OrgName: req.OrgName, Creator: "clew-service"})
			if !errors.As(err, &conflict) || conflict.Reason != ConflictNameTaken {
				t.Errorf("a taken name = %v, want name_taken", err)
			}

			// Malformed requests are refused before they touch anything.
			for name, bad := range map[string]ProvisionRequest{
				"no creator":             {OrgName: "x", Creator: ""},
				"neither org":            {Creator: "c"},
				"both orgs":              {OrgName: "x", OrgID: uuid.New(), Creator: "c"},
				"an org that is not one": {OrgID: uuid.New(), Creator: "c"},
			} {
				if _, err := f.ts.ProvisionTenant(ctx, bad); err == nil {
					t.Errorf("%s was accepted", name)
				}
			}
			if _, err := f.ts.ProvisionTenant(ctx, ProvisionRequest{OrgID: uuid.New(), Creator: "c"}); !errors.Is(err, ErrOrgNotFound) {
				t.Errorf("an unknown org id = %v, want ErrOrgNotFound", err)
			}
		})
	}
}

// A provisional key that lapses is replaced by a replay and cannot be confirmed. The lapse is made
// by writing expires_at into the past rather than by waiting, so nothing here depends on the clock.
func TestALapsedProvisionalKeyIsReplacedByAReplayAndCannotBeConfirmed(t *testing.T) {
	for _, dialect := range []string{DialectPostgres, DialectMSSQL} {
		dialect := dialect
		t.Run(dialect, func(t *testing.T) {
			f := newProvisionFixture(t, dialect)
			ctx := context.Background()
			req := ProvisionRequest{TenantID: uuid.New(), OrgName: "b2b2a-lapse-" + uuid.NewString(), Creator: "clew-service"}
			first, err := f.provision(t, req)
			if err != nil {
				t.Fatal(err)
			}
			past := `now() - interval '1 hour'`
			if dialect == DialectMSSQL {
				past = `DATEADD(hour, -1, SYSUTCDATETIME())`
			}
			if _, err := f.db.Exec(`UPDATE admin.tenant_api_keys SET expires_at = `+past+` WHERE key_id = `+f.ph(1), first.Key.KeyID.String()); err != nil {
				t.Fatalf("UNMEASURED: lapsing the key: %v", err)
			}
			if _, ok := f.resolves(first.Key.Secret); ok {
				t.Fatal("UNMEASURED: the lapsed key still authenticates, so the lapse did not happen")
			}
			if _, err := f.ts.ConfirmProvisionedTenant(ctx, req.TenantID, "clew-service"); !errors.Is(err, ErrNoProvisionalKey) {
				t.Errorf("confirming a lapsed key = %v, want ErrNoProvisionalKey", err)
			}
			again, err := f.provision(t, req)
			if err != nil || again.Key == nil {
				t.Fatalf("replay after the lapse = %+v, %v; want a fresh key", again, err)
			}
			if _, err := f.ts.ConfirmProvisionedTenant(ctx, req.TenantID, "clew-service"); err != nil {
				t.Errorf("confirming the fresh key: %v", err)
			}
		})
	}
}

// The guard that keeps "may create tenants" from becoming "may act as any tenant": a tenant this
// principal did not create is not reachable through this route, whatever its key state. Included
// are the two ways such a tenant arises -- made offline (created_by NULL) and made by someone else.
func TestAPrincipalCannotMintAKeyForATenantItDidNotCreate(t *testing.T) {
	for _, dialect := range []string{DialectPostgres, DialectMSSQL} {
		dialect := dialect
		t.Run(dialect, func(t *testing.T) {
			f := newProvisionFixture(t, dialect)
			ctx := context.Background()
			oid, err := f.ts.CreateOrg(ctx, "b2b2a-guard-"+uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			f.orgs = append(f.orgs, oid)
			// Made the offline way: no creator recorded, and NO live key at all -- the state in which a
			// careless "tenant with no key may be given one" rule would hand the principal a key.
			offline, err := f.ts.CreateTenant(ctx, "b2b2a-offline-"+uuid.NewString(), "", oid)
			if err != nil {
				t.Fatal(err)
			}
			f.tenants = append(f.tenants, offline)

			_, err = f.ts.ProvisionTenant(ctx, ProvisionRequest{TenantID: offline, Name: "irrelevant", OrgID: oid, Creator: "clew-service"})
			var conflict *ProvisionConflictError
			if !errors.As(err, &conflict) || conflict.Reason != ConflictTenantExists {
				t.Errorf("replaying an offline-made tenant = %v, want tenant_exists", err)
			}
			if _, err := f.ts.ConfirmProvisionedTenant(ctx, offline, "clew-service"); !errors.Is(err, ErrTenantNotFound) {
				t.Errorf("confirming an offline-made tenant = %v, want ErrTenantNotFound", err)
			}
			var keys int
			if err := f.db.QueryRow(`SELECT count(*) FROM admin.tenant_api_keys WHERE tenant_id = `+f.ph(1), offline.String()).Scan(&keys); err != nil || keys != 0 {
				t.Errorf("the refused replay left %d key rows for the tenant, %v; want 0", keys, err)
			}
		})
	}
}

// Concurrent creates of one id: exactly one creates, the rest replay, and when the dust settles there
// is exactly one live provisional key -- a second would be a key the caller never learned about that a
// later confirm would make permanent. Several rounds, because a race this narrow is caught by a single
// round only some of the time (cleat#1755 measured 40% for one round of eight).
func TestConcurrentCreatesOfOneTenantLeaveOneProvisionalKey(t *testing.T) {
	for _, dialect := range []string{DialectPostgres, DialectMSSQL} {
		dialect := dialect
		t.Run(dialect, func(t *testing.T) {
			f := newProvisionFixture(t, dialect)
			orgName := "b2b2a-race-" + uuid.NewString()
			for round := 0; round < 6; round++ {
				req := ProvisionRequest{TenantID: uuid.New(), OrgName: orgName, Creator: "clew-service"}
				const racers = 8
				results := make([]ProvisionResult, racers)
				errs := make([]error, racers)
				var wg sync.WaitGroup
				start := make(chan struct{})
				for i := 0; i < racers; i++ {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						<-start
						results[i], errs[i] = f.ts.ProvisionTenant(context.Background(), req)
					}(i)
				}
				close(start)
				wg.Wait()
				f.tenants = append(f.tenants, req.TenantID)

				created := 0
				for i := range results {
					if errs[i] != nil {
						t.Fatalf("round %d racer %d: %v", round, i, errs[i])
					}
					if results[i].Outcome == ProvisionCreated {
						created++
					}
					f.orgs = append(f.orgs, results[i].Tenant.OrgID)
				}
				if created != 1 {
					t.Errorf("round %d: %d racers created the tenant, want exactly 1", round, created)
				}
				var live int
				if err := f.db.QueryRow(`SELECT count(*) FROM admin.tenant_api_keys WHERE tenant_id = `+f.ph(1)+` AND disabled_at IS NULL`, req.TenantID.String()).Scan(&live); err != nil {
					t.Fatal(err)
				}
				if live != 1 {
					t.Errorf("round %d: %d live keys after the race, want exactly 1", round, live)
				}
			}
		})
	}
}

var _ = time.Second
