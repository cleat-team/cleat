package engine

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#982's signature is "an operation that needed a workflow row found
// none", and the issue has spent its whole life asking WHO DELETED IT. On SQL
// Server that is one of two questions, and the other one has never been asked:
// a FILTER predicate makes a row INVISIBLE, and invisible reads exactly like
// absent to every caller.
//
// These tests prove the instrument in engine/testutil can produce both answers
// against a real database, so that when it is armed on the four failures a
// negative means something. An instrument nobody has seen report a positive is
// a claim, and this issue already has eleven of those.

func TestTheDeletionAuditNamesWhoRemovedTheRow(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set")
	}
	t.Setenv(testutil.MSSQLRowAuditEnv, "1")

	raw := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, raw)
	admin := testutil.MSSQLAdminDB(t, raw)
	testutil.CleanupMSSQLTestData(t, raw)

	// Installed and read through the RAW pool, not the admin one. MSSQLAdminDB
	// hands back a pool authenticated as a member of cleat_admin, which is a
	// deliberately limited principal: it can read and delete across tenants and
	// it cannot CREATE TABLE. The instrument is DDL, so it belongs to the DSN's
	// own login; the deletes it watches still go through admin, because those
	// are what have to match rows under the security policies.
	testutil.InstallMSSQLRowDisappearanceAudit(t, raw)
	// Scoped like MSSQLBackend.Setup: a prior test's Setup may already have
	// installed the trigger, and the audit table survives it, so the assertion
	// on len(dels) below must start from a clean table rather than from whatever
	// accumulated.
	testutil.ClearMSSQLRowDisappearanceAudit(t, raw)
	t.Cleanup(func() { testutil.UninstallMSSQLRowDisappearanceAudit(t, raw) })

	// Registered here, not only in the tests chasing the four failures: if this
	// one fails, the report is exactly the diagnosis anyone would want, and a
	// reporter that no test in the tree registers is a reporter nobody has
	// watched fire.
	testutil.ReportMSSQLRowDisappearanceOnFailure(t, raw, raw)

	ctx := context.Background()
	store := openMSSQLTenantStore(t, DefaultTenantUUID)

	const def = "audit-probe"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: def, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	// Three rows: two for the privileged path, which can capture statement
	// text, and one for the limited path, which cannot.
	for _, id := range []string{"audit-targeted", "audit-blanket", "audit-limited"} {
		if _, _, err := store.StartNewRun(ctx, id, def, 1, json.RawMessage(`{}`), "",
			DefaultTenantUUID, 0); err != nil {
			t.Fatalf("start %s: %v", id, err)
		}
	}

	// The seed's own known-positive. A fixture that did not land counts zero
	// afterwards for the same reason a correctly-audited one does, and an audit
	// with nothing to audit reports the reassuring answer -- which is exactly
	// how a retention audit on this dialect came out wrong by a third
	// (IMPROVEMENT-PLAN, "UNMEASURED").
	physical, visible, err := testutil.PhysicalAndVisibleMSSQLRowCount(raw, admin, "workflow_instances")
	if err != nil {
		t.Fatalf("count workflow_instances: %v", err)
	}
	if visible != 3 {
		t.Fatalf("seeded 3 workflows and the admin connection sees %d (physical=%d). "+
			"Nothing below measures anything until this is 3.", visible, physical)
	}

	// THE PRIVILEGED PATH. sa can read sys.dm_exec_input_buffer, so the audit
	// gets statement text; the session context is what makes the rows visible
	// to it at all, and it has to be pinned to one connection because
	// go-mssqldb answers database/sql's ResetSession with sp_reset_connection,
	// which clears it.
	conn, err := raw.Conn(ctx)
	if err != nil {
		t.Fatalf("pin a connection: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx,
		`EXEC sp_set_session_context @key=N'tenant_id', @value=@p1`, DefaultTenantUUID); err != nil {
		t.Fatalf("set the tenant session context: %v", err)
	}

	res, err := conn.ExecContext(ctx,
		`DELETE FROM dbo.workflow_instances WHERE id = @p1`, "audit-targeted")
	if err != nil {
		t.Fatalf("targeted delete: %v", err)
	}
	// The trigger must not move this number. Every fence in the claim and
	// release path reads RowsAffected, so an instrument that inflated it would
	// change the behaviour of the thing it is watching.
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("targeted delete reported RowsAffected=%d err=%v, want 1. The audit "+
			"trigger is adding its own rowcount -- SET NOCOUNT ON is missing.", n, err)
	}

	if res, err = conn.ExecContext(ctx, `DELETE FROM dbo.workflow_instances
		WHERE id IN ('audit-blanket')`); err != nil {
		t.Fatalf("second privileged delete: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("second privileged delete removed %d rows, want 1", n)
	}

	// A genuinely unqualified DELETE, the shape CleanupMSSQLTestData has. It
	// takes whatever is left, which after the two above is audit-limited.
	if _, err := conn.ExecContext(ctx, `DELETE FROM dbo.workflow_instances`); err != nil {
		t.Fatalf("blanket delete: %v", err)
	}

	dels, err := testutil.MSSQLDeletions(raw)
	if err != nil {
		t.Fatalf("read the audit: %v", err)
	}
	if len(dels) != 3 {
		t.Fatalf("the audit recorded %d deletion(s), want 3:\n%s",
			len(dels), testutil.MSSQLRowDisappearanceReport(raw, raw))
	}

	byID := map[string]testutil.MSSQLDeletion{}
	for _, d := range dels {
		byID[d.RowID] = d
	}

	// ATTRIBUTION, on every row. This is the half that needs no permission and
	// is the half cleat#982 actually turns on.
	for _, id := range []string{"audit-targeted", "audit-blanket", "audit-limited"} {
		d, ok := byID[id]
		if !ok {
			t.Fatalf("no audit row for %s; got %v", id, auditedRowIDs(byID))
		}
		if d.HostPID == 0 {
			t.Errorf("%s: the audit recorded no host_process_id, so it cannot attribute "+
				"the delete to a process at all", id)
		}
		if d.Foreign() {
			t.Errorf("%s: deleted by pid %d and reported as another process; this "+
				"process issued it", id, d.HostPID)
		}
		if d.BatchID == "" || d.BatchRows < 1 {
			t.Errorf("%s: batch_id=%q batch_rows=%d -- the permission-free half of "+
				"blanket-versus-targeted is missing", id, d.BatchID, d.BatchRows)
		}
	}

	// A DELETE statement is one batch, so the two single-row deletes must not
	// share one. Without this, a constant batch id would satisfy every
	// assertion above and tell a reader nothing.
	if byID["audit-targeted"].BatchID == byID["audit-blanket"].BatchID {
		t.Errorf("two separate DELETE statements were recorded under one batch id %q; "+
			"batch identity cannot then say how many rows one statement took",
			byID["audit-targeted"].BatchID)
	}

	// THE PRIVILEGED VERDICT, both ways round.
	for _, id := range []string{"audit-targeted", "audit-blanket", "audit-limited"} {
		if !byID[id].StmtCaptured() {
			t.Fatalf("%s: no statement text, though sa issued the delete and "+
				"HAS_PERMS_BY_NAME should have admitted it. Without text neither "+
				"verdict below is measured.", id)
		}
	}
	if byID["audit-targeted"].Blanket() {
		t.Errorf("a delete with a WHERE clause was recorded as a blanket wipe: %q",
			byID["audit-targeted"].Stmt)
	}
	if !byID["audit-limited"].Blanket() {
		t.Errorf("an unqualified DELETE FROM was not recorded as a blanket wipe: %q",
			byID["audit-limited"].Stmt)
	}

	// THE LIMITED PATH gets its own known-positive, because the report has a
	// branch for it and a branch nobody has seen fire is a claim. cleat_admin
	// cannot read sys.dm_exec_input_buffer, so its deletes must be recorded
	// WITH attribution and WITHOUT text -- and must not fail.
	if _, _, err := store.StartNewRun(ctx, "audit-by-limited", def, 1,
		json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
		t.Fatalf("start audit-by-limited: %v", err)
	}
	if _, err := admin.Exec(`DELETE FROM dbo.workflow_instances WHERE id = @p1`,
		"audit-by-limited"); err != nil {
		t.Fatalf("delete through the cleat_admin pool: %v\n\n"+
			"The audit trigger must never make a delete fail. A caught error inside a "+
			"trigger still leaves the transaction uncommittable, which is why the "+
			"statement-text read is guarded by HAS_PERMS_BY_NAME rather than TRY/CATCH.", err)
	}
	dels, err = testutil.MSSQLDeletions(raw)
	if err != nil {
		t.Fatalf("re-read the audit: %v", err)
	}
	var limited *testutil.MSSQLDeletion
	for i := range dels {
		if dels[i].RowID == "audit-by-limited" {
			limited = &dels[i]
		}
	}
	if limited == nil {
		t.Fatalf("the cleat_admin pool's delete was not recorded at all")
	}
	if limited.HostPID == 0 {
		t.Error("a delete by a principal without VIEW SERVER PERFORMANCE STATE lost its " +
			"attribution too; only the statement text is supposed to be privileged")
	}
	// Print it. The report IS the deliverable of this instrument, and a test
	// that asserts on it without ever showing it leaves the next reader to
	// imagine what an armed run looks like.
	t.Logf("what an armed run prints:\n%s", testutil.MSSQLRowDisappearanceReport(raw, raw))

	if limited.StmtCaptured() {
		t.Logf("cleat_admin captured statement text (%q), so this database grants it "+
			"VIEW SERVER PERFORMANCE STATE and the UNMEASURED branch of the report is "+
			"not exercised here", trimForLog(limited.Stmt))
	} else {
		report := testutil.MSSQLRowDisappearanceReport(raw, raw)
		if !strings.Contains(report, "shape UNMEASURED") {
			t.Errorf("a deletion with no statement text is not reported as UNMEASURED; "+
				"an unmeasured shape that prints as a verdict is how this issue "+
				"accumulated negatives that meant nothing:\n%s", report)
		}
	}
}

func trimForLog(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

// TestAnInvisibleRowIsNotAMissingRow used to prove the second question could
// be asked: a row inserted through a connection with no tenant session
// context was accepted -- workflow_instances carried a FILTER predicate and
// no BLOCK predicate, so nothing refused the write -- and was then invisible
// to every subsequent read, including the one that wrote it. That produced
// cleat#982's exact symptom with no deleter to find, which is why a deletion
// audit alone would have answered "nothing deleted it" and been right and
// useless.
//
// cleat#2205's migration 103_a_filtered_write_is_a_blocked_write.sql closed
// exactly that door: workflow_instances now carries an AFTER INSERT block
// predicate checking SESSION_CONTEXT('tenant_id'), so the write this test
// used to manufacture a physically-present-but-invisible row is refused
// before it lands, on every run, rather than landing invisibly. There is no
// longer a way to reach the "PRESENT BUT INVISIBLE" state through an INSERT
// on a context-free connection -- this test proves that refusal instead,
// permanently, rather than skipping around a precondition that can no longer
// be constructed. See the disappearance-report mechanism's other proof,
// TestTheDeletionAuditNamesWhoRemovedTheRow, above -- that one still
// exercises the report through a real DELETE, on a properly connectored
// store, which migration 103 does not touch.
func TestAnInvisibleRowIsNotAMissingRow(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set")
	}
	raw := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, raw)
	testutil.CleanupMSSQLTestData(t, raw)

	ctx := context.Background()
	store := openMSSQLTenantStore(t, DefaultTenantUUID)
	const def = "invisible-probe"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: def, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	// raw is a plain sql.Open pool: no connector, so no sp_set_session_context,
	// and go-mssqldb answers database/sql's ResetSession with
	// sp_reset_connection, which clears any that was set. The engine's own
	// pools re-apply it on every recycle (tenantSessionConn.ResetSession,
	// IMPROVEMENT-PLAN 2.71); this one cannot -- which is exactly the
	// connection shape the block predicate exists to refuse a write from.
	const hidden = "wf-invisible-to-its-own-writer"
	_, err := raw.Exec(`INSERT INTO dbo.workflow_instances (id, def_name, def_version, tenant_id)
		VALUES (@p1, @p2, 1, @p3)`, hidden, def, DefaultTenantUUID)
	if err == nil {
		t.Fatal("an INSERT through a context-free connection succeeded -- " +
			"workflow_instances' AFTER INSERT block predicate (cleat#2205, migration 103) " +
			"did not fire, so a row could once again land physically present and invisible " +
			"to its own writer (cleat#982)")
	}
	if !isMSSQLBlockPredicateError(err) {
		t.Fatalf("the INSERT failed, but not with a block-predicate error: %v", err)
	}

	// No row was ever created, so there is nothing left to find and nothing
	// to clean up.
	var found int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM dbo.workflow_instances WHERE id = @p1`,
		hidden).Scan(&found); err != nil {
		t.Fatalf("confirm the refused row does not exist: %v", err)
	}
	if found != 0 {
		t.Fatalf("the INSERT was refused, but the row exists anyway (count=%d) -- "+
			"the refusal and the table disagree", found)
	}
}

func auditedRowIDs(m map[string]testutil.MSSQLDeletion) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
