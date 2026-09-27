package main

// cleat#1307. --tenant-isolation=role with no usable secret must REFUSE TO
// BOOT, which was the repo owner's decision on the open question.
//
// EVERY CASE HERE IS A REFUSAL EXCEPT THE CONTROLS, and that is deliberate.
// The alternative -- starting and letting tenant pools fail at first use --
// produces one error per workflow, minutes later, on the path that was
// supposed to be carrying the isolation. A worker that is up and cannot do its
// job is worse than one that refused, because the refusal names the cause.
//
// A SILENT DOWNGRADE IS THE FAILURE TO AVOID. Falling back from "role" to
// "rls" on a missing secret would leave an operator believing they had
// per-tenant credentials when they had the shared owner pool -- the same shape
// as the owner-pool fallback removed from TenantPools.For.
//
// NOTE ON THE FILENAME. These live in tenant_isolation_FLAGS_test.go because
// tenant_isolation_test.go already exists and defines twoTenantServer, which
// allowed_signals_api_test.go uses. I wrote this file there first with `cat >`
// and broke the package -- then read the resulting `undefined: twoTenantServer`
// as develop being broken, having "isolated" my change with a `git stash` that
// silently stashed nothing. Both halves of that are in CLAUDE.md.

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

func writeIsolationSecret(t *testing.T, contents string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	return p
}

func goodIsolationSecretFile(t *testing.T) string {
	t.Helper()
	return writeIsolationSecret(t, base64.StdEncoding.EncodeToString(
		make([]byte, plugin.TenantRoleSecretMinBytes)))
}

func TestRoleIsolationRefusesWithoutAUsableSecret(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       string
		secretFile func(*testing.T) string
		driver     string
		want       string
	}{
		{"no secret file at all", "role",
			func(*testing.T) string { return "" }, "postgres",
			"requires --tenant-role-secret-file"},
		{"secret file does not exist", "role",
			func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") }, "postgres",
			"read --tenant-role-secret-file"},
		{"secret is not base64", "role",
			func(t *testing.T) string { return writeIsolationSecret(t, "not base64 !!!") }, "postgres",
			"not valid base64"},
		{"secret is too short", "role",
			func(t *testing.T) string {
				return writeIsolationSecret(t, base64.StdEncoding.EncodeToString(make([]byte, 16)))
			}, "postgres", "at least 32 are required"},
		{"wrong dialect", "role",
			func(*testing.T) string { return "" }, "mysql", "requires --driver=postgres"},
		{"unrecognised mode", "sometimes",
			func(*testing.T) string { return "" }, "postgres", "not recognised"},
		// A secret with rls is a misconfiguration, not a no-op: whoever set it
		// expected per-tenant credentials.
		{"secret set but mode is rls", "rls",
			goodIsolationSecretFile, "postgres", "only used by --tenant-isolation=role"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sf := tc.secretFile(t)
			_, _, err := resolveTenantIsolation(tc.mode, sf, tc.driver)
			if err == nil {
				t.Fatalf("resolveTenantIsolation(%q, %q, %q) returned no error; want one "+
					"mentioning %q.\n\nStarting here means every tenant pool fails at first "+
					"use instead of at startup (cleat#1307).", tc.mode, sf, tc.driver, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not name the cause: %v\n\nwant text containing %q", err, tc.want)
			}
		})
	}
}

// The negative controls. Without these, a resolver that refused EVERYTHING
// would pass every assertion above.
func TestTheTwoValidIsolationConfigurationsAreAccepted(t *testing.T) {
	t.Run("rls with no secret is the default and is accepted", func(t *testing.T) {
		mode, secret, err := resolveTenantIsolation("rls", "", "postgres")
		if err != nil {
			t.Fatalf("the default configuration was refused: %v", err)
		}
		if mode != isolationRLS {
			t.Errorf("mode = %q, want %q", mode, isolationRLS)
		}
		if secret != nil {
			t.Error("rls returned a secret; it has no use for one")
		}
	})

	t.Run("role with a good secret is accepted and carries the key", func(t *testing.T) {
		mode, secret, err := resolveTenantIsolation("role", goodIsolationSecretFile(t), "postgres")
		if err != nil {
			t.Fatalf("a valid role configuration was refused: %v", err)
		}
		if mode != isolationRole {
			t.Errorf("mode = %q, want %q", mode, isolationRole)
		}
		if len(secret) < plugin.TenantRoleSecretMinBytes {
			t.Errorf("secret is %d bytes, want at least %d",
				len(secret), plugin.TenantRoleSecretMinBytes)
		}
	})

	// rls on MySQL must still work: the dialect restriction belongs to role
	// isolation, not to the flag. MySQL is single-tenant by decision, and
	// rls-on-the-owner-pool is exactly what serves that.
	t.Run("rls on a non-postgres dialect is accepted", func(t *testing.T) {
		if _, _, err := resolveTenantIsolation("rls", "", "mysql"); err != nil {
			t.Errorf("rls was refused on mysql: %v", err)
		}
	})
}

// The derived password must be reproducible from the LOADED secret, which is
// what makes a stored credential unnecessary. Asserted here rather than only in
// plugin/ because this is where a file becomes bytes: a loader that trimmed or
// re-encoded wrongly would produce a key that works only on the worker that
// read it, and the failure would look like a wrong password.
func TestTheLoadedSecretDerivesTheSamePasswordAsTheRawKey(t *testing.T) {
	raw := make([]byte, plugin.TenantRoleSecretMinBytes)
	for i := range raw {
		raw[i] = byte(i + 7)
	}
	// Deliberately surrounded by whitespace, which is what a file written by
	// `base64 > secret` actually contains.
	file := writeIsolationSecret(t, "  "+base64.StdEncoding.EncodeToString(raw)+"\n")

	_, loaded, err := resolveTenantIsolation("role", file, "postgres")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	const tenant = "aaaaaaaa-0000-0000-0000-00000000a001"
	fromFile, err := plugin.TenantRolePassword(loaded, tenant)
	if err != nil {
		t.Fatalf("derive from the loaded key: %v", err)
	}
	fromRaw, err := plugin.TenantRolePassword(raw, tenant)
	if err != nil {
		t.Fatalf("derive from the raw key: %v", err)
	}
	if fromFile != fromRaw {
		t.Errorf("the loaded key derives a different password than the raw one.\n\n"+
			"Whitespace handling or re-encoding in the loader would make a role usable "+
			"only from the worker that provisioned it (cleat#1307).\n  file: %s\n  raw:  %s",
			fromFile, fromRaw)
	}
}
