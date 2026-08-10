package blobstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cleat-team/cleat/plugin"
)

// RegisterHostFunctions registers workflow-callable functions on the scoped
// function registry. The plugin name is implicit -- each plugin gets its own
// scope, so function names need not be globally unique.
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if scope == nil {
		return fmt.Errorf("blobstore: nil function registry")
	}
	if err := scope.Register(plugin.FuncOptions{Name: "put"}, p.blobPut); err != nil {
		return err
	}
	// blob_get is safe to re-invoke during replay -- reads from S3, not from
	// event history. Registering as idempotent means the engine will re-invoke
	// the function on replay instead of returning cached output.
	if err := scope.Register(plugin.FuncOptions{Name: "get", Idempotent: true}, p.blobGet); err != nil {
		return err
	}
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
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == "" {
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
	sha256Hex := hex.EncodeToString(hash[:])

	// Store bytes via the selected backend.
	if err := p.backend.Put(ctx, sha256Hex, input.Data, input.ContentType); err != nil {
		return "", fmt.Errorf("blobstore: store content: %w", err)
	}

	// Insert or increment ref_count in blob_content metadata.
	storageBackend := p.config.Backend
	var s3Key *string
	if storageBackend == "s3" {
		s3Key = &sha256Hex
	}
	_, err := p.db.Exec(ctx, plugin.Rebind(upsertBlobContent.For(p.dialect), p.dialect),
		hash[:], len(input.Data), storageBackend, s3Key)
	if err != nil {
		return "", fmt.Errorf("blobstore: store content: %w", err)
	}

	// Record workflow blob reference so the blob is not physically deleted
	// while this workflow is still in-flight.
	wfID := cc.WorkflowID
	if wfID != "" {
		if _, err := p.db.Exec(ctx, plugin.Rebind(upsertBlobRef.For(p.dialect), p.dialect),
			wfID, hash[:]); err != nil {
			p.logger.Warn("blobstore: record blob ref", "workflow_id", wfID, "sha256", sha256Hex, "error", err)
		}
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
		_, err = p.db.Exec(ctx, plugin.Rebind(upsertBlobIndexWithTTL.For(p.dialect), p.dialect),
			input.Key, cc.TenantID, hash[:], len(input.Data), input.ContentType, tagsJSON, *expiresAt)
	} else {
		_, err = p.db.Exec(ctx, plugin.Rebind(upsertBlobIndex.For(p.dialect), p.dialect),
			input.Key, cc.TenantID, hash[:], len(input.Data), input.ContentType, tagsJSON)
	}
	if err != nil {
		return "", fmt.Errorf("blobstore: store index: %w", err)
	}

	output := blobPutOutput{
		Key:    input.Key,
		SHA256: sha256Hex,
		Size:   int64(len(input.Data)),
	}
	outJSON, _ := json.Marshal(output)
	return string(outJSON), nil
}

// blobGet retrieves a blob by key. Returns the data (base64-encoded in JSON)
// along with metadata. Returns an error if the blob is not found or has expired.
func (p *Plugin) blobGet(ctx context.Context, inputJSON string) (string, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == "" {
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
		sha256Bytes []byte
		contentType string
		size        int64
		expiresAt   sql.NullTime
	)
	err := p.db.QueryRow(ctx, plugin.Rebind(`
		SELECT c.sha256, i.content_type, i.size, i.expires_at
		FROM blob_index i
		JOIN blob_content c ON i.sha256 = c.sha256
		WHERE i.key = $1 AND i.tenant_id = $2 AND i.deleted_at IS NULL
	`, p.dialect), input.Key, cc.TenantID).Scan(&sha256Bytes, &contentType, &size, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("blobstore: blob not found: %s", input.Key)
	}
	if err != nil {
		return "", fmt.Errorf("blobstore: get: %w", err)
	}

	// Check for expiry.
	if expiresAt.Valid && expiresAt.Time.Before(time.Now()) {
		return "", fmt.Errorf("blobstore: blob expired: %s", input.Key)
	}

	// Retrieve blob data from the configured backend.
	sha256Hex := hex.EncodeToString(sha256Bytes)
	data, err := p.backend.Get(ctx, sha256Hex)
	if err != nil {
		return "", fmt.Errorf("blobstore: get data: %w", err)
	}

	// Record workflow blob reference so the blob is not physically deleted
	// while this workflow is still in-flight.
	wfID := cc.WorkflowID
	if wfID != "" {
		if _, err := p.db.Exec(ctx, `
			INSERT INTO workflow_blob_refs (workflow_id, sha256)
			VALUES ($1, $2) ON CONFLICT DO NOTHING
		`, wfID, sha256Bytes); err != nil {
			p.logger.Warn("blobstore: record blob ref", "workflow_id", wfID, "sha256", sha256Hex, "error", err)
		}
	}

	output := blobGetOutput{
		Key:         input.Key,
		SHA256:      sha256Hex,
		Size:        size,
		ContentType: contentType,
		Data:        data,
	}
	outJSON, _ := json.Marshal(output)
	return string(outJSON), nil
}
