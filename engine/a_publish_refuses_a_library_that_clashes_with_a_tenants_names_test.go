package engine

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// cleat#3629, owner decision D59-b: a publish refuses when any tenant already holds names under the
// library's prefix, listing each tenant and name, so the operator renames before publishing. This
// replaces the grandfathering part 2 described. Run on real PostgreSQL and SQL Server, and as both
// logins the deploy side uses (the publisher is the same connection either way).
//
// The names are the database's own equality, so the case divergence is pinned here as it is for the
// deploy: owner decision D60 (cleat#3635) makes SQL Server's names case-sensitive, and the case at the
// end is the placeholder #3635 flips.

// registerTenants gives the three test tenants an admin.tenants row, which is what the clash reader
// enumerates tenants from (a PostgreSQL reader whose owner bypasses row-level security needs none, and
// SQL Server's always does). A row that is already there, as the PostgreSQL per-tenant logins make,
// is left alone, and only the rows this made are removed.
func (c *prefixCase) registerTenants(t *testing.T) {
	t.Helper()
	for _, id := range c.tenants {
		id := id
		rows := c.rig.query(t, "", `SELECT count(*) AS n FROM admin.tenants WHERE tenant_id = ?`, id)
		if rows[0]["n"] != "0" {
			continue
		}
		c.rig.exec(t, "", `INSERT INTO admin.tenants (tenant_id, name, display_name) VALUES (?, ?, ?)`, id, "t-"+id, "t-"+id)
		t.Cleanup(func() { c.rig.exec(t, "", `DELETE FROM admin.tenants WHERE tenant_id = ?`, id) })
	}
}

func TestAPublishThatClashesWithATenantsOwnNamesIsRefused(t *testing.T) {
	eachPrefixRig(t, func(t *testing.T, c *prefixCase) {
		c.registerTenants(t)
		c.openPublisher(t)
		lib := c.library(t, "clash")

		// Three tenants, all before the library exists, which is the only way the names can be there.
		mustDeploy := func(tenant, name string, version int) {
			t.Helper()
			if err := c.deploy(tenant, name, version); err != nil {
				t.Fatalf("UNMEASURED: seeding %s@%d for %s: %v", name, version, tenant, err)
			}
		}
		mustDeploy(pfxTenantA, lib+".mine", 1)
		mustDeploy(pfxTenantB, lib+".other", 1)
		mustDeploy(pfxTenantB, lib+".other", 2)
		wildcard := strings.Replace(lib, "_", "X", 1) // a LIKE would match this one
		for _, name := range []string{lib + "x.thing", wildcard + ".thing", lib} {
			mustDeploy(pfxTenantC, name, 1) // none of these is under the prefix
		}

		err := c.publishAsOperator(t, lib)
		if !errors.Is(err, ErrLibraryPublishRefused) {
			t.Fatalf("publishing %q beside tenants' own names: got %v, want ErrLibraryPublishRefused", lib, err)
		}
		msg := err.Error()
		for _, want := range []string{
			"tenant " + pfxTenantA + ": " + lib + ".mine (version 1)",
			"tenant " + pfxTenantB + ": " + lib + ".other (version 1, 2)",
			"D59-b",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("the refusal does not say %q:\n%s", want, msg)
			}
		}
		if strings.Contains(msg, pfxTenantC) {
			t.Errorf("the refusal names tenant C, whose names are not under the prefix:\n%s", msg)
		}
		for _, table := range []string{"bundles", "members", "bundle_members"} {
			rows := c.rig.query(t, "", `SELECT count(*) AS n FROM library.`+table+` WHERE library = ?`, lib)
			if rows[0]["n"] != "0" {
				t.Errorf("a refused publish left %s rows in library.%s, want 0", rows[0]["n"], table)
			}
		}

		t.Run("rename first, then publish succeeds", func(t *testing.T) {
			c.rig.exec(t, pfxTenantA, `DELETE FROM workflow_defs WHERE tenant_id = ?`, pfxTenantA)
			c.rig.exec(t, pfxTenantB, `DELETE FROM workflow_defs WHERE tenant_id = ?`, pfxTenantB)
			if err := c.publishAsOperator(t, lib); err != nil {
				t.Errorf("publishing after the clashing names were removed: %v", err)
			}
		})

		t.Run("control: a library with nothing under its prefix publishes", func(t *testing.T) {
			other := c.library(t, "clear")
			if err := c.publishAsOperator(t, other); err != nil {
				t.Errorf("publishing %q, which no tenant has names under: %v", other, err)
			}
		})

		// Pinned divergence, the publish half: SQL Server holds a tenant's UPPER.prefix names to be under
		// the prefix, PostgreSQL does not. Owner decision D60 (cleat#3635) makes them agree.
		t.Run("pinned divergence: a tenant's names in another case", func(t *testing.T) {
			lib2 := c.library(t, "clashcase")
			upper := strings.ToUpper(lib2) + ".mine"
			mustDeploy(pfxTenantC, upper, 1)
			err := c.publishAsOperator(t, lib2)
			switch c.rig.dialect {
			case DialectMSSQL:
				if !errors.Is(err, ErrLibraryPublishRefused) {
					t.Errorf("SQL Server: publishing %q beside the tenant's %q got %v; the database treats them as the same name, so it must be refused", lib2, upper, err)
				}
			default:
				if err != nil {
					t.Errorf("PostgreSQL: publishing %q was refused (%v), but %q is a different name there", lib2, err, upper)
				}
			}
		})
	})
}

// The reader is narrow: the publisher can ask which tenants hold names under a prefix and cannot read
// a tenant's rows itself. Both halves matter. Without the first the check cannot run as the publisher;
// without the second the reader is not narrow, it is a grant on workflow_defs with extra steps.
func TestThePublisherCanAskForClashesAndCannotReadATenantsRows(t *testing.T) {
	eachPrefixRig(t, func(t *testing.T, c *prefixCase) {
		c.registerTenants(t)
		c.openPublisher(t)
		lib := c.library(t, "narrow")
		if err := c.deploy(pfxTenantA, lib+".mine", 1); err != nil {
			t.Fatalf("UNMEASURED: seeding a name: %v", err)
		}
		if err := c.deploy(pfxTenantB, lib+".yours", 3); err != nil {
			t.Fatalf("UNMEASURED: seeding a name: %v", err)
		}

		clashes, err := libraryPrefixClashes(c.ctx, c.pub, c.rig.dialect, lib)
		if err != nil {
			t.Fatalf("the publisher could not ask which names clash: %v", err)
		}
		got := map[string]string{}
		for _, cl := range clashes {
			vs := make([]string, len(cl.Versions))
			for i, v := range cl.Versions {
				vs[i] = strconv.Itoa(v)
			}
			got[cl.TenantID+"|"+cl.Name] = strings.Join(vs, ",")
		}
		if len(got) != 2 || got[pfxTenantA+"|"+lib+".mine"] != "1" || got[pfxTenantB+"|"+lib+".yours"] != "3" {
			t.Errorf("the reader returned %v, want tenant A's %s.mine (1) and tenant B's %s.yours (3), and nothing else", got, lib, lib)
		}

		var n int
		if err := c.pub.QueryRow(`SELECT count(*) FROM workflow_defs`).Scan(&n); err == nil {
			t.Errorf("the publisher read workflow_defs directly (count %d), so the reader is not what is keeping it out of tenant rows", n)
		}
	})
}

// The rows an install wrote sit under the library's prefix, because every member is named
// "<library>.<name>". The reader must count a tenant's OWN names only, or publishing the next bundle
// version of a library any tenant has installed refuses itself. Nothing else names members under
// their prefix before the library exists, so a reader that counted installed rows stayed green until
// this.
func TestAPublishOfANewBundleVersionIsNotRefusedByTheInstallsOfTheOldOne(t *testing.T) {
	eachPrefixRig(t, func(t *testing.T, c *prefixCase) {
		c.registerTenants(t)
		c.openPublisher(t)
		lib := c.library(t, "nextbundle")
		if err := c.publishBundleAsOperator(t, lib, "1.0.0", 1); err != nil {
			t.Fatalf("the first bundle: %v", err)
		}
		if err := c.install(pfxTenantA, lib); err != nil {
			t.Fatalf("UNMEASURED: installing the first bundle: %v", err)
		}
		if n := c.installedRows(t, pfxTenantA, lib); n != 1 {
			t.Fatalf("UNMEASURED: the tenant holds %d installed row(s), want 1, so there is nothing under the prefix to be miscounted", n)
		}
		if err := c.publishBundleAsOperator(t, lib, "1.1.0", 2); err != nil {
			t.Errorf("publishing the next bundle of a library a tenant has installed was refused: %v", err)
		}
	})
}
