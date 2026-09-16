package engine

// Layer-separation proof for cleat#1534:
// migrations/postgres/083_an_idempotency_key_belongs_to_one_tenant.sql gives
// idempotency_keys a fail-closed policy, which migrations 031 and 061 both
// declined because startNewRun read the table before any RLS context existed.
//
// The commit carrying this file moves those reads onto transactions that have
// the tenant set. Three properties have to hold together, and each of them can
// be true while another is broken:
//
//	the store still works      startNewRun runs on a connection the policy
//	                           APPLIES to -- before the reorder this raises
//	the policy filters         a session seeing only its own rows, with no
//	                           tenant predicate in the statement at all
//	the sweep still crosses    the TTL delete reaching every tenant's expired
//	                           keys, which a fail-closed policy stops dead
//
// Written as one test over one fixture because the fixture is what makes any of
// them mean anything: a policy's USING is a row-level predicate, so against an
// empty table it is never evaluated and every read succeeds whether the policy
// is right, wrong, or absent. Both tenants' rows exist for all three.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// applyIdempotencyKeysRLSMigration applies this fix's migration directly, the
// way applyMemoryProfileRLSMigration does for 061. Redundant with
// testutil.TestDB, which runs the whole migrations/postgres/ directory, and
// kept anyway because it states the dependency locally rather than relying on
// the reader to know it. Idempotent: DROP POLICY IF EXISTS ... CREATE POLICY.
func applyIdempotencyKeysRLSMigration(t *testing.T, db *sql.DB) {
	t.Helper()
	path := filepath.Join("..", "migrations", "postgres",
		"083_an_idempotency_key_belongs_to_one_tenant.sql")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if _, err := db.Exec(string(data)); err != nil {
		t.Fatalf("apply %s: %v", path, err)
	}
}

func TestAnIdempotencyKeyBelongsToOneTenant(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)
	defer testutil.CleanupPostgresTestData(t, adminDB)
	applyIdempotencyKeysRLSMigration(t, adminDB)

	ctx := context.Background()
	const tenantA = "f0000000-0000-4000-8000-00000000000a"
	const tenantB = "f0000000-0000-4000-8000-00000000000b"
	stamp := time.Now().UnixNano()
	defA := fmt.Sprintf("idem-rls-a-%d", stamp)
	defB := fmt.Sprintf("idem-rls-b-%d", stamp)
	// ONE key string for both tenants. That collision is the whole subject of
	// migration 010 -- "order-123" chosen by two customers -- and it is also
	// what makes tenant B's row the row the policy has to EXCLUDE rather than
	// one it would have admitted anyway.
	sharedKey := fmt.Sprintf("order-%d", stamp)

	appDB := testutil.OpenPostgresRLSTestDB(t, adminDB)
	defer appDB.Close()
	assertNotSuperuserBypass(t, appDB)

	t.Cleanup(func() {
		bg := context.Background()
		for _, tenant := range []string{tenantA, tenantB} {
			_, _ = adminDB.ExecContext(bg, `DELETE FROM idempotency_keys WHERE tenant_id = $1`, tenant)
			_, _ = adminDB.ExecContext(bg, `DELETE FROM workflow_instances WHERE tenant_id = $1`, tenant)
			_, _ = adminDB.ExecContext(bg, `DELETE FROM workflow_defs WHERE tenant_id = $1`, tenant)
		}
	})

	// workflow_instances carries a foreign key to workflow_defs, so the
	// definitions exist before the runs do. Seeded over the superuser
	// connection: they are the fixture, not the thing under test.
	for tenant, def := range map[string]string{tenantA: defA, tenantB: defB} {
		if _, err := adminDB.ExecContext(ctx,
			`INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
			 VALUES ($1, 1, $2, 1, 0, $3)`,
			def, []byte{0x00, 0x61, 0x73, 0x6d}, tenant); err != nil {
			t.Fatalf("seed workflow_defs(%s): %v", tenant, err)
		}
	}

	// --- The store, over a connection the policy applies to. ---
	//
	// THIS IS THE REGRESSION TEST FOR THE REORDER, and it is a write path, so
	// it fires on an empty table: WITH CHECK is evaluated per row being
	// inserted, unlike USING. Before the reorder the lookup and the INSERT ran
	// with no tenant on their transaction and this fails with
	//
	//	cleat.tenant_id is not set -- tenant context required for RLS-scoped query
	//
	// appDB is neither superuser nor table owner, which is what makes that
	// true. The same call over adminDB passes whatever the ordering is.
	runs := map[string]string{}
	for tenant, def := range map[string]string{tenantA: defA, tenantB: defB} {
		store := NewPostgresStore(appDB).WithTenant(tenant)
		id, existed, err := store.StartNewRunWithOptions(
			ctx, "", def, 1, json.RawMessage(`{}`), sharedKey, tenant, 0, StartOptions{})
		if err != nil {
			t.Fatalf("StartNewRunWithOptions(%s) over a non-superuser connection: %v\n\n"+
				"If this is \"cleat.tenant_id is not set\", startNewRun is reading "+
				"idempotency_keys before establishing the tenant -- which is what "+
				"cleat#1534 reordered and what migration 083's policy makes fatal.",
				tenant, err)
		}
		if existed {
			t.Fatalf("StartNewRunWithOptions(%s) reported the key already existed. Tenant %s is "+
				"the first user of %q; if this is tenant B, it was handed tenant A's run.",
				tenant, tenant, sharedKey)
		}
		runs[tenant] = id
	}
	if runs[tenantA] == runs[tenantB] {
		t.Fatalf("both tenants were given the same run id %s for key %q", runs[tenantA], sharedKey)
	}

	// The fixture the two checks below depend on. Stated as a fraction rather
	// than asserted as "clean": a count taken from a failed seed looks exactly
	// like a count taken after correct filtering.
	var seeded int
	if err := adminDB.QueryRowContext(ctx,
		`SELECT count(*) FROM idempotency_keys WHERE tenant_id IN ($1, $2)`,
		tenantA, tenantB).Scan(&seeded); err != nil {
		t.Fatalf("count seeded keys: %v", err)
	}
	if seeded != 2 {
		t.Fatalf("PRECONDITION FAILED: %d of 2 idempotency keys seeded. Everything below "+
			"passes trivially against a table with nothing in it to exclude.", seeded)
	}

	// --- Layer 1: the POLICY alone, with no tenant predicate anywhere. ---
	conn, err := appDB.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Close()
	setSessionTenant(t, ctx, conn, tenantA)

	var seenDefs []string
	rows, err := conn.QueryContext(ctx, `SELECT def_name FROM idempotency_keys`)
	if err != nil {
		t.Fatalf("select from idempotency_keys with no tenant predicate: %v", err)
	}
	for rows.Next() {
		var name sql.NullString
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		seenDefs = append(seenDefs, name.String)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("iterate: %v", err)
	}
	rows.Close()
	if len(seenDefs) != 1 || seenDefs[0] != defA {
		t.Errorf("a tenant A session selecting from idempotency_keys with NO WHERE tenant_id saw "+
			"%v, want exactly [%s] of the 2 rows present.\n\nThe policy is not filtering this "+
			"query, so the Go predicate is the only layer -- which is the state cleat#1534 "+
			"exists to leave behind.", seenDefs, defA)
	}

	// --- Layer 2: the Go predicate alone, with the policy bypassed. ---
	//
	// adminDB is a superuser connection, so the policy cannot act on it. This
	// is "policy removed" without dropping it, and the lookup's own
	// `AND tenant_id = $2` is all that is left.
	storeA := NewPostgresStore(adminDB).WithTenant(tenantA)
	id, existed, err := storeA.StartNewRunWithOptions(
		ctx, "", defA, 1, json.RawMessage(`{}`), sharedKey, tenantA, 0, StartOptions{})
	if err != nil {
		t.Fatalf("StartNewRunWithOptions(A, same key again): %v", err)
	}
	if !existed {
		t.Fatalf("CONTROL FAILED: tenant A re-presenting its OWN key %q was given a NEW run %s. "+
			"The lookup is finding nothing at all, so the check below cannot distinguish "+
			"isolation from a query that never matches.", sharedKey, id)
	}
	if id != runs[tenantA] {
		t.Errorf("tenant A re-presenting %q was handed run %s, want its own %s (tenant B holds %s). "+
			"Over a superuser connection the policy is bypassed, so the Go-level tenant_id "+
			"filter is not isolating on its own.", sharedKey, id, runs[tenantA], runs[tenantB])
	}

	// --- The TTL sweep still reaches every tenant. ---
	//
	// cmd/cleat-worker's idempotencyCleanupLoop has no tenant predicate and
	// wants none. Under the tenant policy alone it cannot run at all; migration
	// 083 gives it a second policy reached through cleat_sweep, which is
	// migration 077's shape. Expired here rather than waiting out a TTL.
	if _, err := adminDB.ExecContext(ctx,
		`UPDATE idempotency_keys SET expires_at = now() - INTERVAL '1 hour' WHERE tenant_id = $1`,
		tenantB); err != nil {
		t.Fatalf("expire tenant B's key: %v", err)
	}

	sweepConn, err := appDB.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire sweep connection: %v", err)
	}
	defer sweepConn.Close()
	tx, err := sweepConn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin sweep tx: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL ROLE cleat_sweep`); err != nil {
		t.Fatalf("enter cleat_sweep: %v\n\nThe connecting role needs GRANT cleat_sweep ... WITH "+
			"INHERIT FALSE; testutil.SetupPostgresRLSRole grants it and migration 077 grants "+
			"it to cleat_app.", err)
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM idempotency_keys WHERE expires_at < now() AND tenant_id IN ($1, $2)`,
		tenantA, tenantB)
	if err != nil {
		t.Fatalf("the cross-tenant sweep could not run: %v\n\nThis is the statement migration "+
			"083's second policy exists for; a fail-closed policy alone raises here.", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("rows affected: %v", err)
	}
	// Committed, not rolled back: the survivor check below reads on a different
	// connection and would see the table untouched otherwise -- a check that
	// passes for a reason unrelated to the sweep.
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit sweep: %v", err)
	}
	// 1, not 2 and not 0. Tenant A's key is LIVE and must survive -- a sweep
	// that took both would be reaching across tenants correctly and ignoring
	// its own predicate, which `all clean` could not tell apart from this.
	if n != 1 {
		t.Errorf("the sweep deleted %d rows of the 2 present, want 1 -- tenant B's expired key "+
			"and not tenant A's live one", n)
	}
	var survivors []string
	srows, err := adminDB.QueryContext(ctx,
		`SELECT def_name FROM idempotency_keys WHERE tenant_id IN ($1, $2)`, tenantA, tenantB)
	if err != nil {
		t.Fatalf("read survivors: %v", err)
	}
	for srows.Next() {
		var name sql.NullString
		if err := srows.Scan(&name); err != nil {
			srows.Close()
			t.Fatalf("scan survivor: %v", err)
		}
		survivors = append(survivors, name.String)
	}
	if err := srows.Err(); err != nil {
		srows.Close()
		t.Fatalf("iterate survivors: %v", err)
	}
	srows.Close()
	if len(survivors) != 1 || survivors[0] != defA {
		t.Errorf("after the sweep the table holds %v, want exactly [%s]", survivors, defA)
	}
}

// TestTheConcurrentIdempotencyRereadRunsWithTheTenantSet covers the third of
// startNewRun's three idempotency_keys statements, which the test above does
// not reach.
//
// WHY IT NEEDS ITS OWN TEST. The re-read only runs when the INSERT ... ON
// CONFLICT DO NOTHING affects no row -- the concurrent-insert path -- so an
// uncontended start never touches it. Left untested it is the statement most
// likely to have been missed: it is the one that ran on s.db AFTER a
// tx.Rollback(), so it is not covered by the transaction the other two now
// share, and it needed a second one of its own.
//
// HOW THE PATH IS REACHED WITHOUT A RACE. A row whose expires_at has passed is
// invisible to the lookup, which filters `expires_at > now()`, and still
// collides with the INSERT, whose conflict target is (key_hash, tenant_id) and
// says nothing about expiry. So seeding one expired row drives exactly the
// branch two concurrent starts would, deterministically.
//
// WHAT IS ASSERTED, AND WHAT IS NOT. That the re-read RUNS -- that it reaches
// the database and comes back with an ordinary result rather than
// "cleat.tenant_id is not set". It is deliberately not asserted that the call
// SUCCEEDS: with the key's only row expired, the re-read's own `expires_at >
// now()` filter matches nothing and the caller gets sql.ErrNoRows. That is
// this repo's existing behaviour on an expired-but-unswept key and is
// untouched here; it is filed separately rather than fixed in a PR about RLS.
func TestTheConcurrentIdempotencyRereadRunsWithTheTenantSet(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)
	defer testutil.CleanupPostgresTestData(t, adminDB)
	applyIdempotencyKeysRLSMigration(t, adminDB)

	ctx := context.Background()
	const tenant = "f0000000-0000-4000-8000-00000000000c"
	stamp := time.Now().UnixNano()
	def := fmt.Sprintf("idem-reread-%d", stamp)
	key := fmt.Sprintf("order-reread-%d", stamp)

	appDB := testutil.OpenPostgresRLSTestDB(t, adminDB)
	defer appDB.Close()
	assertNotSuperuserBypass(t, appDB)

	t.Cleanup(func() {
		bg := context.Background()
		_, _ = adminDB.ExecContext(bg, `DELETE FROM idempotency_keys WHERE tenant_id = $1`, tenant)
		_, _ = adminDB.ExecContext(bg, `DELETE FROM workflow_instances WHERE tenant_id = $1`, tenant)
		_, _ = adminDB.ExecContext(bg, `DELETE FROM workflow_defs WHERE tenant_id = $1`, tenant)
	})

	if _, err := adminDB.ExecContext(ctx,
		`INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
		 VALUES ($1, 1, $2, 1, 0, $3)`,
		def, []byte{0x00, 0x61, 0x73, 0x6d}, tenant); err != nil {
		t.Fatalf("seed workflow_defs: %v", err)
	}

	keyHash := sha256.Sum256([]byte(key))
	if _, err := adminDB.ExecContext(ctx,
		`INSERT INTO idempotency_keys (key_hash, workflow_id, expires_at, tenant_id, def_name)
		 VALUES ($1, $2, now() - INTERVAL '1 hour', $3, $4)`,
		keyHash[:], "wf-already-expired", tenant, def); err != nil {
		t.Fatalf("seed the expired key: %v", err)
	}
	var present int
	if err := adminDB.QueryRowContext(ctx,
		`SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1`, tenant).Scan(&present); err != nil {
		t.Fatalf("count the seeded key: %v", err)
	}
	if present != 1 {
		t.Fatalf("PRECONDITION FAILED: %d of 1 expired keys seeded. With no row to collide "+
			"with, the INSERT succeeds and the re-read this test exists for never runs.", present)
	}

	_, _, err := NewPostgresStore(appDB).WithTenant(tenant).StartNewRunWithOptions(
		ctx, "", def, 1, json.RawMessage(`{}`), key, tenant, 0, StartOptions{})
	if err == nil {
		t.Fatal("PRECONDITION FAILED: the start succeeded, so the INSERT did not conflict and " +
			"the concurrent re-read was never reached. Nothing below was measured.")
	}
	if strings.Contains(err.Error(), "cleat.tenant_id is not set") {
		t.Fatalf("the concurrent re-read ran without the tenant established: %v\n\n"+
			"This is the third of startNewRun's idempotency_keys statements -- the one that "+
			"ran on s.db after tx.Rollback(). cleat#1534 gives it a transaction of its own.", err)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Logf("the re-read reached the database and returned %v (not the RLS raise, which is "+
			"what this test asserts)", err)
	}
}
