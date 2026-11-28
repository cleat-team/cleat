package engine

// cleat#3627: ListInstalledLibraries answers for the store's own tenant and no other.
//
// The boundary is the point. tenant_library_installs is FORCE ROW LEVEL SECURITY on PostgreSQL and
// carries a tenant policy on SQL Server, and the read also names its tenant in the WHERE, so two
// layers hold it. Two REAL tenants, on the rig's least-privilege connection, each holding a
// DIFFERENT bundle of the same library, and one of them a second library: a store for A must
// list A's rows exactly, a store for B must list B's exactly, and a tenant that installed nothing
// lists none. The rows seeded for the other tenant are the ones the boundary exists to exclude;
// a row of the tenant under test would pass against a broken boundary.
//
// What this does not do is break one layer and watch the other hold: that is a mutation, run by
// hand and recorded on the PR, because a test cannot remove a predicate from the code it calls.

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestATenantListsOnlyItsOwnLibraryInstalls(t *testing.T) {
	t.Run("postgres as a role the policy applies to", func(t *testing.T) {
		runATenantListsOnlyItsOwnInstalls(t, newPostgresLibRig(t))
	})
	t.Run("sqlserver", func(t *testing.T) {
		runATenantListsOnlyItsOwnInstalls(t, newMSSQLLibRig(t))
	})
}

func runATenantListsOnlyItsOwnInstalls(t *testing.T, rig *libInstallRig) {
	t.Helper()
	ctx := context.Background()
	const (
		tenantA = "f3000000-0000-4000-8000-0000000036a0"
		tenantB = "f3000000-0000-4000-8000-0000000036b0"
		tenantC = "f3000000-0000-4000-8000-0000000036c0"
	)
	tenants := []string{tenantA, tenantB, tenantC}
	rig.registerPlugins(t, "slack-notify")

	stamp := time.Now().UnixNano()
	libOne, libTwo := fmt.Sprintf("lib-3627-a-%d", stamp), fmt.Sprintf("lib-3627-b-%d", stamp)
	for _, l := range []string{libOne, libTwo} {
		rig.cleanup(tenants, l)
		l := l
		t.Cleanup(func() { rig.cleanup(tenants, l) })
	}
	publish := func(library, bundleVersion string, m libMember) {
		t.Helper()
		rig.seedMember(t, library, m)
		rig.exec(t, "", `INSERT INTO library.bundles (library, bundle_version, published_by) VALUES (?, ?, 'test')`, library, bundleVersion)
		rig.exec(t, "", `INSERT INTO library.bundle_members (library, bundle_version, name, version) VALUES (?, ?, ?, ?)`,
			library, bundleVersion, m.name, m.version)
	}
	// Member names carry the library prefix (D59). Two bundles of libOne so A and B hold DIFFERENT ones.
	publish(libOne, "1.0.0", libMember{name: libOne + ".order", version: 1, exposure: "auth", variant: 1})
	publish(libOne, "2.0.0", libMember{name: libOne + ".order", version: 2, exposure: "auth", variant: 2})
	publish(libTwo, "7.0.0", libMember{name: libTwo + ".ship", version: 1, exposure: "auth", variant: 3})

	install := func(tenant, library, bundleVersion, by string) {
		t.Helper()
		if _, err := rig.store(tenant).InstallLibraryBundle(ctx, InstallLibraryRequest{
			Library: library, BundleVersion: bundleVersion, InstalledBy: by,
		}); err != nil {
			t.Fatalf("%s: install %s@%s for %s: %v", rig.name, library, bundleVersion, tenant, err)
		}
	}
	install(tenantA, libOne, "1.0.0", "key-of-a")
	install(tenantB, libOne, "2.0.0", "key-of-b")
	install(tenantB, libTwo, "7.0.0", "key-of-b")

	// UNMEASURED guard: the seeded rows must exist for the boundary to have anything to exclude.
	// Counted tenant by tenant, because SQL Server's policy hides every row from an unscoped session.
	for tenant, want := range map[string]int{tenantA: 1, tenantB: 2, tenantC: 0} {
		seeded := rig.query(t, tenant, `SELECT library FROM tenant_library_installs WHERE tenant_id = ? AND library IN (?, ?)`, tenant, libOne, libTwo)
		if len(seeded) != want {
			t.Fatalf("UNMEASURED: %s: %d install rows were seeded for %s, want %d: %v", rig.name, len(seeded), tenant, want, seeded)
		}
	}

	list := func(tenant string) []InstalledLibrary {
		t.Helper()
		lister, ok := rig.store(tenant).(LibraryInstallLister)
		if !ok {
			t.Fatalf("%s: the store does not implement LibraryInstallLister", rig.name)
		}
		got, err := lister.ListInstalledLibraries(ctx)
		if err != nil {
			t.Fatalf("%s: list installs for %s: %v", rig.name, tenant, err)
		}
		return got
	}
	// pairs reduces a listing to (library, bundle_version, installed_by), the fields a tenant can compare.
	pairs := func(in []InstalledLibrary) [][3]string {
		out := [][3]string{}
		for _, i := range in {
			out = append(out, [3]string{i.Library, i.BundleVersion, i.InstalledBy})
		}
		return out
	}

	a := list(tenantA)
	if want := [][3]string{{libOne, "1.0.0", "key-of-a"}}; !reflect.DeepEqual(pairs(a), want) {
		t.Errorf("%s: tenant A lists %v, want exactly its own install %v", rig.name, pairs(a), want)
	}
	b := list(tenantB)
	// Ordered by library name: libOne < libTwo (a < b in the prefix).
	if want := [][3]string{{libOne, "2.0.0", "key-of-b"}, {libTwo, "7.0.0", "key-of-b"}}; !reflect.DeepEqual(pairs(b), want) {
		t.Errorf("%s: tenant B lists %v, want exactly its own two installs %v", rig.name, pairs(b), want)
	}
	if c := list(tenantC); len(c) != 0 {
		t.Errorf("%s: tenant C installed nothing and lists %v", rig.name, pairs(c))
	}
	// A listing for a tenant with nothing must be an empty LIST, so it encodes as [] and not null.
	if c := list(tenantC); c == nil {
		t.Errorf("%s: a tenant with no installs got a nil listing, which encodes as null", rig.name)
	}
	for _, i := range a {
		if i.InstalledAt.IsZero() || time.Since(i.InstalledAt) > time.Hour || time.Until(i.InstalledAt) > time.Minute {
			t.Errorf("%s: installed_at %v is not a plausible just-now time", rig.name, i.InstalledAt)
		}
	}
}
