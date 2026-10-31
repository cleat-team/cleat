package engine

// cleat#3627: ListPublishedLibraries is the tenant-facing form of `cleatctl library list`: the same
// bundles, read through a TENANT's store, which is the connection a worker has and the role that was
// granted SELECT on the library schema and nothing more.
//
// Two things are pinned. It lists what is published, newest first, each bundle with its members and
// their digests and sizes and never their bytes; and it is the same for a tenant that has installed
// something and one that has not, because the catalogue is global (D45), so a tenant's own installs
// neither add to it nor hide from it. An unknown library filters to an empty LIST, not nil, so it
// encodes as [] and not null.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestATenantReadsThePublishedCatalogue(t *testing.T) {
	t.Run("postgres as a role the policy applies to", func(t *testing.T) {
		runATenantReadsTheCatalogue(t, newPostgresLibRig(t))
	})
	t.Run("sqlserver", func(t *testing.T) {
		runATenantReadsTheCatalogue(t, newMSSQLLibRig(t))
	})
}

func runATenantReadsTheCatalogue(t *testing.T, rig *libInstallRig) {
	t.Helper()
	ctx := context.Background()
	const (
		installer = "f3000000-0000-4000-8000-0000000036d0"
		bystander = "f3000000-0000-4000-8000-0000000036e0"
	)
	tenants := []string{installer, bystander}
	rig.registerPlugins(t, "slack-notify")
	lib := fmt.Sprintf("lib-3627-cat-%d", time.Now().UnixNano())
	rig.cleanup(tenants, lib)
	t.Cleanup(func() { rig.cleanup(tenants, lib) })

	m1 := libMember{name: lib + ".order", version: 1, exposure: "auth", variant: 1}
	m2 := libMember{name: lib + ".order", version: 2, exposure: "internal", variant: 2}
	rig.seedMember(t, lib, m1)
	rig.seedMember(t, lib, m2)
	rig.exec(t, "", `INSERT INTO library.bundles (library, bundle_version, published_by, published_at) VALUES (?, '1.0.0', 'operator-one', ?)`, lib, time.Now().Add(-time.Hour).UTC())
	rig.exec(t, "", `INSERT INTO library.bundles (library, bundle_version, published_by, published_at) VALUES (?, '2.0.0', 'operator-two', ?)`, lib, time.Now().UTC())
	rig.exec(t, "", `INSERT INTO library.bundle_members (library, bundle_version, name, version) VALUES (?, '1.0.0', ?, 1)`, lib, m1.name)
	rig.exec(t, "", `INSERT INTO library.bundle_members (library, bundle_version, name, version) VALUES (?, '2.0.0', ?, 2)`, lib, m2.name)
	if _, err := rig.store(installer).InstallLibraryBundle(ctx, InstallLibraryRequest{Library: lib, BundleVersion: "1.0.0", InstalledBy: "k"}); err != nil {
		t.Fatalf("%s: install: %v", rig.name, err)
	}

	read := func(tenant, library string) []LibraryBundleSummary {
		t.Helper()
		c, ok := rig.store(tenant).(LibraryCatalogue)
		if !ok {
			t.Fatalf("%s: the store does not implement LibraryCatalogue", rig.name)
		}
		got, err := c.ListPublishedLibraries(ctx, library)
		if err != nil {
			t.Fatalf("%s: list the catalogue for %s: %v", rig.name, tenant, err)
		}
		return got
	}
	for _, tenant := range tenants {
		got := read(tenant, lib)
		if len(got) != 2 {
			t.Fatalf("%s: tenant %s sees %d bundles of %s, want 2: %+v", rig.name, tenant, len(got), lib, got)
		}
		// Newest first within a library.
		if got[0].BundleVersion != "2.0.0" || got[1].BundleVersion != "1.0.0" {
			t.Errorf("%s: order %s then %s, want 2.0.0 then 1.0.0", rig.name, got[0].BundleVersion, got[1].BundleVersion)
		}
		for i, want := range []libMember{m2, m1} {
			if len(got[i].Members) != 1 {
				t.Fatalf("%s: bundle %s has %d members, want 1", rig.name, got[i].BundleVersion, len(got[i].Members))
			}
			mem := got[i].Members[0]
			if mem.Name != want.name || mem.Version != want.version || mem.WasmSHA256 != want.digest() || mem.WasmLength != int64(len(want.wasm())) || string(mem.Exposure) != want.exposure {
				t.Errorf("%s: member %+v does not match what was published (%s v%d %s %d)", rig.name, mem, want.name, want.version, want.digest(), len(want.wasm()))
			}
		}
	}
	if got := read(installer, "no-such-library"); got == nil || len(got) != 0 {
		t.Errorf("%s: an unknown library lists %v (nil=%v), want an empty non-nil list", rig.name, got, got == nil)
	}
}
