package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#982. The instrument that tells a deleted row from an invisible one is
// only worth arming if its verdict can come out either way. Both of the
// pairings tried before this one could not: measured 2026-09-16 on a database
// built from the shipped migrations, the raw TestDB handle reads
// physical=1 visible=0 on a HEALTHY database and the admin pool reads
// physical=1 visible=1 on a broken one. A report whose verdict is a constant
// is the "condition that never decides anything" case in CLAUDE.md, and it
// reads as a working instrument for as long as nobody checks.
//
// So this asserts on the TEXT of all three verdicts rather than on a status.
// A status-only assertion is satisfied by a report that lost the reading it is
// named after, because something else supplies the same status.
func TestTheRowDisappearanceVerdictIsNotConstant(t *testing.T) {
	b := &MSSQLBackend{}
	if !b.Enabled() {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}

	raw := testutil.MSSQLTestDB(t)
	t.Cleanup(func() { _ = raw.Close() })
	testutil.SetupMSSQLFullSchema(t, raw)
	applyMSSQLProcedures(t, raw)
	testutil.CleanupMSSQLTestData(t, raw)

	admin := testutil.MSSQLAdminDB(t, raw)
	store := openMSSQLTenantStore(t, DefaultTenantUUID)
	ctx := context.Background()

	readers := testutil.MSSQLRowDisappearanceReaders{
		Stats:    raw,
		Truth:    admin,
		TenantID: DefaultTenantUUID,
		Reader:   store.db,
	}

	seedDef := func(tenant string) {
		t.Helper()
		if _, err := admin.ExecContext(ctx,
			`IF NOT EXISTS (SELECT 1 FROM dbo.workflow_defs
			                WHERE name=N'verdict-wf' AND version=1 AND tenant_id=@p1)
			 INSERT INTO dbo.workflow_defs (name, version, wasm_bytes, tenant_id)
			 VALUES (N'verdict-wf', 1, 0x00, @p1)`, tenant); err != nil {
			t.Fatalf("seed a workflow_def for %s (everything below is UNMEASURED): %v", tenant, err)
		}
	}

	// ---- 1. HEALTHY, one tenant. The verdict must be "consistent". ----
	seedDef(DefaultTenantUUID)
	for i := 0; i < 3; i++ {
		if _, _, err := store.StartNewRun(ctx, "", "verdict-wf", 1,
			json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
			t.Fatalf("seed run %d through the tenant store (UNMEASURED): %v", i, err)
		}
	}
	// KNOWN-POSITIVE on the fixture itself: a store that cannot see its own
	// writes makes every reading below meaningless, and reads as a finding.
	if n := countVisible(t, store.db); n == 0 {
		t.Fatalf("UNMEASURED: the tenant store cannot see the 3 rows it just wrote")
	}
	healthy := testutil.MSSQLRowDisappearanceReportFor(readers)
	reportMustSay(t, healthy, "consistent over this", "a healthy single-tenant store")
	reportMustNotSay(t, healthy, "PRESENT BUT INVISIBLE", "a healthy single-tenant store")

	// ---- 2. ANOTHER TENANT HAS ROWS. Still "consistent": this is the false ----
	// positive the physical-vs-visible pairing could not avoid, because
	// physical counts every tenant's rows and a tenant pool counts one
	// tenant's. Before the tenant-exact reading, this case read physical=5
	// visible=3 and reported a disappearance with nothing wrong.
	const otherTenant = "22222222-2222-2222-2222-222222222222"
	seedDef(otherTenant)
	otherStore := openMSSQLTenantStore(t, otherTenant)
	for i := 0; i < 2; i++ {
		if _, _, err := otherStore.StartNewRun(ctx, "", "verdict-wf", 1,
			json.RawMessage(`{}`), "", otherTenant, 0); err != nil {
			t.Fatalf("seed another tenant's run %d (case 2 UNMEASURED): %v", i, err)
		}
	}
	crowded := testutil.MSSQLRowDisappearanceReportFor(readers)
	reportMustSay(t, crowded, "consistent over this", "another tenant holding rows")
	reportMustNotSay(t, crowded, "PRESENT BUT INVISIBLE", "another tenant holding rows")
	// And the context line must still show the raw disparity, labelled as
	// expected rather than suppressed -- suppressing it would hide the very
	// reading that makes the tenant-exact one necessary.
	reportMustSay(t, crowded, "across ALL tenants", "another tenant holding rows")
	reportMustSay(t, crowded, "EXPECTED if another tenant has rows", "another tenant holding rows")

	// ---- 3. GENUINELY INVISIBLE. The known-positive for the verdict itself. ----
	//
	// THE READER LOSES ITS CONTEXT, not the writer. The first version of this
	// case wrote a row through the raw sa handle and expected it to be hidden,
	// on the strength of "a context-free write is accepted and then invisible".
	// It is not invisible to everyone: that row carries the tenant's own
	// tenant_id, so a pool whose SESSION_CONTEXT holds that tenant matches it
	// and reads it back. The reading was truth=4 visible=4, and the case proved
	// nothing.
	//
	// cleat#982's shape is the other one. A row is written correctly, and a
	// LATER read goes through a connection with no tenant context -- which is
	// what a plain sql.Open pool becomes, because go-mssqldb answers
	// database/sql's ResetSession with sp_reset_connection and that clears
	// SESSION_CONTEXT. The engine's own pools re-apply it on every recycle
	// (tenantSessionConn.ResetSession); a plain one does not. So the plain
	// handle IS the failing reader, modelled exactly.
	lostContext := readers
	lostContext.Reader = raw
	invisible := testutil.MSSQLRowDisappearanceReportFor(lostContext)
	reportMustSay(t, invisible, "PRESENT BUT INVISIBLE", "a reader that lost its tenant context")
	reportMustSay(t, invisible, "TO ITS OWN TENANT", "a reader that lost its tenant context")

	// And the same reader over a tenant with no rows must NOT say that -- this
	// is what the physical-vs-visible pairing could not do, and the reason the
	// verdict is computed over one population instead.
	emptyTenant := readers
	emptyTenant.Reader = raw
	emptyTenant.TenantID = "33333333-3333-3333-3333-333333333333"
	quiet := testutil.MSSQLRowDisappearanceReportFor(emptyTenant)
	reportMustNotSay(t, quiet, "PRESENT BUT INVISIBLE", "a context-less reader over a tenant with no rows")

	// The three verdicts are what makes this a check rather than a claim.
	if healthy == invisible {
		t.Fatalf("the report is identical on a healthy store and on one that cannot see its\n"+
			"own row, so its verdict decides nothing:\n%s", healthy)
	}
}

func countVisible(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM dbo.workflow_instances`).Scan(&n); err != nil {
		t.Fatalf("count through the tenant store: %v", err)
	}
	return n
}

func reportMustSay(t *testing.T, report, want, state string) {
	t.Helper()
	if !strings.Contains(report, want) {
		t.Errorf("with %s the report should contain %q, and does not:\n%s", state, want, report)
	}
}

func reportMustNotSay(t *testing.T, report, unwanted, state string) {
	t.Helper()
	if strings.Contains(report, unwanted) {
		t.Errorf("with %s the report must NOT contain %q, and does:\n%s", state, unwanted, report)
	}
}
