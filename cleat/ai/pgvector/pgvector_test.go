package pgvector

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// ---------------------------------------------------------------------------
// Mock infrastructure
// ---------------------------------------------------------------------------

// mockCallRecorder captures every invocation of the plugin call function so
// tests can inspect the serialized requests and verify routing.
type mockCallRecorder struct {
	responses []string
	count     int
	Inputs    []string
	Plugins   []string
	Functions []string
}

func newMockCallRecorder(responses ...string) *mockCallRecorder {
	return &mockCallRecorder{
		responses: responses,
		Inputs:    make([]string, 0, len(responses)),
		Plugins:   make([]string, 0, len(responses)),
		Functions: make([]string, 0, len(responses)),
	}
}

func (m *mockCallRecorder) call(pluginName, functionName, inputJSON string) (string, error) {
	m.Plugins = append(m.Plugins, pluginName)
	m.Functions = append(m.Functions, functionName)
	m.Inputs = append(m.Inputs, inputJSON)
	if m.count >= len(m.responses) {
		return "", errors.New("mock: no more responses")
	}
	resp := m.responses[m.count]
	m.count++
	return resp, nil
}

// requireUnmarshal parses raw JSON into dst and fails the test on error.
func requireUnmarshal(t *testing.T, data string, dst interface{}) {
	t.Helper()
	if err := json.Unmarshal([]byte(data), dst); err != nil {
		t.Fatalf("unmarshal: %v\nJSON: %s", err, data)
	}
}

// ---------------------------------------------------------------------------
// Tests: Client creation
// ---------------------------------------------------------------------------

func TestNewClient(t *testing.T) {
	mock := newMockCallRecorder(`{"results":[]}`)
	client := NewClient(mock.call)
	if client == nil {
		t.Fatal("NewClient returned nil")
	}

	// Verify the client can make a Search call.
	resp, err := client.Search(context.Background(), SearchRequest{
		Table:  "documents",
		Vector: Vector{0.1, 0.2, 0.3},
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if resp.Results == nil {
		t.Error("Results should not be nil")
	}
	if len(resp.Results) != 0 {
		t.Errorf("len(Results) = %d, want 0", len(resp.Results))
	}
	if len(mock.Inputs) != 1 {
		t.Fatalf("expected 1 call, got %d", len(mock.Inputs))
	}
}

// ---------------------------------------------------------------------------
// Tests: Search request field name mapping
// ---------------------------------------------------------------------------

func TestSearchRequest_FieldNameMapping(t *testing.T) {
	mock := newMockCallRecorder(`{"results":[]}`)
	client := NewClient(mock.call)

	_, err := client.Search(context.Background(), SearchRequest{
		Table:       "documents",
		Vector:      Vector{0.1, 0.2, 0.3, 0.4},
		Limit:       10,
		Filter:      map[string]any{"source": "web"},
		MinScore:    0.75,
		IncludeMeta: true,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	// Verify routing.
	if mock.Plugins[0] != "pgvector" {
		t.Errorf("plugin = %q, want %q", mock.Plugins[0], "pgvector")
	}
	if mock.Functions[0] != "search" {
		t.Errorf("function = %q, want %q", mock.Functions[0], "search")
	}

	// Verify all field name mappings.
	var req map[string]any
	requireUnmarshal(t, mock.Inputs[0], &req)

	if req["collection"] != "documents" {
		t.Errorf("collection = %v, want %q", req["collection"], "documents")
	}
	vec, ok := req["query_vector"].([]any)
	if !ok || len(vec) != 4 || vec[0].(float64) != 0.1 {
		t.Errorf("query_vector = %v, want [0.1 0.2 0.3 0.4]", req["query_vector"])
	}
	if req["top_k"] != float64(10) {
		t.Errorf("top_k = %v, want 10", req["top_k"])
	}
	if req["min_score"] != float64(0.75) {
		t.Errorf("min_score = %v, want 0.75", req["min_score"])
	}
	if req["include_meta"] != true {
		t.Errorf("include_meta = %v, want true", req["include_meta"])
	}

	// Verify the filter field.
	filter, ok := req["filter"].(map[string]any)
	if !ok || filter["source"] != "web" {
		t.Errorf("filter = %v, want {source: web}", req["filter"])
	}
}

func TestSearchRequest_OmitEmptyFields(t *testing.T) {
	mock := newMockCallRecorder(`{"results":[]}`)
	client := NewClient(mock.call)

	// Minimal request: only Table is required, everything else should be
	// omitted in JSON when zero-valued (except collection).
	_, err := client.Search(context.Background(), SearchRequest{
		Table: "documents",
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	var req map[string]any
	requireUnmarshal(t, mock.Inputs[0], &req)

	if req["collection"] != "documents" {
		t.Errorf("collection = %v, want %q", req["collection"], "documents")
	}
	// These fields should be omitted (not present) since they are zero-valued
	// and tagged with omitempty.
	if _, ok := req["query_vector"]; ok {
		t.Error("query_vector should be omitted when empty")
	}
	if _, ok := req["top_k"]; ok {
		t.Error("top_k should be omitted when 0")
	}
	if _, ok := req["filter"]; ok {
		t.Error("filter should be omitted when nil")
	}
	if _, ok := req["min_score"]; ok {
		t.Error("min_score should be omitted when 0")
	}
	if _, ok := req["include_meta"]; ok {
		t.Error("include_meta should be omitted when false")
	}
}

// ---------------------------------------------------------------------------
// Tests: Search response deserialization
// ---------------------------------------------------------------------------

func TestSearchResponseDeserialization(t *testing.T) {
	pluginResp := `{
		"results": [
			{"id": "doc1", "score": 0.95, "metadata": {"title": "Doc 1"}},
			{"id": "doc2", "score": 0.87},
			{"id": "doc3", "score": 0.76, "metadata": {"title": "Doc 3"}}
		]
	}`
	mock := newMockCallRecorder(pluginResp)
	client := NewClient(mock.call)

	resp, err := client.Search(context.Background(), SearchRequest{
		Table:  "documents",
		Vector: Vector{0.1, 0.2},
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	if len(resp.Results) != 3 {
		t.Fatalf("len(Results) = %d, want 3", len(resp.Results))
	}

	// Verify first result.
	r0 := resp.Results[0]
	if r0.ID != "doc1" {
		t.Errorf("Results[0].ID = %q, want %q", r0.ID, "doc1")
	}
	if r0.Score != 0.95 {
		t.Errorf("Results[0].Score = %f, want 0.95", r0.Score)
	}
	if r0.Metadata == nil || r0.Metadata["title"] != "Doc 1" {
		t.Errorf("Results[0].Metadata = %v, want {title: Doc 1}", r0.Metadata)
	}

	// Verify second result (no metadata).
	r1 := resp.Results[1]
	if r1.ID != "doc2" {
		t.Errorf("Results[1].ID = %q, want %q", r1.ID, "doc2")
	}
	if r1.Score != 0.87 {
		t.Errorf("Results[1].Score = %f, want 0.87", r1.Score)
	}

	// Verify third result.
	r2 := resp.Results[2]
	if r2.ID != "doc3" {
		t.Errorf("Results[2].ID = %q, want %q", r2.ID, "doc3")
	}
	if r2.Score != 0.76 {
		t.Errorf("Results[2].Score = %f, want 0.76", r2.Score)
	}
}

func TestSearch_NilResultsToEmpty(t *testing.T) {
	// The plugin returns no "results" field at all.
	mock := newMockCallRecorder(`{}`)
	client := NewClient(mock.call)

	resp, err := client.Search(context.Background(), SearchRequest{
		Table:  "documents",
		Vector: Vector{0.1},
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	// The client must convert nil to empty slice.
	if resp.Results == nil {
		t.Error("Results should not be nil after nil-to-empty conversion")
	}
	if len(resp.Results) != 0 {
		t.Errorf("len(Results) = %d, want 0", len(resp.Results))
	}
}

// ---------------------------------------------------------------------------
// Tests: Upsert request field name mapping
// ---------------------------------------------------------------------------

func TestUpsertRequest_FieldNameMapping(t *testing.T) {
	mock := newMockCallRecorder("")
	client := NewClient(mock.call)

	err := client.Upsert(context.Background(), UpsertRequest{
		Table:   "documents",
		ID:      "ext-42",
		Content: "This is a document about Go testing.",
		Vector:  Vector{0.5, 0.6, 0.7},
		Metadata: map[string]any{
			"author": "test",
			"pages":  100,
		},
	})
	if err != nil {
		t.Fatalf("Upsert failed: %v", err)
	}

	// Verify routing.
	if mock.Plugins[0] != "pgvector" {
		t.Errorf("plugin = %q, want %q", mock.Plugins[0], "pgvector")
	}
	if mock.Functions[0] != "upsert" {
		t.Errorf("function = %q, want %q", mock.Functions[0], "upsert")
	}

	// Verify all field name mappings.
	var req map[string]any
	requireUnmarshal(t, mock.Inputs[0], &req)

	if req["collection"] != "documents" {
		t.Errorf("collection = %v, want %q", req["collection"], "documents")
	}
	if req["external_id"] != "ext-42" {
		t.Errorf("external_id = %v, want %q", req["external_id"], "ext-42")
	}
	if req["content"] != "This is a document about Go testing." {
		t.Errorf("content = %v, want %q", req["content"], "This is a document about Go testing.")
	}
	emb, ok := req["embedding"].([]any)
	if !ok || len(emb) != 3 || emb[0].(float64) != 0.5 {
		t.Errorf("embedding = %v, want [0.5 0.6 0.7]", req["embedding"])
	}
	meta, ok := req["metadata"].(map[string]any)
	if !ok || meta["author"] != "test" || meta["pages"] != float64(100) {
		t.Errorf("metadata = %v, want {author: test, pages: 100}", req["metadata"])
	}
}

func TestUpsertRequest_OmitEmptyFields(t *testing.T) {
	mock := newMockCallRecorder("")
	client := NewClient(mock.call)

	// Only Table is required; other fields should be omitted when zero-valued.
	err := client.Upsert(context.Background(), UpsertRequest{
		Table: "documents",
	})
	if err != nil {
		t.Fatalf("Upsert failed: %v", err)
	}

	var req map[string]any
	requireUnmarshal(t, mock.Inputs[0], &req)

	if req["collection"] != "documents" {
		t.Errorf("collection = %v, want %q", req["collection"], "documents")
	}
	if _, ok := req["external_id"]; ok {
		t.Error("external_id should be omitted when empty")
	}
	if _, ok := req["content"]; ok {
		t.Error("content should be omitted when empty")
	}
	if _, ok := req["embedding"]; ok {
		t.Error("embedding should be omitted when empty")
	}
	if _, ok := req["metadata"]; ok {
		t.Error("metadata should be omitted when nil")
	}
}

// ---------------------------------------------------------------------------
// Tests: Upsert response
// ---------------------------------------------------------------------------

func TestUpsertResponse(t *testing.T) {
	// Upsert returns only an error; the response body is ignored.
	mock := newMockCallRecorder(`{"status":"ok"}`)
	client := NewClient(mock.call)

	err := client.Upsert(context.Background(), UpsertRequest{
		Table:  "documents",
		ID:     "ext-42",
		Vector: Vector{0.1, 0.2},
	})
	if err != nil {
		t.Fatalf("Upsert failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tests: Delete request field name mapping
// ---------------------------------------------------------------------------

func TestDeleteRequest_FieldNameMapping(t *testing.T) {
	mock := newMockCallRecorder("")
	client := NewClient(mock.call)

	err := client.Delete(context.Background(), DeleteRequest{
		Table: "documents",
		ID:    "ext-99",
	})
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Verify routing.
	if mock.Plugins[0] != "pgvector" {
		t.Errorf("plugin = %q, want %q", mock.Plugins[0], "pgvector")
	}
	if mock.Functions[0] != "delete" {
		t.Errorf("function = %q, want %q", mock.Functions[0], "delete")
	}

	// Verify field name mappings.
	var req map[string]any
	requireUnmarshal(t, mock.Inputs[0], &req)

	if req["collection"] != "documents" {
		t.Errorf("collection = %v, want %q", req["collection"], "documents")
	}
	if req["external_id"] != "ext-99" {
		t.Errorf("external_id = %v, want %q", req["external_id"], "ext-99")
	}
}

func TestDeleteRequest_OmitEmptyFields(t *testing.T) {
	mock := newMockCallRecorder("")
	client := NewClient(mock.call)

	// Only Table is required.
	err := client.Delete(context.Background(), DeleteRequest{
		Table: "documents",
	})
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	var req map[string]any
	requireUnmarshal(t, mock.Inputs[0], &req)

	if req["collection"] != "documents" {
		t.Errorf("collection = %v, want %q", req["collection"], "documents")
	}
	if _, ok := req["external_id"]; ok {
		t.Error("external_id should be omitted when empty")
	}
}

// ---------------------------------------------------------------------------
// Tests: Delete response
// ---------------------------------------------------------------------------

func TestDeleteResponse(t *testing.T) {
	// Delete returns only an error; the response body is ignored.
	mock := newMockCallRecorder(`{"status":"ok"}`)
	client := NewClient(mock.call)

	err := client.Delete(context.Background(), DeleteRequest{
		Table: "documents",
		ID:    "ext-42",
	})
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tests: Error handling
// ---------------------------------------------------------------------------

func TestSearchError(t *testing.T) {
	// Override the call function to simulate a transport-level error.
	client := NewClient(func(_, _, _ string) (string, error) {
		return "", errors.New("connection refused")
	})

	_, err := client.Search(context.Background(), SearchRequest{
		Table:  "documents",
		Vector: Vector{0.1},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "pgvector: search failed: connection refused" {
		t.Errorf("err = %q, want %q", err.Error(), "pgvector: search failed: connection refused")
	}
}

func TestSearchUnmarshalError(t *testing.T) {
	mock := newMockCallRecorder(`not valid json`)
	client := NewClient(mock.call)

	_, err := client.Search(context.Background(), SearchRequest{
		Table:  "documents",
		Vector: Vector{0.1},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestUpsertError(t *testing.T) {
	// Simulate a transport-level failure.
	client := NewClient(func(_, _, _ string) (string, error) {
		return "", errors.New("rate limit exceeded")
	})

	err := client.Upsert(context.Background(), UpsertRequest{
		Table:  "documents",
		ID:     "ext-42",
		Vector: Vector{0.1},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "pgvector: upsert failed: rate limit exceeded" {
		t.Errorf("err = %q, want %q", err.Error(), "pgvector: upsert failed: rate limit exceeded")
	}
}

func TestDeleteError(t *testing.T) {
	// Simulate a transport-level failure.
	client := NewClient(func(_, _, _ string) (string, error) {
		return "", errors.New("table not found")
	})

	err := client.Delete(context.Background(), DeleteRequest{
		Table: "documents",
		ID:    "ext-99",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "pgvector: delete failed: table not found" {
		t.Errorf("err = %q, want %q", err.Error(), "pgvector: delete failed: table not found")
	}
}

// ---------------------------------------------------------------------------
// Tests: JSON unmarshal error (response body not parseable)
// ---------------------------------------------------------------------------

func TestSearchJSONError(t *testing.T) {
	// The plugin call succeeds but returns malformed JSON.
	mock := newMockCallRecorder(`{{invalid`)
	client := NewClient(mock.call)

	_, err := client.Search(context.Background(), SearchRequest{
		Table:  "documents",
		Vector: Vector{0.1},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
