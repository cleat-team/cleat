package blobstore

import (
	"context"
	"errors"
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
//
// failAccessKeyID/failSecretAccessKey fail ONE name selectively, independent
// of the blanket fail -- needed for TestDeploymentSecretsCredentialsProvider_PartialFailureReturnsZeroValue
// (M1), which fails the case a coordinator/cleat-review review found the
// tests could not distinguish: only the SECOND Get failing, after the first
// already succeeded.
type fakeBlobstoreSecrets struct {
	accessKeyID         string
	secretAccessKey     string
	fail                bool
	failAccessKeyID     bool
	failSecretAccessKey bool
}

func (f *fakeBlobstoreSecrets) Get(ctx context.Context, name string) (string, error) {
	if f.fail {
		return "", fmt.Errorf("fakeBlobstoreSecrets: %q: retired", name)
	}
	switch name {
	case "blobstore.access_key_id":
		if f.failAccessKeyID {
			return "", fmt.Errorf("fakeBlobstoreSecrets: %q: retired", name)
		}
		return f.accessKeyID, nil
	case "blobstore.secret_access_key":
		if f.failSecretAccessKey {
			return "", fmt.Errorf("fakeBlobstoreSecrets: %q: retired", name)
		}
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

// TestDeploymentSecretsCredentialsProvider_RetrieveForwards proves the
// deprecated Retrieve() -- kept only because credentials.Provider's
// interface requires it -- still forwards correctly rather than being an
// untested stub. Nothing in this tree calls Retrieve() directly (minio-go
// calls RetrieveWithCredContext on every path this plugin exercises), so
// without a direct test scripts/check-dead-exports.sh reports it as
// exported code with zero callers anywhere, tests included -- a real
// finding, not noise: a scan that cannot see minio-go's own interface
// dispatch has no other way to know this method is still reachable at all.
func TestDeploymentSecretsCredentialsProvider_RetrieveForwards(t *testing.T) {
	secrets := &fakeBlobstoreSecrets{accessKeyID: "AKIATEST", secretAccessKey: "secret1"}
	p := newDeploymentSecretsCredentialsProvider(secrets)

	v, err := p.Retrieve()
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if v.AccessKeyID != "AKIATEST" || v.SecretAccessKey != "secret1" {
		t.Errorf("got %+v, want the same Value RetrieveWithCredContext(nil) would return", v)
	}

	// And the failure path forwards too, not just the success path.
	secrets.fail = true
	if _, err := p.Retrieve(); err == nil {
		t.Fatal("expected the fake's error to propagate through Retrieve(), got nil")
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
// TestDeploymentSecretsCredentialsProvider_PartialFailureReturnsZeroValue is
// the coordinator/cleat-review M1 finding: the fake used everywhere else in
// this file fails BOTH names together, so it cannot distinguish "returns a
// zero Value on any failure" from "returns a zero Value only when everything
// fails" -- and the latter would let minio-go sign a request with an
// access_key_id but no secret_access_key (or vice versa) if only the SECOND
// Get failed. Confirmed this is a coverage gap, not a live bug: the
// production code already returns credentials.Value{} unconditionally on
// either error, before ever reaching the success return. This test fails
// without that early return and passes with it -- see the falsification
// note beside it in the PR.
func TestDeploymentSecretsCredentialsProvider_PartialFailureReturnsZeroValue(t *testing.T) {
	secrets := &fakeBlobstoreSecrets{
		accessKeyID:         "AKIATEST",
		secretAccessKey:     "secret1",
		failSecretAccessKey: true,
	}
	p := newDeploymentSecretsCredentialsProvider(secrets)

	v, err := p.RetrieveWithCredContext(nil)
	if err == nil {
		t.Fatal("expected the secret_access_key failure to propagate, got nil")
	}
	if v != (credentials.Value{}) {
		t.Errorf("expected a zero Value when only secret_access_key fails, got %+v -- "+
			"a partial pair would let minio-go sign with an access key and no secret", v)
	}
}

// TestDeploymentSecretsCredentialsProvider_RetrieveSetsTTL is the
// coordinator/cleat-review M4 finding: every other test in this file
// backdates expiresAt directly rather than observing RetrieveWithCredContext
// actually set it, so a regression that dropped the "p.expiresAt =
// time.Now().Add(deploymentSecretsCredentialsTTL)" lines entirely would
// leave every existing test green. This brackets time.Now() immediately
// before and after the real call -- no sleeping, per CLAUDE.md's
// timing-test discipline -- and asserts expiresAt falls in that window
// offset by the TTL, which is tight enough to catch "never set" (IsExpired
// would stay true, well outside the window) without being sensitive to
// scheduling jitter between the two time.Now() calls.
func TestDeploymentSecretsCredentialsProvider_RetrieveSetsTTL(t *testing.T) {
	secrets := &fakeBlobstoreSecrets{accessKeyID: "AKIATEST", secretAccessKey: "secret1"}
	p := newDeploymentSecretsCredentialsProvider(secrets)

	before := time.Now()
	if _, err := p.RetrieveWithCredContext(nil); err != nil {
		t.Fatalf("RetrieveWithCredContext: %v", err)
	}
	after := time.Now()

	p.mu.Lock()
	got := p.expiresAt
	p.mu.Unlock()

	wantMin := before.Add(deploymentSecretsCredentialsTTL)
	wantMax := after.Add(deploymentSecretsCredentialsTTL)
	if got.Before(wantMin) || got.After(wantMax) {
		t.Errorf("expiresAt = %v, want between %v and %v (deploymentSecretsCredentialsTTL applied)",
			got, wantMin, wantMax)
	}
}

// TestDeploymentSecretsCredentialsProvider_EmptyValueIsUnavailable is the
// nit cleat-review raised: an empty stored value -- as opposed to a Get
// error -- must fail closed the same way slacknotify's signingSecret does,
// rather than letting minio-go sign a request with an empty (i.e.
// effectively anonymous) access key or secret.
func TestDeploymentSecretsCredentialsProvider_EmptyValueIsUnavailable(t *testing.T) {
	tests := []struct {
		name            string
		accessKeyID     string
		secretAccessKey string
	}{
		{"empty access_key_id", "", "secret1"},
		{"empty secret_access_key", "AKIATEST", ""},
		{"both empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secrets := &fakeBlobstoreSecrets{accessKeyID: tt.accessKeyID, secretAccessKey: tt.secretAccessKey}
			p := newDeploymentSecretsCredentialsProvider(secrets)

			v, err := p.RetrieveWithCredContext(nil)
			if err == nil {
				t.Fatal("expected an error for an empty stored value, got nil")
			}
			if !errors.Is(err, errDeploymentSecretsUnavailable) {
				t.Errorf("expected errDeploymentSecretsUnavailable in the chain, got: %v", err)
			}
			if v != (credentials.Value{}) {
				t.Errorf("expected a zero Value alongside the error, got %+v", v)
			}
		})
	}
}

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
