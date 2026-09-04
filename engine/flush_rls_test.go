package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// appRolePassword is granted to cleat_app by this test. 005_app_role.sql creates
// the role NOLOGIN with no password deliberately -- a committed credential would
// be the defect that migration exists to prevent -- so the deployment supplies
// one. Here the test is the deployment.
//
// The value is shared verbatim with tests/crash (see its appPassword). Both
// suites need to connect as cleat_app, and cleat_app is one role in one
// database: if they chose different passwords, whichever ran second would
// re-ALTER the role and the other's connections would start failing
// authentication mid-run. `go test ./...` runs packages in parallel, so that is
// the ordinary local command, not an exotic case. Concurrent ALTERs to the same
// value are harmless.
const appRolePassword = "cleat-app-test-pw"

// appRoleDB returns a connection as cleat_app: unprivileged, NOBYPASSRLS, and
// therefore subject to every row-level security policy.
//
// This is the whole point of the test. Every other database test in this package
// connects as the owner, which on PostgreSQL is a superuser and is exempt from
// RLS. That is the §1.10 shape exactly -- "the policies were present, correct,
// tested, and bypassed in practice by every connection that had ever run against
// them" -- and it is why the defect below survived: the code path is only wrong
// on the connection that ships, and no test used it.
func appRoleDB(t *testing.T, owner *sql.DB) *sql.DB {
	t.Helper()

	if _, err := owner.Exec(fmt.Sprintf(
		`ALTER ROLE cleat_app LOGIN PASSWORD '%s'`, appRolePassword)); err != nil {
		t.Fatalf("granting cleat_app a login (is 005_app_role.sql applied?): %v", err)
	}

	dsn := testutil.PostgresTestDSN()
	at := strings.Index(dsn, "@")
	scheme := strings.Index(dsn, "://")
	if at < 0 || scheme < 0 {
		t.Fatalf("cannot derive a cleat_app DSN from the configured test DSN")
	}
	appDSN := dsn[:scheme+3] + "cleat_app:" + appRolePassword + dsn[at:]

	db, err := sql.Open("postgres", appDSN)
	if err != nil {
		t.Fatalf("opening the cleat_app connection: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("cleat_app cannot connect: %v", err)
	}

	// Confirm the premise rather than assuming it. If this connection turned
	// out to be RLS-exempt, the test below would pass for the wrong reason and
	// prove nothing.
	var bypass bool
	if err := db.QueryRow(
		`SELECT rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass); err != nil {
		t.Fatalf("checking rolbypassrls: %v", err)
	}
	if bypass {
		t.Fatalf("cleat_app has BYPASSRLS, so this test cannot observe an RLS " +
			"failure; 005_app_role.sql asserts NOBYPASSRLS on every run")
	}
	return db
}

// TestEventFlushPersistsUnderRLS is the regression test for a defect found by
// building the §2.4 crash harness: on the connection cleat actually ships,
// per-step event flush fails on every event and the failure is swallowed.
//
// engine/flush.go writes through e.db.ExecContext directly. Unlike the store
// methods, which go through beginTxWithRLS, no flush path -- not flush.go, not
// batch_flush.go, not adaptive_flush.go -- ever sets cleat.tenant_id on its
// connection. The RLS policy on event_history is
// `tenant_id = assert_tenant_set()`, and assert_tenant_set() reads that unset
// setting and casts it to uuid, so every INSERT fails with:
//
//	pq: invalid input syntax for type uuid: "" (22P02)
//
// engine/lifecycle.go:179 logs that error and continues. Observed end to end
// against a real worker: three durable calls, three flush failures, and the
// workflow finishing with status "done", a result, and zero rows in
// event_history.
//
// The consequence is not just missing history. A workflow with no persisted
// events has nothing to replay from, so a crash re-executes every side effect
// it has already performed -- which is a strictly larger contract violation than
// the at-least-once-per-step behaviour docs/durable-calls.md documents.
func TestEventFlushPersistsUnderRLS(t *testing.T) {
	owner := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, owner, testutil.DialectPostgres)
	applyPostgresProcedures(t, owner)
	testutil.CleanupPostgresTestData(t, owner)
	defer owner.Close()

	applyAppRoleMigration(t, owner)
	appDB := appRoleDB(t, owner)

	ctx := context.Background()
	store := NewPostgresStore(owner)
	setupTestData(t, store)

	runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
		json.RawMessage(`{}`), "flush-rls-1", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	// An engine configured the way the worker configures it: the tenant of the
	// claimed workflow, and the application connection for flushing.
	eng := NewEngine(nil, nil,
		WithWorkflowID(runID),
		WithTenantID(DefaultTenantUUID),
		WithDB(appDB),
	)

	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "payments",
		Op:        "Charge",
		Request:   `{"amount":100}`,
		Response:  `{"charge_id":"chg-1"}`,
	}

	if err := eng.flushEvent(ctx, runID, rec, ""); err != nil {
		t.Fatalf("flushEvent failed on the connection the worker actually uses: %v\n\n"+
			"No flush path sets cleat.tenant_id, so the RLS policy on "+
			"event_history (tenant_id = assert_tenant_set()) casts an unset "+
			"setting to uuid and rejects every insert. engine/lifecycle.go:179 "+
			"logs this and continues, so a workflow completes with no durable "+
			"history and nothing to replay from.", err)
	}

	var n int
	if err := owner.QueryRow(
		`SELECT count(*) FROM event_history WHERE workflow_id = $1`, runID).Scan(&n); err != nil {
		t.Fatalf("counting events: %v", err)
	}
	if n != 1 {
		t.Errorf("event_history has %d rows for the flushed event, want 1", n)
	}
}

// TestEventFlushSucceedsAsOwner is the control, and it is what makes the test
// above a finding rather than a broken fixture.
//
// The identical flush against the owner connection must succeed. If both fail,
// the problem is the event record or the schema, not the RLS context.
func TestEventFlushSucceedsAsOwner(t *testing.T) {
	owner := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, owner, testutil.DialectPostgres)
	applyPostgresProcedures(t, owner)
	testutil.CleanupPostgresTestData(t, owner)
	defer owner.Close()

	ctx := context.Background()
	store := NewPostgresStore(owner)
	setupTestData(t, store)

	runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
		json.RawMessage(`{}`), "flush-rls-2", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	eng := NewEngine(nil, nil,
		WithWorkflowID(runID),
		WithTenantID(DefaultTenantUUID),
		WithDB(owner),
	)

	rec := EventRecord{
		Step: 0, EventType: EventTypeCall,
		Service: "payments", Op: "Charge",
		Request: `{"amount":100}`, Response: `{"charge_id":"chg-1"}`,
	}
	if err := eng.flushEvent(ctx, runID, rec, ""); err != nil {
		t.Fatalf("flushEvent failed even as the owner, so the RLS test above is "+
			"not isolating RLS: %v", err)
	}
}

// applyAppRoleMigration applies 005_app_role.sql. SetupFullSchema builds the
// tables but does not create the role, and this suite needs it.
//
// # Why this takes a dedicated connection and resets it
//
// 005_app_role.sql line 52 is `SET search_path = public`, which is
// SESSION-scoped, not statement-scoped. Handing the file to db.Exec runs it on
// whichever pooled connection is free and leaves that setting on it, so every
// later query on the same *sql.DB inherits a search_path the caller never asked
// for. Measured 2026-09-03 against a live PostgreSQL: before the file,
// `SHOW search_path` returns `"$user", public`; after it, 40 consecutive
// queries on the same pool all return `public`.
//
// On a database where those two are equivalent -- any DSN whose role has no
// same-named schema -- this is invisible, which is why it has survived. It is
// NOT equivalent in CI: ci.yml's cluster and Tier 1 jobs connect as role
// `cleat`, and `cleat` is also the name of the schema 001_schema.sql creates
// for assert_tenant_set(). The default `"$user", public` resolves to
// `cleat, public` there.
//
// The symptom is `relation "<table>" does not exist` from a test that has
// already built its schema and whose neighbours in the same file pass, because
// only the connection this ran on carries the change. That is a long way from
// the cause and reads like a broken migration; it cost a full diagnosis on
// #638. WORKSTREAM.md records the role/schema collision as an environment
// hazard -- the container's POSTGRES_USER=cleat collides with the cleat
// schema -- and this is the code path that turns it into a test failure.
//
// db.Conn pins one physical connection for the life of the returned handle, so
// RESET ALL is guaranteed to undo the SET on the same session it was set on,
// before Close returns that connection to the pool.
func applyAppRoleMigration(t *testing.T, db *sql.DB) {
	t.Helper()
	body, err := os.ReadFile("../migrations/postgres/005_app_role.sql")
	if err != nil {
		t.Fatalf("reading 005_app_role.sql: %v", err)
	}

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("reserving a connection for 005_app_role.sql: %v", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, string(body)); err != nil {
		t.Fatalf("applying 005_app_role.sql: %v", err)
	}
	// RESET ALL rather than `RESET search_path`: it undoes every session
	// parameter the file set, so a future edit adding a second SET does not
	// reintroduce this silently.
	if _, err := conn.ExecContext(ctx, `RESET ALL`); err != nil {
		t.Fatalf("resetting session state after 005_app_role.sql: %v", err)
	}
}

// TestApplyingTheAppRoleMigrationLeavesThePoolAlone is the regression test for
// the leak described above, and it is deliberately about the HELPER rather than
// about any one test that uses it.
//
// The failure it guards is not "this assertion is wrong" but "an unrelated test
// later in the file cannot see its own tables", which is why a test aimed at
// any single caller would be the wrong shape. It went undiagnosed through a
// full local run because the leak is invisible on any DSN whose role has no
// same-named schema -- so this asserts on search_path directly rather than on
// any table being reachable, which is the only form that fails everywhere
// rather than only in CI.
func TestApplyingTheAppRoleMigrationLeavesThePoolAlone(t *testing.T) {
	owner := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, owner, testutil.DialectPostgres)
	defer owner.Close()

	// One connection, so the check cannot pass by being handed a different,
	// untouched member of the pool.
	owner.SetMaxOpenConns(1)

	var before string
	if err := owner.QueryRow(`SHOW search_path`).Scan(&before); err != nil {
		t.Fatalf("reading search_path before: %v", err)
	}

	applyAppRoleMigration(t, owner)

	var after string
	if err := owner.QueryRow(`SHOW search_path`).Scan(&after); err != nil {
		t.Fatalf("reading search_path after: %v", err)
	}
	if after != before {
		t.Fatalf("applyAppRoleMigration changed the pool's search_path from %q to %q.\n\n"+
			"005_app_role.sql ends with a session-scoped `SET search_path = public`. "+
			"Leaking it means every later query on this *sql.DB runs with a path the "+
			"caller never chose -- harmless where `\"$user\"` names no real schema, and "+
			"fatal in CI, where the role is `cleat` and so is the schema holding the "+
			"tables. Restore the RESET ALL on a pinned db.Conn.", before, after)
	}
}
