package pgvector

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/cleat-team/cleat/internal/plugin"
)

// RegisterHostFunctions registers workflow-callable functions on the scoped
// function registry. The plugin name is implicit -- each plugin gets its own
// scope, so function names need not be globally unique.
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if scope == nil {
		return fmt.Errorf("pgvector: nil function registry")
	}
	if err := scope.Register(plugin.FuncOptions{Name: "search", Idempotent: true}, p.search); err != nil {
		return err
	}
	if err := scope.Register(plugin.FuncOptions{Name: "upsert"}, p.upsert); err != nil {
		return err
	}
	if err := scope.Register(plugin.FuncOptions{Name: "delete", Idempotent: true}, p.delete); err != nil {
		return err
	}
	return nil
}

// ---- Input/output types ----

type searchInput struct {
	Collection  string            `json:"collection"`
	QueryVector []float64         `json:"query_vector,omitempty"`
	TopK        int               `json:"top_k,omitempty"`
	Filter      map[string]any    `json:"filter,omitempty"`
	IncludeMeta bool              `json:"include_meta,omitempty"`
}

type searchOutput struct {
	Results []searchResult `json:"results"`
}

type searchResult struct {
	ID        string         `json:"id"`
	ExternalID string        `json:"external_id,omitempty"`
	Content   string         `json:"content,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Score     float64        `json:"score"`
}

type upsertInput struct {
	Collection string         `json:"collection"`
	ExternalID string         `json:"external_id,omitempty"`
	Content    string         `json:"content,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	Embedding  []float64      `json:"embedding,omitempty"`
}

type upsertOutput struct {
	ID string `json:"id"`
}

type deleteInput struct {
	Collection string `json:"collection"`
	ID         string `json:"id,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
}

type deleteOutput struct {
	Deleted int `json:"deleted"`
}

// ---- Host functions ----

// search performs a vector similarity search on the given collection. It finds
// the top-k nearest neighbor vectors by cosine distance. This function is
// idempotent and safe to re-invoke during replay.
func (p *Plugin) search(ctx context.Context, inputJSON string) (string, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == uuid.Nil {
		return "", fmt.Errorf("pgvector: no tenant context")
	}

	var input searchInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("pgvector: invalid input: %w", err)
	}
	if input.Collection == "" {
		return "", fmt.Errorf("pgvector: collection is required")
	}
	if input.TopK <= 0 {
		input.TopK = 10
	}

	// Look up collection.
	var collectionID uuid.UUID
	var dimensions int
	err := p.db.QueryRow(ctx, `
		SELECT id, dimensions
		FROM pgvector_collections
		WHERE name = $1
	`, input.Collection).Scan(&collectionID, &dimensions)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("pgvector: collection not found: %s", input.Collection)
	}
	if err != nil {
		return "", fmt.Errorf("pgvector: lookup collection: %w", err)
	}

	// Build a query with optional filter on metadata.
	var rows plugin.Rows
	if len(input.QueryVector) > 0 {
		vectorLit := vectorLiteral(input.QueryVector)
		query := fmt.Sprintf(`
			SELECT e.id, e.external_id, e.content, e.metadata,
			       1 - (e.embedding <=> '%s'::vector) AS score
			FROM pgvector_embeddings e
			WHERE e.tenant_id = $1 AND e.collection_id = $2
			ORDER BY e.embedding <=> '%s'::vector
			LIMIT $3
		`, vectorLit, vectorLit)
		rows, err = p.db.Query(ctx, query, cc.TenantID, collectionID, input.TopK)
	} else {
		// No query vector: return most recently added embeddings.
		rows, err = p.db.Query(ctx, `
			SELECT e.id, e.external_id, e.content, e.metadata, 0.0 AS score
			FROM pgvector_embeddings e
			WHERE e.tenant_id = $1 AND e.collection_id = $2
			ORDER BY e.created_at DESC
			LIMIT $3
		`, cc.TenantID, collectionID, input.TopK)
	}
	if err != nil {
		return "", fmt.Errorf("pgvector: search: %w", err)
	}
	defer rows.Close()

	var results []searchResult
	for rows.Next() {
		var (
			id         uuid.UUID
			externalID sql.NullString
			content    sql.NullString
			metaJSON   []byte
			score      float64
		)
		if err := rows.Scan(&id, &externalID, &content, &metaJSON, &score); err != nil {
			return "", fmt.Errorf("pgvector: scan: %w", err)
		}
		r := searchResult{
			ID:    id.String(),
			Score: score,
		}
		if externalID.Valid {
			r.ExternalID = externalID.String
		}
		if content.Valid {
			r.Content = content.String
		}
		if input.IncludeMeta && len(metaJSON) > 0 {
			var meta map[string]any
			if err := json.Unmarshal(metaJSON, &meta); err == nil {
				r.Metadata = meta
			}
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("pgvector: rows: %w", err)
	}

	if results == nil {
		results = []searchResult{}
	}

	output := searchOutput{Results: results}
	outJSON, err := json.Marshal(output)
	if err != nil {
		return "", fmt.Errorf("pgvector: marshal output: %w", err)
	}
	return string(outJSON), nil
}

// upsert inserts or updates an embedding vector in a collection. If external_id
// is provided and already exists for the tenant+collection, the existing row is
// updated. Otherwise a new row is inserted.
func (p *Plugin) upsert(ctx context.Context, inputJSON string) (string, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == uuid.Nil {
		return "", fmt.Errorf("pgvector: no tenant context")
	}

	var input upsertInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("pgvector: invalid input: %w", err)
	}
	if input.Collection == "" {
		return "", fmt.Errorf("pgvector: collection is required")
	}

	// Look up collection.
	var collectionID uuid.UUID
	err := p.db.QueryRow(ctx, `
		SELECT id FROM pgvector_collections WHERE name = $1
	`, input.Collection).Scan(&collectionID)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("pgvector: collection not found: %s", input.Collection)
	}
	if err != nil {
		return "", fmt.Errorf("pgvector: lookup collection: %w", err)
	}

	// Marshal metadata.
	var metaJSON []byte
	if input.Metadata != nil {
		metaJSON, err = json.Marshal(input.Metadata)
		if err != nil {
			return "", fmt.Errorf("pgvector: marshal metadata: %w", err)
		}
	} else {
		metaJSON = []byte("{}")
	}

	// Convert embedding vector to pgvector literal.
	var vectorStr string
	if len(input.Embedding) > 0 {
		vectorStr = vectorLiteral(input.Embedding)
	}

	var id uuid.UUID

	if input.ExternalID != "" && vectorStr != "" {
		// Upsert by external_id.
		err = p.db.QueryRow(ctx, `
			INSERT INTO pgvector_embeddings (tenant_id, collection_id, external_id, content, metadata, embedding)
			VALUES ($1, $2, $3, $4, $5, $6::vector)
			ON CONFLICT (tenant_id, collection_id, external_id) DO UPDATE
			SET content = EXCLUDED.content,
			    metadata = EXCLUDED.metadata,
			    embedding = EXCLUDED.embedding,
			    updated_at = now()
			RETURNING id
		`, cc.TenantID, collectionID, input.ExternalID, input.Content, metaJSON, vectorStr).Scan(&id)
	} else if input.ExternalID != "" {
		// Upsert without embedding.
		err = p.db.QueryRow(ctx, `
			INSERT INTO pgvector_embeddings (tenant_id, collection_id, external_id, content, metadata)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant_id, collection_id, external_id) DO UPDATE
			SET content = EXCLUDED.content,
			    metadata = EXCLUDED.metadata,
			    updated_at = now()
			RETURNING id
		`, cc.TenantID, collectionID, input.ExternalID, input.Content, metaJSON).Scan(&id)
	} else if vectorStr != "" {
		// Insert new row with embedding.
		err = p.db.QueryRow(ctx, `
			INSERT INTO pgvector_embeddings (tenant_id, collection_id, content, metadata, embedding)
			VALUES ($1, $2, $3, $4, $5::vector)
			RETURNING id
		`, cc.TenantID, collectionID, input.Content, metaJSON, vectorStr).Scan(&id)
	} else {
		// Insert new row without embedding.
		err = p.db.QueryRow(ctx, `
			INSERT INTO pgvector_embeddings (tenant_id, collection_id, content, metadata)
			VALUES ($1, $2, $3, $4)
			RETURNING id
		`, cc.TenantID, collectionID, input.Content, metaJSON).Scan(&id)
	}
	if err != nil {
		return "", fmt.Errorf("pgvector: upsert: %w", err)
	}

	output := upsertOutput{ID: id.String()}
	outJSON, err := json.Marshal(output)
	if err != nil {
		return "", fmt.Errorf("pgvector: marshal output: %w", err)
	}
	return string(outJSON), nil
}

// delete removes embedding vectors from a collection by row ID or external_id.
// This function is idempotent and safe to re-invoke during replay.
func (p *Plugin) delete(ctx context.Context, inputJSON string) (string, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == uuid.Nil {
		return "", fmt.Errorf("pgvector: no tenant context")
	}

	var input deleteInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("pgvector: invalid input: %w", err)
	}
	if input.Collection == "" {
		return "", fmt.Errorf("pgvector: collection is required")
	}
	if input.ID == "" && input.ExternalID == "" {
		return "", fmt.Errorf("pgvector: id or external_id is required")
	}

	// Look up collection.
	var collectionID uuid.UUID
	err := p.db.QueryRow(ctx, `
		SELECT id FROM pgvector_collections WHERE name = $1
	`, input.Collection).Scan(&collectionID)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("pgvector: collection not found: %s", input.Collection)
	}
	if err != nil {
		return "", fmt.Errorf("pgvector: lookup collection: %w", err)
	}

	var result int64
	if input.ID != "" {
		result, err = p.db.Exec(ctx, `
			DELETE FROM pgvector_embeddings
			WHERE id = $1 AND tenant_id = $2 AND collection_id = $3
		`, input.ID, cc.TenantID, collectionID)
	} else {
		result, err = p.db.Exec(ctx, `
			DELETE FROM pgvector_embeddings
			WHERE external_id = $1 AND tenant_id = $2 AND collection_id = $3
		`, input.ExternalID, cc.TenantID, collectionID)
	}
	if err != nil {
		return "", fmt.Errorf("pgvector: delete: %w", err)
	}

	output := deleteOutput{Deleted: int(result)}
	outJSON, err := json.Marshal(output)
	if err != nil {
		return "", fmt.Errorf("pgvector: marshal output: %w", err)
	}
	return string(outJSON), nil
}

// vectorLiteral formats a float64 slice as a pgvector literal string.
// Example: [1.0, 2.0, 3.0] -> "[1,2,3]"
func vectorLiteral(v []float64) string {
	b := make([]byte, 1, len(v)*8+2)
	b[0] = '['
	for i, f := range v {
		if i > 0 {
			b = append(b, ',')
		}
		b = fmt.Appendf(b, "%g", f)
	}
	b = append(b, ']')
	return string(b)
}
