package testutil

// Administrative access for SQL Server test teardown.
//
// IMPROVEMENT-PLAN 3.37 / 2.71. SQL Server applies a security policy to every
// principal -- sysadmin, db_owner and dbo included -- so on a database built
// from the shipped migrations a plain pool cannot see across tenants. Teardown
// that issues DELETE on such a pool matches nothing, reports no error, and
// leaves every row behind. migrations/mssql/001_schema.sql (née 012_admin_role.sql,
// before the SQL Server compaction folded it into the baseline) introduces the
// cleat_admin role as the exemption; this file provisions a member of it for
// the test harness.
//
// The gate is "does this database have security policies", not "does the role
// exist". On the hand-written schema there are no policies, so a plain
// connection already is an administrative one and nothing needs provisioning;
// on the shipped schema there are, and then the absence of the role is a hard
// error rather than something to work around silently. Getting that condition
// backwards would reintroduce the exact defect -- teardown that quietly does
// nothing.

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/migration"
)

const (
	// A login the harness creates on the test server. Not a credential in any
	// meaningful sense -- CLEAT_TEST_MSSQL already carries sa's password in
	// the clear, and this only ever exists on a throwaway test instance. The
	// shipped migration deliberately creates no login at all, leaving that to
	// the deployment (migrations/mssql/001_schema.sql creates the empty
	// cleat_admin role; it was 012_admin_role.sql until the SQL Server
	// compaction folded it into the baseline, and that filename no longer
	// exists).
	mssqlTestAdminLogin    = "cleat_test_admin"
	mssqlTestAdminPassword = "CleatTestAdmin123!"
)

var (
	mssqlAdminMu    sync.Mutex
	mssqlAdminPools = map[string]*sql.DB{}
	// mssqlAdminRefs counts live callers of MSSQLAdminDB per DSN. cleat#2125:
	// applyMSSQLCrossTenantOptIn flips the WHOLE DATABASE's predicate form
	// from 'plain' to 'admin', and nothing used to flip it back -- so the
	// first test in a package (or a whole `go test` process) to call
	// MSSQLAdminDB left every later test, in every later package sharing the
	// same CLEAT_TEST_MSSQL, running against a predicate no default deployment
	// installs. Refcounted rather than restored on the first Cleanup: several
	// tests (including parallel ones) can hold the admin pool at once, and
	// restoring while any of them is still mid-test would pull the form out
	// from under it. Only the caller whose Cleanup drops the count to zero
	// restores 'plain' and evicts the cached pool, so the next caller
	// re-establishes 'admin' from scratch rather than reusing a pool that
	// authenticates fine but now grants nothing (cleat#1541's failure mode).
	//
	// PER-PROCESS ONLY -- see mssqlAdminLockConns below, cleat#2857, for the
	// cross-process half this map cannot provide. Kept as a fast in-process
	// short-circuit (no network round trip) ahead of the authoritative
	// cross-process check; every holder counted here also holds the lock
	// below, so this can only ever agree with it or be less informed, never
	// contradict it.
	mssqlAdminRefs = map[string]int{}
	// mssqlAdminLockConns holds the dedicated, pinned *sql.Conn each baseDSN's
	// FIRST MSSQLAdminDB caller used to acquire mssqlAdminLockResource in
	// SHARED mode, for as long as the cached pool above lives. cleat#2857: the
	// refcount above is per-process, so it cannot tell "no process anywhere
	// still needs 'admin'" from "no process *in this one* does" -- a second
	// `go test` process sharing the same server reads this process's empty
	// refcount and heals the predicate out from under a still-live first
	// process. The lock makes liveness a property of the SERVER, which every
	// process sees the same way, rather than of any one process's memory.
	//
	// Pinned deliberately: database/sql may otherwise hand this physical
	// connection to an unrelated query and give the lock-holder a different
	// one for its next statement, silently dropping the SESSION-scoped lock.
	// Holding the *sql.Conn without ever calling Close() keeps one specific
	// session -- and therefore the lock on it -- alive for exactly the cached
	// pool's lifetime; mssqlReleaseAdminDB's Close() is what lets it go.
	// Verified empirically (not assumed from the docs) before relying on it:
	// a held, unclosed *sql.Conn keeps a SHARED sp_getapplock blocking a
	// separate connection's EXCLUSIVE attempt across an 8s idle gap and
	// across a wholly separate *sql.DB pool (the cross-process stand-in);
	// closing the Conn releases it immediately, confirmed by the next
	// EXCLUSIVE attempt succeeding right after.
	mssqlAdminLockConns = map[string]*sql.Conn{}
	// mssqlAdminLockDBs holds the *sql.DB each baseDSN's mssqlAdminLockConns
	// entry was drawn from. cleat#2899 (G3a): the lock connection is now
	// opened BEFORE the admin pool exists (see MSSQLAdminDB), on a *sql.DB
	// dedicated to the lock alone -- never the caller's own db, which the
	// caller's own `defer teardown()` may close first (the same hazard
	// restoreMSSQLPlainPredicate's doc comment already describes), and never
	// the admin pool itself, since at acquisition time it has not been
	// opened yet. Closed alongside lockConn in mssqlReleaseAdminDB.
	mssqlAdminLockDBs = map[string]*sql.DB{}
)

// mssqlAdminLockResource is the sp_getapplock resource name MSSQLAdminDB and
// selfHealMSSQLAdminPredicate share. App locks are scoped to the connection's
// current database by default, and every connection here is opened against
// baseDSN's own `?database=` -- so a single fixed name is already scoped per
// database; it does not need baseDSN folded into it.
const mssqlAdminLockResource = "cleat_testutil_mssql_admin"

// mssqlTryApplock runs sp_getapplock on conn and returns its return code:
// 0 or 1 mean the lock was granted (immediately, or after waiting); any
// negative value (-1 timeout, -2 cancelled, -3 deadlock victim, -999 a
// parameter or other error) means it was not. Documented return codes, not
// re-derived here: https://learn.microsoft.com/sql/relational-databases/system-stored-procedures/sp-getapplock-transact-sql
func mssqlTryApplock(t *testing.T, ctx context.Context, conn *sql.Conn, mode string, timeoutMs int) int {
	t.Helper()
	var code int
	if err := conn.QueryRowContext(ctx,
		`DECLARE @r INT; EXEC @r = sp_getapplock @Resource=@p1, @LockMode=@p2, @LockOwner='Session', @LockTimeout=@p3; SELECT @r`,
		mssqlAdminLockResource, mode, timeoutMs,
	).Scan(&code); err != nil {
		t.Fatalf("sp_getapplock(%s, timeout=%dms): %v", mode, timeoutMs, err)
	}
	return code
}

// mssqlReleaseApplock releases a lock acquired by mssqlTryApplock, on the
// same connection. Best-effort: logged rather than fatal, because every
// caller of this reaches it only when the connection may be on its way out
// anyway (eviction) or the lock was only ever a probe (the heal's exclusive
// check), and failing a test over a release that didn't matter would be the
// wrong trade.
func mssqlReleaseApplock(t *testing.T, ctx context.Context, conn *sql.Conn) {
	t.Helper()
	if _, err := conn.ExecContext(ctx,
		`EXEC sp_releaseapplock @Resource=@p1, @LockOwner='Session'`, mssqlAdminLockResource,
	); err != nil {
		t.Logf("sp_releaseapplock: %v", err)
	}
}

// mssqlHasCoreSecurityPolicies reports whether this database enforces RLS on
// the CORE tables specifically -- a predicate bound to dbo.fn_tenant_filter,
// the function migration 001 installs and migration 103 (cleat#2205) adds
// BLOCK predicates to. That is deliberately narrower than "any security
// policy exists": plugin/migration.go's applyTenantScopingMSSQL installs its
// own policies, bound to dbo.fn_plugin_tenant_filter, on every plugin table,
// and cleat_admin membership means nothing to that function -- it has no
// IS_ROLEMEMBER branch at all (see CrossTenantConn's doc comment). Counting
// ANY sys.security_policies row, as this used to, made a database with only
// plugin policies applied -- which migrations/mssql/*.sql never reaches, since
// core migrations are what add those -- read as "needs cleat_admin", and
// migration 012 (which creates that role) never runs unless something
// applied the core schema. Measured in cleat#2226's CI (Plugin Migrations
// job): that job's MSSQL database only ever runs plugin.RunMigrations with
// coreMigrations=nil, so it accumulates plugin policies and never
// dbo.fn_tenant_filter or the cleat_admin role -- a fixture that only ever
// touches plugin tables through CrossTenantConn does not need either, and
// must not be made to Fatal over their absence.
func mssqlHasCoreSecurityPolicies(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sys.security_predicates WHERE predicate_definition LIKE N'%fn_tenant_filter%'`,
	).Scan(&n); err != nil {
		t.Fatalf("count core (fn_tenant_filter) security predicates: %v", err)
	}
	return n > 0
}

// MSSQLAdminDB returns a handle that can read and delete across tenants.
//
// On a database with no security policies it returns db unchanged: there is
// nothing to be exempt from, and handing back a second pool would only add a
// connection. On a database that does enforce RLS it provisions a member of
// cleat_admin and returns a pool authenticated as it.
//
// Pools are cached per DSN. Teardown runs once per test and opening a fresh
// pool each time would leave hundreds of them behind over a suite.
func MSSQLAdminDB(t *testing.T, db *sql.DB) *sql.DB {
	t.Helper()
	if !mssqlHasCoreSecurityPolicies(t, db) {
		return db
	}

	baseDSN := os.Getenv("CLEAT_TEST_MSSQL")
	if baseDSN == "" {
		baseDSN = "sqlserver://sa:CleatTest123!@localhost:1433?database=cleat"
	}

	mssqlAdminMu.Lock()
	defer mssqlAdminMu.Unlock()
	if pool, ok := mssqlAdminPools[baseDSN]; ok {
		mssqlAdminRefs[baseDSN]++
		t.Cleanup(func() { mssqlReleaseAdminDB(t, baseDSN) })
		return pool
	}

	requireMSSQLAdminRole(t, db)

	// cleat#2899 (G3a): acquire the cross-process SHARED lock BEFORE the
	// opt-in write below, not after everything else as this used to. With
	// the opt-in going first, there was a window -- provision/Open/Ping and
	// the two verification queries wide -- between this process setting
	// admin.rls_predicate_form='admin' and this process taking SHARED on
	// mssqlAdminLockResource. A concurrent process's selfHealMSSQLAdminPredicate
	// probes EXCLUSIVE(0) and sees nothing holding SHARED in that window, so
	// it heals the predicate back to 'plain' out from under a setup that is
	// still in progress -- measured with the window widened by a 3s sleep
	// after the opt-in, 2/2: this function's own form=='admin' check below
	// then fails with "installed predicate is \"plain\"" (cleat#1541's
	// symptom, reached by a different route). Acquiring SHARED first closes
	// it: a concurrent heal's EXCLUSIVE(0) attempt now blocks on US and skips
	// healing (cleat-review, cleat#2899).
	//
	// On a connection dedicated to this lock's lifetime, not `db` -- the
	// caller's own `defer teardown()` can close db before this function's
	// later t.Cleanup runs (restoreMSSQLPlainPredicate's doc comment
	// describes the identical hazard) -- and not the admin pool, which does
	// not exist yet at this point in the function.
	ctx := context.Background()
	lockDB, err := sql.Open("sqlserver", baseDSN)
	if err != nil {
		t.Fatalf("open the dedicated applock connection: %v", err)
	}
	lockConn, err := lockDB.Conn(ctx)
	if err != nil {
		lockDB.Close()
		t.Fatalf("open the dedicated applock connection: %v", err)
	}
	// Released on any early return below (a Fatalf from this point on calls
	// runtime.Goexit, which still runs deferred functions) -- an orphaned
	// SHARED hold would block every future selfHealMSSQLAdminPredicate probe
	// on this server until the process exits, which is worse than the bug
	// this lock exists to prevent.
	lockEstablished := false
	defer func() {
		if lockEstablished {
			return
		}
		mssqlReleaseApplock(t, ctx, lockConn)
		lockConn.Close()
		lockDB.Close()
	}()
	if code := mssqlTryApplock(t, ctx, lockConn, "Shared", 5000); code < 0 {
		t.Fatalf("sp_getapplock(%q, Shared) returned %d -- a shared lock should never "+
			"conflict with another shared holder, so this is a real failure (timeout, "+
			"cancellation, or a parameter error), not contention", mssqlAdminLockResource, code)
	}

	// Since cleat#1541 the SHIPPED predicate does not mention IS_ROLEMEMBER, so
	// membership on its own grants nothing and this whole path would hand back a
	// pool that deletes silently -- the exact failure the comment below is
	// about, arriving by a route that comment could not anticipate.
	//
	// Test teardown is a legitimate cross-tenant reader: CleanupMSSQLTestData
	// deletes every tenant's rows by design. So the suite opts in, the way a
	// deployment that wants --claim-across-tenants does.
	applyMSSQLCrossTenantOptIn(t, db)
	provisionMSSQLAdminLogin(t, db)

	u, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatalf("parse CLEAT_TEST_MSSQL: %v", err)
	}
	u.User = url.UserPassword(mssqlTestAdminLogin, mssqlTestAdminPassword)
	pool, err := sql.Open("sqlserver", u.String())
	if err != nil {
		t.Fatalf("open the administrative SQL Server connection: %v", err)
	}
	if err := pool.Ping(); err != nil {
		t.Fatalf("ping as %s: %v", mssqlTestAdminLogin, err)
	}

	// Prove it before handing it out. A pool that authenticates but is not
	// actually in the role would delete nothing and report success, which is
	// the failure this whole path exists to remove -- so it is checked here
	// rather than trusted from the GRANT above having not errored.
	var isMember sql.NullInt64
	if err := pool.QueryRow(`SELECT IS_ROLEMEMBER(N'cleat_admin')`).Scan(&isMember); err != nil {
		t.Fatalf("check cleat_admin membership: %v", err)
	}
	if isMember.Int64 != 1 {
		t.Fatalf("%s authenticated but reads IS_ROLEMEMBER('cleat_admin') = %v; "+
			"teardown through this connection would silently delete nothing",
			mssqlTestAdminLogin, isMember)
	}

	// AND that membership buys something. Since cleat#1541 the two questions
	// came apart: a member under the plain predicate reads IS_ROLEMEMBER = 1
	// and still sees zero rows, so the check above passes while teardown
	// deletes nothing -- which is precisely what it was written to prevent,
	// reached by a route that did not exist when it was written.
	var form string
	if err := pool.QueryRow(`SELECT form FROM admin.rls_predicate_form`).Scan(&form); err != nil {
		t.Fatalf("read admin.rls_predicate_form as %s: %v. Migration 075 creates it; "+
			"without it there is no way to tell whether cleat_admin membership grants "+
			"anything", mssqlTestAdminLogin, err)
	}
	if form != "admin" {
		t.Fatalf("%s is a member of cleat_admin but the installed predicate is %q, so "+
			"membership admits nothing and teardown would silently delete nothing "+
			"(cleat#1541). applyMSSQLCrossTenantOptIn should have opted this database in",
			mssqlTestAdminLogin, form)
	}

	// lockConn/lockDB were already acquired above, before the opt-in --
	// nothing left to do here but cache them alongside the pool they now
	// cover, and mark the early-return release defer as no longer needed.
	mssqlAdminPools[baseDSN] = pool
	mssqlAdminLockConns[baseDSN] = lockConn
	mssqlAdminLockDBs[baseDSN] = lockDB
	mssqlAdminRefs[baseDSN]++
	lockEstablished = true
	t.Cleanup(func() { mssqlReleaseAdminDB(t, baseDSN) })
	return pool
}

// mssqlReleaseAdminDB is the Cleanup counterpart of every MSSQLAdminDB call
// that flipped or reused the admin predicate form. When the count for baseDSN
// reaches zero, no live caller IN THIS PROCESS still needs 'admin' -- but
// that is not the same as no live caller anywhere (cleat#2857), so this no
// longer restores 'plain' unconditionally. It releases this process's own
// SHARED hold, then probes EXCLUSIVE with no wait: granted means nothing
// else anywhere still holds SHARED, so it is safe to restore (migration 075
// is idempotent -- see restoreMSSQLPlainPredicate) and the cached pool is
// evicted either way, so the next MSSQLAdminDB call re-provisions and
// re-verifies rather than handing back a pool that now authenticates into a
// predicate it never checked.
//
// cleat#2899 (G2): this used to restore unconditionally right after
// releasing its own SHARED hold, which is the release-side mirror of G3's
// ordering bug -- a different, still-live process's SHARED hold does not
// prevent THIS process from clobbering the predicate out from under it.
// Measured 3/3 plus a control: process B holds MSSQLAdminDB 8-10s, process A
// (a 1s hold, started once B is confirmed live) releases and restores
// 'plain' unconditionally; B, still live, then reads 'plain' instead of
// 'admin'. The EXCLUSIVE(0) probe below is the same check
// selfHealMSSQLAdminPredicate already does on the heal side, applied here on
// the release side, and held THROUGH the restore for the same reason that
// function now holds it through its own heal (G3b): releasing before
// restoring would reopen the identical window for a MSSQLAdminDB caller
// that acquires SHARED in the gap.
//
// Takes baseDSN only, not the caller's plain db -- see
// restoreMSSQLPlainPredicate's own comment for why db cannot be used here:
// every StoreBackend.Setup in this package hands back a teardown the test
// body defers, and that defer closes db before this Cleanup ever runs.
func mssqlReleaseAdminDB(t *testing.T, baseDSN string) {
	t.Helper()
	mssqlAdminMu.Lock()
	defer mssqlAdminMu.Unlock()

	mssqlAdminRefs[baseDSN]--
	if mssqlAdminRefs[baseDSN] > 0 {
		return
	}
	delete(mssqlAdminRefs, baseDSN)

	pool, ok := mssqlAdminPools[baseDSN]
	if !ok {
		// Already restored and evicted by a concurrent last-releaser, or this
		// DSN's database has no security policies (MSSQLAdminDB returned db
		// unchanged and never incremented the refcount in the first place, in
		// which case this function is never registered as a Cleanup at all).
		return
	}
	delete(mssqlAdminPools, baseDSN)

	lockConn, hasLock := mssqlAdminLockConns[baseDSN]
	lockDB := mssqlAdminLockDBs[baseDSN]
	delete(mssqlAdminLockConns, baseDSN)
	delete(mssqlAdminLockDBs, baseDSN)

	if hasLock {
		ctx := context.Background()
		mssqlReleaseApplock(t, ctx, lockConn)

		if code := mssqlTryApplock(t, ctx, lockConn, "Exclusive", 0); code < 0 {
			t.Logf("releasing this process's own hold on %s, but another live caller "+
				"(in this process or another sharing this database server) still needs "+
				"'admin' (sp_getapplock EXCLUSIVE returned %d) -- leaving the predicate "+
				"as-is rather than restoring 'plain' out from under it", mssqlAdminLockResource, code)
		} else {
			restoreMSSQLPlainPredicate(t, baseDSN)
			mssqlReleaseApplock(t, ctx, lockConn)
		}

		if err := lockConn.Close(); err != nil {
			t.Logf("closing the dedicated applock connection: %v", err)
		}
		if lockDB != nil {
			if err := lockDB.Close(); err != nil {
				t.Logf("closing the dedicated applock connection's pool: %v", err)
			}
		}
	} else {
		// Should not happen -- MSSQLAdminDB always acquires the lock before
		// caching a pool as of cleat#2899 -- but fail safe rather than
		// silently dropping the restore this branch existed for before
		// cleat#2857 added the lock at all.
		restoreMSSQLPlainPredicate(t, baseDSN)
	}

	if err := pool.Close(); err != nil {
		t.Logf("closing the administrative SQL Server pool: %v", err)
	}
}

// restoreMSSQLPlainPredicate re-applies
// migrations/mssql/075_the_admin_bypass_is_opt_in.sql, which is idempotent
// and restores exactly what a real, fully-migrated, not-opted-in deployment
// has -- migration.Runner never re-runs an already-applied file, so this
// helper is the only path back to 075's form once a test has opted in.
//
// cleat#2125 briefly pointed this at a since-deleted migration 102 instead,
// which carried a cleat_dispatcher exemption on the same predicate. That
// design (an OR-disjunct added to dbo.fn_tenant_filter) was dropped after
// cleat-review measured it turning index seeks into scans on every table
// sharing the predicate, not just workflow_instances -- see
// plugins/blobstore/background.go's sweepStaleWorkflowRefsMSSQL for the
// per-tenant replacement. 075 was never touched by that migration and never
// needed to be.
//
// Takes baseDSN, not the caller's plain db, and opens its OWN connection --
// store_backends_test.go's mssqlRowDisappearanceReporter already documents
// exactly why, for the identical shape of bug, one comment above where this
// function is called from: "the test's `defer teardown()` closes db first"
// runs BEFORE any t.Cleanup callback, defers in a test body being ordinary
// Go defers that fire as the test function returns, ahead of the testing
// package's own Cleanup machinery. Every StoreBackend.Setup in this package
// returns exactly that shape -- teardown calls db.Close() directly, and the
// test body says `defer teardown()` -- so by the time THIS function's own
// t.Cleanup fires, db is already closed and db.Begin() fails with "sql:
// database is closed" (measured: every registeredBackends-driven mssql test
// in the package failed this way the first time this used db instead).
func restoreMSSQLPlainPredicate(t *testing.T, baseDSN string) {
	t.Helper()
	restoreDB, err := sql.Open("sqlserver", baseDSN)
	if err != nil {
		t.Fatalf("open a connection to restore the plain predicate: %v", err)
	}
	defer restoreDB.Close()

	// Replay the baseline's seed half and its routines half, in that order.
	//
	// Before cleat#2434 this replayed one file,
	// 075_the_admin_bypass_is_opt_in.sql, which carried both things the plain
	// form needs: the MERGE that records admin.rls_predicate_form.form='plain',
	// and the function and policy definitions that implement it. The compaction
	// split those across 002_defaults.sql (the MERGE) and 003_procedures.sql
	// (the definitions), so the restore follows the content rather than the
	// filename. 003 opens by dropping every policy, which is what lets it run
	// against a database already in the opt-in form.
	//
	// 003_procedures.sql is the GENERATED baseline and bundles every routine
	// together, finalize_workflow_status included -- so replaying 003 alone
	// has the side effect of reverting ANY later migration that redefines a
	// routine 003 also defines, not only the RLS predicate this function
	// exists to restore. Any such later file has to be replayed here too, in
	// order, after 003. cleat#3171 (migrations/mssql/013) is the first one
	// since the 2434 rebaseline -- found by this exact revert, mid-suite:
	// a fix landed in 013, the FIRST MSSQL test in the process to touch this
	// path released the admin pool in its teardown, this function replayed
	// 003 alone, and finalize_workflow_status went back to its pre-013 body
	// for every subtest after that one. TestProcedureMigrationListsAreComplete
	// (engine/store_backends_procedures_test.go) catches a routine missing
	// from ITS list; nothing catches one missing from THIS list, because this
	// file has no equivalent test -- read every migration after 003 that
	// redefines a routine 003 defines before trusting this list is complete.
	root := repoRootForMSSQLTestutil(t)
	for _, name := range []string{
		"002_defaults.sql",
		"003_procedures.sql",
		"013_a_promise_resolved_mid_segment_wakes_the_workflow.sql",
	} {
		execMSSQLBatchFile(t, restoreDB, filepath.Join(root, "migrations", "mssql", name))
	}
}

// AdminDB returns the handle a test should use when it needs to see or change
// rows without regard to which tenant owns them -- seeding a fixture for
// another tenant, reading a row back to check what the code under test wrote,
// or deleting one at teardown.
//
// It exists because the three dialects give a test wildly different privileges
// by default, and only one of them says so out loud:
//
//	PostgreSQL  the test role is a superuser, and a superuser bypasses RLS
//	            unconditionally. Every raw read-back in the suite already runs
//	            exempt from the policies, silently.
//	MySQL       has no row-level security. Tenancy is a separate database.
//	SQL Server  applies its security policies to every principal, sa and dbo
//	            included, so the same read-back returns nothing at all.
//
// So a dialect-generic test that wrote through a store and then read the row
// back on its own connection was doing two different things: on PostgreSQL it
// bypassed the fence, and on SQL Server it hit it. Under the hand-written test
// schema that never showed, because that schema had no policies. Built from the
// shipped migrations it shows immediately -- and in both directions. In
// TestCascadeDelete the unscoped DELETE matched no rows, so nothing cascaded;
// three of the five child-table assertions then failed, and the other two
// *passed*, because their rows were still there and the same policy hid them
// from the count.
//
// Returning db unchanged for the two dialects that need nothing keeps the call
// sites free of dialect switches, and keeps the asymmetry documented in one
// place rather than restated at each of them.
func AdminDB(t *testing.T, db *sql.DB, dialect Dialect) *sql.DB {
	t.Helper()
	if dialect == DialectMSSQL {
		return MSSQLAdminDB(t, db)
	}
	return db
}

// requireMSSQLAdminRole fails loudly when the database enforces RLS but has not
// had migration 012 applied. Falling back to the plain pool here is what would
// make teardown silently no-op again.
func requireMSSQLAdminRole(t *testing.T, db *sql.DB) {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sys.database_principals WHERE name = N'cleat_admin' AND type = 'R'`).
		Scan(&n); err != nil {
		t.Fatalf("look up the cleat_admin role: %v", err)
	}
	if n == 0 {
		t.Fatalf("this database enforces row-level security but has no cleat_admin role, " +
			"so nothing can read or delete across tenants.\n" +
			"The role is created empty by migrations/mssql/001_schema.sql (IMPROVEMENT-PLAN 3.37; " +
			"this file was named 012_admin_role.sql before the SQL Server migration compaction " +
			"folded it into the baseline). Drop and recreate the test database so the shipped " +
			"migrations run from scratch.")
	}
}

// provisionMSSQLAdminLogin creates the login, maps it into this database and
// puts it in cleat_admin. Every step tolerates "already exists", because the
// login is server-level and shared by every package that runs concurrently
// against the same instance.
func provisionMSSQLAdminLogin(t *testing.T, db *sql.DB) {
	t.Helper()
	steps := []struct {
		what string
		sql  string
	}{
		{"create login", fmt.Sprintf(
			`IF SUSER_ID('%s') IS NULL
			 CREATE LOGIN [%s] WITH PASSWORD = '%s', CHECK_POLICY = OFF`,
			mssqlTestAdminLogin, mssqlTestAdminLogin, mssqlTestAdminPassword)},
		{"create user", fmt.Sprintf(
			`IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'%s')
			 CREATE USER [%s] FOR LOGIN [%s]`,
			mssqlTestAdminLogin, mssqlTestAdminLogin, mssqlTestAdminLogin)},
		// Teardown needs to read sys.tables as well as delete, and metadata
		// visibility follows object permissions.
		{"grant DML", fmt.Sprintf(
			`GRANT SELECT, INSERT, UPDATE, DELETE ON SCHEMA::dbo TO [%s]`, mssqlTestAdminLogin)},
		{"grant DML on admin schema", fmt.Sprintf(
			`GRANT SELECT, INSERT, UPDATE, DELETE ON SCHEMA::admin TO [%s]`, mssqlTestAdminLogin)},
		{"grant VIEW DEFINITION", fmt.Sprintf(
			`GRANT VIEW DEFINITION ON SCHEMA::dbo TO [%s]`, mssqlTestAdminLogin)},
		{"add to cleat_admin", fmt.Sprintf(
			`ALTER ROLE cleat_admin ADD MEMBER [%s]`, mssqlTestAdminLogin)},
	}
	for _, s := range steps {
		if _, err := db.Exec(s.sql); err != nil {
			// A concurrent package may have won the race between the guard and
			// the statement.
			if isMSSQLAlreadyExists(err) {
				continue
			}
			t.Fatalf("provision the administrative principal (%s): %v", s.what, err)
		}
	}
}

func isMSSQLAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "is already a member") ||
		strings.Contains(msg, "already a member of the role")
}

// applyMSSQLCrossTenantOptIn applies migrations/mssql/optional/cross_tenant_claim.sql.
//
// That file is deliberately outside the auto-applied set (cleat#1541): the
// disjunction it installs costs the index seek on any query that does not carry
// its own tenant predicate, so a deployment opts in only if it wants
// --claim-across-tenants. The TEST SUITE wants it for a different reason --
// CleanupMSSQLTestData deletes across tenants by design -- and that is exactly
// the shape the opt-in exists to serve.
//
// Idempotent, and applied once per database because MSSQLAdminDB caches its
// pool per DSN and calls this before provisioning.
func applyMSSQLCrossTenantOptIn(t *testing.T, db *sql.DB) {
	t.Helper()
	path := filepath.Join(repoRootForMSSQLTestutil(t), "migrations", "mssql", "optional", "cross_tenant_claim.sql")
	execMSSQLBatchFile(t, db, path)
}

// execMSSQLBatchFile runs a .sql file's GO-separated batches over db, in
// order. Shared by applyMSSQLCrossTenantOptIn and restoreMSSQLPlainPredicate,
// which switch dbo.fn_tenant_filter between its two forms.
//
// Uses migration.SplitMSSQL -- the same splitter migration.Runner applies to
// every shipped migration -- rather than a bare strings.Split(raw, "\nGO\n").
// That simpler form is what this function used until cleat#2125: it requires
// an exact "\nGO\n" and 075_the_admin_bypass_is_opt_in.sql's GO lines carry no
// guarantee of that exact spacing, so a batch boundary was missed and the
// #cleat_bound_policies temp table 075 creates in one batch and reads in a
// later one came apart into two separate batches sent as one -- "Invalid
// object name '#cleat_bound_policies'", the temp table having gone out of
// scope with the batch that never actually ended where this function thought
// it did. migration.SplitMSSQL matches GO case-insensitively and tolerates
// trailing whitespace, which is what the real migration Runner has always
// required this exact file to survive.
func execMSSQLBatchFile(t *testing.T, db *sql.DB, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	// A single *sql.Tx, not db.Exec in a loop -- #cleat_bound_policies (this
	// file's own #temp table, and cross_tenant_claim.sql's) is SESSION-scoped,
	// and database/sql's pool does not guarantee two Exec calls on the plain
	// *sql.DB land on the same underlying connection. migration.Runner pins
	// one connection per file for exactly this reason (see its own comment on
	// applyMigration, and 075's header: "The captured policy set survives the
	// batch boundary because it is a #temp table: those live for the session,
	// and migration.Runner.applyMigration runs every batch of a file on one
	// connection inside one transaction"). This function used to loop
	// db.Exec() directly and got away with it only because an otherwise-idle
	// pool tends to hand back its one most-recently-released connection --
	// not a guarantee, and it broke the first time this helper ran 075 from a
	// pool that already had more than one connection open (cleat#2125): batch
	// 1 created and populated the temp table on one connection, a later batch
	// reading it landed on another, and SQL Server reported "Invalid object
	// name '#cleat_bound_policies'" for a table that very much existed --
	// just not on the connection asking.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin transaction for %s: %v", path, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	for _, batch := range migration.SplitMSSQL(string(raw)) {
		if strings.TrimSpace(batch) == "" {
			continue
		}
		if _, err := tx.Exec(batch); err != nil {
			t.Fatalf("applying %s: %v", path, err)
		}
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit %s: %v", path, err)
	}
}

// repoRootForMSSQLTestutil finds the repository root from this package, so the
// migration path does not depend on which package's test binary is running.
func repoRootForMSSQLTestutil(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}
