package blobstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/cleat-team/cleat/internal/plugin"
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
	if err == sql.ErrNoRows {
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

func newS3Backend(ctx context.Context, cfg Config) (*s3Backend, error) {
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "s3." + cfg.Region + ".amazonaws.com"
	}

	var creds *credentials.Credentials
	if cfg.AccessKeyID != "" {
		creds = credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, "")
	} else {
		// Fall back to the default AWS credential chain: env vars, then EC2
		// instance profile / ECS task role.
		creds = credentials.NewChainCredentials([]credentials.Provider{
			&credentials.EnvAWS{},
			&credentials.IAM{
				Client: &http.Client{},
			},
		})
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
