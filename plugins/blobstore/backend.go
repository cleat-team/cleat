package blobstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Backend stores and retrieves blob bytes. Implementations must be safe for
// concurrent use.
type Backend interface {
	// Put stores blob bytes addressed by the hex-encoded SHA-256 hash.
	Put(ctx context.Context, sha256 string, data []byte, contentType string) error
	// Get retrieves blob bytes by hex-encoded SHA-256 hash.
	Get(ctx context.Context, sha256 string) ([]byte, error)
	// Delete removes blob bytes by hex-encoded SHA-256 hash.
	Delete(ctx context.Context, sha256 string) error
}

// memoryBackend stores blobs in the PostgreSQL blob_content.data BYTEA column.
// This is the default backend for dev/testing and requires no external services.
type memoryBackend struct {
	db      plugin.PluginDB
	dialect plugin.Dialect
}

func newMemoryBackend(db plugin.PluginDB, dialect plugin.Dialect) *memoryBackend {
	return &memoryBackend{db: db, dialect: dialect}
}

func (b *memoryBackend) Put(ctx context.Context, sha256Str string, data []byte, _ string) error {
	sha256Bytes, err := hex.DecodeString(sha256Str)
	if err != nil {
		return fmt.Errorf("blobstore: decode sha256: %w", err)
	}
	_, err = b.db.Exec(ctx, plugin.Rebind(upsertBlobContentData.For(b.dialect), b.dialect),
		sha256Bytes, len(data), data)
	return err
}

func (b *memoryBackend) Get(ctx context.Context, sha256Str string) ([]byte, error) {
	sha256Bytes, err := hex.DecodeString(sha256Str)
	if err != nil {
		return nil, fmt.Errorf("blobstore: decode sha256: %w", err)
	}
	var data []byte
	err = b.db.QueryRow(ctx, plugin.Rebind(`SELECT data FROM blob_content WHERE sha256 = $1`, b.dialect), sha256Bytes).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("blobstore: content not found: %s", sha256Str)
	}
	return data, err
}

func (*memoryBackend) Delete(_ context.Context, _ string) error {
	// No-op: row deletion in blob_content handles cleanup for memory backend.
	return nil
}

// s3Backend stores blobs in S3-compatible object storage (AWS S3, MinIO, GCS).
type s3Backend struct {
	client *minio.Client
	bucket string
}

// newS3Backend builds the S3 client's credential source from cfg.
// UseIAMCredentials, not from whether a deployment secret happens to be set:
// these are two different credential MODELS, and letting one silently fall
// back to the other would mean a retired or unresolvable
// "blobstore.access_key_id" quietly starts signing requests as whatever
// identity the instance metadata service hands out, rather than failing the
// request -- the opposite of "a failed Retrieve must fail the request, not
// fall back to anonymous" (or to a different identity).
//
//   - UseIAMCredentials false (default): credentials come from
//     deploymentSecretsCredentialsProvider below, which reads
//     "blobstore.access_key_id"/"blobstore.secret_access_key" fresh on every
//     signing attempt (gated by a short TTL, not cached for the client's
//     lifetime) -- the same per-use convention as every other cleat#1992 part
//     1b conversion. A Retrieve error is returned as-is; minio-go fails the
//     S3 call rather than proceeding unsigned.
//   - UseIAMCredentials true: the original env-var / EC2-instance-profile /
//     ECS-task-role chain, unchanged, for a deployment with no static keys at
//     all. This never touches deployment_secrets.
func newS3Backend(ctx context.Context, cfg Config, secrets plugin.DeploymentSecrets) (*s3Backend, error) {
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "s3." + cfg.Region + ".amazonaws.com"
	}

	var creds *credentials.Credentials
	if cfg.UseIAMCredentials {
		creds = credentials.NewChainCredentials([]credentials.Provider{
			&credentials.EnvAWS{},
			&credentials.IAM{
				// DELIBERATELY NOT the egress-guarded transport, and this is
				// the one place in the tree where that is correct.
				//
				// cleat#1565's floor refuses link-local precisely because
				// 169.254.169.254 is the cloud instance metadata endpoint, and
				// reaching it is the entire job of this client: it is how the
				// EC2 instance-profile / ECS task-role credential chain gets
				// credentials. Routing it through the guard would refuse the
				// request the guard is named after.
				//
				// It is safe in a way the plugin clients are not, because the
				// destination is not configurable: it comes from the AWS SDK,
				// not from a tenant, a plugin config or a workflow. Nothing a
				// guest supplies reaches it.
				//
				// Exempted BY NAME in
				// TestEveryPluginRoutesItsEgressThroughTheGuard, so this
				// remains a decision somebody made rather than a client
				// somebody missed.
				Client: &http.Client{},
			},
		})
	} else {
		creds = credentials.New(newDeploymentSecretsCredentialsProvider(secrets))
	}

	secure := cfg.Secure
	if cfg.Endpoint == "" {
		secure = true // AWS S3 always uses TLS
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  creds,
		Region: cfg.Region,
		Secure: secure,
	})
	if err != nil {
		return nil, fmt.Errorf("blobstore: create s3 client: %w", err)
	}

	return &s3Backend{
		client: client,
		bucket: cfg.Bucket,
	}, nil
}

func (b *s3Backend) Put(ctx context.Context, sha256Str string, data []byte, contentType string) error {
	_, err := b.client.PutObject(ctx, b.bucket, sha256Str, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
		ContentType: contentType,
	})
	return err
}

func (b *s3Backend) Get(ctx context.Context, sha256Str string) ([]byte, error) {
	obj, err := b.client.GetObject(ctx, b.bucket, sha256Str, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("blobstore: s3 get: %w", err)
	}
	defer obj.Close()
	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, fmt.Errorf("blobstore: s3 read: %w", err)
	}
	return data, nil
}

func (b *s3Backend) Delete(ctx context.Context, sha256Str string) error {
	return b.client.RemoveObject(ctx, b.bucket, sha256Str, minio.RemoveObjectOptions{})
}

// deploymentSecretsCredentialsTTL bounds how long a resolved credential pair
// is reused before the next S3 request triggers a fresh
// deploymentSecrets.Get -- the same rotation-without-restart property every
// other cleat#1992 part 1b conversion gets from fetching per call, adapted to
// a Provider interface that is only asked "IsExpired?" rather than called
// fresh every time. 60s: short enough that a retired or rotated credential
// stops being used within one minute of the operator's cleatctl command,
// long enough that a bucket under real traffic is not re-decrypting the
// ciphertext on every single PutObject/GetObject/RemoveObject.
const deploymentSecretsCredentialsTTL = 60 * time.Second

// deploymentSecretsCredentialsProvider implements
// credentials.Provider (github.com/minio/minio-go/v7/pkg/credentials) by
// reading "blobstore.access_key_id"/"blobstore.secret_access_key" from
// plugin.DeploymentSecrets, instead of the static access key minio-go's own
// credentials.NewStaticV4 would hold for the client's entire lifetime.
//
// minio-go caches whatever RetrieveWithCredContext last returned until
// IsExpired reports true, so unlike scheduledbackup's backupDSN or
// slacknotify's signingSecret -- which call DeploymentSecrets.Get on every
// single use with no cache of their own -- this type IS a cache, because
// minio-go's Credentials wrapper (not this type) is what every S3 call
// actually asks for a Value. The cache is bounded by
// deploymentSecretsCredentialsTTL rather than the client's lifetime, which is
// what makes rotation and retirement visible without a worker restart at
// all -- the property this conversion exists to add.
type deploymentSecretsCredentialsProvider struct {
	secrets plugin.DeploymentSecrets

	mu        sync.Mutex
	expiresAt time.Time
}

func newDeploymentSecretsCredentialsProvider(secrets plugin.DeploymentSecrets) *deploymentSecretsCredentialsProvider {
	// expiresAt is left at the zero Time, which is already in the past, so
	// IsExpired reports true and the first S3 request always fetches fresh
	// rather than serving an empty Value.
	return &deploymentSecretsCredentialsProvider{secrets: secrets}
}

// RetrieveWithCredContext implements credentials.Provider. cc.Context is the
// context minio-go's own request carried when it decided a refresh was due;
// nil (no in-flight request context, or an older minio-go build that does
// not populate it) falls back to context.Background() rather than panicking.
//
// A failed Get is returned as-is, never swallowed into an empty Value: this
// is what makes a retired or unresolvable secret fail the S3 request rather
// than have minio-go sign it with a zero-value (i.e. effectively anonymous)
// credential.
func (p *deploymentSecretsCredentialsProvider) RetrieveWithCredContext(cc *credentials.CredContext) (credentials.Value, error) {
	if p.secrets == nil {
		return credentials.Value{}, fmt.Errorf("blobstore: no deployment secret store configured")
	}

	ctx := context.Background()
	if cc != nil && cc.Context != nil {
		ctx = cc.Context
	}

	accessKeyID, err := p.secrets.Get(ctx, "blobstore.access_key_id")
	if err != nil {
		return credentials.Value{}, fmt.Errorf("blobstore: access_key_id: %w", err)
	}
	secretAccessKey, err := p.secrets.Get(ctx, "blobstore.secret_access_key")
	if err != nil {
		return credentials.Value{}, fmt.Errorf("blobstore: secret_access_key: %w", err)
	}

	p.mu.Lock()
	p.expiresAt = time.Now().Add(deploymentSecretsCredentialsTTL)
	p.mu.Unlock()

	return credentials.Value{
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
		SignerType:      credentials.SignatureV4,
	}, nil
}

// Retrieve implements credentials.Provider's deprecated method, kept only
// because the interface requires it; minio-go calls RetrieveWithCredContext
// on every path this plugin exercises.
func (p *deploymentSecretsCredentialsProvider) Retrieve() (credentials.Value, error) {
	return p.RetrieveWithCredContext(nil)
}

// IsExpired implements credentials.Provider.
func (p *deploymentSecretsCredentialsProvider) IsExpired() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return time.Now().After(p.expiresAt)
}
