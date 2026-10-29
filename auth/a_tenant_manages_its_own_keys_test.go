package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3552, B2c. A tenant mints, lists and revokes its OWN keys. The rows below are the second and
// third calls of each guarantee, because a first call passes against a store that scopes nothing.

func TestTheKeyStatementsAreScopedToATenantAndNeverSelectTheHash(t *testing.T) {
	for _, tc := range []struct{ dialect, first string }{{DialectPostgres, "$1"}, {DialectMSSQL, "@p1"}, {DialectMySQL, "?"}} {
		st, err := keyStatements(tc.dialect)
		if err != nil {
			t.Fatalf("%s: %v", tc.dialect, err)
		}
		for name, stmt := range map[string]string{
			"lockTenant": st.lockTenant, "liveCount": st.liveCount, "insertKey": st.insertKey, "list": st.list,
			"keyRow": st.keyRow, "liveOthers": st.liveOthers, "revoke": st.revoke,
		} {
			if !strings.Contains(stmt, tc.first) {
				t.Errorf("%s %s does not use %s: %s", tc.dialect, name, tc.first, stmt)
			}
			if name != "insertKey" && !strings.Contains(stmt, "tenant_id") {
				t.Errorf("%s %s is not scoped by tenant_id: %s", tc.dialect, name, stmt)
			}
		}
		// A statement that reaches a single key by id must ALSO carry the tenant, or a key id from
		// another tenant would resolve.
		for name, stmt := range map[string]string{"keyRow": st.keyRow, "liveOthers": st.liveOthers, "revoke": st.revoke} {
			if !strings.Contains(stmt, "key_id") || !strings.Contains(stmt, "tenant_id") {
				t.Errorf("%s %s does not carry both key_id and tenant_id: %s", tc.dialect, name, stmt)
			}
		}
		if strings.Contains(st.list, "key_hash") {
			t.Errorf("%s list selects the hash: %s", tc.dialect, st.list)
		}
	}
	if _, err := keyStatements("oracle"); err == nil {
		t.Error("an unknown dialect was accepted")
	}
}

type keysFixture struct {
	t       *testing.T
	db      *sql.DB
	ts      *TenantStore
	dialect string
	minted  []uuid.UUID
	tenants []uuid.UUID
	orgs    []uuid.UUID
}

func (f *keysFixture) ph(n int) string {
	switch f.dialect {
	case DialectMSSQL:
		return fmt.Sprintf("@p%d", n)
	case DialectMySQL:
		return "?"
	}
	return fmt.Sprintf("$%d", n)
}

func newKeysFixture(t *testing.T, dialect string) *keysFixture {
	t.Helper()
	db := testutil.TestDB(t, testutil.Dialect(dialect))
	ts, err := NewTenantStoreForDialect(db, dialect)
	if err != nil {
		t.Fatal(err)
	}
	f := &keysFixture{t: t, db: db, ts: ts, dialect: dialect}
	t.Cleanup(func() {
		table := "admin.tenant_api_keys"
		if dialect == DialectMySQL {
			table = "tenant_api_keys"
		}
		for _, id := range f.minted {
			_, _ = db.Exec(`DELETE FROM `+table+` WHERE key_id = `+f.ph(1), id.String())
		}
		for _, tid := range f.tenants {
			_, _ = db.Exec(`DELETE FROM `+table+` WHERE tenant_id = `+f.ph(1), tid.String())
			_, _ = db.Exec(`DELETE FROM admin.tenants WHERE tenant_id = `+f.ph(1), tid.String())
		}
		for _, oid := range f.orgs {
			_, _ = db.Exec(`DELETE FROM admin.orgs WHERE org_id = `+f.ph(1), oid.String())
		}
	})
	return f
}

// tenant returns a tenant to work in. MySQL is single-tenant, so it is the default tenant there, and
// the test must not assume it owns every key the tenant has.
func (f *keysFixture) tenant() uuid.UUID {
	f.t.Helper()
	if f.dialect == DialectMySQL {
		return uuid.Nil
	}
	oid, err := f.ts.CreateOrg(context.Background(), "b2c-org-"+uuid.NewString())
	if err != nil {
		f.t.Fatal(err)
	}
	f.orgs = append(f.orgs, oid)
	tid, err := f.ts.CreateTenant(context.Background(), "b2c-tenant-"+uuid.NewString(), "", oid)
	if err != nil {
		f.t.Fatal(err)
	}
	f.tenants = append(f.tenants, tid)
	return tid
}

func (f *keysFixture) mint(tid uuid.UUID, desc string, exp *time.Time) (TenantKey, string) {
	f.t.Helper()
	k, secret, err := f.ts.MintKey(context.Background(), tid, desc, exp)
	if err != nil {
		f.t.Fatalf("MintKey on %s: %v", f.dialect, err)
	}
	f.minted = append(f.minted, k.KeyID)
	return k, secret
}

func (f *keysFixture) resolves(secret string) (uuid.UUID, bool) {
	tid, err := f.ts.ResolveTenantFromAPIKey(context.Background(), sha256Hash(secret))
	return tid, err == nil
}

func (f *keysFixture) find(tid, id uuid.UUID) (TenantKey, bool) {
	f.t.Helper()
	keys, err := f.ts.ListKeys(context.Background(), tid)
	if err != nil {
		f.t.Fatalf("ListKeys on %s: %v", f.dialect, err)
	}
	for _, k := range keys {
		if k.KeyID == id {
			return k, true
		}
	}
	return TenantKey{}, false
}

func TestATenantMintsListsAndRevokesItsOwnKeys(t *testing.T) {
	for _, dialect := range []string{DialectPostgres, DialectMySQL, DialectMSSQL} {
		dialect := dialect
		t.Run(dialect, func(t *testing.T) {
			f := newKeysFixture(t, dialect)
			ctx := context.Background()
			tid := f.tenant()

			// MINT. The secret authenticates as this tenant, and is not in the listing.
			k1, s1 := f.mint(tid, "ci", nil)
			if got, ok := f.resolves(s1); !ok || got != tid {
				t.Fatalf("the minted secret resolves to %v, %v; want tenant %s", got, ok, tid)
			}
			future := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
			k2, s2 := f.mint(tid, "temp", &future)
			if _, ok := f.resolves(s2); !ok {
				t.Fatal("a key with a future expiry does not authenticate")
			}

			// LIST.
			listed, ok := f.find(tid, k1.KeyID)
			if !ok || listed.Description != "ci" || listed.Provisional || listed.ExpiresAt != nil || listed.DisabledAt != nil {
				t.Errorf("listed %+v (found=%v), want an active permanent key described ci", listed, ok)
			}
			if listed.Status(time.Now()) != "active" {
				t.Errorf("status = %q, want active", listed.Status(time.Now()))
			}
			l2, _ := f.find(tid, k2.KeyID)
			if l2.ExpiresAt == nil || !l2.ExpiresAt.Equal(future) {
				t.Errorf("expiry round trip: stored %v, want %v", l2.ExpiresAt, future)
			}
			all, _ := f.ts.ListKeys(ctx, tid)
			for _, k := range all {
				if strings.Contains(fmt.Sprintf("%+v", k), s1) || strings.Contains(fmt.Sprintf("%+v", k), s2) {
					t.Fatal("a listing carries a key's secret")
				}
			}
			for i := 1; i < len(all); i++ {
				if all[i].CreatedAt.After(all[i-1].CreatedAt) {
					t.Errorf("the listing is not newest first: %s before %s", all[i-1].CreatedAt, all[i].CreatedAt)
				}
			}

			// REVOKE. The secret stops authenticating; a repeat is not an error; a stranger's id is not found.
			if r, err := f.ts.RevokeKey(ctx, tid, k1.KeyID, false); err != nil || r.AlreadyRevoked {
				t.Fatalf("revoke = %+v, %v", r, err)
			}
			if _, ok := f.resolves(s1); ok {
				t.Error("a revoked key still authenticates")
			}
			if k, _ := f.find(tid, k1.KeyID); k.Status(time.Now()) != "revoked" || k.DisabledAt == nil {
				t.Errorf("revoked key lists as %+v", k)
			}
			if r, err := f.ts.RevokeKey(ctx, tid, k1.KeyID, false); err != nil || !r.AlreadyRevoked {
				t.Errorf("a repeat revoke = %+v, %v; want already revoked", r, err)
			}
			if _, err := f.ts.RevokeKey(ctx, tid, uuid.New(), false); !errors.Is(err, ErrKeyNotFound) {
				t.Errorf("revoking an unknown key = %v, want ErrKeyNotFound", err)
			}
			if _, ok := f.resolves(s2); !ok {
				t.Error("revoking one key revoked another")
			}
		})
	}
}

func TestATenantCannotRevokeOrSeeAnotherTenantsKeys(t *testing.T) {
	for _, dialect := range []string{DialectPostgres, DialectMSSQL} { // MySQL has one tenant
		dialect := dialect
		t.Run(dialect, func(t *testing.T) {
			f := newKeysFixture(t, dialect)
			ctx := context.Background()
			a, b := f.tenant(), f.tenant()
			ka, _ := f.mint(a, "a1", nil)
			f.mint(a, "a2", nil)
			kb, sb := f.mint(b, "b1", nil)
			f.mint(b, "b2", nil)

			// B's key id presented to A: not found, and B's key is untouched.
			if _, err := f.ts.RevokeKey(ctx, a, kb.KeyID, true); !errors.Is(err, ErrKeyNotFound) {
				t.Errorf("A revoking B's key (force) = %v, want ErrKeyNotFound", err)
			}
			if _, ok := f.resolves(sb); !ok {
				t.Error("A revoked B's key")
			}
			// A's listing holds none of B's.
			if _, ok := f.find(a, kb.KeyID); ok {
				t.Error("A's listing includes B's key")
			}
			if _, ok := f.find(b, ka.KeyID); ok {
				t.Error("B's listing includes A's key")
			}
			// A tenant that does not exist cannot mint.
			if _, _, err := f.ts.MintKey(ctx, uuid.New(), "x", nil); !errors.Is(err, ErrTenantNotFound) {
				t.Errorf("minting for an unknown tenant = %v, want ErrTenantNotFound", err)
			}
		})
	}
}

func TestTheLastLiveKeyIsKeptUnlessForced(t *testing.T) {
	for _, dialect := range []string{DialectPostgres, DialectMSSQL} {
		dialect := dialect
		t.Run(dialect, func(t *testing.T) {
			f := newKeysFixture(t, dialect)
			ctx := context.Background()
			tid := f.tenant()
			k1, s1 := f.mint(tid, "old", nil)
			k2, s2 := f.mint(tid, "new", nil)

			// Rotation: the old one goes while the new one lives.
			if _, err := f.ts.RevokeKey(ctx, tid, k1.KeyID, false); err != nil {
				t.Fatalf("revoking the old key during a rotation: %v", err)
			}
			// The new one is now the last.
			if _, err := f.ts.RevokeKey(ctx, tid, k2.KeyID, false); !errors.Is(err, ErrLastKey) {
				t.Fatalf("revoking the last live key = %v, want ErrLastKey", err)
			}
			if _, ok := f.resolves(s2); !ok {
				t.Fatal("a refused revoke still revoked the key")
			}
			// An already-revoked key is not a live one, so repeating its revoke is not blocked.
			if r, err := f.ts.RevokeKey(ctx, tid, k1.KeyID, false); err != nil || !r.AlreadyRevoked {
				t.Errorf("repeating the old revoke = %+v, %v", r, err)
			}
			// Force.
			if _, err := f.ts.RevokeKey(ctx, tid, k2.KeyID, true); err != nil {
				t.Fatalf("forced revoke: %v", err)
			}
			if _, ok := f.resolves(s2); ok {
				t.Error("a forced revoke left the key working")
			}
			_ = s1
		})
	}
}

// Two revokes of the last two keys must not both succeed. Many rounds of many racers, because a race
// this narrow is caught by few rounds only some of the time: with six rounds of four racers, removing
// the lock failed this test in 1 of 3 runs on PostgreSQL and 2 of 3 on SQL Server.
func TestRevokingTheLastTwoKeysConcurrentlyLeavesOne(t *testing.T) {
	for _, dialect := range []string{DialectPostgres, DialectMSSQL} {
		dialect := dialect
		t.Run(dialect, func(t *testing.T) {
			f := newKeysFixture(t, dialect)
			for round := 0; round < 25; round++ {
				tid := f.tenant()
				ka, _ := f.mint(tid, "a", nil)
				kb, _ := f.mint(tid, "b", nil)
				ids := []uuid.UUID{ka.KeyID, kb.KeyID, ka.KeyID, kb.KeyID, ka.KeyID, kb.KeyID, ka.KeyID, kb.KeyID}
				errs := make([]error, len(ids))
				var wg sync.WaitGroup
				start := make(chan struct{})
				for i, id := range ids {
					wg.Add(1)
					go func(i int, id uuid.UUID) {
						defer wg.Done()
						<-start
						_, errs[i] = f.ts.RevokeKey(context.Background(), tid, id, false)
					}(i, id)
				}
				close(start)
				wg.Wait()
				var live int
				if err := f.db.QueryRow(`SELECT count(*) FROM admin.tenant_api_keys WHERE tenant_id = `+f.ph(1)+` AND disabled_at IS NULL`, tid.String()).Scan(&live); err != nil {
					t.Fatal(err)
				}
				if live != 1 {
					t.Errorf("round %d: %d live keys after revoking both concurrently, want exactly 1 (errors: %v)", round, live, errs)
				}
			}
		})
	}
}

func TestATenantCannotHoldMoreThanTheLiveKeyCap(t *testing.T) {
	for _, dialect := range []string{DialectPostgres, DialectMySQL, DialectMSSQL} {
		dialect := dialect
		t.Run(dialect, func(t *testing.T) {
			f := newKeysFixture(t, dialect)
			ctx := context.Background()
			tid := f.tenant()
			var live int
			table := "admin.tenant_api_keys"
			now := "now()"
			switch dialect {
			case DialectMySQL:
				table, now = "tenant_api_keys", "NOW(6)"
			case DialectMSSQL:
				now = "SYSUTCDATETIME()"
			}
			if err := f.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE tenant_id = `+f.ph(1)+` AND disabled_at IS NULL AND (expires_at IS NULL OR expires_at > `+now+`)`, tid.String()).Scan(&live); err != nil {
				t.Fatal(err)
			}
			if live >= MaxLiveTenantKeys {
				t.Fatalf("UNMEASURED: a fresh tenant already holds %d live keys, so the cap cannot be reached by minting; the fixture is broken, not the cap", live)
			}
			var last TenantKey
			for i := live; i < MaxLiveTenantKeys; i++ {
				last, _ = f.mint(tid, "fill", nil)
			}
			if _, _, err := f.ts.MintKey(ctx, tid, "one too many", nil); !errors.Is(err, ErrKeyLimit) {
				t.Fatalf("minting past the cap = %v, want ErrKeyLimit", err)
			}
			// Revoking one frees a slot; an expired key never counted.
			if _, err := f.ts.RevokeKey(ctx, tid, last.KeyID, false); err != nil {
				t.Fatal(err)
			}
			if _, secret, err := f.ts.MintKey(ctx, tid, "after a revoke", nil); err != nil {
				t.Errorf("minting after a revoke freed a slot: %v", err)
			} else {
				_ = secret
			}
		})
	}
}
