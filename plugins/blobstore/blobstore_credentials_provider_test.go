package blobstore

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7/pkg/credentials"
)

// fakeBlobstoreSecrets is a plugin.DeploymentSecrets stand-in: it answers a
// fixed value for "blobstore.access_key_id"/"blobstore.secret_access_key",
// and can be flipped to error on demand -- simulating a retired or
// unresolvable secret without needing a real secret store.
type fakeBlobstoreSecrets struct {
	accessKeyID     string
	secretAccessKey string
	fail            bool
}

func (f *fakeBlobstoreSecrets) Get(ctx context.Context, name string) (string, error) {
	if f.fail {
		return "", fmt.Errorf("fakeBlobstoreSecrets: %q: retired", name)
	}
	switch name {
	case "blobstore.access_key_id":
		return f.accessKeyID, nil
	case "blobstore.secret_access_key":
		return f.secretAccessKey, nil
	default:
		return "", fmt.Errorf("fakeBlobstoreSecrets: %q not set", name)
	}
}

// TestDeploymentSecretsCredentialsProvider_RetrieveSuccess proves a normal
// resolve reaches both names and returns them as a signed V4 Value.
func TestDeploymentSecretsCredentialsProvider_RetrieveSuccess(t *testing.T) {
	secrets := &fakeBlobstoreSecrets{accessKeyID: "AKIATEST", secretAccessKey: "secret1"}
	p := newDeploymentSecretsCredentialsProvider(secrets)

	v, err := p.RetrieveWithCredContext(nil)
	if err != nil {
		t.Fatalf("RetrieveWithCredContext: %v", err)
	}
	if v.AccessKeyID != "AKIATEST" || v.SecretAccessKey != "secret1" {
		t.Errorf("got %+v", v)
	}
	if v.SignerType != credentials.SignatureV4 {
		t.Errorf("want SignatureV4, got %v", v.SignerType)
	}
}

// TestDeploymentSecretsCredentialsProvider_NilSecretsStoreErrors is the
// no-config-at-all case: a blobstore plugin with backend "s3" but no
// deployment secret store wired must refuse to sign, not sign as an empty
// (i.e. effectively anonymous) identity.
func TestDeploymentSecretsCredentialsProvider_NilSecretsStoreErrors(t *testing.T) {
	p := newDeploymentSecretsCredentialsProvider(nil)

	_, err := p.RetrieveWithCredContext(nil)
	if err == nil {
		t.Fatal("expected an error with no deployment secret store configured, got nil")
	}
	if !strings.Contains(err.Error(), "no deployment secret store configured") {
		t.Errorf("got: %v", err)
	}
}

// TestDeploymentSecretsCredentialsProvider_RetrieveFailureIsNotSwallowed is
// the doc comment's central claim, proven directly: a Get failure on either
// name is returned as-is, never turned into a zero Value with a nil error.
func TestDeploymentSecretsCredentialsProvider_RetrieveFailureIsNotSwallowed(t *testing.T) {
	secrets := &fakeBlobstoreSecrets{fail: true}
	p := newDeploymentSecretsCredentialsProvider(secrets)

	v, err := p.RetrieveWithCredContext(nil)
	if err == nil {
		t.Fatal("expected the fake's error to propagate, got nil")
	}
	if v != (credentials.Value{}) {
		t.Errorf("expected a zero Value alongside the error, got %+v", v)
	}
}

// TestDeploymentSecretsCredentialsProvider_IsExpiredStartsTrue proves the
// first S3 request always fetches fresh rather than serving an empty Value:
// a freshly constructed provider's expiresAt is the zero Time, already in
// the past.
func TestDeploymentSecretsCredentialsProvider_IsExpiredStartsTrue(t *testing.T) {
	p := newDeploymentSecretsCredentialsProvider(&fakeBlobstoreSecrets{})
	if !p.IsExpired() {
		t.Error("a freshly constructed provider must report expired so the first request fetches")
	}
}

// TestDeploymentSecretsCredentialsProvider_TTLRotation proves rotation
// reaches minio-go's own credentials.Credentials cache, not just this
// package's provider: two GetWithContext calls through the SAME
// *credentials.Credentials, with the underlying secret changed and the TTL forced to have elapsed in
// between, return two different Values. Forcing expiresAt directly rather
// than sleeping deploymentSecretsCredentialsTTL -- a wall-clock wait in a
// test is exactly what CLAUDE.md's "Is this result real?" section warns
// against relying on.
func TestDeploymentSecretsCredentialsProvider_TTLRotation(t *testing.T) {
	secrets := &fakeBlobstoreSecrets{accessKeyID: "AKIAOLD", secretAccessKey: "old-secret"}
	p := newDeploymentSecretsCredentialsProvider(secrets)
	creds := credentials.New(p)

	v1, err := creds.GetWithContext(nil)
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if v1.AccessKeyID != "AKIAOLD" {
		t.Fatalf("got %+v", v1)
	}

	// Still within the TTL: the cache must NOT call Retrieve again, so
	// changing the fake's answer must not yet be visible.
	secrets.accessKeyID = "AKIANEW"
	v2, err := creds.GetWithContext(nil)
	if err != nil {
		t.Fatalf("second Get (still cached): %v", err)
	}
	if v2.AccessKeyID != "AKIAOLD" {
		t.Errorf("expected the cached value within the TTL, got %+v", v2)
	}

	// Force the TTL to have elapsed -- same effect as
	// `cleatctl set-deployment-secret` rotating the value 60s ago.
	p.mu.Lock()
	p.expiresAt = time.Now().Add(-time.Second)
	p.mu.Unlock()

	v3, err := creds.GetWithContext(nil)
	if err != nil {
		t.Fatalf("third Get (post-TTL): %v", err)
	}
	if v3.AccessKeyID != "AKIANEW" {
		t.Errorf("expected the rotated value once the TTL elapsed, got %+v", v3)
	}
}

// TestDeploymentSecretsCredentialsProvider_RetiredSecretFailsRatherThanFallingBack
// is the coordinator's explicit requirement, proven through minio-go's own
// wrapper: once the TTL elapses and the deployment secret has been retired
// (Get now errors), credentials.Credentials.GetWithContext must return that
// error -- never a stale cached Value and never a zero-value Value that
// minio-go would sign as an empty (i.e. effectively anonymous) credential.
func TestDeploymentSecretsCredentialsProvider_RetiredSecretFailsRatherThanFallingBack(t *testing.T) {
	secrets := &fakeBlobstoreSecrets{accessKeyID: "AKIAOLD", secretAccessKey: "old-secret"}
	p := newDeploymentSecretsCredentialsProvider(secrets)
	creds := credentials.New(p)

	if _, err := creds.GetWithContext(nil); err != nil {
		t.Fatalf("first Get: %v", err)
	}

	p.mu.Lock()
	p.expiresAt = time.Now().Add(-time.Second)
	p.mu.Unlock()
	secrets.fail = true

	if _, err := creds.GetWithContext(nil); err == nil {
		t.Fatal("expected the retired secret's error to surface through credentials.Credentials.Get, got nil")
	}
}
