package auth

// cleat#3578: MintRecoveryKey gives a tenant that cannot act for itself a way back in, and gives
// nobody a way into a tenant that can.
//
// The guard is one fact read in the same transaction as the insert, under the tenant's row lock: the
// tenant has NO live key. A live key that is permanent, one that is provisional and unexpired (so a second
// call is refused while a recovery key is outstanding), and a suspended tenant are all refusals; a tenant
// whose keys are revoked or expired is the case it exists for. Every refusal is checked by counting the
// key rows before and after, because "it returned an error" does not say nothing was minted.
//
// Real PostgreSQL and SQL Server. The rows beside each case are the second and third calls of each
// guarantee, since a first call passes against a store that scopes nothing: a recovery for one tenant
// must write no row for another.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func forEachRecoveryDialect(t *testing.T, fn func(t *testing.T, f *keysFixture, schema string)) {
	t.Helper()
	for _, d := range []struct{ dialect, schema string }{{DialectPostgres, "public"}, {DialectMSSQL, ""}} {
		d := d
		t.Run(d.dialect, func(t *testing.T) { fn(t, newKeysFixture(t, d.dialect), d.schema) })
	}
}

// rows counts every key row of the tenant, live or not.
func (f *keysFixture) rows(tenant uuid.UUID) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(`SELECT count(*) FROM admin.tenant_api_keys WHERE tenant_id = `+f.ph(1), tenant.String()).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// live counts the keys that authenticate now: active, or provisional and not yet lapsed.
func (f *keysFixture) live(tenant uuid.UUID) int {
	f.t.Helper()
	keys, err := f.ts.ListKeys(context.Background(), tenant)
	if err != nil {
		f.t.Fatal(err)
	}
	n := 0
	for _, k := range keys {
		if s := k.Status(time.Now()); s == "active" || s == "provisional" {
			n++
		}
	}
	return n
}

func (f *keysFixture) lapse(keyID uuid.UUID) {
	f.t.Helper()
	past := `now() - interval '1 minute'`
	if f.dialect == DialectMSSQL {
		past = `DATEADD(minute, -1, SYSUTCDATETIME())`
	}
	if _, err := f.db.Exec(`UPDATE admin.tenant_api_keys SET expires_at = `+past+` WHERE key_id = `+f.ph(1), keyID.String()); err != nil {
		f.t.Fatal(err)
	}
}

func refusedWith(err error, reason string) bool {
	var r *RecoveryRefusedError
	return errors.As(err, &r) && r.Reason == reason
}

func TestARecoveryKeyIsMintedOnlyForATenantWithNoLiveKey(t *testing.T) {
	forEachRecoveryDialect(t, func(t *testing.T, f *keysFixture, schema string) {
		ctx := context.Background()

		t.Run("a tenant with no key gets a provisional key that authenticates and lapses", func(t *testing.T) {
			tenant := f.tenant()
			before := f.rows(tenant)
			k, err := f.ts.MintRecoveryKey(ctx, tenant, "operator recovery")
			if err != nil {
				t.Fatalf("a keyless tenant was refused: %v", err)
			}
			f.minted = append(f.minted, k.KeyID)
			if !strings.HasPrefix(k.Secret, "cleat_sk_") || len(k.Secret) != len("cleat_sk_")+64 {
				t.Errorf("secret %q is not an API key", k.Secret)
			}
			if k.ValidFor != ProvisionalKeyTTL {
				t.Errorf("valid for %s, want the provisional TTL %s", k.ValidFor, ProvisionalKeyTTL)
			}
			if f.rows(tenant) != before+1 || f.live(tenant) != 1 {
				t.Errorf("rows %d->%d, live %d, want exactly one new live row", before, f.rows(tenant), f.live(tenant))
			}
			// It authenticates NOW, as the tenant it was minted for.
			gotTenant, gotKey, err := f.ts.ResolveAPIKey(ctx, sha256Hash(k.Secret))
			if err != nil || gotTenant != tenant || gotKey != k.KeyID {
				t.Errorf("the recovery key resolves to tenant %s key %s (%v), want %s and %s", gotTenant, gotKey, err, tenant, k.KeyID)
			}
			// The database holds the HASH and never the secret.
			var n int
			if err := f.db.QueryRow(`SELECT count(*) FROM admin.tenant_api_keys WHERE key_hash = `+f.ph(1), []byte(k.Secret)).Scan(&n); err != nil || n != 0 {
				t.Errorf("a row holds the secret itself as its key_hash (%d, %v)", n, err)
			}
			keys, _ := f.ts.ListKeys(ctx, tenant)
			if len(keys) != 1 || !keys[0].Provisional || keys[0].ExpiresAt == nil || time.Until(*keys[0].ExpiresAt) > ProvisionalKeyTTL+time.Minute {
				t.Errorf("the key is %+v, want provisional and lapsing within the TTL", keys)
			}
		})

		t.Run("a second call while the recovery key is outstanding is refused, and writes nothing", func(t *testing.T) {
			tenant := f.tenant()
			first, err := f.ts.MintRecoveryKey(ctx, tenant, "first")
			if err != nil {
				t.Fatal(err)
			}
			f.minted = append(f.minted, first.KeyID)
			before := f.rows(tenant)
			_, err = f.ts.MintRecoveryKey(ctx, tenant, "second")
			if !refusedWith(err, RecoveryRefusedLiveKeys) {
				t.Fatalf("a second recovery mint inside the TTL = %v, want %s", err, RecoveryRefusedLiveKeys)
			}
			if f.rows(tenant) != before {
				t.Errorf("a refused mint wrote %d row(s)", f.rows(tenant)-before)
			}
			// After it lapses unseen, the tenant is keyless again and a retry works: the reason it is provisional.
			f.lapse(first.KeyID)
			second, err := f.ts.MintRecoveryKey(ctx, tenant, "retry after the first lapsed")
			if err != nil {
				t.Fatalf("a retry after the first key lapsed: %v", err)
			}
			f.minted = append(f.minted, second.KeyID)
		})

		t.Run("a tenant that can still act for itself is refused, and writes nothing", func(t *testing.T) {
			tenant := f.tenant()
			own, _, err := f.ts.MintKey(ctx, tenant, "the tenant's own key", nil)
			if err != nil {
				t.Fatal(err)
			}
			f.minted = append(f.minted, own.KeyID)
			before := f.rows(tenant)
			_, err = f.ts.MintRecoveryKey(ctx, tenant, "should be refused")
			var r *RecoveryRefusedError
			if !errors.As(err, &r) || r.Reason != RecoveryRefusedLiveKeys || r.LiveKeys != 1 {
				t.Fatalf("a tenant with a live key = %v, want %s naming 1 live key", err, RecoveryRefusedLiveKeys)
			}
			if f.rows(tenant) != before || f.live(tenant) != 1 {
				t.Errorf("rows %d->%d, live %d: a refused recovery changed the tenant's keys", before, f.rows(tenant), f.live(tenant))
			}
		})

		t.Run("a tenant whose only keys are revoked or expired is the case it exists for", func(t *testing.T) {
			tenant := f.tenant()
			revoked, _, err := f.ts.MintKey(ctx, tenant, "revoked", nil)
			if err != nil {
				t.Fatal(err)
			}
			f.minted = append(f.minted, revoked.KeyID)
			if _, err := f.ts.RevokeKey(ctx, tenant, revoked.KeyID, true); err != nil { // force: the last key
				t.Fatal(err)
			}
			soon := time.Now().Add(time.Hour)
			expired, _, err := f.ts.MintKey(ctx, tenant, "expired", &soon)
			if err != nil {
				t.Fatal(err)
			}
			f.minted = append(f.minted, expired.KeyID)
			f.lapse(expired.KeyID)
			if f.live(tenant) != 0 {
				t.Fatalf("UNMEASURED: the tenant still has %d live key(s), so this is not the locked-out case", f.live(tenant))
			}
			k, err := f.ts.MintRecoveryKey(ctx, tenant, "recover")
			if err != nil {
				t.Fatalf("a tenant with only revoked and expired keys was refused: %v", err)
			}
			f.minted = append(f.minted, k.KeyID)
			// The disabled and expired rows stay as history: a recovery ADDS one and revokes nothing.
			if f.rows(tenant) != 3 || f.live(tenant) != 1 {
				t.Errorf("rows %d live %d, want 3 rows (two kept as history) and 1 live", f.rows(tenant), f.live(tenant))
			}
		})

		t.Run("a suspended tenant is refused until it is resumed", func(t *testing.T) {
			tenant := f.tenant()
			if err := f.ts.SetTenantSuspended(ctx, tenant, true); err != nil {
				t.Fatal(err)
			}
			before := f.rows(tenant)
			if _, err := f.ts.MintRecoveryKey(ctx, tenant, "while suspended"); !refusedWith(err, RecoveryRefusedSuspended) {
				t.Fatalf("a suspended tenant = %v, want %s", err, RecoveryRefusedSuspended)
			}
			if f.rows(tenant) != before {
				t.Errorf("a refused mint wrote a row")
			}
			if err := f.ts.SetTenantSuspended(ctx, tenant, false); err != nil {
				t.Fatal(err)
			}
			k, err := f.ts.MintRecoveryKey(ctx, tenant, "after resume")
			if err != nil {
				t.Fatalf("after the resume: %v", err)
			}
			f.minted = append(f.minted, k.KeyID)
		})

		t.Run("an unknown, the default and a dropped tenant are all 'not found'", func(t *testing.T) {
			if _, err := f.ts.MintRecoveryKey(ctx, uuid.New(), "x"); !errors.Is(err, ErrTenantNotFound) {
				t.Errorf("an unknown tenant = %v, want ErrTenantNotFound", err)
			}
			if _, err := f.ts.MintRecoveryKey(ctx, uuid.Nil, "x"); !errors.Is(err, ErrTenantNotFound) {
				t.Errorf("the default tenant = %v, want ErrTenantNotFound", err)
			}
			gone := f.tenant()
			if err := f.ts.SetTenantSuspended(ctx, gone, true); err != nil {
				t.Fatal(err)
			}
			if _, err := f.ts.DropTenant(ctx, DropRequest{TenantID: gone, DroppedBy: "operator:test", Schema: schema}); err != nil {
				t.Fatalf("UNMEASURED: could not drop the tenant this case needs: %v", err)
			}
			t.Cleanup(func() {
				_, _ = f.db.Exec(`DELETE FROM admin.tenant_tombstones WHERE dropped_tenant_id = `+f.ph(1), gone.String())
			})
			if _, err := f.ts.MintRecoveryKey(ctx, gone, "x"); !errors.Is(err, ErrTenantNotFound) {
				t.Errorf("a dropped tenant = %v, want ErrTenantNotFound", err)
			}
		})

		t.Run("a recovery for one tenant writes no row for another", func(t *testing.T) {
			a, b := f.tenant(), f.tenant()
			ownB, _, err := f.ts.MintKey(ctx, b, "b's own key", nil)
			if err != nil {
				t.Fatal(err)
			}
			f.minted = append(f.minted, ownB.KeyID)
			bBefore := f.rows(b)
			k, err := f.ts.MintRecoveryKey(ctx, a, "for a only")
			if err != nil {
				t.Fatal(err)
			}
			f.minted = append(f.minted, k.KeyID)
			if f.rows(b) != bBefore {
				t.Errorf("tenant B has %d key rows after A's recovery, had %d", f.rows(b), bBefore)
			}
			if gotTenant, _, _ := f.ts.ResolveAPIKey(ctx, sha256Hash(k.Secret)); gotTenant != a {
				t.Errorf("A's recovery key resolves to %s, want A (%s)", gotTenant, a)
			}
		})
	})
}

// THE RACES. A guard that reads a count and then writes is the shape that passes a single run and fails one
// in ten, so each case runs several rounds on fresh tenants, and the file is run with -count=30 on head
// (and on a mutant with the lock removed, to see that it is caught). Each round checks an INVARIANT that
// holds under every interleaving, not an outcome that depends on which goroutine won.
func TestARecoveryMintRacesAreDecidedUnderTheTenantsRowLock(t *testing.T) {
	const rounds = 6
	forEachRecoveryDialect(t, func(t *testing.T, f *keysFixture, schema string) {
		ctx := context.Background()

		t.Run("eight recoveries at once on a keyless tenant: exactly one wins", func(t *testing.T) {
			for round := 0; round < rounds; round++ {
				tenant := f.tenant()
				var wins, refused, other atomic.Int32
				var mu sync.Mutex
				var won []uuid.UUID
				var ready, start, done sync.WaitGroup
				ready.Add(8)
				start.Add(1)
				for i := 0; i < 8; i++ {
					done.Add(1)
					go func() {
						defer done.Done()
						ready.Done()
						start.Wait()
						k, err := f.ts.MintRecoveryKey(ctx, tenant, "race")
						switch {
						case err == nil:
							wins.Add(1)
							mu.Lock()
							won = append(won, k.KeyID)
							mu.Unlock()
						case refusedWith(err, RecoveryRefusedLiveKeys):
							refused.Add(1)
						default:
							other.Add(1)
							t.Errorf("unexpected error: %v", err)
						}
					}()
				}
				ready.Wait()
				start.Done()
				done.Wait()
				f.minted = append(f.minted, won...)
				if wins.Load() != 1 || refused.Load() != 7 || f.live(tenant) != 1 || f.rows(tenant) != 1 {
					t.Fatalf("round %d: %d won, %d refused, %d live, %d rows; want 1, 7, 1, 1", round, wins.Load(), refused.Load(), f.live(tenant), f.rows(tenant))
				}
			}
		})

		t.Run("the tenant minting its own key while a recovery runs: the recovery never overrides a tenant that has a key", func(t *testing.T) {
			for round := 0; round < rounds; round++ {
				tenant := f.tenant()
				own, _, err := f.ts.MintKey(ctx, tenant, "held throughout", nil)
				if err != nil {
					t.Fatal(err)
				}
				f.minted = append(f.minted, own.KeyID)
				var recoveryErr, mintErr error
				var ready, start, done sync.WaitGroup
				ready.Add(2)
				start.Add(1)
				done.Add(2)
				go func() {
					defer done.Done()
					ready.Done()
					start.Wait()
					_, recoveryErr = f.ts.MintRecoveryKey(ctx, tenant, "race")
				}()
				go func() {
					defer done.Done()
					ready.Done()
					start.Wait()
					_, _, mintErr = f.ts.MintKey(ctx, tenant, "the tenant's second", nil)
				}()
				ready.Wait()
				start.Done()
				done.Wait()
				if mintErr != nil {
					t.Fatalf("round %d: the tenant's own mint failed: %v", round, mintErr)
				}
				if !refusedWith(recoveryErr, RecoveryRefusedLiveKeys) {
					t.Fatalf("round %d: a recovery for a tenant that held a live key throughout = %v, want a refusal", round, recoveryErr)
				}
				if f.rows(tenant) != 2 {
					t.Fatalf("round %d: %d key rows, want exactly the tenant's two", round, f.rows(tenant))
				}
			}
		})

		t.Run("the tenant force-revoking its last key while a recovery runs: never both a refusal and a lockout of nobody", func(t *testing.T) {
			var recovered, refusedRounds int
			for round := 0; round < rounds; round++ {
				tenant := f.tenant()
				own, _, err := f.ts.MintKey(ctx, tenant, "the last key", nil)
				if err != nil {
					t.Fatal(err)
				}
				f.minted = append(f.minted, own.KeyID)
				var recoveryErr, revokeErr error
				var rk RecoveryKey
				var ready, start, done sync.WaitGroup
				ready.Add(2)
				start.Add(1)
				done.Add(2)
				go func() {
					defer done.Done()
					ready.Done()
					start.Wait()
					rk, recoveryErr = f.ts.MintRecoveryKey(ctx, tenant, "race")
				}()
				go func() {
					defer done.Done()
					ready.Done()
					start.Wait()
					_, revokeErr = f.ts.RevokeKey(ctx, tenant, own.KeyID, true)
				}()
				ready.Wait()
				start.Done()
				done.Wait()
				if revokeErr != nil {
					t.Fatalf("round %d: the tenant's revoke failed: %v", round, revokeErr)
				}
				live := f.live(tenant)
				switch {
				case recoveryErr == nil:
					// Revoke first, then recovery saw no live key: exactly the recovery key is live.
					recovered++
					f.minted = append(f.minted, rk.KeyID)
					if live != 1 {
						t.Fatalf("round %d: the recovery succeeded and %d key(s) are live; want exactly the recovery key", round, live)
					}
				case refusedWith(recoveryErr, RecoveryRefusedLiveKeys):
					// Recovery first, saw the live key and refused; the revoke then left the tenant with none.
					refusedRounds++
					if live != 0 {
						t.Fatalf("round %d: the recovery was refused and %d key(s) are live; want none", round, live)
					}
				default:
					t.Fatalf("round %d: unexpected error %v", round, recoveryErr)
				}
			}
			t.Logf("%s: %d rounds, recovery succeeded in %d and was refused in %d (both orders are legal; the invariant held in each)", f.dialect, rounds, recovered, refusedRounds)
		})
	})
}
