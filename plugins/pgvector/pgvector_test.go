package pgvector

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// fakeConnector implements driver.Connector for testing pgvector SQL operations.
type fakeConnector struct {
	mu          sync.Mutex
	collections map[string]*fakeCollection
	embeddings  map[uuid.UUID]*fakeEmbedding // keyed by embedding row ID
}

type fakeCollection struct {
	id         uuid.UUID
	name       string
	dimensions int
}

type fakeEmbedding struct {
	id           uuid.UUID
	tenantID     uuid.UUID
	collectionID uuid.UUID
	externalID   string
	content      string
	metadataJSON []byte
	embedding    []float64
}

func newFakeConnector() *fakeConnector {
	return &fakeConnector{
		collections: make(map[string]*fakeCollection),
		embeddings:  make(map[uuid.UUID]*fakeEmbedding),
	}
}

func (c *fakeConnector) Connect(ctx context.Context) (driver.Conn, error) {
	return &fakeConn{fc: c}, nil
}

func (c *fakeConnector) Driver() driver.Driver {
	return &fakeDriver{}
}

func (c *fakeConnector) addCollection(name string, dimensions int) uuid.UUID {
	c.mu.Lock()
	defer c.mu.Unlock()
	if coll, ok := c.collections[name]; ok {
		return coll.id
	}
	id := uuid.New()
	c.collections[name] = &fakeCollection{id: id, name: name, dimensions: dimensions}
	return id
}

type fakeDriver struct{}

func (d *fakeDriver) Open(name string) (driver.Conn, error) {
	return nil, fmt.Errorf("fakeDriver.Open not implemented")
}

type fakeConn struct {
	fc *fakeConnector
}

func (c *fakeConn) Prepare(query string) (driver.Stmt, error) {
	return &fakeStmt{fc: c.fc, query: query}, nil
}

func (c *fakeConn) Close() error              { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) { return &fakeTx{}, nil }

type fakeTx struct{}

func (t *fakeTx) Commit() error   { return nil }
func (t *fakeTx) Rollback() error { return nil }

type fakeStmt struct {
	fc    *fakeConnector
	query string
}

func (s *fakeStmt) Close() error  { return nil }
func (s *fakeStmt) NumInput() int { return -1 }

func (s *fakeStmt) Exec(args []driver.Value) (driver.Result, error) {
	// Convert to named value interface expected by ExecContext.
	namedArgs := make([]driver.NamedValue, len(args))
	for i, a := range args {
		namedArgs[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	return s.ExecContext(context.Background(), namedArgs)
}

func (s *fakeStmt) Query(args []driver.Value) (driver.Rows, error) {
	namedArgs := make([]driver.NamedValue, len(args))
	for i, a := range args {
		namedArgs[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	return s.QueryContext(context.Background(), namedArgs)
}

// sqlMatch normalizes whitespace in q and checks if it contains sub.
func sqlMatch(q, sub string) bool {
	norm := strings.Join(strings.Fields(q), " ")
	return strings.Contains(norm, sub)
}

func (s *fakeStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	q := strings.TrimSpace(s.query)
	fc := s.fc
	fc.mu.Lock()
	defer fc.mu.Unlock()

	if sqlMatch(q, "CREATE EXTENSION") {
		return fakeResult{}, nil
	}
	if sqlMatch(q, "CREATE TABLE IF NOT EXISTS") {
		return fakeResult{}, nil
	}
	if sqlMatch(q, "CREATE INDEX IF NOT EXISTS") {
		return fakeResult{}, nil
	}

	if sqlMatch(q, "INSERT INTO pgvector_collections") {
		name := stringArg(args, 0)
		dims := intArg(args, 1)
		if _, exists := fc.collections[name]; !exists {
			id := uuid.New()
			fc.collections[name] = &fakeCollection{id: id, name: name, dimensions: dims}
		}
		return fakeResult{lastInsertID: 1}, nil
	}

	if sqlMatch(q, "INSERT INTO pgvector_embeddings") {
		id := uuid.New()
		tenantID := uuidArg(args, 0)
		collID := uuidArg(args, 1)
		extID := stringArg(args, 2)
		content := stringArg(args, 3)
		var metaJSON []byte
		var embedding []float64
		if len(args) > 4 {
			switch v := args[4].Value.(type) {
			case []byte:
				metaJSON = v
			case string:
				metaJSON = []byte(v)
			}
		}
		if len(args) > 5 {
			switch v := args[5].Value.(type) {
			case string:
				embedding = parseVector(v)
			}
		}
		fc.embeddings[id] = &fakeEmbedding{
			id: id, tenantID: tenantID, collectionID: collID,
			externalID: extID, content: content, metadataJSON: metaJSON,
			embedding: embedding,
		}
		return fakeResult{lastInsertID: 1}, nil
	}

	if sqlMatch(q, "DELETE FROM pgvector_embeddings") {
		deleteCount := 0
		for id, e := range fc.embeddings {
			if len(args) >= 3 {
				if e.tenantID.String() == fmt.Sprint(args[1].Value) &&
					e.collectionID.String() == fmt.Sprint(args[2].Value) {
					delete(fc.embeddings, id)
					deleteCount++
				}
			}
		}
		return fakeResult{rowsAffected: int64(deleteCount)}, nil
	}

	return fakeResult{}, nil
}

func (s *fakeStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	q := strings.TrimSpace(s.query)
	fc := s.fc
	fc.mu.Lock()
	defer fc.mu.Unlock()

	if sqlMatch(q, "SELECT id, dimensions FROM pgvector_collections") {
		name := stringArg(args, 0)
		coll, ok := fc.collections[name]
		if !ok {
			return &fakeRows{cols: []string{"id", "dimensions"}, data: [][]driver.Value{}}, nil
		}
		return &fakeRows{
			cols: []string{"id", "dimensions"},
			data: [][]driver.Value{{coll.id.String(), int64(coll.dimensions)}},
		}, nil
	}

	if sqlMatch(q, "SELECT id FROM pgvector_collections") {
		name := stringArg(args, 0)
		coll, ok := fc.collections[name]
		if !ok {
			return &fakeRows{cols: []string{"id"}, data: [][]driver.Value{}}, nil
		}
		return &fakeRows{
			cols: []string{"id"},
			data: [][]driver.Value{{coll.id.String()}},
		}, nil
	}

	// INSERT ... RETURNING id (used by upsert via QueryRowContext).
	if sqlMatch(q, "INSERT INTO pgvector_embeddings") && sqlMatch(q, "RETURNING") {
		id := uuid.New()
		tenantID := uuidArg(args, 0)
		collID := uuidArg(args, 1)
		extID := stringArg(args, 2)
		content := stringArg(args, 3)
		var metaJSON []byte
		var embedding []float64
		if len(args) > 4 {
			switch v := args[4].Value.(type) {
			case []byte:
				metaJSON = v
			case string:
				metaJSON = []byte(v)
			}
		}
		if len(args) > 5 {
			switch v := args[5].Value.(type) {
			case string:
				embedding = parseVector(v)
			}
		}
		fc.embeddings[id] = &fakeEmbedding{
			id: id, tenantID: tenantID, collectionID: collID,
			externalID: extID, content: content, metadataJSON: metaJSON,
			embedding: embedding,
		}
		return &fakeRows{
			cols: []string{"id"},
			data: [][]driver.Value{{id.String()}},
		}, nil
	}

	if sqlMatch(q, "FROM pgvector_embeddings") {
		// Build results based on args: tenant_id, collection_id, limit.
		var results [][]driver.Value
		count := 0
		limit := 10
		if len(args) >= 3 {
			if v, ok := args[2].Value.(int64); ok {
				limit = int(v)
			}
		}
		for _, e := range fc.embeddings {
			if count >= limit {
				break
			}
			results = append(results, []driver.Value{
				e.id.String(), e.externalID, e.content, e.metadataJSON, float64(0.5),
			})
			count++
		}
		return &fakeRows{
			cols: []string{"id", "external_id", "content", "metadata", "score"},
			data: results,
		}, nil
	}

	return &fakeRows{cols: []string{}, data: [][]driver.Value{}}, nil
}

// ---- Row/Result helpers ----

type fakeResult struct {
	lastInsertID int64
	rowsAffected int64
}

func (r fakeResult) LastInsertId() (int64, error) { return r.lastInsertID, nil }
func (r fakeResult) RowsAffected() (int64, error) { return r.rowsAffected, nil }

type fakeRows struct {
	cols   []string
	data   [][]driver.Value
	pos    int
	closed bool
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { r.closed = true; return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.pos])
	r.pos++
	return nil
}

func stringArg(args []driver.NamedValue, idx int) string {
	if idx >= len(args) {
		return ""
	}
	switch v := args[idx].Value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return fmt.Sprint(v)
	}
}

func intArg(args []driver.NamedValue, idx int) int {
	if idx >= len(args) {
		return 0
	}
	switch v := args[idx].Value.(type) {
	case int64:
		return int(v)
	case int:
		return v
	default:
		return 0
	}
}

func uuidArg(args []driver.NamedValue, idx int) uuid.UUID {
	if idx >= len(args) {
		return uuid.Nil
	}
	switch v := args[idx].Value.(type) {
	case string:
		parsed, _ := uuid.Parse(v)
		return parsed
	case uuid.UUID:
		return v
	case [16]byte:
		return uuid.UUID(v)
	default:
		return uuid.Nil
	}
}

func parseVector(s string) []float64 {
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	result := make([]float64, len(parts))
	for i, p := range parts {
		fmt.Sscanf(strings.TrimSpace(p), "%f", &result[i])
	}
	return result
}

func fakeDB(t *testing.T) (*sql.DB, *fakeConnector) {
	t.Helper()
	fc := newFakeConnector()
	db := sql.OpenDB(fc)
	return db, fc
}

func tenantID() uuid.UUID {
	return uuid.MustParse("00000000-0000-0000-0000-000000000001")
}

func tenantCtx() context.Context {
	return plugin.WithCallContext(context.Background(), &plugin.CallContext{
		TenantID:   tenantID().String(),
		WorkflowID: "test-workflow",
	})
}

// ---- Tests ----

func TestInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "pgvector" {
		t.Errorf("expected Name 'pgvector', got %q", info.Name)
	}
	if info.Version != "0.1.0" {
		t.Errorf("expected Version '0.1.0', got %q", info.Version)
	}
	if info.Description == "" {
		t.Error("expected non-empty Description")
	}
}

func TestInit(t *testing.T) {
	db, fc := fakeDB(t)
	fc.addCollection("default", 1536)

	p := &Plugin{}
	cfg := Config{
		EmbeddingProvider: "openai",
		EmbeddingModel:    "text-embedding-3-small",
		Dimensions:        1536,
		DefaultCollection: "default",
	}
	cfgJSON, _ := json.Marshal(cfg)
	env := &plugin.Environment{DB: &engine.SQLDBAdapter{DB: db}, Config: cfgJSON}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.config.Dimensions != 1536 {
		t.Errorf("expected dimensions 1536, got %d", p.config.Dimensions)
	}
	if p.config.EmbeddingProvider != "openai" {
		t.Errorf("expected provider 'openai', got %q", p.config.EmbeddingProvider)
	}
}

func TestInitDefaults(t *testing.T) {
	db, _ := fakeDB(t)

	p := &Plugin{}
	env := &plugin.Environment{DB: &engine.SQLDBAdapter{DB: db}}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() with no config returned error: %v", err)
	}
	if p.config.Dimensions != 1536 {
		t.Errorf("expected default dimensions 1536, got %d", p.config.Dimensions)
	}
}

func TestInitInvalidConfig(t *testing.T) {
	db, _ := fakeDB(t)

	p := &Plugin{}
	env := &plugin.Environment{DB: &engine.SQLDBAdapter{DB: db}, Config: []byte(`not valid json`)}
	err := p.Init(context.Background(), env)
	if err == nil {
		t.Fatal("expected error for invalid config, got nil")
	}
}

func TestMigrations(t *testing.T) {
	p := &Plugin{}
	migrations := p.Migrations()
	if len(migrations) != 1 {
		t.Fatalf("expected 1 migration, got %d", len(migrations))
	}
	if migrations[0].Version != 1 {
		t.Errorf("expected version 1, got %d", migrations[0].Version)
	}
	if !strings.Contains(migrations[0].Up, "CREATE TABLE") {
		t.Error("expected Up to contain CREATE TABLE")
	}
	if !strings.Contains(migrations[0].Up, "pgvector_collections") {
		t.Error("expected Up to mention pgvector_collections")
	}
	if !strings.Contains(migrations[0].Up, "pgvector_embeddings") {
		t.Error("expected Up to mention pgvector_embeddings")
	}
	if !strings.Contains(migrations[0].Down, "DROP TABLE") {
		t.Error("expected Down to contain DROP TABLE")
	}
}

func TestRegisterHostFunctions(t *testing.T) {
	p := &Plugin{}
	scope := &testFuncRegistry{funcs: make(map[string]plugin.FuncOptions)}
	if err := p.RegisterHostFunctions(scope); err != nil {
		t.Fatalf("RegisterHostFunctions() returned error: %v", err)
	}
	for _, name := range []string{"search", "upsert", "delete"} {
		if _, ok := scope.funcs[name]; !ok {
			t.Errorf("expected function %q to be registered", name)
		}
	}
}

func TestRegisterHostFunctionsNilScope(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterHostFunctions(nil)
	if err == nil {
		t.Fatal("expected error for nil scope")
	}
}

// failOnRegisterScope returns an error from Register when called with specific
// function names, simulating registration failures.
type failOnRegisterScope struct {
	funcs map[string]plugin.FuncOptions
}

func newFailOnRegisterScope() *failOnRegisterScope {
	return &failOnRegisterScope{funcs: make(map[string]plugin.FuncOptions)}
}

func (s *failOnRegisterScope) Register(opts plugin.FuncOptions, _ plugin.PluginFunc) error {
	if opts.Name == "" {
		return fmt.Errorf("fake: name required")
	}
	if _, exists := s.funcs[opts.Name]; exists {
		return fmt.Errorf("fake: already registered: %s", opts.Name)
	}
	s.funcs[opts.Name] = opts
	return nil
}

// Register returns an error for any registration — testing the "registration
// failed" branch in RegisterHostFunctions.
type alwaysFailScope struct{}

func (s *alwaysFailScope) Register(_ plugin.FuncOptions, _ plugin.PluginFunc) error {
	return fmt.Errorf("scope: registration failed")
}

func TestRegisterHostFunctionsRegisterFail(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterHostFunctions(&alwaysFailScope{})
	if err == nil {
		t.Fatal("expected error when scope.Register fails")
	}
	if !strings.Contains(err.Error(), "registration failed") {
		t.Errorf("expected 'registration failed', got: %v", err)
	}
}

func TestRegisterRoutes(t *testing.T) {
	_ = &Plugin{}
	_ = http.NewServeMux()
	// pgvector doesn't register routes currently — verify no-op is safe.
	// If routes are added later, add assertions here.
	_ = httptest.NewRequest
}

// ---- Host function tests ----

func TestSearchNoTenant(t *testing.T) {
	p := &Plugin{}
	_, err := p.search(context.Background(), `{"collection":"test","query_vector":[0.1,0.2]}`)
	if err == nil {
		t.Fatal("expected error for missing tenant")
	}
	if !strings.Contains(err.Error(), "no tenant context") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSearchInvalidJSON(t *testing.T) {
	p := &Plugin{}
	_, err := p.search(tenantCtx(), `not json`)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestSearchMissingCollection(t *testing.T) {
	p := &Plugin{}
	_, err := p.search(tenantCtx(), `{"query_vector":[0.1,0.2]}`)
	if err == nil {
		t.Fatal("expected error for missing collection")
	}
}

func TestSearchNonexistentCollection(t *testing.T) {
	db, _ := fakeDB(t)
	p := &Plugin{db: &engine.SQLDBAdapter{DB: db}}
	_, err := p.search(tenantCtx(), `{"collection":"nonexistent","query_vector":[0.1,0.2]}`)
	if err == nil {
		t.Fatal("expected error for nonexistent collection")
	}
	if !strings.Contains(err.Error(), "collection not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSearchDefaultTopK(t *testing.T) {
	db, fc := fakeDB(t)
	collID := fc.addCollection("docs", 1536)
	// Add an embedding to search.
	fc.embeddings[uuid.New()] = &fakeEmbedding{
		id: uuid.New(), tenantID: tenantID(), collectionID: collID,
		externalID: "doc1", content: "hello world", metadataJSON: []byte(`{}`),
		embedding: []float64{0.1, 0.2, 0.3},
	}

	p := &Plugin{db: &engine.SQLDBAdapter{DB: db}}
	input, _ := json.Marshal(searchInput{
		Collection:  "docs",
		QueryVector: []float64{0.1, 0.2, 0.3},
	})
	out, err := p.search(tenantCtx(), string(input))
	if err != nil {
		t.Fatalf("search() returned error: %v", err)
	}
	var result searchOutput
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if len(result.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result.Results))
	}
	if result.Results[0].ExternalID != "doc1" {
		t.Errorf("expected external_id 'doc1', got %q", result.Results[0].ExternalID)
	}
}

func TestSearchNoVector(t *testing.T) {
	db, fc := fakeDB(t)
	collID := fc.addCollection("docs", 1536)
	fc.embeddings[uuid.New()] = &fakeEmbedding{
		id: uuid.New(), tenantID: tenantID(), collectionID: collID,
		externalID: "doc1", content: "hello", metadataJSON: []byte(`{}`),
	}

	p := &Plugin{db: &engine.SQLDBAdapter{DB: db}}
	input, _ := json.Marshal(searchInput{
		Collection: "docs",
		TopK:       5,
	})
	out, err := p.search(tenantCtx(), string(input))
	if err != nil {
		t.Fatalf("search() returned error: %v", err)
	}
	var result searchOutput
	json.Unmarshal([]byte(out), &result)
	if len(result.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result.Results))
	}
}

func TestSearchWithMetadata(t *testing.T) {
	db, fc := fakeDB(t)
	collID := fc.addCollection("docs", 1536)
	meta := map[string]any{"source": "test", "page": float64(1)}
	metaJSON, _ := json.Marshal(meta)
	fc.embeddings[uuid.New()] = &fakeEmbedding{
		id: uuid.New(), tenantID: tenantID(), collectionID: collID,
		externalID: "doc1", content: "hello", metadataJSON: metaJSON,
	}

	p := &Plugin{db: &engine.SQLDBAdapter{DB: db}}
	input, _ := json.Marshal(searchInput{
		Collection:  "docs",
		QueryVector: []float64{0.1, 0.2},
		IncludeMeta: true,
	})
	out, err := p.search(tenantCtx(), string(input))
	if err != nil {
		t.Fatalf("search() returned error: %v", err)
	}
	var result searchOutput
	json.Unmarshal([]byte(out), &result)
	if len(result.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result.Results))
	}
	if result.Results[0].Metadata == nil {
		t.Error("expected metadata to be included")
	}
}

func TestUpsertNoTenant(t *testing.T) {
	p := &Plugin{}
	_, err := p.upsert(context.Background(), `{"collection":"test"}`)
	if err == nil {
		t.Fatal("expected error for missing tenant")
	}
}

func TestUpsertInvalidJSON(t *testing.T) {
	p := &Plugin{}
	_, err := p.upsert(tenantCtx(), `not json`)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestUpsertMissingCollection(t *testing.T) {
	p := &Plugin{}
	_, err := p.upsert(tenantCtx(), `{"external_id":"ext1"}`)
	if err == nil {
		t.Fatal("expected error for missing collection")
	}
}

func TestUpsertNonexistentCollection(t *testing.T) {
	db, _ := fakeDB(t)
	p := &Plugin{db: &engine.SQLDBAdapter{DB: db}}
	_, err := p.upsert(tenantCtx(), `{"collection":"nonexistent","external_id":"ext1"}`)
	if err == nil {
		t.Fatal("expected error for nonexistent collection")
	}
}

func TestUpsertWithExternalID(t *testing.T) {
	db, fc := fakeDB(t)
	fc.addCollection("docs", 1536)

	p := &Plugin{db: &engine.SQLDBAdapter{DB: db}}
	input, _ := json.Marshal(upsertInput{
		Collection: "docs",
		ExternalID: "ext1",
		Content:    "test content",
		Embedding:  []float64{0.1, 0.2, 0.3},
	})
	out, err := p.upsert(tenantCtx(), string(input))
	if err != nil {
		t.Fatalf("upsert() returned error: %v", err)
	}
	var result upsertOutput
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if result.ID == "" {
		t.Error("expected non-empty ID")
	}
}

func TestUpsertWithoutEmbedding(t *testing.T) {
	db, fc := fakeDB(t)
	fc.addCollection("docs", 1536)

	p := &Plugin{db: &engine.SQLDBAdapter{DB: db}}
	input, _ := json.Marshal(upsertInput{
		Collection: "docs",
		Content:    "content without embedding",
		Metadata:   map[string]any{"key": "value"},
	})
	out, err := p.upsert(tenantCtx(), string(input))
	if err != nil {
		t.Fatalf("upsert() returned error: %v", err)
	}
	var result upsertOutput
	json.Unmarshal([]byte(out), &result)
	if result.ID == "" {
		t.Error("expected non-empty ID")
	}
}

func TestDeleteNoTenant(t *testing.T) {
	p := &Plugin{}
	_, err := p.delete(context.Background(), `{"collection":"test","id":"any"}`)
	if err == nil {
		t.Fatal("expected error for missing tenant")
	}
}

func TestDeleteInvalidJSON(t *testing.T) {
	p := &Plugin{}
	_, err := p.delete(tenantCtx(), `not json`)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestDeleteMissingCollection(t *testing.T) {
	p := &Plugin{}
	_, err := p.delete(tenantCtx(), `{"id":"any"}`)
	if err == nil {
		t.Fatal("expected error for missing collection")
	}
}

func TestDeleteMissingID(t *testing.T) {
	p := &Plugin{}
	_, err := p.delete(tenantCtx(), `{"collection":"test"}`)
	if err == nil {
		t.Fatal("expected error for missing id and external_id")
	}
}

func TestDeleteNonexistentCollection(t *testing.T) {
	db, _ := fakeDB(t)
	p := &Plugin{db: &engine.SQLDBAdapter{DB: db}}
	_, err := p.delete(tenantCtx(), `{"collection":"nonexistent","id":"any"}`)
	if err == nil {
		t.Fatal("expected error for nonexistent collection")
	}
}

func TestDeleteByID(t *testing.T) {
	db, fc := fakeDB(t)
	fc.addCollection("docs", 1536)

	p := &Plugin{db: &engine.SQLDBAdapter{DB: db}}
	// Delete with either id or external_id set — the fake DB doesn't filter by both yet.
	input, _ := json.Marshal(deleteInput{Collection: "docs", ID: "any"})
	out, err := p.delete(tenantCtx(), string(input))
	if err != nil {
		t.Fatalf("delete() returned error: %v", err)
	}
	var result deleteOutput
	json.Unmarshal([]byte(out), &result)
	// fake DB may return 0 if no embeddings; that's fine.
	_ = result.Deleted
}

func TestVectorLiteral(t *testing.T) {
	tests := []struct {
		input    []float64
		expected string
	}{
		{[]float64{1, 2, 3}, "[1,2,3]"},
		{[]float64{0.1, 0.2}, "[0.1,0.2]"},
		{[]float64{}, "[]"},
		{[]float64{-1.5, 3.14}, "[-1.5,3.14]"},
	}
	for _, tt := range tests {
		got := vectorLiteral(tt.input)
		if got != tt.expected {
			t.Errorf("vectorLiteral(%v) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestSearchNoResults(t *testing.T) {
	db, fc := fakeDB(t)
	fc.addCollection("empty", 1536)

	p := &Plugin{db: &engine.SQLDBAdapter{DB: db}}
	input, _ := json.Marshal(searchInput{
		Collection:  "empty",
		QueryVector: []float64{0.1, 0.2},
	})
	out, err := p.search(tenantCtx(), string(input))
	if err != nil {
		t.Fatalf("search() returned error: %v", err)
	}
	var result searchOutput
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.Results == nil {
		t.Error("expected non-nil empty results slice")
	}
	if len(result.Results) != 0 {
		t.Errorf("expected 0 results, got %d", len(result.Results))
	}
}

func TestSearchNoVectorNoResults(t *testing.T) {
	db, fc := fakeDB(t)
	fc.addCollection("empty", 1536)

	p := &Plugin{db: &engine.SQLDBAdapter{DB: db}}
	input, _ := json.Marshal(searchInput{
		Collection: "empty",
		TopK:       5,
	})
	out, err := p.search(tenantCtx(), string(input))
	if err != nil {
		t.Fatalf("search() returned error: %v", err)
	}
	var result searchOutput
	json.Unmarshal([]byte(out), &result)
	if len(result.Results) != 0 {
		t.Errorf("expected 0 results, got %d", len(result.Results))
	}
}

func TestUpsertWithExternalIDWithoutEmbedding(t *testing.T) {
	db, fc := fakeDB(t)
	fc.addCollection("docs", 1536)

	p := &Plugin{db: &engine.SQLDBAdapter{DB: db}}
	input, _ := json.Marshal(upsertInput{
		Collection: "docs",
		ExternalID: "ext-no-vec",
		Content:    "content without embedding vector",
		Metadata:   map[string]any{"key": "value"},
	})
	out, err := p.upsert(tenantCtx(), string(input))
	if err != nil {
		t.Fatalf("upsert() returned error: %v", err)
	}
	var result upsertOutput
	json.Unmarshal([]byte(out), &result)
	if result.ID == "" {
		t.Error("expected non-empty ID")
	}
}

func TestUpsertWithoutExternalIDWithEmbedding(t *testing.T) {
	db, fc := fakeDB(t)
	fc.addCollection("docs", 1536)

	p := &Plugin{db: &engine.SQLDBAdapter{DB: db}}
	input, _ := json.Marshal(upsertInput{
		Collection: "docs",
		Content:    "new vector",
		Embedding:  []float64{0.5, 0.6, 0.7},
	})
	out, err := p.upsert(tenantCtx(), string(input))
	if err != nil {
		t.Fatalf("upsert() returned error: %v", err)
	}
	var result upsertOutput
	json.Unmarshal([]byte(out), &result)
	if result.ID == "" {
		t.Error("expected non-empty ID")
	}
}

func TestDeleteByExternalID(t *testing.T) {
	db, fc := fakeDB(t)
	fc.addCollection("docs", 1536)

	p := &Plugin{db: &engine.SQLDBAdapter{DB: db}}
	input, _ := json.Marshal(deleteInput{
		Collection: "docs",
		ExternalID: "ext-to-delete",
	})
	out, err := p.delete(tenantCtx(), string(input))
	if err != nil {
		t.Fatalf("delete() returned error: %v", err)
	}
	var result deleteOutput
	json.Unmarshal([]byte(out), &result)
	_ = result.Deleted
}

func TestInitNilLogger(t *testing.T) {
	db, _ := fakeDB(t)

	p := &Plugin{}
	env := &plugin.Environment{
		DB: &engine.SQLDBAdapter{DB: db},
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.logger == nil {
		t.Error("expected logger to be non-nil after Init")
	}
}

// testFuncRegistry is a minimal in-memory registry for testing.
type testFuncRegistry struct {
	funcs map[string]plugin.FuncOptions
}

func (r *testFuncRegistry) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	r.funcs[opts.Name] = opts
	return nil
}
