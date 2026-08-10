// Package pgvector provides typed Go wrappers for the "pgvector" plugin.
//
// Usage:
//
//	client := pgvector.NewClient(h.PluginCall)
//	resp, err := client.Search(ctx, pgvector.SearchRequest{
//	    Table:  "documents",
//	    Vector: []float64{0.1, 0.2, 0.3},
//	    Limit:  10,
//	})
package pgvector

import (
	"context"
	"encoding/json"
	"fmt"
)

// Vector is a sequence of float64 values representing an embedding vector.
type Vector []float64

// SearchRequest is the typed request for a vector similarity search.
type SearchRequest struct {
	Table       string         `json:"collection"`             // maps to plugin's "collection"
	Vector      Vector         `json:"query_vector,omitempty"` // maps to plugin's "query_vector"
	Limit       int            `json:"top_k,omitempty"`        // maps to plugin's "top_k"
	Filter      map[string]any `json:"filter,omitempty"`       // optional metadata filter
	MinScore    float64        `json:"min_score,omitempty"`
	IncludeMeta bool           `json:"include_meta,omitempty"`
}

// SearchResult is a single result from a vector similarity search.
type SearchResult struct {
	ID       string         `json:"id"`
	Score    float64        `json:"score"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// SearchResponse is the typed response from a vector similarity search.
type SearchResponse struct {
	Results []SearchResult `json:"results"`
}

// UpsertRequest is the typed request to upsert an embedding vector.
type UpsertRequest struct {
	Table    string         `json:"collection"`            // maps to plugin's "collection"
	ID       string         `json:"external_id,omitempty"` // maps to plugin's "external_id"
	Content  string         `json:"content,omitempty"`
	Vector   Vector         `json:"embedding,omitempty"` // maps to plugin's "embedding"
	Metadata map[string]any `json:"metadata,omitempty"`
}

// DeleteRequest is the typed request to delete an embedding vector.
type DeleteRequest struct {
	Table string `json:"collection"`            // maps to plugin's "collection"
	ID    string `json:"external_id,omitempty"` // maps to plugin's "external_id"
}

// Client wraps a plugin call function for typed pgvector operations.
type Client struct {
	call func(pluginName, functionName, inputJSON string) (string, error)
}

// NewClient creates a new pgvector Client backed by the given call function.
func NewClient(call func(pluginName, functionName, inputJSON string) (string, error)) *Client {
	return &Client{call: call}
}

// Search performs a vector similarity search on the given table/collection.
func (c *Client) Search(ctx context.Context, req SearchRequest) (*SearchResponse, error) {
	inputJSON, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("pgvector: marshal search request: %w", err)
	}

	outputJSON, err := c.call("pgvector", "search", string(inputJSON))
	if err != nil {
		return nil, fmt.Errorf("pgvector: search failed: %w", err)
	}

	var resp SearchResponse
	if err := json.Unmarshal([]byte(outputJSON), &resp); err != nil {
		return nil, fmt.Errorf("pgvector: unmarshal search response: %w", err)
	}

	if resp.Results == nil {
		resp.Results = []SearchResult{}
	}

	return &resp, nil
}

// Upsert inserts or updates an embedding vector in the given table/collection.
func (c *Client) Upsert(ctx context.Context, req UpsertRequest) error {
	inputJSON, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("pgvector: marshal upsert request: %w", err)
	}

	_, err = c.call("pgvector", "upsert", string(inputJSON))
	if err != nil {
		return fmt.Errorf("pgvector: upsert failed: %w", err)
	}

	return nil
}

// Delete removes an embedding vector from the given table/collection.
func (c *Client) Delete(ctx context.Context, req DeleteRequest) error {
	inputJSON, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("pgvector: marshal delete request: %w", err)
	}

	_, err = c.call("pgvector", "delete", string(inputJSON))
	if err != nil {
		return fmt.Errorf("pgvector: delete failed: %w", err)
	}

	return nil
}
