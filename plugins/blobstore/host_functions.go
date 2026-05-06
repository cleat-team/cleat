package blobstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/durable/internal/plugin"
)

// RegisterHostFunctions registers workflow-callable functions on the scoped
// function registry. The plugin name is implicit -- each plugin gets its own
// scope, so function names need not be globally unique.
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if scope == nil {
		return fmt.Errorf("blobstore: nil function registry")
	}
	scope.Register("put", p.blobPut)
	scope.Register("get", p.blobGet)
	return nil
}

// ---- Input/output types ----

type blobPutInput struct {
	Key         string            `json:"key"`
	ContentType string            `json:"content_type,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
	TTL         string            `json:"ttl,omitempty"` // duration string like "1h", "30m"
	Data        []byte            `json:"data"`          // base64-encoded in JSON
}

type blobPutOutput struct {
	Key    string `json:"key"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type blobGetInput struct {
	Key string `json:"key"`
}

type blobGetOutput struct {
	Key         string `json:"key"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
	Data        []byte `json:"data"` // base64-encoded in JSON
}

// ---- Host functions ----

// blobPut stores a blob by key. It is content-addressed: uploading the same
// data under different keys shares a single blob_content row and increments
// ref_count. Re-uploading with the same tenant_id and key overwrites the
// index entry.
func (p *Plugin) blobPut(ctx context.Context, inputJSON string) (string, error) {
	tid := plugin.TenantFromContext(ctx)
	if tid == uuid.Nil {
		return "", fmt.Errorf("blobstore: no tenant context")
	}

	var input blobPutInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("blobstore: invalid input: %w", err)
	}
	if input.Key == "" {
		return "", fmt.Errorf("blobstore: key is required")
	}
	if len(input.Data) == 0 {
		return "", fmt.Errorf("blobstore: data is required")
	}
	if input.ContentType == "" {
		input.ContentType = "application/octet-stream"
	}

	// Compute SHA-256 hash of the data for content-addressing.
	hash := sha256.Sum256(input.Data)

	// Insert or increment ref_count in blob_content.
	_, err := p.db.ExecContext(ctx, `
		INSERT INTO blob_content (sha256, size, data, ref_count)
		VALUES ($1, $2, $3, 1)
		ON CONFLICT (sha256) DO UPDATE
		SET ref_count = blob_content.ref_count + 1
	`, hash[:], len(input.Data), input.Data)
	if err != nil {
		return "", fmt.Errorf("blobstore: store content: %w", err)
	}

	// Marshal tags for the JSONB column.
	var tagsJSON []byte
	if input.Tags != nil {
		tagsJSON, err = json.Marshal(input.Tags)
		if err != nil {
			return "", fmt.Errorf("blobstore: marshal tags: %w", err)
		}
	} else {
		tagsJSON = []byte("{}")
	}

	// Handle optional TTL.
	var expiresAt *time.Time
	if input.TTL != "" {
		d, err := time.ParseDuration(input.TTL)
		if err == nil {
			t := time.Now().Add(d)
			expiresAt = &t
		}
	}

	// Insert or update blob_index.
	if expiresAt != nil {
		_, err = p.db.ExecContext(ctx, `
			INSERT INTO blob_index (key, tenant_id, sha256, size, content_type, tags, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (tenant_id, key) DO UPDATE
			SET sha256 = EXCLUDED.sha256, size = EXCLUDED.size,
			    content_type = EXCLUDED.content_type, tags = EXCLUDED.tags,
			    expires_at = EXCLUDED.expires_at
		`, input.Key, tid, hash[:], len(input.Data), input.ContentType, tagsJSON, *expiresAt)
	} else {
		_, err = p.db.ExecContext(ctx, `
			INSERT INTO blob_index (key, tenant_id, sha256, size, content_type, tags)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (tenant_id, key) DO UPDATE
			SET sha256 = EXCLUDED.sha256, size = EXCLUDED.size,
			    content_type = EXCLUDED.content_type, tags = EXCLUDED.tags,
			    expires_at = NULL
		`, input.Key, tid, hash[:], len(input.Data), input.ContentType, tagsJSON)
	}
	if err != nil {
		return "", fmt.Errorf("blobstore: store index: %w", err)
	}

	output := blobPutOutput{
		Key:    input.Key,
		SHA256: hex.EncodeToString(hash[:]),
		Size:   int64(len(input.Data)),
	}
	outJSON, _ := json.Marshal(output)
	return string(outJSON), nil
}

// blobGet retrieves a blob by key. Returns the data (base64-encoded in JSON)
// along with metadata. Returns an error if the blob is not found or has expired.
func (p *Plugin) blobGet(ctx context.Context, inputJSON string) (string, error) {
	tid := plugin.TenantFromContext(ctx)
	if tid == uuid.Nil {
		return "", fmt.Errorf("blobstore: no tenant context")
	}

	var input blobGetInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("blobstore: invalid input: %w", err)
	}
	if input.Key == "" {
		return "", fmt.Errorf("blobstore: key is required")
	}

	var (
		data        []byte
		sha256Bytes []byte
		contentType string
		size        int64
		expiresAt   sql.NullTime
	)
	err := p.db.QueryRowContext(ctx, `
		SELECT c.data, c.sha256, i.content_type, i.size, i.expires_at
		FROM blob_index i
		JOIN blob_content c ON i.sha256 = c.sha256
		WHERE i.key = $1 AND i.tenant_id = $2
	`, input.Key, tid).Scan(&data, &sha256Bytes, &contentType, &size, &expiresAt)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("blobstore: blob not found: %s", input.Key)
	}
	if err != nil {
		return "", fmt.Errorf("blobstore: get: %w", err)
	}

	// Check for expiry.
	if expiresAt.Valid && expiresAt.Time.Before(time.Now()) {
		return "", fmt.Errorf("blobstore: blob expired: %s", input.Key)
	}

	output := blobGetOutput{
		Key:         input.Key,
		SHA256:      hex.EncodeToString(sha256Bytes),
		Size:        size,
		ContentType: contentType,
		Data:        data,
	}
	outJSON, _ := json.Marshal(output)
	return string(outJSON), nil
}
