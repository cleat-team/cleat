package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3629, owner decision D59, part 2: a published library's name prefix is reserved.
//
//	a tenant's DEPLOY refuses any name under a published library's prefix, at any version
//	an INSTALL refuses when the tenant already holds its OWN name under the prefix, at any version
//
// Measured in #3629 on PostgreSQL and SQL Server: without these, a tenant's own X@2 shadows an
// installed X@1 (and a library X@3 shadows a tenant's own X@1), because a versionless start runs
// the highest version whatever its origin. The tests run on real databases through the real stores.
//
// Names are compared IN SQL with each database's own equality, so the dialects differ on purpose and
// the difference is pinned here: SQL Server compares names case-insensitively (CI_AS), PostgreSQL
// does not. `ACME.checkout` is the same workflow as `acme.checkout` on SQL Server and a different one
// on PostgreSQL, so it is refused on the first and accepted on the second.

const (
	pfxTenantA = "f3000000-0000-4000-8000-0000000036a0"
	pfxTenantB = "f3000000-0000-4000-8000-0000000036b0"
	pfxTenantC = "f3000000-0000-4000-8000-0000000036c0"
)

type prefixCase struct {
	rig     *libInstallRig
	ctx     context.Context
	tenants []string
	// pub is a connection as a member of the library publisher role, opened once per case.
	pub      *sql.DB
	tdialect testutil.Dialect
}

func newPrefixCase(t *testing.T, rig *libInstallRig) *prefixCase {
	t.Helper()
	c := &prefixCase{rig: rig, ctx: context.Background(), tenants: []string{pfxTenantA, pfxTenantB, pfxTenantC}}
	// libMember declares slack-notify, and an install is refused for an unregistered plugin (cleat#3600).
	rig.registerPlugins(t, "slack-notify")
	return c
}

// library returns a fresh library name with lower-case letters and an underscore, so a case-folding
// comparison and a LIKE wildcard would both show up, and registers its cleanup.
func (c *prefixCase) library(t *testing.T, tag string) string {
	t.Helper()
	lib := fmt.Sprintf("pfx_%s%d", tag, time.Now().UnixNano()%1_000_000_000)
	c.rig.cleanup(c.tenants, lib)
	t.Cleanup(func() { c.rig.cleanup(c.tenants, lib) })
	return lib
}

// publish makes the library exist the way a publish leaves it: members, a bundle, and its listing.
// The members are written straight to the tables (as the install tests do) so these cases test the
// refusals and not publish validation, which has its own tests.
func (c *prefixCase) publish(t *testing.T, lib string, members ...libMember) {
	t.Helper()
	c.publishBundle(t, lib, "1.0.0", members, members)
}

// publishBundle writes a bundle whose members are `listed`, seeding only the `fresh` ones: a later
// bundle version lists members an earlier one already seeded.
func (c *prefixCase) publishBundle(t *testing.T, lib, bundleVersion string, fresh, listed []libMember) {
	t.Helper()
	c.rig.exec(t, "", `INSERT INTO library.bundles (library, bundle_version, published_by) VALUES (?, ?, 'test')`, lib, bundleVersion)
	for _, m := range fresh {
		c.rig.seedMember(t, lib, m)
	}
	for _, m := range listed {
		c.rig.exec(t, "", `INSERT INTO library.bundle_members (library, bundle_version, name, version) VALUES (?, ?, ?, ?)`,
			lib, bundleVersion, m.name, m.version)
	}
}

func (c *prefixCase) deploy(tenant, name string, version int) error {
	return c.rig.store(tenant).DeployWorkflowDef(c.ctx, &WorkflowDef{Name: name, Version: version, WASMBytes: []byte("own:" + name), ABIVersion: 2})
}

func (c *prefixCase) install(tenant, lib string) error { return c.installVersion(tenant, lib, "1.0.0") }

func (c *prefixCase) installVersion(tenant, lib, bundleVersion string) error {
	_, err := c.rig.store(tenant).InstallLibraryBundle(c.ctx, InstallLibraryRequest{Library: lib, BundleVersion: bundleVersion, InstalledBy: "test"})
	return err
}

func (c *prefixCase) count(t *testing.T, tenant, where string, args ...any) int {
	t.Helper()
	rows := c.rig.query(t, tenant, `SELECT count(*) AS n FROM workflow_defs WHERE tenant_id = ? AND `+where, append([]any{tenant}, args...)...)
	n, err := strconv.Atoi(rows[0]["n"])
	if err != nil {
		t.Fatalf("UNMEASURED: reading a row count: %v (%v)", err, rows)
	}
	return n
}

func (c *prefixCase) ownRows(t *testing.T, tenant string) int {
	return c.count(t, tenant, `library IS NULL`)
}

func (c *prefixCase) installedRows(t *testing.T, tenant, lib string) int {
	return c.count(t, tenant, `library = ?`, lib)
}

// eachPrefixRig runs body once per dialect, building that dialect's rig INSIDE the subtest so a
// dialect with no database configured skips only itself.
func eachPrefixRig(t *testing.T, body func(t *testing.T, c *prefixCase)) {
	t.Helper()
	for _, d := range []struct {
		name string
		mk   func(*testing.T) *libInstallRig
		t    testutil.Dialect
	}{
		{"postgres", newPostgresLibRig, testutil.DialectPostgres},
		{"sqlserver", newMSSQLLibRig, testutil.DialectMSSQL},
	} {
		t.Run(d.name, func(t *testing.T) {
			c := newPrefixCase(t, d.mk(t))
			c.tdialect = d.t
			body(t, c)
		})
	}
}

func TestADeployUnderAPublishedLibraryPrefixIsRefused(t *testing.T) {
	eachPrefixRig(t, func(t *testing.T, c *prefixCase) {
		rig := c.rig
		lib := c.library(t, "dep")
		c.publish(t, lib, libMember{name: lib + ".review", version: 1, exposure: "auth"})

		t.Run("any name under the prefix, at any version, for any tenant", func(t *testing.T) {
			for _, tc := range []struct {
				tenant, name string
				version      int
			}{
				{pfxTenantA, lib + ".review", 9},   // the library's own name at a version it does not have
				{pfxTenantA, lib + ".brandnew", 1}, // a name the library does not contain
				{pfxTenantB, lib + ".review", 1},   // another tenant: the prefix is global
				{pfxTenantA, lib + ".a.b.c", 2},    // deeper under the prefix
			} {
				err := c.deploy(tc.tenant, tc.name, tc.version)
				var pe *WorkflowNameUnderLibraryPrefixError
				if !errors.Is(err, ErrWorkflowNameUnderLibraryPrefix) || !errors.As(err, &pe) {
					t.Errorf("deploy %s@%d: got %v, want a refusal under the prefix of %q", tc.name, tc.version, err, lib)
					continue
				}
				if pe.Library != lib || !strings.Contains(err.Error(), lib+".") || !strings.Contains(err.Error(), "D59") {
					t.Errorf("deploy %s@%d: the refusal does not name the library and the prefix: %v", tc.name, tc.version, err)
				}
			}
			for _, tenant := range []string{pfxTenantA, pfxTenantB} {
				if n := c.ownRows(t, tenant); n != 0 {
					t.Errorf("tenant %s holds %d row(s) of its own after refused deploys, want 0: a refusal must write nothing", tenant, n)
				}
			}
		})

		t.Run("controls: what is NOT under the prefix still deploys", func(t *testing.T) {
			wildcard := strings.Replace(lib, "_", "X", 1) // `_` is a LIKE wildcard: a LIKE check would refuse this
			for _, name := range []string{
				"unrelated",          // nothing to do with the library
				lib,                  // the library name alone, no dot
				lib + "x.review",     // extends the library name, so the prefix is not a prefix of it
				wildcard + ".review", // differs from the prefix only where a LIKE wildcard would match
			} {
				if err := c.deploy(pfxTenantC, name, 1); err != nil {
					t.Errorf("deploy %q was refused, but it is not under %q: %v", name, lib+".", err)
				}
			}
		})

		t.Run("before the library is published the same name deploys: the refusal tracks publication", func(t *testing.T) {
			unpublished := c.library(t, "unpub")
			if err := c.deploy(pfxTenantC, unpublished+".review", 1); err != nil {
				t.Errorf("deploy under the prefix of a library nobody has published was refused: %v", err)
			}
		})

		// The dialects differ on purpose, and this is where the difference is a decision: the check
		// asks the database whether two names are the same, and SQL Server says yes.
		t.Run("pinned divergence: the prefix in another case", func(t *testing.T) {
			upper := strings.ToUpper(lib) + ".review"
			err := c.deploy(pfxTenantB, upper, 2)
			switch rig.dialect {
			case DialectMSSQL:
				if !errors.Is(err, ErrWorkflowNameUnderLibraryPrefix) {
					t.Errorf("SQL Server: deploy %q beside %q got %v; the database treats them as the same name, so it must be refused", upper, lib+".review", err)
				}
			default:
				if err != nil {
					t.Errorf("PostgreSQL: deploy %q was refused (%v), but PostgreSQL treats it as a different workflow from %q and it cannot shadow it", upper, err, lib+".review")
				}
			}
		})
	})
}

func TestAnInstallIsRefusedWhenTheTenantHoldsItsOwnNamesUnderThePrefix(t *testing.T) {
	eachPrefixRig(t, func(t *testing.T, c *prefixCase) {
		rig := c.rig
		lib := c.library(t, "inst")

		// The reverse order measured in #3629: the tenant's own name exists BEFORE the library does.
		if err := c.deploy(pfxTenantA, lib+".mine", 1); err != nil {
			t.Fatalf("UNMEASURED: seeding the tenant's own name before publication: %v", err)
		}
		c.publish(t, lib, libMember{name: lib + ".review", version: 1, exposure: "auth"})

		err := c.install(pfxTenantA, lib)
		var he *LibraryPrefixHeldError
		if !errors.As(err, &he) || !errors.Is(err, ErrLibraryDefinitionCollision) {
			t.Fatalf("install over the tenant's own %s.mine: got %v, want a LibraryPrefixHeldError that matches ErrLibraryDefinitionCollision", lib, err)
		}
		if he.Library != lib || len(he.Names) != 1 || he.Names[0] != lib+".mine" || !strings.Contains(err.Error(), lib+".mine") {
			t.Errorf("the refusal does not name the library and the names in the way: %+v / %v", he, err)
		}
		if n := c.installedRows(t, pfxTenantA, lib); n != 0 {
			t.Errorf("a refused install left %d installed row(s), want 0", n)
		}
		installs := rig.query(t, pfxTenantA, `SELECT count(*) AS n FROM tenant_library_installs WHERE tenant_id = ? AND library = ?`, pfxTenantA, lib)
		if installs[0]["n"] != "0" {
			t.Errorf("a refused install recorded %s install row(s), want 0", installs[0]["n"])
		}

		t.Run("grandfathered: the tenant's own row keeps running and cannot move forward", func(t *testing.T) {
			b, err := rig.store(pfxTenantA).LoadWASM(c.ctx, lib+".mine", 1)
			if err != nil || string(b) != "own:"+lib+".mine" {
				t.Errorf("the tenant's own %s.mine@1 no longer loads: %q, %v", lib, b, err)
			}
			if err := c.deploy(pfxTenantA, lib+".mine", 2); !errors.Is(err, ErrWorkflowNameUnderLibraryPrefix) {
				t.Errorf("redeploying %s.mine at a new version: got %v, want it refused now the library is published", lib, err)
			}
		})

		t.Run("controls: the refusal is per tenant and per name", func(t *testing.T) {
			if err := c.install(pfxTenantB, lib); err != nil {
				t.Errorf("a tenant with nothing under the prefix could not install: %v", err)
			}
			if err := c.deploy(pfxTenantC, lib+"x.mine", 1); err != nil {
				t.Fatalf("UNMEASURED: seeding a name that only resembles the prefix: %v", err)
			}
			if err := c.install(pfxTenantC, lib); err != nil {
				t.Errorf("a tenant whose own name merely resembles the prefix (%sx.mine) was refused: %v", lib, err)
			}
		})

		// Pinned divergence, the install half: SQL Server holds `LIB.mine` to be the same name.
		t.Run("pinned divergence: the tenant's own name in another case", func(t *testing.T) {
			lib2 := c.library(t, "inst2")
			if err := c.deploy(pfxTenantC, strings.ToUpper(lib2)+".mine", 1); err != nil {
				t.Fatalf("UNMEASURED: seeding the upper-case own name: %v", err)
			}
			c.publish(t, lib2, libMember{name: lib2 + ".review", version: 1, exposure: "auth"})
			err := c.install(pfxTenantC, lib2)
			switch rig.dialect {
			case DialectMSSQL:
				if !errors.Is(err, ErrLibraryDefinitionCollision) {
					t.Errorf("SQL Server: install beside the tenant's %s.mine got %v; the database treats it as the same name, so it must be refused", strings.ToUpper(lib2), err)
				}
			default:
				if err != nil {
					t.Errorf("PostgreSQL: install was refused (%v), but %s.mine is a different workflow there", err, strings.ToUpper(lib2))
				}
			}
		})
	})
}

// waitForALockWait polls until some session is WAITING for an application-level lock in this
// database, which is how these tests know the other side of a race is blocked without sleeping a
// guessed duration. The bound is only the failure case.
func waitForALockWait(t *testing.T, c *prefixCase) {
	t.Helper()
	q := `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`
	if c.rig.dialect == DialectMSSQL {
		q = `SELECT count(*) FROM sys.dm_tran_locks WHERE resource_type = 'APPLICATION' AND request_status = 'WAIT' AND resource_database_id = DB_ID()`
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := c.rig.admin.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("UNMEASURED: reading the lock table: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("UNMEASURED: no session ever waited on the library-names lock, so nothing here was shown to block")
}

// heldTx opens a transaction on the admin connection scoped to a tenant, as the stores do.
func (c *prefixCase) heldTx(t *testing.T, tenant string) (*sql.Tx, func()) {
	t.Helper()
	if c.rig.dialect == DialectMSSQL {
		conn, err := c.rig.admin.Conn(c.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if tenant != "" { // an empty value is refused by sp_set_session_context, and the publisher is unscoped
			if _, err := conn.ExecContext(c.ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, tenant); err != nil {
				conn.Close()
				t.Fatalf("scoping the connection: %v", err)
			}
		}
		tx, err := conn.BeginTx(c.ctx, nil)
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		return tx, func() { _ = tx.Rollback(); conn.Close() }
	}
	tx, err := c.rig.admin.BeginTx(c.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tenant != "" {
		if _, err := tx.ExecContext(c.ctx, `SELECT set_config('cleat.tenant_id', $1, true)`, tenant); err != nil {
			_ = tx.Rollback()
			t.Fatalf("scoping the transaction: %v", err)
		}
	}
	return tx, func() { _ = tx.Rollback() }
}

// deployInTx runs a deploy's check and then its insert inside tx, and does NOT commit: the state a
// real deploy is in between its prefix check and its commit.
func (c *prefixCase) deployInTx(t *testing.T, tx *sql.Tx, tenant, name string, version int) error {
	t.Helper()
	def := &WorkflowDef{Name: name, Version: version, WASMBytes: []byte("own:" + name), ABIVersion: 2}
	var err error
	ins := `INSERT INTO workflow_defs (tenant_id, name, version, wasm_bytes, abi_version) VALUES ($1, $2, $3, $4, 2)`
	if c.rig.dialect == DialectMSSQL {
		store, ok := c.rig.store(tenant).(*MSSQLStore)
		if !ok {
			t.Fatalf("UNMEASURED: the SQL Server rig's store is %T", c.rig.store(tenant))
		}
		err = store.checkWorkflowDefReplacement(c.ctx, tx, def)
		ins = `INSERT INTO workflow_defs (tenant_id, name, version, wasm_bytes, abi_version) VALUES (@p1, @p2, @p3, @p4, 2)`
	} else {
		err = CheckWorkflowDefReplacementPostgres(c.ctx, tx, tenant, name, version, def.WASMBytes, false)
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(c.ctx, ins, tenant, name, version, def.WASMBytes); err != nil {
		t.Fatalf("UNMEASURED: the held deploy's insert failed: %v", err)
	}
	return nil
}

func (c *prefixCase) publishAsOperator(t *testing.T, lib string) error {
	if c.pub == nil {
		t.Fatalf("UNMEASURED: openPublisher was not called before publishing")
	}
	_, err := PublishLibraryBundle(c.ctx, c.pub, c.rig.dialect, inLibrary(PublishLibraryRequest{
		Library: lib, BundleVersion: "1.0.0", Members: []PublishLibraryMember{pubMember("review", 1, "r", nil)},
	}))
	return err
}

func (c *prefixCase) openPublisher(t *testing.T) {
	t.Helper()
	c.pub = testutil.OpenLibraryLoginDB(t, c.rig.admin, c.tdialect, true)
}

// The window #3629 found by tracing the interleaving: a deploy passes its check before the library
// exists, the library is published and committed, an install checks the tenant's own names and sees
// nothing (the deploy has not committed), and then the deploy commits. The lock closes it: a publish
// cannot commit while a deploy that passed its check is open.
//
// These two cases hold one side open and watch the other BLOCK, so they test the mechanism and not
// luck. Remove the lock and the first one fails because the publish does not wait.
func TestAPublishWaitsForADeployThatHasPassedItsCheck(t *testing.T) {
	eachPrefixRig(t, func(t *testing.T, c *prefixCase) {
		lib := c.library(t, "lk1")
		c.openPublisher(t)
		tx, closeTx := c.heldTx(t, pfxTenantA)
		defer closeTx()
		if err := c.deployInTx(t, tx, pfxTenantA, lib+".held", 1); err != nil {
			t.Fatalf("UNMEASURED: the deploy was refused before the library existed: %v", err)
		}

		done := make(chan error, 1)
		go func() { done <- c.publishAsOperator(t, lib) }()
		waitForALockWait(t, c)
		select {
		case err := <-done:
			t.Fatalf("the publish finished (%v) while a deploy that had passed its check was still open: nothing made it wait", err)
		default:
		}

		if err := tx.Commit(); err != nil {
			t.Fatalf("committing the held deploy: %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("the publish failed after the deploy committed: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the publish never finished after the deploy committed")
		}

		// Now the install sees the committed own name and refuses: the window is closed.
		err := c.install(pfxTenantA, lib)
		var he *LibraryPrefixHeldError
		if !errors.As(err, &he) {
			t.Fatalf("install after the deploy committed: got %v, want it refused for the tenant's own %s.held", err, lib)
		}
		if n := c.installedRows(t, pfxTenantA, lib); n != 0 {
			t.Errorf("%d installed row(s) beside the tenant's own name, want 0", n)
		}
	})
}

func TestADeployWaitsForAPublishInFlightAndIsThenRefused(t *testing.T) {
	eachPrefixRig(t, func(t *testing.T, c *prefixCase) {
		rig := c.rig
		lib := c.library(t, "lk2")
		tx, closeTx := c.heldTx(t, "")
		defer closeTx()
		if err := lockLibraryNames(c.ctx, tx, rig.dialect, true); err != nil {
			t.Fatalf("UNMEASURED: taking the exclusive lock: %v", err)
		}
		ins := `INSERT INTO library.bundles (library, bundle_version, published_by) VALUES ($1, '1.0.0', 'test')`
		if rig.dialect == DialectMSSQL {
			ins = `INSERT INTO library.bundles (library, bundle_version, published_by) VALUES (@p1, N'1.0.0', N'test')`
		}
		if _, err := tx.ExecContext(c.ctx, ins, lib); err != nil {
			t.Fatalf("UNMEASURED: inserting the bundle in the held publish: %v", err)
		}

		done := make(chan error, 1)
		go func() { done <- c.deploy(pfxTenantA, lib+".late", 1) }()
		waitForALockWait(t, c)
		select {
		case err := <-done:
			t.Fatalf("a deploy finished (%v) while a publish held the library-names lock: it did not wait", err)
		default:
		}

		if err := tx.Commit(); err != nil {
			t.Fatalf("committing the held publish: %v", err)
		}
		select {
		case err := <-done:
			if !errors.Is(err, ErrWorkflowNameUnderLibraryPrefix) {
				t.Fatalf("the deploy that waited: got %v, want it refused now the library is published", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the deploy never finished after the publish committed")
		}
		if n := c.ownRows(t, pfxTenantA); n != 0 {
			t.Errorf("the refused deploy left %d row(s), want 0", n)
		}
	})
}

// The second line of defence: the same three actors released together, over and over. After each
// round the tenant must not hold both its own `<lib>.x` and rows installed from the library. Run it
// with -count=30 on head and on base (clew#411): a window this narrow is not found by one run.
func TestARaceBetweenDeployAndPublishNeverLeavesBothOrigins(t *testing.T) {
	const rounds = 6
	eachPrefixRig(t, func(t *testing.T, c *prefixCase) {
		c.openPublisher(t)
		for i := 0; i < rounds; i++ {
			lib := c.library(t, "race"+strconv.Itoa(i))
			var wg sync.WaitGroup
			start := make(chan struct{})
			var deployErr, publishErr error
			wg.Add(2)
			go func() { defer wg.Done(); <-start; deployErr = c.deploy(pfxTenantA, lib+".x", 1) }()
			go func() { defer wg.Done(); <-start; publishErr = c.publishAsOperator(t, lib) }()
			close(start)
			wg.Wait()
			if publishErr != nil {
				t.Fatalf("round %d: the publish failed: %v", i, publishErr)
			}
			if deployErr != nil && !errors.Is(deployErr, ErrWorkflowNameUnderLibraryPrefix) {
				t.Fatalf("round %d: the deploy failed for another reason: %v", i, deployErr)
			}
			installErr := c.install(pfxTenantA, lib)
			own := c.count(t, pfxTenantA, `library IS NULL AND name = ?`, lib+".x")
			installed := c.installedRows(t, pfxTenantA, lib)
			if own > 0 && installed > 0 {
				t.Fatalf("round %d: the tenant holds its own %s.x AND %d row(s) installed from the library (deploy=%v install=%v): shadowing",
					i, lib, installed, deployErr, installErr)
			}
			if own == 0 && installed == 0 {
				t.Fatalf("round %d: neither the tenant's name nor the install exists (deploy=%v install=%v): the round measured nothing", i, deployErr, installErr)
			}
		}
	})
}

// The rows an install itself wrote sit under the library's prefix, because every member is named
// "<library>.<name>". The install's own-names check must look at the tenant's OWN rows only, or the
// second install of a library, and every upgrade, refuses itself. Nothing else in the suite names
// members under their prefix, so a check that counted installed rows too stayed green until this.
func TestAReinstallAndAnUpgradeAreNotRefusedByTheirOwnRows(t *testing.T) {
	eachPrefixRig(t, func(t *testing.T, c *prefixCase) {
		lib := c.library(t, "again")
		v1 := libMember{name: lib + ".review", version: 1, exposure: "auth"}
		v2 := libMember{name: lib + ".review", version: 2, exposure: "auth", variant: 1}
		c.publish(t, lib, v1)
		c.publishBundle(t, lib, "2.0.0", []libMember{v2}, []libMember{v2}) // a bundle holds one version of each name

		if err := c.install(pfxTenantA, lib); err != nil {
			t.Fatalf("the first install: %v", err)
		}
		if err := c.install(pfxTenantA, lib); err != nil {
			t.Errorf("reinstalling the bundle the tenant already holds was refused: %v", err)
		}
		if err := c.installVersion(pfxTenantA, lib, "2.0.0"); err != nil {
			t.Errorf("upgrading to the next bundle was refused: %v", err)
		}
		if n := c.installedRows(t, pfxTenantA, lib); n != 2 {
			t.Errorf("after the upgrade the tenant holds %d installed row(s), want 2", n)
		}
		if n := c.ownRows(t, pfxTenantA); n != 0 {
			t.Errorf("the tenant holds %d row(s) of its own, want 0", n)
		}
	})
}

// On a sharded store the deploy is preflighted on every shard before any is written (cleat#3497), and
// the prefix check rides on that: a library published on ONE shard refuses the deploy for all of
// them, and nothing is written anywhere. A check that ran only inside each shard's own write would
// let the shards without the library accept the name first.
func TestADeployUnderALibraryPublishedOnOneShardWritesNothingOnAnyShard(t *testing.T) {
	rs := newRealShards(t, "cleat_shard_defs_prefix", "a", "b")
	ctx := context.Background()
	lib := fmt.Sprintf("shardpfx_%d", time.Now().UnixNano()%1_000_000_000)
	name := lib + ".checkout"
	designated := rs.shardOf(name)
	other := otherThan(designated, len(rs.names))

	// The library exists only on the shard that does NOT hold the designated copy of the name.
	if _, err := rs.dbs[other].Exec(`INSERT INTO library.bundles (library, bundle_version, published_by) VALUES ($1, '1.0.0', 'test')`, lib); err != nil {
		t.Fatalf("UNMEASURED: publishing on shard %q: %v", rs.names[other], err)
	}
	var published int
	if err := rs.dbs[designated].QueryRow(`SELECT count(*) FROM library.bundles WHERE library = $1`, lib).Scan(&published); err != nil || published != 0 {
		t.Fatalf("UNMEASURED: shard %q should not have the library (count %d): %v", rs.names[designated], published, err)
	}

	err := rs.deploy(ctx, name, 1, defBytesA, false)
	var pe *WorkflowNameUnderLibraryPrefixError
	if !errors.Is(err, ErrWorkflowNameUnderLibraryPrefix) || !errors.As(err, &pe) || pe.Library != lib {
		t.Fatalf("deploying %q with %q published on shard %q gave %v, want a refusal under its prefix", name, lib, rs.names[other], err)
	}
	if !strings.Contains(err.Error(), "no shard was written") {
		t.Errorf("the refusal %q does not say no shard was written", err)
	}
	for i := range rs.names {
		var n int
		if err := rs.dbs[i].QueryRow(`SELECT count(*) FROM workflow_defs WHERE name = $1`, name).Scan(&n); err != nil {
			t.Fatalf("UNMEASURED: counting on shard %q: %v", rs.names[i], err)
		}
		if n != 0 {
			t.Errorf("shard %q holds %d row(s) of %q after a refused deploy, want 0", rs.names[i], n, name)
		}
	}
}
