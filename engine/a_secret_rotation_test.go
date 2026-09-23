package engine

// Master-key rotation for tenant secrets (cleat#1991), on all three dialects.
//
// The design these tests pin is the one specs/CleatKeyRotation.tla checks. The
// tests that carry a model trace say so, because the trace is the reason the
// test exists and a reader deciding whether it can be deleted should know:
//
//   - TestResealDoesNotOverwriteAConcurrentSetSecret is the model's S2
//     counterexample with ResealCAS = FALSE (8 states): set-secret writes,
//     reseal reads, a second set-secret lands from an environment not yet on the
//     new ring, reseal writes back the older value.
//   - TestCheckKeyRingCountsSuspendedTenantsAndNamesTheUnopenableVersion is the
//     state S1 exists to prevent (a serving worker holding a row it cannot open),
//     as the boot check sees it.
//
// EVERY TEST RUNS THE STORE AS THE ROLE A WORKER CONNECTS AS. On PostgreSQL
// that is cleat_app, which row-level security applies to; the superuser the
// other tests use bypasses it, and a read that only works because it bypasses
// the policy is the failure cleat#2123 is about.

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/internal/tenantctx"
	"github.com/google/uuid"
)

type rotationEnv struct {
	dialect testutil.Dialect
	owner   *sql.DB
	worker  *sql.DB
	// tenants[0] is the default tenant. A second, SUSPENDED tenant follows where
	// the dialect can hold one (MySQL is single-tenant by constraint).
	tenants []uuid.UUID
}

func newRotationEnv(t *testing.T, dialect testutil.Dialect) *rotationEnv {
	t.Helper()
	owner := testutil.TestDB(t, dialect)
	t.Cleanup(func() { owner.Close() })
	testutil.SetupFullSchema(t, owner, dialect)

	worker := owner
	if dialect == testutil.DialectPostgres {
		applyPostgresProcedures(t, owner)
		applyAppRoleMigration(t, owner)
		worker = appRoleDB(t, owner)
	}
	e := &rotationEnv{dialect: dialect, owner: owner, worker: worker,
		tenants: []uuid.UUID{uuid.MustParse(DefaultTenantUUID)}}

	if dialect != testutil.DialectMySQL {
		suspended := uuid.New()
		ins := map[testutil.Dialect]string{
			testutil.DialectPostgres: `INSERT INTO admin.tenants (tenant_id, name, suspended) VALUES ($1, $2, true)`,
			testutil.DialectMSSQL:    `INSERT INTO admin.tenants (tenant_id, name, suspended) VALUES (@p1, @p2, 1)`,
		}[dialect]
		if _, err := owner.Exec(ins, suspended.String(), "cleat-1991-"+suspended.String()[:8]); err != nil {
			t.Fatalf("seed a suspended tenant: %v", err)
		}
		e.tenants = append(e.tenants, suspended)
		t.Cleanup(func() {
			// The foreign key cascades the tenant's secrets with it.
			owner.Exec(deleteTenantStmtForTest(dialect), suspended.String()) //nolint:errcheck // best-effort cleanup
		})
	}
	return e
}

func (e *rotationEnv) store(ring *KeyRing) *SecretStore {
	return NewSecretStoreWithRing(e.worker, string(e.dialect), ring)
}

func (e *rotationEnv) ctx(tenant uuid.UUID) context.Context {
	return tenantctx.With(context.Background(), tenant)
}

// claim reserves a secret name for one test: removes any row an earlier run
// left, and removes the row again when the test ends. Without the first, a
// leftover turns PutSecret's insert into an update and the test exercises a
// different path than it names.
func (e *rotationEnv) claim(t *testing.T, tenant uuid.UUID, name string) {
	t.Helper()
	deleteSecretRowForTest(t, e.owner, e.dialect, tenant, name)
	t.Cleanup(func() { deleteSecretRowForTest(t, e.owner, e.dialect, tenant, name) })
}

func (e *rotationEnv) put(t *testing.T, ring *KeyRing, tenant uuid.UUID, name, value string) {
	t.Helper()
	if err := e.store(ring).PutSecret(e.ctx(tenant), tenant.String(), name, value); err != nil {
		t.Fatalf("PutSecret %q: %v", name, err)
	}
}

// rawKeyVersion reads the column itself, not through the store under test.
func (e *rotationEnv) rawKeyVersion(t *testing.T, tenant uuid.UUID, name string) int {
	t.Helper()
	probe := NewSecretStoreWithRing(e.owner, string(e.dialect), nil)
	ctx := e.ctx(tenant)
	q := map[testutil.Dialect]string{
		testutil.DialectPostgres: `SELECT key_version FROM tenant_secrets WHERE tenant_id = $1 AND name = $2`,
		testutil.DialectMySQL:    `SELECT key_version FROM tenant_secrets WHERE tenant_id = ? AND name = ?`,
		testutil.DialectMSSQL:    `SELECT key_version FROM tenant_secrets WHERE tenant_id = @p1 AND name = @p2`,
	}[e.dialect]
	var v int
	if err := probe.execTenantScoped(ctx, func(qr querier) error {
		return qr.QueryRowContext(ctx, q, tenant.String(), name).Scan(&v)
	}); err != nil {
		t.Fatalf("read key_version of %q: %v", name, err)
	}
	return v
}

// mine reports whether a run left any of THIS test's rows unreadable, ignoring
// rows another test left behind. ResealSecrets is global by design, so its report
// covers every tenant's rows, and a row leaked by a different test (cleat#2126:
// the retired-secret test leaves one on SQL Server) sealed under a key this test
// does not hold will always be in it. Asserting on the whole report would make
// this test fail for another test's defect; asserting on its own rows cannot.
func mine(res SecretReseal, names ...string) (unreadable []UnreadableSecret) {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	for _, u := range res.Unreadable {
		if want[u.Name] {
			unreadable = append(unreadable, u)
		}
	}
	return unreadable
}

func forEachDialect(t *testing.T, fn func(t *testing.T, e *rotationEnv)) {
	for _, dialect := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMySQL, testutil.DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) { fn(t, newRotationEnv(t, dialect)) })
	}
}

var (
	rotV1 = VersionedKey{Version: 1, Key: rawKey(0x11)}
	rotV2 = VersionedKey{Version: 2, Key: rawKey(0x22)}
)

// A write records the key that sealed it; a row sealed before the rotation
// still opens afterwards under the previous key; and once the previous key is
// gone a read says WHICH version is missing.
func TestAWriteRecordsItsKeyVersionAndAnOlderRowStillOpens(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *rotationEnv) {
		tenant := e.tenants[0]
		const name = "cleat-1991-versions"
		e.claim(t, tenant, name)
		ctx := e.ctx(tenant)

		e.put(t, ringOf(t, rotV1), tenant, name, "sk-before-rotation")
		if got := e.rawKeyVersion(t, tenant, name); got != 1 {
			t.Fatalf("a write under a ring whose current key is version 1 stored key_version %d", got)
		}

		both := ringOf(t, rotV2, rotV1)
		got, err := e.store(both).GetSecret(ctx, tenant.String(), name)
		if err != nil || got != "sk-before-rotation" {
			t.Fatalf("a row sealed before the rotation must open under the previous key: %q, %v", got, err)
		}

		// A new write goes under the current key -- on the UPDATE arm, which is
		// the arm a re-run of set-secret takes.
		e.put(t, both, tenant, name, "sk-after-rotation")
		if got := e.rawKeyVersion(t, tenant, name); got != 2 {
			t.Fatalf("a write under a ring whose current key is version 2 stored key_version %d, "+
				"which is the UPDATE arm not carrying the version", got)
		}

		// The previous key is removed while the row is on the NEW one: still fine.
		if got, err := e.store(ringOf(t, rotV2)).GetSecret(ctx, tenant.String(), name); err != nil || got != "sk-after-rotation" {
			t.Fatalf("a row on the current key must open once the previous key is gone: %q, %v", got, err)
		}

		// And the reverse: a ring without version 2 cannot open it, and says so.
		_, err = e.store(ringOf(t, rotV1)).GetSecret(ctx, tenant.String(), name)
		var verr *SecretKeyVersionError
		if !errors.As(err, &verr) || verr.Version != 2 {
			t.Fatalf("err = %v, want a *SecretKeyVersionError naming version 2", err)
		}
	})
}

// ResealSecrets moves every row -- across tenants, a suspended tenant's and a
// retired secret's included -- under the current key, verifies it, and is
// idempotent. The previous key is then not needed to read any of them.
func TestResealSecretsMovesEveryRowUnderTheCurrentKey(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *rotationEnv) {
		type row struct {
			tenant uuid.UUID
			name   string
			value  string
		}
		var rows []row
		var names []string
		for i, tn := range e.tenants {
			name := "cleat-1991-reseal-" + string(rune('a'+i))
			e.claim(t, tn, name)
			e.put(t, ringOf(t, rotV1), tn, name, "value-"+name)
			rows = append(rows, row{tn, name, "value-" + name})
			names = append(names, name)
		}
		// A retired secret: still ciphertext under the old key, and revival needs
		// it readable (cleat#1989).
		retired := "cleat-1991-reseal-retired"
		e.claim(t, e.tenants[0], retired)
		e.put(t, ringOf(t, rotV1), e.tenants[0], retired, "value-retired")
		if _, err := e.store(ringOf(t, rotV1)).RetireSecret(e.ctx(e.tenants[0]), e.tenants[0].String(), retired); err != nil {
			t.Fatalf("RetireSecret: %v", err)
		}
		rows = append(rows, row{e.tenants[0], retired, "value-retired"})
		names = append(names, retired)

		rotating := e.store(ringOf(t, rotV2, rotV1))

		// A dry run reports the work and writes nothing.
		dry, err := rotating.ResealSecrets(context.Background(), true)
		if err != nil {
			t.Fatalf("dry-run ResealSecrets: %v", err)
		}
		if dry.Resealed < len(rows) {
			t.Fatalf("dry run would reseal %d, want at least the %d rows seeded (%+v)", dry.Resealed, len(rows), dry)
		}
		for _, r := range rows {
			if v := e.rawKeyVersion(t, r.tenant, r.name); v != 1 {
				t.Fatalf("a DRY RUN changed %q to key_version %d", r.name, v)
			}
		}

		res, err := rotating.ResealSecrets(context.Background(), false)
		if err != nil {
			t.Fatalf("ResealSecrets: %v", err)
		}
		if u := mine(res, names...); len(u) != 0 || res.Changed != 0 {
			t.Fatalf("this test's rows were left unreadable or changed: unreadable=%+v changed=%d (%+v)", u, res.Changed, res)
		}
		if res.Resealed < len(rows) {
			t.Fatalf("resealed %d, want at least the %d rows seeded (%+v)", res.Resealed, len(rows), res)
		}

		afterOnly := e.store(ringOf(t, rotV2))
		for _, r := range rows {
			if v := e.rawKeyVersion(t, r.tenant, r.name); v != 2 {
				t.Errorf("%q is key_version %d after reseal, want 2", r.name, v)
			}
			if r.name == retired {
				// Retired stays retired, and its value is still reachable once revived.
				exists, disabledAt, err := afterOnly.SecretMeta(e.ctx(r.tenant), r.tenant.String(), r.name)
				if err != nil || !exists || !disabledAt.Valid {
					t.Errorf("a retired secret must stay retired through a reseal: exists=%v disabled=%v err=%v",
						exists, disabledAt.Valid, err)
				}
				continue
			}
			got, err := afterOnly.GetSecret(e.ctx(r.tenant), r.tenant.String(), r.name)
			if err != nil || got != r.value {
				t.Errorf("%q under the NEW key alone: %q, %v (want %q)", r.name, got, err, r.value)
			}
		}

		again, err := rotating.ResealSecrets(context.Background(), false)
		if err != nil || again.Resealed != 0 || again.Changed != 0 || len(mine(again, names...)) != 0 {
			t.Fatalf("a second reseal must find nothing to do: %+v, %v", again, err)
		}
	})
}

// A row this ring cannot open is REPORTED, with its version, and left exactly as
// it was. It must not be skipped silently, and it must not be touched.
func TestResealSecretsReportsARowItCannotOpenAndLeavesItAlone(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *rotationEnv) {
		tenant := e.tenants[0]
		const name = "cleat-1991-unreadable"
		e.claim(t, tenant, name)
		rotV3 := VersionedKey{Version: 3, Key: rawKey(0x33)}
		e.put(t, ringOf(t, rotV3), tenant, name, "sealed-under-a-key-this-ring-lacks")

		res, err := e.store(ringOf(t, rotV2, rotV1)).ResealSecrets(context.Background(), false)
		if err != nil {
			t.Fatalf("ResealSecrets: %v", err)
		}
		var found *UnreadableSecret
		for i := range res.Unreadable {
			if res.Unreadable[i].Name == name {
				found = &res.Unreadable[i]
			}
		}
		if found == nil {
			t.Fatalf("the row was not reported; Unreadable = %+v", res.Unreadable)
		}
		if found.KeyVersion != 3 || found.TenantID != tenant.String() {
			t.Errorf("reported %+v, want tenant %s version 3", *found, tenant)
		}
		if res.Converged() {
			t.Error("a run that left an unreadable row reported itself converged")
		}
		if len(res.Unreadable) == 0 {
			t.Error("Converged() is false but nothing is listed as unreadable")
		}
		if v := e.rawKeyVersion(t, tenant, name); v != 3 {
			t.Errorf("the unreadable row was TOUCHED: key_version is now %d, want 3", v)
		}
		// It still opens under the ring that has its key: nothing was corrupted.
		if got, err := e.store(ringOf(t, rotV3)).GetSecret(e.ctx(tenant), tenant.String(), name); err != nil ||
			got != "sealed-under-a-key-this-ring-lacks" {
			t.Errorf("the row no longer opens under its own key: %q, %v", got, err)
		}
	})
}

// The model's S2 counterexample, on all three dialects: reseal reads a row, a
// set-secret from an environment not yet on the new ring lands before reseal
// writes, and reseal must NOT write back the value it read.
func TestResealDoesNotOverwriteAConcurrentSetSecret(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *rotationEnv) {
		tenant := e.tenants[0]
		const name = "cleat-1991-cas"
		e.claim(t, tenant, name)
		e.put(t, ringOf(t, rotV1), tenant, name, "value-read-by-reseal")

		rotating := e.store(ringOf(t, rotV2, rotV1))
		// The stale environment: an operator's laptop still on {1}.
		stale := e.store(ringOf(t, rotV1))
		landed := false
		rotating.beforeResealWrite = func(tid, n string) {
			if n != name || landed {
				return
			}
			landed = true
			if err := stale.PutSecret(e.ctx(tenant), tid, n, "value-set-during-reseal"); err != nil {
				t.Errorf("the concurrent set-secret failed: %v", err)
			}
		}

		res, err := rotating.ResealSecrets(context.Background(), false)
		if err != nil {
			t.Fatalf("ResealSecrets: %v", err)
		}
		if !landed {
			t.Fatal("the concurrent write never ran: this test measured nothing")
		}
		if res.Changed < 1 {
			t.Errorf("Changed = %d, want at least 1: the row changed under the sweep and it must say so (%+v)", res.Changed, res)
		}
		got, err := e.store(ringOf(t, rotV2, rotV1)).GetSecret(e.ctx(tenant), tenant.String(), name)
		if err != nil {
			t.Fatalf("GetSecret: %v", err)
		}
		if got != "value-set-during-reseal" {
			t.Fatalf("the value is %q: reseal wrote back the OLD plaintext over a newer set-secret "+
				"(specs/CleatKeyRotation.tla, S2_NoLostWrite with ResealCAS = FALSE)", got)
		}

		// A second run converges, and the newer value survives it.
		rotating.beforeResealWrite = nil
		if again, err := rotating.ResealSecrets(context.Background(), false); err != nil ||
			again.Changed != 0 || len(mine(again, name)) != 0 {
			t.Fatalf("second reseal: %+v, %v", again, err)
		}
		if got, _ := e.store(ringOf(t, rotV2)).GetSecret(e.ctx(tenant), tenant.String(), name); got != "value-set-during-reseal" {
			t.Fatalf("after convergence the value is %q, want the one set during the first run", got)
		}
	})
}

// The boot check's census: rows counted across EVERY tenant (a suspended
// tenant's included), sorted into "opens", "opens but is not current" and "no
// key". Measured as a DELTA over what the same call returned before seeding, so
// a row another test left behind cannot make a broken census look right.
func TestCheckKeyRingCountsSuspendedTenantsAndNamesTheUnopenableVersion(t *testing.T) {
	forEachDialect(t, func(t *testing.T, e *rotationEnv) {
		onlyV2 := e.store(ringOf(t, rotV2))
		both := e.store(ringOf(t, rotV2, rotV1))
		noRing := e.store(nil)

		for i, tn := range e.tenants {
			e.claim(t, tn, "cleat-1991-census-"+string(rune('a'+i)))
		}
		beforeOnly, err := onlyV2.CheckKeyRing(context.Background())
		if err != nil {
			t.Fatalf("CheckKeyRing before seeding: %v", err)
		}
		beforeBoth, _ := both.CheckKeyRing(context.Background())
		beforeNone, _ := noRing.CheckKeyRing(context.Background())

		for i, tn := range e.tenants {
			e.put(t, ringOf(t, rotV1), tn, "cleat-1991-census-"+string(rune('a'+i)), "v")
		}
		seeded := len(e.tenants)

		// The new ring alone cannot open version 1: this is the state the boot
		// check refuses, and the suspended tenant's row is part of it.
		afterOnly, err := onlyV2.CheckKeyRing(context.Background())
		if err != nil {
			t.Fatalf("CheckKeyRing: %v", err)
		}
		if d := afterOnly.Unopenable[1] - beforeOnly.Unopenable[1]; d != seeded {
			t.Fatalf("Unopenable[1] grew by %d, want %d -- a census that skips suspended tenants "+
				"reads short here, and the boot check would pass a worker that cannot open a row", d, seeded)
		}
		if len(afterOnly.OnPrevious) != 0 {
			t.Errorf("OnPrevious = %v with no previous key configured", afterOnly.OnPrevious)
		}

		// With the previous key present the same rows are openable, and are
		// reported as still on the previous key: what reseal exists to clear.
		afterBoth, _ := both.CheckKeyRing(context.Background())
		if d := afterBoth.OnPrevious[1] - beforeBoth.OnPrevious[1]; d != seeded {
			t.Errorf("OnPrevious[1] grew by %d, want %d", d, seeded)
		}
		if afterBoth.Unopenable[1] != 0 {
			t.Errorf("Unopenable[1] = %d with the previous key present", afterBoth.Unopenable[1])
		}

		// No key at all: every row is unopenable, and it says so.
		afterNone, _ := noRing.CheckKeyRing(context.Background())
		if d := afterNone.Unopenable[1] - beforeNone.Unopenable[1]; d != seeded {
			t.Errorf("with no ring Unopenable[1] grew by %d, want %d", d, seeded)
		}
	})
}
