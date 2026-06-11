package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
)

// ---------------------------------------------------------------------------
// dbWorkflowState
// ---------------------------------------------------------------------------

func TestDBWorkflowState_Version(t *testing.T) {
	s := &dbWorkflowState{version: 42, minVersion: 10}
	if v := s.Version(); v != 42 {
		t.Errorf("Version() = %d, want 42", v)
	}
}

func TestDBWorkflowState_MinVersion(t *testing.T) {
	s := &dbWorkflowState{version: 42, minVersion: 10}
	if v := s.MinVersion(); v != 10 {
		t.Errorf("MinVersion() = %d, want 10", v)
	}
}

func TestDBWorkflowState_Defaults(t *testing.T) {
	s := &dbWorkflowState{}
	if v := s.Version(); v != 0 {
		t.Errorf("default Version() = %d, want 0", v)
	}
	if v := s.MinVersion(); v != 0 {
		t.Errorf("default MinVersion() = %d, want 0", v)
	}
}

// ---------------------------------------------------------------------------
// dbServiceCaller.Call
// ---------------------------------------------------------------------------

func TestDBCaller_CallUnknownService(t *testing.T) {
	c := &dbServiceCaller{}
	_, err := c.Call(context.Background(), "unknown", "op", `{}`)
	if err == nil {
		t.Fatal("expected error for unknown service")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("expected 'not configured' error, got: %v", err)
	}
}

func TestDBCaller_CallHTTPFetchInvalidJSON(t *testing.T) {
	c := &dbServiceCaller{}
	_, err := c.Call(context.Background(), "http", "fetch", `{invalid}`)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid request JSON") {
		t.Errorf("expected 'invalid request JSON' error, got: %v", err)
	}
}

func TestDBCaller_CallHTTPFetchEmptyURL(t *testing.T) {
	c := &dbServiceCaller{}
	_, err := c.Call(context.Background(), "http", "fetch", `{"url":""}`)
	if err == nil {
		t.Fatal("expected error for empty URL")
	}
	if !strings.Contains(err.Error(), "url is required") {
		t.Errorf("expected 'url is required' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// hostPluginRegistryAdapter.Register
// ---------------------------------------------------------------------------

func TestRegistryAdapter_RegisterEmptyName(t *testing.T) {
	reg := engine.NewPluginRegistry()
	adapter := &hostPluginRegistryAdapter{
		registry:       reg,
		streamRegistry: engine.NewPluginStreamRegistry(),
		pluginName:     "test-plugin",
	}
	err := adapter.Register(plugin.FuncOptions{Name: ""}, nil)
	if err == nil {
		t.Fatal("expected error for empty name")
	}
	if !strings.Contains(err.Error(), "name must not be empty") {
		t.Errorf("expected 'name must not be empty', got: %v", err)
	}
}

func TestRegistryAdapter_RegisterInvalidChars(t *testing.T) {
	reg := engine.NewPluginRegistry()
	adapter := &hostPluginRegistryAdapter{
		registry:       reg,
		streamRegistry: engine.NewPluginStreamRegistry(),
		pluginName:     "test-plugin",
	}
	err := adapter.Register(plugin.FuncOptions{Name: "bad/name"}, nil)
	if err == nil {
		t.Fatal("expected error for invalid chars")
	}
	if !strings.Contains(err.Error(), "invalid characters") {
		t.Errorf("expected 'invalid characters' error, got: %v", err)
	}
}

func TestRegistryAdapter_RegisterNullByte(t *testing.T) {
	reg := engine.NewPluginRegistry()
	adapter := &hostPluginRegistryAdapter{
		registry:       reg,
		streamRegistry: engine.NewPluginStreamRegistry(),
		pluginName:     "test-plugin",
	}
	err := adapter.Register(plugin.FuncOptions{Name: "bad\x00name"}, nil)
	if err == nil {
		t.Fatal("expected error for null byte")
	}
	if !strings.Contains(err.Error(), "invalid characters") {
		t.Errorf("expected 'invalid characters' error, got: %v", err)
	}
}

func TestRegistryAdapter_RegisterDuplicate(t *testing.T) {
	reg := engine.NewPluginRegistry()
	adapter := &hostPluginRegistryAdapter{
		registry:       reg,
		streamRegistry: engine.NewPluginStreamRegistry(),
		pluginName:     "test-plugin",
	}
	fn := func(_ context.Context, _ string) (string, error) { return "ok", nil }

	if err := adapter.Register(plugin.FuncOptions{Name: "myfunc"}, fn); err != nil {
		t.Fatalf("first register should succeed: %v", err)
	}
	err := adapter.Register(plugin.FuncOptions{Name: "myfunc"}, fn)
	if err == nil {
		t.Fatal("expected error for duplicate registration")
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Errorf("expected 'already registered' error, got: %v", err)
	}
}

func TestRegistryAdapter_RegisterIdempotent(t *testing.T) {
	reg := engine.NewPluginRegistry()
	adapter := &hostPluginRegistryAdapter{
		registry:       reg,
		streamRegistry: engine.NewPluginStreamRegistry(),
		pluginName:     "test-plugin",
	}
	fn := func(_ context.Context, _ string) (string, error) { return "ok", nil }

	err := adapter.Register(plugin.FuncOptions{Name: "idemfunc", Idempotent: true}, fn)
	if err != nil {
		t.Fatalf("idempotent register should succeed: %v", err)
	}

	err = adapter.Register(plugin.FuncOptions{Name: "idemfunc", Idempotent: true}, fn)
	if err == nil {
		t.Fatal("expected error for duplicate idempotent registration")
	}
}

func TestRegistryAdapter_RegisterSuccess(t *testing.T) {
	reg := engine.NewPluginRegistry()
	adapter := &hostPluginRegistryAdapter{
		registry:       reg,
		streamRegistry: engine.NewPluginStreamRegistry(),
		pluginName:     "test-plugin",
	}
	fn := func(_ context.Context, _ string) (string, error) { return "ok", nil }

	err := adapter.Register(plugin.FuncOptions{Name: "validfunc"}, fn)
	if err != nil {
		t.Fatalf("register should succeed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// hostPluginRegistryAdapter.RegisterStream
// ---------------------------------------------------------------------------

func TestRegistryAdapter_RegisterStreamEmptyName(t *testing.T) {
	adapter := &hostPluginRegistryAdapter{
		registry:       engine.NewPluginRegistry(),
		streamRegistry: engine.NewPluginStreamRegistry(),
		pluginName:     "test-plugin",
	}
	err := adapter.RegisterStream(plugin.FuncOptions{Name: ""}, nil)
	if err == nil {
		t.Fatal("expected error for empty name")
	}
	if !strings.Contains(err.Error(), "name must not be empty") {
		t.Errorf("expected 'name must not be empty', got: %v", err)
	}
}

func TestRegistryAdapter_RegisterStreamInvalidChars(t *testing.T) {
	adapter := &hostPluginRegistryAdapter{
		registry:       engine.NewPluginRegistry(),
		streamRegistry: engine.NewPluginStreamRegistry(),
		pluginName:     "test-plugin",
	}
	err := adapter.RegisterStream(plugin.FuncOptions{Name: "bad/name"}, nil)
	if err == nil {
		t.Fatal("expected error for invalid chars")
	}
	if !strings.Contains(err.Error(), "invalid characters") {
		t.Errorf("expected 'invalid characters' error, got: %v", err)
	}
}

func TestRegistryAdapter_RegisterStreamNilRegistry(t *testing.T) {
	adapter := &hostPluginRegistryAdapter{
		registry:       engine.NewPluginRegistry(),
		streamRegistry: nil,
		pluginName:     "test-plugin",
	}
	err := adapter.RegisterStream(plugin.FuncOptions{Name: "streamfunc"}, nil)
	if err == nil {
		t.Fatal("expected error for nil stream registry")
	}
	if !strings.Contains(err.Error(), "not initialized") {
		t.Errorf("expected 'not initialized' error, got: %v", err)
	}
}

func TestRegistryAdapter_RegisterStreamDuplicate(t *testing.T) {
	reg := engine.NewPluginStreamRegistry()
	adapter := &hostPluginRegistryAdapter{
		registry:       engine.NewPluginRegistry(),
		streamRegistry: reg,
		pluginName:     "test-plugin",
	}
	sfn := func(_ context.Context, _ string) (<-chan plugin.StreamEvent, error) {
		return nil, nil
	}
	if err := adapter.RegisterStream(plugin.FuncOptions{Name: "sfunc"}, sfn); err != nil {
		t.Fatalf("first register should succeed: %v", err)
	}
	err := adapter.RegisterStream(plugin.FuncOptions{Name: "sfunc"}, sfn)
	if err == nil {
		t.Fatal("expected error for duplicate stream registration")
	}
}

func TestRegistryAdapter_RegisterStreamSuccess(t *testing.T) {
	adapter := &hostPluginRegistryAdapter{
		registry:       engine.NewPluginRegistry(),
		streamRegistry: engine.NewPluginStreamRegistry(),
		pluginName:     "test-plugin",
	}
	sfn := func(_ context.Context, _ string) (<-chan plugin.StreamEvent, error) {
		return nil, nil
	}
	err := adapter.RegisterStream(plugin.FuncOptions{Name: "validstream"}, sfn)
	if err != nil {
		t.Fatalf("register stream should succeed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// loadShardConfigs
// ---------------------------------------------------------------------------

func TestLoadShardConfigs_FileNotFound(t *testing.T) {
	_, err := loadShardConfigs("/nonexistent/shard-config.json")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
	if !strings.Contains(err.Error(), "read shards file") {
		t.Errorf("expected 'read shards file' error, got: %v", err)
	}
}

func TestLoadShardConfigs_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shards.json")
	if err := os.WriteFile(path, []byte(`not json`), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := loadShardConfigs(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "parse shards file") {
		t.Errorf("expected 'parse shards file' error, got: %v", err)
	}
}

func TestLoadShardConfigs_Empty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shards.json")
	if err := os.WriteFile(path, []byte(`[]`), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := loadShardConfigs(path)
	if err == nil {
		t.Fatal("expected error for empty shards")
	}
	if !strings.Contains(err.Error(), "no shard definitions") {
		t.Errorf("expected 'no shard definitions' error, got: %v", err)
	}
}

func TestLoadShardConfigs_EmptyName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shards.json")
	data := `[{"name": "", "conn_str": "postgres://localhost/db"}]`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := loadShardConfigs(path)
	if err == nil {
		t.Fatal("expected error for empty shard name")
	}
	if !strings.Contains(err.Error(), "empty name") {
		t.Errorf("expected 'empty name' error, got: %v", err)
	}
}

func TestLoadShardConfigs_EmptyConnStr(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shards.json")
	data := `[{"name": "shard1", "conn_str": ""}]`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := loadShardConfigs(path)
	if err == nil {
		t.Fatal("expected error for empty conn_str")
	}
	if !strings.Contains(err.Error(), "empty conn_str") {
		t.Errorf("expected 'empty conn_str' error, got: %v", err)
	}
}

func TestLoadShardConfigs_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shards.json")
	data := `[{"name": "shard1", "conn_str": "postgres://host1/db"}, {"name": "shard2", "conn_str": "postgres://host2/db"}]`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
	configs, err := loadShardConfigs(path)
	if err != nil {
		t.Fatalf("loadShardConfigs: %v", err)
	}
	if len(configs) != 2 {
		t.Errorf("expected 2 configs, got %d", len(configs))
	}
	if configs[0].Name != "shard1" || configs[1].Name != "shard2" {
		t.Errorf("unexpected configs: %+v", configs)
	}
}

// ---------------------------------------------------------------------------
// MemoryController: WorkflowMemoryEstimate, DefEstimates, LoadEstimates
// ---------------------------------------------------------------------------

func TestMemoryController_WorkflowMemoryEstimate_Default(t *testing.T) {
	monitor := NewMemoryMonitor(5 * time.Second)
	mc := NewMemoryController(monitor, nil, "test-worker", 10, 0.8, 0.95)
	est := mc.WorkflowMemoryEstimate("nonexistent")
	if est != defaultMemoryEstimate {
		t.Errorf("expected default memory estimate %d, got %d", defaultMemoryEstimate, est)
	}
}

func TestMemoryController_WorkflowMemoryEstimate_Zero(t *testing.T) {
	monitor := NewMemoryMonitor(5 * time.Second)
	mc := NewMemoryController(monitor, nil, "test-worker", 10, 0.8, 0.95)
	mc.mu.Lock()
	mc.defEstimates = map[string]float64{"mywf": 0}
	mc.mu.Unlock()
	est := mc.WorkflowMemoryEstimate("mywf")
	if est != defaultMemoryEstimate {
		t.Errorf("expected default for zero estimate %d, got %d", defaultMemoryEstimate, est)
	}
}

func TestMemoryController_WorkflowMemoryEstimate_Custom(t *testing.T) {
	monitor := NewMemoryMonitor(5 * time.Second)
	mc := NewMemoryController(monitor, nil, "test-worker", 10, 0.8, 0.95)
	mc.mu.Lock()
	mc.defEstimates = map[string]float64{"mywf": 64 * 1024 * 1024}
	mc.mu.Unlock()
	est := mc.WorkflowMemoryEstimate("mywf")
	if est != 64*1024*1024 {
		t.Errorf("expected 64MB estimate, got %d", est)
	}
}

func TestMemoryController_DefEstimates(t *testing.T) {
	monitor := NewMemoryMonitor(5 * time.Second)
	mc := NewMemoryController(monitor, nil, "test-worker", 10, 0.8, 0.95)
	mc.mu.Lock()
	mc.defEstimates = map[string]float64{"wf-a": 10.5, "wf-b": 20.5}
	mc.mu.Unlock()
	ests := mc.DefEstimates()
	if len(ests) != 2 {
		t.Errorf("expected 2 estimates, got %d", len(ests))
	}
	if ests["wf-a"] != 10.5 {
		t.Errorf("expected wf-a=10.5, got %f", ests["wf-a"])
	}
	if ests["wf-b"] != 20.5 {
		t.Errorf("expected wf-b=20.5, got %f", ests["wf-b"])
	}
	ests["new-key"] = 100
	mc.mu.RLock()
	_, exists := mc.defEstimates["new-key"]
	mc.mu.RUnlock()
	if exists {
		t.Error("DefEstimates should return a copy")
	}
}

func TestMemoryController_LoadEstimates(t *testing.T) {
	ms := &mockStore{
		loadMemoryEstimatesFn: func(_ context.Context) (map[string]float64, error) {
			return map[string]float64{"wf": 42.0}, nil
		},
	}
	monitor := NewMemoryMonitor(5 * time.Second)
	mc := NewMemoryController(monitor, ms, "test-worker", 10, 0.8, 0.95)

	if err := mc.LoadEstimates(context.Background()); err != nil {
		t.Fatalf("LoadEstimates: %v", err)
	}
	est := mc.WorkflowMemoryEstimate("wf")
	if est != 42 {
		t.Errorf("expected estimate 42, got %d", est)
	}
}

func TestMemoryController_LoadEstimates_Error(t *testing.T) {
	ms := &mockStore{
		loadMemoryEstimatesFn: func(_ context.Context) (map[string]float64, error) {
			return nil, errors.New("db error")
		},
	}
	monitor := NewMemoryMonitor(5 * time.Second)
	mc := NewMemoryController(monitor, ms, "test-worker", 10, 0.8, 0.95)

	err := mc.LoadEstimates(context.Background())
	if err == nil {
		t.Fatal("expected error from LoadEstimates")
	}
	if !strings.Contains(err.Error(), "db error") {
		t.Errorf("expected 'db error', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// MemoryMonitor: readGoHeap
// ---------------------------------------------------------------------------

func TestReadGoHeap(t *testing.T) {
	info, err := readGoHeap()
	if err != nil {
		t.Fatalf("readGoHeap: %v", err)
	}
	if info.TotalBytes == 0 {
		t.Error("readGoHeap: TotalBytes should be > 0")
	}
	if info.Source != "goheap" {
		t.Errorf("readGoHeap: Source = %q, want %q", info.Source, "goheap")
	}
	if info.CollectedAt.IsZero() {
		t.Error("readGoHeap: CollectedAt should not be zero")
	}
}

// ---------------------------------------------------------------------------
// emitMemoryMetrics
// ---------------------------------------------------------------------------

func TestMemoryMetrics(t *testing.T) {
	monitor := NewMemoryMonitor(5 * time.Second)
	mc := NewMemoryController(monitor, nil, "test-worker", 10, 0.8, 0.95)
	state := mc.State()
	_ = state
	_ = mc.DefEstimates()
}

// ---------------------------------------------------------------------------
// writeJSON / writeError
// ---------------------------------------------------------------------------

func TestWriteJSON(t *testing.T) {
	s := &apiServer{}
	rec := newMockResponseWriter()
	s.writeJSON(rec, 200, map[string]string{"status": "ok"})
	if rec.status != 200 {
		t.Errorf("expected status 200, got %d", rec.status)
	}
	if !strings.Contains(rec.body.String(), "ok") {
		t.Errorf("expected body to contain 'ok', got: %s", rec.body.String())
	}
}

func TestWriteError(t *testing.T) {
	s := &apiServer{}
	rec := newMockResponseWriter()
	s.writeError(rec, 500, "internal error")
	if rec.status != 500 {
		t.Errorf("expected status 500, got %d", rec.status)
	}
	if !strings.Contains(rec.body.String(), "internal error") {
		t.Errorf("expected body to contain 'internal error', got: %s", rec.body.String())
	}
}

// mockResponseWriter implements http.ResponseWriter for testing.
type mockResponseWriter struct {
	status int
	body   strings.Builder
	header http.Header
}

func newMockResponseWriter() *mockResponseWriter {
	return &mockResponseWriter{header: make(http.Header)}
}

func (m *mockResponseWriter) Header() http.Header {
	return m.header
}

func (m *mockResponseWriter) WriteHeader(status int) {
	m.status = status
}

func (m *mockResponseWriter) Write(b []byte) (int, error) {
	return m.body.Write(b)
}

// Stub imports that would otherwise be unused.

// ---------------------------------------------------------------------------
// F56 queue depth gauge
// ---------------------------------------------------------------------------


// ---------------------------------------------------------------------------
// F57 throughput gauges
// ---------------------------------------------------------------------------

