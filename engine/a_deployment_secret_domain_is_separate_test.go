package engine

// DeploymentSecretStore shares its KeyRing with SecretStore (tenant secrets)
// on purpose -- see deployment_secrets.go's type doc comment. What keeps that
// safe is domain separation in seal/open, not a second master key: a distinct
// HKDF info string (deploymentSecretInfo vs "cleat-tenant-secret-v1"), plus
// binding each ciphertext to its own name/tenantID as AAD. These tests are
// the acceptance criteria deployment_secrets.go's comments point at, and are
// deliberately DB-free -- seal/open are pure functions of a KeyRing, so this
// needs no CLEAT_TEST_* dialect to run or to mean anything.

import "testing"

func testDeploymentMasterKey(t *testing.T) *KeyRing {
	t.Helper()
	ring, err := NewKeyRing(VersionedKey{Version: 1, Key: make32ByteKey("deployment-secret-domain-test")})
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}
	return ring
}

// make32ByteKey turns an arbitrary label into a deterministic 32-byte AES-256
// key for tests -- not a real key derivation, just a fixture.
func make32ByteKey(label string) []byte {
	out := make([]byte, 32)
	copy(out, label)
	return out
}

// TestDeploymentSecretRoundTrips is the known-positive: prove the store can
// seal and open its own ciphertext before trusting any test that expects it
// to fail.
func TestDeploymentSecretRoundTrips(t *testing.T) {
	s := &DeploymentSecretStore{ring: testDeploymentMasterKey(t)}

	sealed, err := s.seal("email.sendgrid_api_key", "sk-real-value")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, err := s.open("email.sendgrid_api_key", sealed, 1)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got != "sk-real-value" {
		t.Fatalf("round trip: got %q, want %q", got, "sk-real-value")
	}
}

// TestDeploymentSecretDomainIsSeparateFromTenantSecrets proves that sharing a
// master key between DeploymentSecretStore and SecretStore does not let a
// ciphertext from one open as the other, even when the identifier is
// identical (a tenant ID that happens to equal a deployment secret name).
// This is the case deployment_secrets.go's type doc comment calls out --
// "the default: SAME KEY RING" -- so it has to be exercised, not assumed.
func TestDeploymentSecretDomainIsSeparateFromTenantSecrets(t *testing.T) {
	ring := testDeploymentMasterKey(t)
	dep := &DeploymentSecretStore{ring: ring}
	tenant := &SecretStore{ring: ring}

	const sharedIdentifier = "shared-name"

	// A tenant secret sealed under an ID equal to a deployment secret name
	// must NOT open as that deployment secret.
	tenantSealed, err := tenant.seal(sharedIdentifier, "tenant-value")
	if err != nil {
		t.Fatalf("tenant seal: %v", err)
	}
	if _, err := dep.open(sharedIdentifier, tenantSealed, 1); err == nil {
		t.Fatal("a tenant secret's ciphertext opened as a deployment secret -- domain separation is broken")
	}

	// And the inverse: a deployment secret must not open as a tenant secret
	// under the matching tenant ID.
	depSealed, err := dep.seal(sharedIdentifier, "deployment-value")
	if err != nil {
		t.Fatalf("deployment seal: %v", err)
	}
	if _, err := tenant.open(sharedIdentifier, depSealed, 1); err == nil {
		t.Fatal("a deployment secret's ciphertext opened as a tenant secret -- domain separation is broken")
	}
}

// TestDeploymentSecretKeyDerivationIsIsolatedByInfoStringAlone isolates the
// specific mechanism deployment_secrets.go's type doc comment names --
// deploymentSecretInfo -- from the salt, which also differs between the two
// derivations in ordinary use and would mask a broken info string on its
// own (verified: TestDeploymentSecretDomainIsSeparateFromTenantSecrets keeps
// passing even if deploymentSecretInfo is set equal to tenantKey's info
// string, because tenantKey's salt is still the tenant ID). tenantKey's HKDF
// salt is the tenant ID; deploymentSecretKey's is always nil. An empty
// tenant ID makes both salts the RFC 5869 default (zero-filled, for a
// zero-length salt) -- so with the identifier held equal at "", the info
// string is the ONLY input left that can separate the two derived keys.
func TestDeploymentSecretKeyDerivationIsIsolatedByInfoStringAlone(t *testing.T) {
	master := make32ByteKey("info-string-isolation")

	depKey, err := deploymentSecretKey(master)
	if err != nil {
		t.Fatalf("deploymentSecretKey: %v", err)
	}
	tenKey, err := tenantKey(master, "")
	if err != nil {
		t.Fatalf("tenantKey: %v", err)
	}
	if string(depKey) == string(tenKey) {
		t.Fatal("deploymentSecretKey and tenantKey derived the same key from the same master " +
			"and an empty identifier -- deploymentSecretInfo no longer differs from tenantKey's info string")
	}
}

// TestDeploymentSecretCiphertextIsBoundToItsName proves the AAD binding
// deployment_secrets.go's seal doc comment claims: "a row copied to another
// name ... fails to open rather than decrypting to something."
func TestDeploymentSecretCiphertextIsBoundToItsName(t *testing.T) {
	s := &DeploymentSecretStore{ring: testDeploymentMasterKey(t)}

	sealed, err := s.seal("email.sendgrid_api_key", "sk-real-value")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := s.open("llm.providers.openai.api_key", sealed, 1); err == nil {
		t.Fatal("a ciphertext sealed under one name opened under a different name -- AAD binding is broken")
	}
}
