package blobstore

import (
	"errors"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// TestS3BackendExpireCredsOnAuthError is the coordinator/cleat-review
// rotation-window fix (torn pair, or an operator revoking the old key
// immediately): InvalidAccessKeyId and SignatureDoesNotMatch -- the two S3
// error codes a stale or half-rotated credential pair produces -- must force
// the NEXT S3 request to re-resolve credentials rather than reuse the ones
// that just failed for up to deploymentSecretsCredentialsTTL. A focused unit
// test of the extracted helper against a directly-constructed
// minio.ErrorResponse, rather than a full mock-server round trip: the
// behavior under test is entirely "does credentials.Credentials.Expire() get
// called", which minio.ToErrorResponse's own type switch
// (api-error-response.go) makes reachable without a real HTTP response body.
func TestS3BackendExpireCredsOnAuthError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantExpire bool
	}{
		{"InvalidAccessKeyId expires", minio.ErrorResponse{Code: "InvalidAccessKeyId"}, true},
		{"SignatureDoesNotMatch expires", minio.ErrorResponse{Code: "SignatureDoesNotMatch"}, true},
		{"an unrelated S3 error does not expire", minio.ErrorResponse{Code: "NoSuchKey"}, false},
		{"a non-ErrorResponse error does not expire", errors.New("connection reset"), false},
		{"nil does not expire", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secrets := &fakeBlobstoreSecrets{accessKeyID: "AKIATEST", secretAccessKey: "secret1"}
			creds := credentials.New(newDeploymentSecretsCredentialsProvider(secrets))
			// Resolve once so IsExpired starts false -- otherwise every case
			// would read "expired" whether or not Expire() ran, since a
			// freshly constructed provider already reports expired (see
			// TestDeploymentSecretsCredentialsProvider_IsExpiredStartsTrue).
			if _, err := creds.GetWithContext(nil); err != nil {
				t.Fatalf("warm the cache: %v", err)
			}
			if creds.IsExpired() {
				t.Fatal("expected IsExpired() == false immediately after a successful resolve")
			}

			b := &s3Backend{creds: creds}
			b.expireCredsOnAuthError(tt.err)

			if got := creds.IsExpired(); got != tt.wantExpire {
				t.Errorf("IsExpired() = %v, want %v", got, tt.wantExpire)
			}
		})
	}
}

// TestS3BackendExpireCredsOnAuthErrorNilCredsDoesNotPanic covers
// UseIAMCredentials mode, where newS3Backend deliberately leaves creds nil
// (the env/instance-profile/task-role chain is not this plugin's cache to
// force a refresh of) -- and also plain struct literals built directly in
// other tests in this package (e.g. blobstore_s3_backend_test.go's
// newS3BackendForTest), which predate the creds field and construct
// *s3Backend without it.
func TestS3BackendExpireCredsOnAuthErrorNilCredsDoesNotPanic(t *testing.T) {
	b := &s3Backend{}
	b.expireCredsOnAuthError(minio.ErrorResponse{Code: "InvalidAccessKeyId"})
}
