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
// The names are the database's own equality, which is exact on both databases since owner decision D60
// (cleat#3635): the case at the end asserts that a name in another case is not in the way anywhere.

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

		// Names are compared exactly on both databases (owner decision D60, cleat#3635), so a tenant's
		// UPPER.prefix name is a different name from the library's and is not in the way. Until D60 SQL
		// Server held it to be the same name and refused here; PostgreSQL never did.
		t.Run("a tenant's names in another case are different names on both databases", func(t *testing.T) {
			lib2 := c.library(t, "clashcase")
			upper := strings.ToUpper(lib2) + ".mine"
			mustDeploy(pfxTenantC, upper, 1)
			if err := c.publishAsOperator(t, lib2); err != nil {
				t.Errorf("%s: publishing %q beside the tenant's %q was refused (%v), but %s compares names exactly, so they are different names", c.rig.dialect, lib2, upper, err, c.rig.dialect)
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

// The reader is executable by the publisher, so the publisher can call it with ANY argument, not only
// the "<library>." the engine sends. Unchecked, "" is a prefix of every name and "z" of every name
// that starts with one, and the role this PR keeps out of tenant rows reads them all through the
// reader (measured by the reviewer: two rows for "", one for "z", on both dialects and both rigs).
// So the reader answers only for a library prefix: the library grammar, at most 128 characters, then
// one ".". The refusal is asserted on its TEXT as well as its status, so a refusal by anything else
// (a missing grant, a dead connection) cannot stand in for the check.
func TestThePublisherCannotListTenantNamesByAskingForAPrefixThatIsNotALibrary(t *testing.T) {
	const marker = "is not a library prefix"
	eachPrefixRig(t, func(t *testing.T, c *prefixCase) {
		c.registerTenants(t)
		c.openPublisher(t)
		lib := c.library(t, "wide")
		secret := "z" + lib + ".payroll" // starts with "z", and sits under no library
		if err := c.deploy(pfxTenantA, secret, 1); err != nil {
			t.Fatalf("UNMEASURED: seeding a name: %v", err)
		}
		// ask calls the reader as the publisher with a RAW argument, which libraryPrefixClashes cannot do.
		ask := func(prefix string) ([]string, error) {
			query := `SELECT tenant_id::text, name, versions FROM library_clashing_names($1)`
			if c.rig.dialect == DialectMSSQL {
				query = `EXEC dbo.library_clashing_names @p1`
			}
			rows, err := c.pub.QueryContext(c.ctx, query, prefix)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var names []string
			for rows.Next() {
				var tenant, name, versions string
				if err := rows.Scan(&tenant, &name, &versions); err != nil {
					return nil, err
				}
				names = append(names, tenant+"|"+name)
			}
			return names, rows.Err()
		}

		// Known-positive: a real library prefix reaches the seeded name, so a refusal below is the
		// check's doing and not a reader that returns nothing to everyone.
		got, err := ask("z" + lib + ".")
		if err != nil || len(got) != 1 || got[0] != pfxTenantA+"|"+secret {
			t.Fatalf("UNMEASURED: the reader asked for %q returned %v (%v), want tenant A's %s", "z"+lib+".", got, err, secret)
		}
		// Every character a library name may hold must be ACCEPTED, each in a position that reaches the
		// character-class test: a validator stricter than the grammar refuses real publishes. The first
		// version of the SQL Server check refused a hyphen, and only other tests' library names
		// ("pub-...", "shardclash-...") noticed; this test's own names held none.
		for _, ok := range []string{
			"a-b.", "a_b.", "a--b.", "a-.", "a_.", "A-Z9.", "9a.", "Z.", "a1-2_3b.",
			strings.Repeat("a", 128) + ".", // the longest library name still finds nothing, and is not refused
		} {
			if got, err := ask(ok); err != nil || len(got) != 0 {
				t.Errorf("the library prefix %q returned %v (%v), want an answer of no names and no refusal", ok, got, err)
			}
		}

		for _, bad := range []struct{ why, prefix string }{
			{"empty, a prefix of every name", ""},
			{"a single letter, a prefix of every name that starts with it", "z"},
			{"a letter and nothing after it", "zz"},
			{"two dots", "a.b."},
			{"no library name before the dot", "."},
			{"a dot in the middle", "a.b"},
			{"a library name without its dot", "z" + lib},
			{"starts with a hyphen", "-a."},
			{"starts with an underscore", "_a."},
			{"a space inside", "a b."},
			{"a trailing space after the dot", "a. "},
			{"a trailing newline after the dot", "a.\n"},
			{"a non-ASCII letter first", "\u00e9."},
			// After the first character, so only the character-class test sees them: under a
			// case- and accent-insensitive collation the range [a-z] would cover an accented letter.
			{"a non-ASCII letter after the first", "a\u00e9b."},
			{"a full-width letter after the first", "a\uff21."},
			{"a wildcard", "a%."},
			{"129 characters before the dot", strings.Repeat("a", 129) + "."},
		} {
			names, err := ask(bad.prefix)
			if err == nil {
				t.Errorf("the publisher asked the reader for %q (%s) and was answered with %v, want a refusal", bad.prefix, bad.why, names)
				continue
			}
			if !strings.Contains(err.Error(), marker) {
				t.Errorf("the reader refused %q (%s) with %q, want the refusal to say %q", bad.prefix, bad.why, err, marker)
			}
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
