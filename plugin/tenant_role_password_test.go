package plugin

// cleat#1307. Tenant role passwords are derived from the worker's key rather
// than generated and stored, so the properties the derivation must have are the
// ones this file pins.
//
// DETERMINISM IS THE LOAD-BEARING ONE. The whole point is that any worker can
// open a tenant pool without reading a stored credential, which only works if
// every worker derives the same password from the same key. A derivation that
// accidentally depended on time, map order, or process state would authenticate
// on the worker that provisioned the role and fail on every other one -- a
// failure that appears only in a multi-worker deployment.

import (
	"errors"
	"strings"
	"testing"
)

func testSecret(b byte) []byte {
	s := make([]byte, TenantRoleSecretMinBytes)
	for i := range s {
		s[i] = b + byte(i)
	}
	return s
}

func TestTenantRolePasswordIsDeterministic(t *testing.T) {
	secret := testSecret(1)
	const id = "11111111-2222-3333-4444-555555555555"

	first, err := TenantRolePassword(secret, id)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	for i := 0; i < 100; i++ {
		again, err := TenantRolePassword(secret, id)
		if err != nil {
			t.Fatalf("derive %d: %v", i, err)
		}
		if again != first {
			t.Fatalf("derivation %d differs: %q vs %q.\n\n"+
				"Every worker must derive the same password from the same key, or a role "+
				"provisioned by one worker cannot be used by another (cleat#1307).", i, again, first)
		}
	}
	if len(first) != 64 {
		t.Errorf("password is %d characters, want 64 (256 bits of hex)", len(first))
	}
	if strings.ToLower(first) != first {
		t.Errorf("password is not lowercase hex: %q", first)
	}
}

func TestTenantRolePasswordSeparatesTenantsAndKeys(t *testing.T) {
	a, _ := TenantRolePassword(testSecret(1), "aaaaaaaa-0000-0000-0000-000000000001")
	b, _ := TenantRolePassword(testSecret(1), "bbbbbbbb-0000-0000-0000-000000000002")
	if a == b {
		t.Error("two tenants derived the same password under one key; the tenant id is " +
			"not reaching the derivation")
	}

	// And a rotated key must change every password, which is what makes
	// rotation meaningful -- and what ReconcileTenantRolePasswords exists for.
	c, _ := TenantRolePassword(testSecret(9), "aaaaaaaa-0000-0000-0000-000000000001")
	if a == c {
		t.Error("changing the key did not change the password; the key is not reaching " +
			"the derivation")
	}
}

// The same tenant must derive the same password however its UUID was spelled.
// A mixed-case id producing a different password is an authentication failure
// that looks like a wrong key rather than a formatting difference.
func TestTenantRolePasswordNormalisesTheTenantID(t *testing.T) {
	secret := testSecret(1)
	want, err := TenantRolePassword(secret, "abcdefab-1111-2222-3333-444444444444")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	for _, spelling := range []string{
		"ABCDEFAB-1111-2222-3333-444444444444",
		"  abcdefab-1111-2222-3333-444444444444  ",
		"AbCdEfAb-1111-2222-3333-444444444444",
	} {
		got, err := TenantRolePassword(secret, spelling)
		if err != nil {
			t.Fatalf("derive %q: %v", spelling, err)
		}
		if got != want {
			t.Errorf("%q derived a different password than its lowercase form", spelling)
		}
	}
}

// A short key is REFUSED rather than accepted, because the failure is otherwise
// invisible: a 4-byte key still produces a plausible 64-character hex string,
// and every tenant password in the deployment is weak with nothing to show it.
func TestAShortSecretIsRefused(t *testing.T) {
	for _, n := range []int{0, 1, 8, 16, 31} {
		if _, err := TenantRolePassword(make([]byte, n), "aaaaaaaa-0000-0000-0000-000000000001"); err == nil {
			t.Errorf("a %d-byte secret was accepted; want at least %d", n, TenantRoleSecretMinBytes)
		} else if !errors.Is(err, ErrTenantRoleSecretTooShort) {
			t.Errorf("a %d-byte secret failed with the wrong error: %v", n, err)
		}
	}
	// The negative control: exactly the minimum is accepted.
	if _, err := TenantRolePassword(make([]byte, TenantRoleSecretMinBytes), "a-0-0-0-1"); err != nil {
		t.Errorf("a %d-byte secret was refused: %v", TenantRoleSecretMinBytes, err)
	}
}

func TestAnEmptyTenantIDIsRefused(t *testing.T) {
	for _, id := range []string{"", "   ", "\t"} {
		if _, err := TenantRolePassword(testSecret(1), id); err == nil {
			t.Errorf("an empty tenant id (%q) derived a password; every tenant would share "+
				"it", id)
		}
	}
}

func TestTenantRoleNameMatchesTheSQLConvention(t *testing.T) {
	// admin.create_tenant_role builds
	//   'cleat_tenant_' || replace(p_tenant_id::text, '-', '_')
	// and a mismatch here produces "role does not exist" at connect time, which
	// reads as a provisioning failure rather than a naming one.
	got := TenantRoleName("11111111-2222-3333-4444-555555555555")
	want := "cleat_tenant_11111111_2222_3333_4444_555555555555"
	if got != want {
		t.Errorf("TenantRoleName = %q, want %q", got, want)
	}
	if TenantRoleName("ABCDEF01-0000-0000-0000-000000000000") !=
		TenantRoleName("abcdef01-0000-0000-0000-000000000000") {
		t.Error("role name is case-sensitive; PostgreSQL role names here are lowercase")
	}
}
