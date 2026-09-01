package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestAssertTenantSetRejectsEmptyStringLikeNull pins the fix in
// migrations/postgres/034_assert_tenant_set_empty_string.sql.
//
// cleat.assert_tenant_set() raises when cleat.tenant_id is missing, so a query
// reaching an RLS-forced table without a tenant context fails loudly. It tested
// only for NULL. But setRLSOnTx sets the GUC with
// set_config('cleat.tenant_id', $1, true) -- is_local, so it is scoped to the
// transaction and reset when it ends. Reset is not undefined: once a session has
// set it even once, current_setting returns the EMPTY STRING rather than NULL on
// that connection, the IS NULL guard misses, and `RETURN tid::uuid` fails with
// "invalid input syntax for type uuid" (22P02) instead of the intended message.
//
// Measured on one pinned connection before the fix:
//
//	fresh connection                      -> cleat.tenant_id is not set (P0001)
//	after one RLS transaction, same conn  -> invalid input syntax for uuid (22P02)
//
// Both correctly refuse the query, so this is a diagnostics defect rather than a
// data-integrity one. It still matters: connections come from a pool, so which
// error a query produces depends on whether that connection happened to serve an
// RLS transaction earlier -- effectively random. Identical bugs arrive wearing
// two different messages, and the 22P02 one does not mention tenants at all,
// reading like malformed input from the caller rather than a query that forgot
// its tenant context.
//
// The test pins a single connection with db.Conn. Without that the pool is free
// to hand out a different, never-used connection for the second query, which
// would produce the NULL path and pass against the unfixed function.
func TestAssertTenantSetRejectsEmptyStringLikeNull(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)
	defer testutil.CleanupPostgresTestData(t, adminDB)

	ctx := context.Background()
	tenant := "eeeeeeee-eeee-4eee-eeee-eeeeeeeeeeee"

	// A row must exist: PostgreSQL evaluates the policy expression per candidate
	// row, so against an empty table assert_tenant_set is never called and this
	// test would pass against any definition of it.
	if _, err := adminDB.Exec(
		`INSERT INTO workflow_defs (name, version, wasm_bytes, min_version, abi_version, tenant_id)
		 VALUES ('ats-def', 1, '\x0061736d', 1, 1, $1)`, tenant); err != nil {
		t.Fatalf("seed workflow_defs: %v", err)
	}
	if _, err := adminDB.Exec(
		`INSERT INTO workflow_instances (id, def_name, def_version, status, input, tenant_id)
		 VALUES ('ats-wf', 'ats-def', 1, 'ready', '{}', $1)`, tenant); err != nil {
		t.Fatalf("seed workflow_instances: %v", err)
	}

	appDB := testutil.OpenPostgresRLSTestDB(t, adminDB)
	defer appDB.Close()

	conn, err := appDB.Conn(ctx)
	if err != nil {
		t.Fatalf("pinning a connection: %v", err)
	}
	defer conn.Close()

	const wantMsg = "cleat.tenant_id is not set"
	probe := `SELECT COUNT(*) FROM workflow_instances WHERE id = 'ats-wf'`

	// Control: on a connection that has never set the GUC, current_setting
	// returns NULL and the original guard already handled it. If this stops
	// holding, the case below proves nothing about the empty string
	// specifically.
	var n int
	err = conn.QueryRowContext(ctx, probe).Scan(&n)
	if err == nil {
		t.Fatalf("a raw read of an RLS-forced table succeeded with no tenant context; "+
			"RLS is not being enforced for this role, so neither half of this test "+
			"means anything (got %d rows)", n)
	}
	if !strings.Contains(err.Error(), wantMsg) {
		t.Fatalf("fresh connection: error = %v, want one containing %q", err, wantMsg)
	}

	// Run one RLS transaction exactly as beginTxWithRLS does, which leaves the
	// GUC defined-but-empty on this connection.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := tx.Exec("SELECT set_config('cleat.tenant_id', $1, true)", tenant); err != nil {
		t.Fatalf("set_config: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Confirm the precondition rather than assuming it: if the GUC came back
	// NULL here, the assertion below would pass for the wrong reason.
	var raw string
	if err := conn.QueryRowContext(ctx,
		`SELECT COALESCE(current_setting('cleat.tenant_id', true), '<NULL>')`).Scan(&raw); err != nil {
		t.Fatalf("reading current_setting: %v", err)
	}
	if raw != "" {
		t.Fatalf("after an RLS transaction current_setting = %q, want \"\"; this test "+
			"exists for the empty-string case and is not exercising it", raw)
	}

	err = conn.QueryRowContext(ctx, probe).Scan(&n)
	if err == nil {
		t.Fatal("a raw read succeeded on a connection whose tenant context had been reset")
	}
	if !strings.Contains(err.Error(), wantMsg) {
		t.Errorf("reused connection: error = %v\nwant one containing %q\n\n"+
			"assert_tenant_set checked only IS NULL, but set_config(..., true) is "+
			"transaction-local, so a connection that has served an RLS transaction "+
			"reports the GUC as \"\" rather than NULL. The guard misses and "+
			"`RETURN tid::uuid` fails with a uuid syntax error that never mentions "+
			"tenants. See migrations/postgres/034_assert_tenant_set_empty_string.sql.",
			err, wantMsg)
	}
}
