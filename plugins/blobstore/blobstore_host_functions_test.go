package blobstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/cleat-team/cleat/internal/plugin"
	"github.com/cleat-team/cleat/internal/host"
)

// ---------------------------------------------------------------------------
// Test helpers for host function tests
// ---------------------------------------------------------------------------

// setupHostFuncTest creates a Plugin wired to a testMemBackend and a fake SQL
// database, ready for direct host function calls (no HTTP routes).
func setupHostFuncTest(t *testing.T) (*Plugin, *fakeDBStore, *fakeClock) {
	t.Helper()

	clock := newFakeClock()
	store := newFakeDBStore()
	store.now = clock.Now

	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{
		db:      &host.SQLDBAdapter{DB: db},
		backend: newTestMemBackend(),
		logger:  slog.Default(),
		config:  Config{Backend: "memory"},
	}

	return p, store, clock
}

// hostFuncContext wraps a context with a plugin.CallContext for testing host functions.
func hostFuncContext(ctx context.Context, tenantID uuid.UUID, workflowID string) context.Context {
	return plugin.WithCallContext(ctx, &plugin.CallContext{
		TenantID:   tenantID.String(),
		WorkflowID: workflowID,
	})
}

// ---------------------------------------------------------------------------
// 4b: Host function tests
// ---------------------------------------------------------------------------

func TestBlobPut(t *testing.T) {
	p, _, _ := setupHostFuncTest(t)
	ctx := hostFuncContext(context.Background(), testTenantID, "")

	input := blobPutInput{
		Key:         "my-key",
		ContentType: "text/plain",
		Data:        []byte("hello world"),
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}

	outputJSON, err := p.blobPut(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("blobPut: %v", err)
	}

	var output blobPutOutput
	if err := json.Unmarshal([]byte(outputJSON), &output); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}

	if output.Key != "my-key" {
		t.Errorf("expected key 'my-key', got %q", output.Key)
	}
	expectedSHA256 := fmt.Sprintf("%x", sha256.Sum256([]byte("hello world")))
	if output.SHA256 != expectedSHA256 {
		t.Errorf("expected sha256 %q, got %q", expectedSHA256, output.SHA256)
	}
	if output.Size != 11 {
		t.Errorf("expected size 11, got %d", output.Size)
	}
}

func TestBlobGet(t *testing.T) {
	p, _, _ := setupHostFuncTest(t)
	ctx := hostFuncContext(context.Background(), testTenantID, "")

	// First put a blob
	putInput := blobPutInput{
		Key:         "get-test-key",
		ContentType: "application/json",
		Data:        []byte(`{"foo":"bar"}`),
	}
	putJSON, _ := json.Marshal(putInput)
	putOutput, err := p.blobPut(ctx, string(putJSON))
	if err != nil {
		t.Fatalf("blobPut: %v", err)
	}

	var putResp blobPutOutput
	json.Unmarshal([]byte(putOutput), &putResp)

	// Now get it
	getInput := blobGetInput{Key: "get-test-key"}
	getJSON, _ := json.Marshal(getInput)

	getOutput, err := p.blobGet(ctx, string(getJSON))
	if err != nil {
		t.Fatalf("blobGet: %v", err)
	}

	var getResp blobGetOutput
	if err := json.Unmarshal([]byte(getOutput), &getResp); err != nil {
		t.Fatalf("unmarshal get output: %v", err)
	}

	if getResp.Key != "get-test-key" {
		t.Errorf("expected key 'get-test-key', got %q", getResp.Key)
	}
	if getResp.SHA256 != putResp.SHA256 {
		t.Errorf("expected sha256 %q, got %q", putResp.SHA256, getResp.SHA256)
	}
	if getResp.Size != 13 {
		t.Errorf("expected size 13, got %d", getResp.Size)
	}
	if getResp.ContentType != "application/json" {
		t.Errorf("expected content_type 'application/json', got %q", getResp.ContentType)
	}
	if string(getResp.Data) != `{"foo":"bar"}` {
		t.Errorf("expected data %q, got %q", `{"foo":"bar"}`, string(getResp.Data))
	}
}

func TestBlobPutLarge(t *testing.T) {
	p, _, _ := setupHostFuncTest(t)
	ctx := hostFuncContext(context.Background(), testTenantID, "")

	// 1MB blob
	size := 1024 * 1024
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 256)
	}

	input := blobPutInput{
		Key:  "large-blob",
		Data: data,
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}

	outputJSON, err := p.blobPut(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("blobPut large: %v", err)
	}

	var output blobPutOutput
	if err := json.Unmarshal([]byte(outputJSON), &output); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}

	if output.Size != int64(size) {
		t.Errorf("expected size %d, got %d", size, output.Size)
	}
	if output.Key != "large-blob" {
		t.Errorf("expected key 'large-blob', got %q", output.Key)
	}
}

func TestBlobGetMissingKey(t *testing.T) {
	p, _, _ := setupHostFuncTest(t)
	ctx := hostFuncContext(context.Background(), testTenantID, "")

	getInput := blobGetInput{Key: "nonexistent-key"}
	getJSON, _ := json.Marshal(getInput)

	_, err := p.blobGet(ctx, string(getJSON))
	if err == nil {
		t.Fatal("expected error for missing blob")
	}
	if !strings.Contains(err.Error(), "blob not found") {
		t.Errorf("expected 'blob not found' error, got: %v", err)
	}
}

func TestBlobPutGetRoundTrip(t *testing.T) {
	p, _, _ := setupHostFuncTest(t)
	ctx := hostFuncContext(context.Background(), testTenantID, "")

	// Put a blob with various content types and verify the round-trip
	testCases := []struct {
		key         string
		data        string
		contentType string
	}{
		{"str:round-trip-1", "Hello, World!", "text/plain"},
		{"str:round-trip-2", "{}", "application/json"},
		{"str:round-trip-3", string([]byte{0, 1, 2, 3, 255, 254, 128, 127}), "application/octet-stream"},
	}

	for _, tc := range testCases {
		t.Run(tc.key, func(t *testing.T) {
			// Put
			putInput := blobPutInput{
				Key:         tc.key,
				Data:        []byte(tc.data),
				ContentType: tc.contentType,
			}
			putJSON, _ := json.Marshal(putInput)

			putOutput, err := p.blobPut(ctx, string(putJSON))
			if err != nil {
				t.Fatalf("blobPut: %v", err)
			}

			var putResp blobPutOutput
			if err := json.Unmarshal([]byte(putOutput), &putResp); err != nil {
				t.Fatalf("unmarshal put: %v", err)
			}

			// Get
			getInput := blobGetInput{Key: tc.key}
			getJSON, _ := json.Marshal(getInput)

			getOutput, err := p.blobGet(ctx, string(getJSON))
			if err != nil {
				t.Fatalf("blobGet: %v", err)
			}

			var getResp blobGetOutput
			if err := json.Unmarshal([]byte(getOutput), &getResp); err != nil {
				t.Fatalf("unmarshal get: %v", err)
			}

			// Verify
			if getResp.Key != tc.key {
				t.Errorf("key: expected %q, got %q", tc.key, getResp.Key)
			}
			if getResp.SHA256 != putResp.SHA256 {
				t.Errorf("sha256: expected %q, got %q", putResp.SHA256, getResp.SHA256)
			}
			if getResp.Size != int64(len(tc.data)) {
				t.Errorf("size: expected %d, got %d", len(tc.data), getResp.Size)
			}
			if string(getResp.Data) != tc.data {
				t.Errorf("data: expected %q, got %q", tc.data, string(getResp.Data))
			}
			if getResp.ContentType != tc.contentType {
				t.Errorf("content_type: expected %q, got %q", tc.contentType, getResp.ContentType)
			}
		})
	}
}

func TestBlobPutRequiresTenant(t *testing.T) {
	p, _, _ := setupHostFuncTest(t)

	// No CallContext in context → should error
	ctx := context.Background()
	input := blobPutInput{
		Key:  "no-tenant-key",
		Data: []byte("data"),
	}
	inputJSON, _ := json.Marshal(input)

	_, err := p.blobPut(ctx, string(inputJSON))
	if err == nil {
		t.Fatal("expected error for missing tenant context")
	}
	if !strings.Contains(err.Error(), "no tenant context") {
		t.Errorf("expected 'no tenant context' error, got: %v", err)
	}
}

func TestBlobGetRequiresTenant(t *testing.T) {
	p, _, _ := setupHostFuncTest(t)

	ctx := context.Background()
	input := blobGetInput{Key: "some-key"}
	inputJSON, _ := json.Marshal(input)

	_, err := p.blobGet(ctx, string(inputJSON))
	if err == nil {
		t.Fatal("expected error for missing tenant context")
	}
	if !strings.Contains(err.Error(), "no tenant context") {
		t.Errorf("expected 'no tenant context' error, got: %v", err)
	}
}

func TestBlobPutRequiresKey(t *testing.T) {
	p, _, _ := setupHostFuncTest(t)
	ctx := hostFuncContext(context.Background(), testTenantID, "")

	input := blobPutInput{
		Data: []byte("data"),
	}
	inputJSON, _ := json.Marshal(input)

	_, err := p.blobPut(ctx, string(inputJSON))
	if err == nil {
		t.Fatal("expected error for empty key")
	}
	if !strings.Contains(err.Error(), "key is required") {
		t.Errorf("expected 'key is required' error, got: %v", err)
	}
}

func TestBlobPutRequiresData(t *testing.T) {
	p, _, _ := setupHostFuncTest(t)
	ctx := hostFuncContext(context.Background(), testTenantID, "")

	input := blobPutInput{
		Key: "no-data-key",
	}
	inputJSON, _ := json.Marshal(input)

	_, err := p.blobPut(ctx, string(inputJSON))
	if err == nil {
		t.Fatal("expected error for empty data")
	}
	if !strings.Contains(err.Error(), "data is required") {
		t.Errorf("expected 'data is required' error, got: %v", err)
	}
}

func TestBlobPutDefaultsContentType(t *testing.T) {
	p, _, _ := setupHostFuncTest(t)
	ctx := hostFuncContext(context.Background(), testTenantID, "")

	input := blobPutInput{
		Key:  "default-ct-key",
		Data: []byte("data"),
		// ContentType intentionally omitted
	}
	inputJSON, _ := json.Marshal(input)

	outputJSON, err := p.blobPut(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("blobPut: %v", err)
	}

	var output blobPutOutput
	json.Unmarshal([]byte(outputJSON), &output)

	// Verify via get that content type defaulted correctly
	getInput := blobGetInput{Key: "default-ct-key"}
	getJSON, _ := json.Marshal(getInput)

	getOutput, err := p.blobGet(ctx, string(getJSON))
	if err != nil {
		t.Fatalf("blobGet: %v", err)
	}

	var getResp blobGetOutput
	json.Unmarshal([]byte(getOutput), &getResp)
	if getResp.ContentType != "application/octet-stream" {
		t.Errorf("expected default content_type 'application/octet-stream', got %q", getResp.ContentType)
	}
}

func TestBlobPutWithTTL(t *testing.T) {
	p, _, _ := setupHostFuncTest(t)
	ctx := hostFuncContext(context.Background(), testTenantID, "")

	input := blobPutInput{
		Key:  "ttl-key",
		Data: []byte("ephemeral"),
		TTL:  "1h",
	}
	inputJSON, _ := json.Marshal(input)

	outputJSON, err := p.blobPut(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("blobPut with TTL: %v", err)
	}

	var output blobPutOutput
	if err := json.Unmarshal([]byte(outputJSON), &output); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if output.Key != "ttl-key" {
		t.Errorf("expected key 'ttl-key', got %q", output.Key)
	}
}

func TestBlobPutOverwritesExisting(t *testing.T) {
	p, _, _ := setupHostFuncTest(t)
	ctx := hostFuncContext(context.Background(), testTenantID, "")

	// Put first blob
	input1 := blobPutInput{
		Key:  "overwrite-key",
		Data: []byte("original"),
	}
	input1JSON, _ := json.Marshal(input1)
	_, err := p.blobPut(ctx, string(input1JSON))
	if err != nil {
		t.Fatalf("first blobPut: %v", err)
	}

	// Overwrite with new data
	input2 := blobPutInput{
		Key:  "overwrite-key",
		Data: []byte("replacement"),
	}
	input2JSON, _ := json.Marshal(input2)
	_, err = p.blobPut(ctx, string(input2JSON))
	if err != nil {
		t.Fatalf("second blobPut: %v", err)
	}

	// Get should return the new data
	getInput := blobGetInput{Key: "overwrite-key"}
	getJSON, _ := json.Marshal(getInput)

	getOutput, err := p.blobGet(ctx, string(getJSON))
	if err != nil {
		t.Fatalf("blobGet: %v", err)
	}

	var getResp blobGetOutput
	json.Unmarshal([]byte(getOutput), &getResp)
	if string(getResp.Data) != "replacement" {
		t.Errorf("expected 'replacement', got %q", string(getResp.Data))
	}
}

func TestBlobPutGetWithWorkflowID(t *testing.T) {
	p, _, _ := setupHostFuncTest(t)
	ctx := hostFuncContext(context.Background(), testTenantID, "test-workflow-123")

	// Put with a workflow ID
	input := blobPutInput{
		Key:  "wf-blob",
		Data: []byte("workflow data"),
	}
	inputJSON, _ := json.Marshal(input)

	_, err := p.blobPut(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("blobPut with workflow ID: %v", err)
	}

	// Get with the same workflow ID should work
	getInput := blobGetInput{Key: "wf-blob"}
	getJSON, _ := json.Marshal(getInput)

	getOutput, err := p.blobGet(ctx, string(getJSON))
	if err != nil {
		t.Fatalf("blobGet with workflow ID: %v", err)
	}

	var getResp blobGetOutput
	json.Unmarshal([]byte(getOutput), &getResp)
	if string(getResp.Data) != "workflow data" {
		t.Errorf("expected 'workflow data', got %q", string(getResp.Data))
	}
}

func TestBlobGetExpired(t *testing.T) {
	p, _, clock := setupHostFuncTest(t)
	ctx := hostFuncContext(context.Background(), testTenantID, "")

	// Put with a very short TTL
	input := blobPutInput{
		Key:  "expire-fast",
		Data: []byte("bye"),
		TTL:  "1s",
	}
	inputJSON, _ := json.Marshal(input)
	_, err := p.blobPut(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("blobPut: %v", err)
	}

	// The expires_at was stored using time.Now(), but the store's clock is separate.
	// We simulate expiry by advancing the store's clock. Since blobPut uses time.Now()
	// for expiresAt but queryBlobByKey uses store.now() for comparison, we need the
	// store clock to advance past the expiry time.
	//
	// To make this reliable, we put with a TTL of 10ms and wait briefly, then set
	// the store clock to the future.
	// Actually, let's do this differently: advance the store clock well past now
	// so that the stored expiresAt (which was computed from time.Now() during the put)
	// appears to be in the past relative to the store's controllable clock.

	// Instead, simply put a blob where we manually set the store state to be expired.
	// But we went through blobPut which used time.Now() for expiresAt.
	// The store's clock and real time diverge, so we can advance the store clock.

	// The store's fake clock starts at time.Now() (from newFakeClock).
	// blobPut used time.Now() to compute expiresAt = time.Now() + 1s.
	// The store's now() returns the clock's time which started at time.Now().
	// If we advance the clock by 2s, store.now() > expiresAt.
	clock.Advance(2 * time.Second)

	// Now try to get it — the fake DB's queryBlobByKey checks expiresAt < store.now()
	getInput := blobGetInput{Key: "expire-fast"}
	getJSON, _ := json.Marshal(getInput)

	_, err = p.blobGet(ctx, string(getJSON))
	if err == nil {
		t.Fatal("expected error for expired blob")
	}
	if !strings.Contains(err.Error(), "blob not found") {
		t.Errorf("expected 'blob not found' error (fake DB returns expired as not found), got: %v", err)
	}
}

func TestBlobGetNotExpired(t *testing.T) {
	p, _, _ := setupHostFuncTest(t)
	ctx := hostFuncContext(context.Background(), testTenantID, "")

	// Put with a long TTL
	input := blobPutInput{
		Key:  "long-lived",
		Data: []byte("still here"),
		TTL:  "24h",
	}
	inputJSON, _ := json.Marshal(input)
	_, err := p.blobPut(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("blobPut: %v", err)
	}

	// Get should succeed
	getInput := blobGetInput{Key: "long-lived"}
	getJSON, _ := json.Marshal(getInput)

	_, err = p.blobGet(ctx, string(getJSON))
	if err != nil {
		t.Fatalf("blobGet: %v", err)
	}
}

func TestBlobPutContentAddressing(t *testing.T) {
	p, store, _ := setupHostFuncTest(t)
	ctx := hostFuncContext(context.Background(), testTenantID, "")

	// Same content under two different keys should share ref_count
	data := []byte("shared content")
	input1 := blobPutInput{Key: "key-a", Data: data}
	input2 := blobPutInput{Key: "key-b", Data: data}

	input1JSON, _ := json.Marshal(input1)
	input2JSON, _ := json.Marshal(input2)

	if _, err := p.blobPut(ctx, string(input1JSON)); err != nil {
		t.Fatalf("blobPut key-a: %v", err)
	}
	if _, err := p.blobPut(ctx, string(input2JSON)); err != nil {
		t.Fatalf("blobPut key-b: %v", err)
	}

	sha256Hex := fmt.Sprintf("%x", sha256.Sum256(data))
	store.mu.RLock()
	cr, ok := store.blobContent[sha256Hex]
	store.mu.RUnlock()
	if !ok {
		t.Fatal("expected blob_content entry")
	}
	if cr.refCount != 2 {
		t.Errorf("expected ref_count=2 for shared content, got %d", cr.refCount)
	}
}

// RegisterHostFunctions should return an error when given a nil registry.
func TestRegisterHostFunctionsNilScope(t *testing.T) {
	p := &Plugin{logger: slog.Default()}
	err := p.RegisterHostFunctions(nil)
	if err == nil {
		t.Fatal("expected error for nil scope")
	}
	if !strings.Contains(err.Error(), "nil function registry") {
		t.Errorf("expected 'nil function registry' error, got: %v", err)
	}
}

// fakeScope implements plugin.FuncRegistry for testing registration.
type fakeScope struct {
	mu  sync.Mutex
	fns map[string]plugin.FuncOptions
}

func newFakeScope() *fakeScope {
	return &fakeScope{fns: make(map[string]plugin.FuncOptions)}
}

func (s *fakeScope) Register(opts plugin.FuncOptions, _ plugin.PluginFunc) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if opts.Name == "" {
		return fmt.Errorf("fakeScope: name required")
	}
	if _, exists := s.fns[opts.Name]; exists {
		return fmt.Errorf("fakeScope: already registered: %s", opts.Name)
	}
	s.fns[opts.Name] = opts
	return nil
}

func TestRegisterHostFunctions(t *testing.T) {
	p := &Plugin{logger: slog.Default()}
	scope := newFakeScope()

	if err := p.RegisterHostFunctions(scope); err != nil {
		t.Fatalf("RegisterHostFunctions: %v", err)
	}

	// Verify both functions were registered
	scope.mu.Lock()
	putOpts, hasPut := scope.fns["put"]
	getOpts, hasGet := scope.fns["get"]
	scope.mu.Unlock()

	if !hasPut {
		t.Error("expected 'put' function to be registered")
	}
	if !hasGet {
		t.Error("expected 'get' function to be registered")
	}

	if putOpts.Idempotent {
		t.Error("expected put.Idempotent to be false")
	}
	if !getOpts.Idempotent {
		t.Error("expected get.Idempotent to be true (idempotent/replay-safe)")
	}
}
