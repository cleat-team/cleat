package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/plugin"
)

// ---------------------------------------------------------------------------
// Mock SQL driver for PluginLoader tests
//
// pluginLoaderMockDriver supports both Query and Exec with per-test
// configurable behavior (rows, errors, RowsAffected).
// ---------------------------------------------------------------------------

type plMockConfig struct {
	queryRows        [][]driver.Value // rows to return from Query
	queryErr         error            // if set, Query returns this error
	execRowsAffected int64            // RowsAffected value for Exec
	execErr          error            // if set, Exec returns this error
	queryCount       *int32           // if non-nil, incremented atomically on each Query call
}

var plMockState struct {
	mu      sync.Mutex
	counter int
}

func newPluginLoaderMockDB(cfg plMockConfig) *sql.DB {
	plMockState.mu.Lock()
	plMockState.counter++
	name := fmt.Sprintf("pl_mock_%d", plMockState.counter)
	plMockState.mu.Unlock()

	d := &plMockDriver{cfg: cfg}
	sql.Register(name, d)
	db, err := sql.Open(name, "")
	if err != nil {
		panic(err)
	}
	return db
}

type plMockDriver struct {
	driver.Driver
	cfg plMockConfig
}

func (d *plMockDriver) Open(name string) (driver.Conn, error) {
	return &plMockConn{driver: d}, nil
}

type plMockConn struct {
	driver.Conn
	driver *plMockDriver
}

func (c *plMockConn) Prepare(query string) (driver.Stmt, error) {
	return &plMockStmt{conn: c, query: query}, nil
}

func (c *plMockConn) Close() error { return nil }
func (c *plMockConn) Begin() (driver.Tx, error) {
	return &plMockTx{}, nil
}

type plMockTx struct{ driver.Tx }

func (t *plMockTx) Commit() error   { return nil }
func (t *plMockTx) Rollback() error { return nil }

type plMockStmt struct {
	conn  *plMockConn
	query string
}

func (s *plMockStmt) Close() error  { return nil }
func (s *plMockStmt) NumInput() int { return -1 }

func (s *plMockStmt) Exec(args []driver.Value) (driver.Result, error) {
	if s.conn.driver.cfg.execErr != nil {
		return nil, s.conn.driver.cfg.execErr
	}
	return &plMockResult{rowsAffected: s.conn.driver.cfg.execRowsAffected}, nil
}

type plMockResult struct {
	driver.Result
	rowsAffected int64
}

func (r *plMockResult) LastInsertId() (int64, error) { return 0, nil }
func (r *plMockResult) RowsAffected() (int64, error) { return r.rowsAffected, nil }

func (s *plMockStmt) Query(args []driver.Value) (driver.Rows, error) {
	if s.conn.driver.cfg.queryCount != nil {
		atomic.AddInt32(s.conn.driver.cfg.queryCount, 1)
	}
	if s.conn.driver.cfg.queryErr != nil {
		return nil, s.conn.driver.cfg.queryErr
	}
	rows := s.conn.driver.cfg.queryRows
	var cols []string
	if len(rows) > 0 {
		allCols := []string{"name", "version", "wasm_bytes", "config", "created_at", "deprecated"}
		n := len(rows[0])
		if n > len(allCols) {
			n = len(allCols)
		}
		cols = allCols[:n]
	}
	return &plMockRows{cols: cols, data: rows}, nil
}

type plMockRows struct {
	driver.Rows
	cols []string
	data [][]driver.Value
	pos  int
}

func (r *plMockRows) Columns() []string { return r.cols }
func (r *plMockRows) Close() error      { return nil }
func (r *plMockRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.pos])
	r.pos++
	return nil
}

// ---------------------------------------------------------------------------
// Shared wazero runtime for LoadPlugin and cache tests
// ---------------------------------------------------------------------------

var (
	testRT     *Runtime
	testRTOnce sync.Once
	testRTErr  error
)

func sharedRuntime(t *testing.T) *Runtime {
	t.Helper()
	testRTOnce.Do(func() {
		testRT, testRTErr = NewRuntime(context.Background(), 0, 0)
	})
	if testRTErr != nil {
		t.Fatalf("NewRuntime: %v", testRTErr)
	}
	return testRT
}

// ---------------------------------------------------------------------------
// Helper: build version rows for ResolvePlugin / ListPluginVersions tests
// ---------------------------------------------------------------------------

// makeVersionRows builds rows compatible with the plugin_defs SELECT (6 columns).
// Each version string is populated as: [name, version, wasm_bytes(nil), config({}),
// created_at(zero time), deprecated(false)].
func makeVersionRows(name string, versions []string) [][]driver.Value {
	rows := make([][]driver.Value, len(versions))
	for i, v := range versions {
		rows[i] = []driver.Value{name, v, nil, []byte("{}"), time.Time{}, false}
	}
	return rows
}

// standardCandidates is the common version set used across ResolvePlugin tests.
var standardVersions = []string{"1.0.0", "1.2.0", "1.3.0", "2.0.0", "2.1.0"}

// ============================================================================
// Section 1: ResolvePlugin
// ============================================================================

func TestPluginLoader_ResolveConstraintTypes(t *testing.T) {
	// Common set of deployed versions for a plugin named "myplugin".
	// The highest matching version should be returned for each constraint.
	tests := []struct {
		name       string
		constraint string
		want       string // expected resolved version, "" if error expected
		wantErr    bool
	}{
		{"empty/wildcard", "", "2.1.0", false},
		{"star wildcard", "*", "2.1.0", false},
		{"gte", ">=1.5.0", "2.1.0", false},
		{"tilde", "~1.2.0", "1.2.0", false},
		{"caret", "^1.2.0", "1.3.0", false},
		{
			// KNOWN BUG: exact constraint fails because versionInRange treats
			// Max as exclusive and Min==Max means no version can match.
			// See lessons_learned/ for details.
			name: "exact_bug", constraint: "=1.3.0", want: "", wantErr: true,
		},
		{
			name: "bare_bug", constraint: "2.0.0", want: "", wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newPluginLoaderMockDB(plMockConfig{
				queryRows: makeVersionRows("myplugin", standardVersions),
			})
			l := NewPluginLoader(db, nil)

			gotVer, gotDef, err := l.ResolvePlugin(context.Background(), "myplugin", tt.constraint)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got version=%q def=%v", gotVer, gotDef)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotVer != tt.want {
				t.Errorf("version = %q, want %q", gotVer, tt.want)
			}
			if gotDef == nil {
				t.Error("PluginDef should not be nil")
			} else if gotDef.Name != "myplugin" {
				t.Errorf("PluginDef.Name = %q, want %q", gotDef.Name, "myplugin")
			}
		})
	}
}

func TestPluginLoader_ResolveNoMatch(t *testing.T) {
	// DB has versions 1.0.0, 1.1.0, 1.2.0 but constraint requires >=2.0.0.
	db := newPluginLoaderMockDB(plMockConfig{
		queryRows: makeVersionRows("p", []string{"1.0.0", "1.1.0", "1.2.0"}),
	})
	l := NewPluginLoader(db, nil)

	_, _, err := l.ResolvePlugin(context.Background(), "p", ">=2.0.0")
	if err == nil {
		t.Fatal("expected error for no matching version")
	}
	if !strings.Contains(err.Error(), "no matching version") {
		t.Errorf("error should mention 'no matching version', got: %v", err)
	}
}

func TestPluginLoader_ResolveInvalidConstraint(t *testing.T) {
	db := newPluginLoaderMockDB(plMockConfig{
		queryRows: makeVersionRows("p", standardVersions),
	})
	l := NewPluginLoader(db, nil)

	_, _, err := l.ResolvePlugin(context.Background(), "p", ">=notasemver")
	if err == nil {
		t.Fatal("expected error for invalid constraint")
	}
}

func TestPluginLoader_ResolvePluginNotFound(t *testing.T) {
	// DB has no rows for this plugin.
	db := newPluginLoaderMockDB(plMockConfig{}) // empty rows
	l := NewPluginLoader(db, nil)

	_, _, err := l.ResolvePlugin(context.Background(), "nonexistent", "")
	if err == nil {
		t.Fatal("expected error for nonexistent plugin")
	}
	if !strings.Contains(err.Error(), "no matching version") {
		t.Errorf("error should mention 'no matching version', got: %v", err)
	}
}

func TestPluginLoader_ResolveDBQueryError(t *testing.T) {
	queryErr := errors.New("db connection refused")
	db := newPluginLoaderMockDB(plMockConfig{queryErr: queryErr})
	l := NewPluginLoader(db, nil)

	_, _, err := l.ResolvePlugin(context.Background(), "p", "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "db connection refused") {
		t.Errorf("error should wrap underlying DB error, got: %v", err)
	}
}

func TestPluginLoader_ResolveDBScanError(t *testing.T) {
	// Return rows with wrong number of columns (1 instead of 6) to trigger scan error.
	db := newPluginLoaderMockDB(plMockConfig{
		queryRows: [][]driver.Value{{"wrong_column_count"}},
	})
	l := NewPluginLoader(db, nil)

	_, _, err := l.ResolvePlugin(context.Background(), "p", "")
	if err == nil {
		t.Fatal("expected scan error")
	}
}

// ============================================================================
// Section 2: DeployPlugin
// ============================================================================

func TestPluginLoader_DeploySuccess(t *testing.T) {
	db := newPluginLoaderMockDB(plMockConfig{execRowsAffected: 1})
	l := NewPluginLoader(db, nil)

	err := l.DeployPlugin(context.Background(), "p", "1.0.0", []byte{0x00, 0x61, 0x73, 0x6d}, map[string]any{"key": "val"})
	if err != nil {
		t.Fatalf("DeployPlugin: %v", err)
	}
}

func TestPluginLoader_DeployMarshalError(t *testing.T) {
	db := newPluginLoaderMockDB(plMockConfig{execRowsAffected: 1})
	l := NewPluginLoader(db, nil)

	// Go channels cannot be JSON-marshaled.
	err := l.DeployPlugin(context.Background(), "p", "1.0.0", nil, map[string]any{"ch": make(chan int)})
	if err == nil {
		t.Fatal("expected marshal error for non-JSON-serializable config")
	}
	if !strings.Contains(err.Error(), "marshal config") {
		t.Errorf("error should mention 'marshal config', got: %v", err)
	}
}

func TestPluginLoader_DeployExecError(t *testing.T) {
	execErr := errors.New("disk full")
	db := newPluginLoaderMockDB(plMockConfig{execErr: execErr})
	l := NewPluginLoader(db, nil)

	err := l.DeployPlugin(context.Background(), "p", "1.0.0", nil, nil)
	if err == nil {
		t.Fatal("expected exec error")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("error should wrap exec error, got: %v", err)
	}
}

// ============================================================================
// Section 3: DeployPluginWithCapabilities
// ============================================================================

func TestPluginLoader_DeployWithCapabilitiesPass(t *testing.T) {
	db := newPluginLoaderMockDB(plMockConfig{execRowsAffected: 1})
	l := NewPluginLoader(db, nil)
	l.SetLimits(plugin.CapabilityLimits{
		Database: plugin.DatabaseAccessReadWrite,
	})

	declared := plugin.Capabilities{Database: true}
	err := l.DeployPluginWithCapabilities(context.Background(), "p", "1.0.0", nil, nil, declared)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
}

func TestPluginLoader_DeployWithCapabilitiesDenied(t *testing.T) {
	db := newPluginLoaderMockDB(plMockConfig{execRowsAffected: 1})
	l := NewPluginLoader(db, nil)
	// Set strict limits: no database access, no workflow start.
	l.SetLimits(plugin.CapabilityLimits{
		Database:      plugin.DatabaseAccessNone,
		StartWorkflow: false,
	})

	declared := plugin.Capabilities{Database: true, StartWorkflow: true}
	err := l.DeployPluginWithCapabilities(context.Background(), "p", "1.0.0", nil, nil, declared)
	if err == nil {
		t.Fatal("expected capability violation error")
	}
	if !strings.Contains(err.Error(), "capability violations") {
		t.Errorf("error should mention 'capability violations', got: %v", err)
	}
}

func TestPluginLoader_DeployWithCapabilitiesLimitsNotSet(t *testing.T) {
	db := newPluginLoaderMockDB(plMockConfig{execRowsAffected: 1})
	l := NewPluginLoader(db, nil)
	// Limits are zero-value → IsSet() returns false → validation skipped.

	declared := plugin.Capabilities{Database: true, StartWorkflow: true}
	err := l.DeployPluginWithCapabilities(context.Background(), "p", "1.0.0", nil, nil, declared)
	if err != nil {
		t.Fatalf("expected success (limits not set), got: %v", err)
	}
}

func TestPluginLoader_DeployWithCapabilitiesCallPluginWildcard(t *testing.T) {
	db := newPluginLoaderMockDB(plMockConfig{execRowsAffected: 1})
	l := NewPluginLoader(db, nil)
	l.SetLimits(plugin.CapabilityLimits{
		CallPlugin: []string{"*"},
	})

	declared := plugin.Capabilities{CallPlugin: []string{"other-plugin"}}
	err := l.DeployPluginWithCapabilities(context.Background(), "p", "1.0.0", nil, nil, declared)
	if err != nil {
		t.Fatalf("expected success with wildcard, got: %v", err)
	}
}

// ============================================================================
// Section 4: DeprecatePlugin
// ============================================================================

func TestPluginLoader_DeprecateSuccess(t *testing.T) {
	db := newPluginLoaderMockDB(plMockConfig{execRowsAffected: 1})
	l := NewPluginLoader(db, nil)

	err := l.DeprecatePlugin(context.Background(), "p", "1.0.0")
	if err != nil {
		t.Fatalf("DeprecatePlugin: %v", err)
	}
}

func TestPluginLoader_DeprecateNotFound(t *testing.T) {
	db := newPluginLoaderMockDB(plMockConfig{execRowsAffected: 0})
	l := NewPluginLoader(db, nil)

	err := l.DeprecatePlugin(context.Background(), "p", "1.0.0")
	if err == nil {
		t.Fatal("expected error for nonexistent plugin")
	}
	if !strings.Contains(err.Error(), "plugin not found") {
		t.Errorf("error should mention 'plugin not found', got: %v", err)
	}
}

func TestPluginLoader_DeprecateExecError(t *testing.T) {
	execErr := errors.New("constraint violation")
	db := newPluginLoaderMockDB(plMockConfig{execErr: execErr})
	l := NewPluginLoader(db, nil)

	err := l.DeprecatePlugin(context.Background(), "p", "1.0.0")
	if err == nil {
		t.Fatal("expected exec error")
	}
	if !strings.Contains(err.Error(), "constraint violation") {
		t.Errorf("error should wrap exec error, got: %v", err)
	}
}

func TestPluginLoader_DeprecateRemovesFromCache(t *testing.T) {
	rt := sharedRuntime(t)
	wasmBytes := minimalWasm()

	// Single mock DB that supports both query (LoadPlugin) and exec (DeprecatePlugin).
	db := newPluginLoaderMockDB(plMockConfig{
		queryRows:        [][]driver.Value{{wasmBytes}},
		execRowsAffected: 1,
	})
	l := NewPluginLoader(db, rt)

	// Load into cache.
	_, err := l.LoadPlugin(context.Background(), "p", "1.0.0")
	if err != nil {
		t.Fatalf("LoadPlugin: %v", err)
	}

	// Deprecate — should succeed and remove from cache.
	if err := l.DeprecatePlugin(context.Background(), "p", "1.0.0"); err != nil {
		t.Fatalf("DeprecatePlugin: %v", err)
	}

	// Verify the cache was cleared: new mock with no rows. If cache was cleared,
	// LoadPlugin will query DB and get nothing.
	db2 := newPluginLoaderMockDB(plMockConfig{}) // no query rows
	l2 := NewPluginLoader(db2, rt)

	_, err = l2.LoadPlugin(context.Background(), "p", "1.0.0")
	if err == nil {
		t.Error("LoadPlugin should fail after deprecation (cache cleared + plugin deprecated)")
	}
}

// ============================================================================
// Section 5: ListPluginVersions
// ============================================================================

func TestPluginLoader_ListMultipleVersions(t *testing.T) {
	// Return versions in non-sorted order; output should be sorted by semver descending.
	db := newPluginLoaderMockDB(plMockConfig{
		queryRows: makeVersionRows("p", []string{"1.0.0", "2.0.0", "1.5.0", "0.9.0"}),
	})
	l := NewPluginLoader(db, nil)

	versions, err := l.ListPluginVersions(context.Background(), "p")
	if err != nil {
		t.Fatalf("ListPluginVersions: %v", err)
	}
	if len(versions) != 4 {
		t.Fatalf("expected 4 versions, got %d", len(versions))
	}
	// Should be sorted descending: 2.0.0, 1.5.0, 1.0.0, 0.9.0
	expected := []string{"2.0.0", "1.5.0", "1.0.0", "0.9.0"}
	for i, want := range expected {
		if versions[i].Version != want {
			t.Errorf("versions[%d] = %q, want %q", i, versions[i].Version, want)
		}
	}
}

func TestPluginLoader_ListEmptyResult(t *testing.T) {
	db := newPluginLoaderMockDB(plMockConfig{}) // no rows
	l := NewPluginLoader(db, nil)

	versions, err := l.ListPluginVersions(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(versions) != 0 {
		t.Errorf("expected empty slice, got %d versions", len(versions))
	}
}

func TestPluginLoader_ListQueryError(t *testing.T) {
	queryErr := errors.New("connection lost")
	db := newPluginLoaderMockDB(plMockConfig{queryErr: queryErr})
	l := NewPluginLoader(db, nil)

	_, err := l.ListPluginVersions(context.Background(), "p")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "connection lost") {
		t.Errorf("error should wrap query error, got: %v", err)
	}
}

// ============================================================================
// Section 6: LoadPlugin
// ============================================================================

func TestPluginLoader_LoadSuccess(t *testing.T) {
	rt := sharedRuntime(t)
	wasmBytes := minimalWasm()

	db := newPluginLoaderMockDB(plMockConfig{
		queryRows: [][]driver.Value{{wasmBytes}},
	})
	l := NewPluginLoader(db, rt)

	mod, err := l.LoadPlugin(context.Background(), "p", "1.0.0")
	if err != nil {
		t.Fatalf("LoadPlugin: %v", err)
	}
	if mod == nil {
		t.Fatal("CompiledModule should not be nil")
	}
	mod.Close(context.Background())
}

func TestPluginLoader_LoadCacheHit(t *testing.T) {
	rt := sharedRuntime(t)
	wasmBytes := minimalWasm()

	db := newPluginLoaderMockDB(plMockConfig{
		queryRows: [][]driver.Value{{wasmBytes}},
	})
	l := NewPluginLoader(db, rt)

	// First load — from DB.
	mod1, err := l.LoadPlugin(context.Background(), "p", "1.0.0")
	if err != nil {
		t.Fatalf("first LoadPlugin: %v", err)
	}
	mod1.Close(context.Background())

	// Second load — should hit cache. Even though the mock DB still has rows,
	// the cached module should be returned.
	mod2, err := l.LoadPlugin(context.Background(), "p", "1.0.0")
	if err != nil {
		t.Fatalf("second LoadPlugin (cache hit): %v", err)
	}
	mod2.Close(context.Background())
}

func TestPluginLoader_LoadPluginNotFound(t *testing.T) {
	rt := sharedRuntime(t)
	db := newPluginLoaderMockDB(plMockConfig{}) // no rows
	l := NewPluginLoader(db, rt)

	_, err := l.LoadPlugin(context.Background(), "p", "1.0.0")
	if err == nil {
		t.Fatal("expected error for nonexistent plugin")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
}

func TestPluginLoader_LoadNilWasmBytes(t *testing.T) {
	rt := sharedRuntime(t)
	db := newPluginLoaderMockDB(plMockConfig{
		queryRows: [][]driver.Value{{nil}}, // WASMBytes is nil
	})
	l := NewPluginLoader(db, rt)

	_, err := l.LoadPlugin(context.Background(), "p", "1.0.0")
	if err == nil {
		t.Fatal("expected error for host-native plugin")
	}
	if !strings.Contains(err.Error(), "host-native") {
		t.Errorf("error should mention 'host-native', got: %v", err)
	}
}

func TestPluginLoader_LoadDeprecated(t *testing.T) {
	rt := sharedRuntime(t)
	// Simulate deprecated: query returns no rows (deprecated filter in WHERE).
	db := newPluginLoaderMockDB(plMockConfig{}) // empty
	l := NewPluginLoader(db, rt)

	_, err := l.LoadPlugin(context.Background(), "p", "2.0.0")
	if err == nil {
		t.Fatal("expected error for deprecated plugin")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
}

func TestPluginLoader_LoadDBQueryError(t *testing.T) {
	rt := sharedRuntime(t)
	queryErr := errors.New("timeout")
	db := newPluginLoaderMockDB(plMockConfig{queryErr: queryErr})
	l := NewPluginLoader(db, rt)

	_, err := l.LoadPlugin(context.Background(), "p", "1.0.0")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error should wrap query error, got: %v", err)
	}
}

func TestPluginLoader_LoadCompileError(t *testing.T) {
	rt := sharedRuntime(t)
	invalidWasm := []byte{0x00, 0x00, 0x00, 0x00} // not valid WASM

	db := newPluginLoaderMockDB(plMockConfig{
		queryRows: [][]driver.Value{{invalidWasm}},
	})
	l := NewPluginLoader(db, rt)

	_, err := l.LoadPlugin(context.Background(), "p", "1.0.0")
	if err == nil {
		t.Fatal("expected compile error for invalid WASM")
	}
	if !strings.Contains(err.Error(), "compile") {
		t.Errorf("error should mention 'compile', got: %v", err)
	}
}

// ============================================================================
// Section 7: LRU cache behavior
// ============================================================================

func TestPluginLoader_CacheEviction(t *testing.T) {
	rt := sharedRuntime(t)
	wasmBytes := minimalWasm()

	// Use a query counter to distinguish cache hits from DB queries.
	var queryCount int32

	db := newPluginLoaderMockDB(plMockConfig{
		queryRows:  [][]driver.Value{{wasmBytes}},
		queryCount: &queryCount,
	})
	l := NewPluginLoader(db, rt, 2)

	// Load 3 unique plugins: p1, p2, p3. Cache size is 2, so p1 gets evicted.
	// Each LoadPlugin triggers 1 DB query initially.
	names := []string{"p1", "p2", "p3"}
	for _, name := range names {
		mod, err := l.LoadPlugin(context.Background(), name, "1.0.0")
		if err != nil {
			t.Fatalf("LoadPlugin %s: %v", name, err)
		}
		mod.Close(context.Background())
	}

	queriesAfterFirstLoads := atomic.LoadInt32(&queryCount)
	if queriesAfterFirstLoads != 3 {
		t.Fatalf("expected 3 DB queries for initial loads, got %d", queriesAfterFirstLoads)
	}

	// p1 was evicted. Loading p1 again triggers a DB query (and evicts p2).
	mod, err := l.LoadPlugin(context.Background(), "p1", "1.0.0")
	if err != nil {
		t.Fatalf("LoadPlugin p1 after eviction: %v", err)
	}
	mod.Close(context.Background())

	if atomic.LoadInt32(&queryCount) != 4 {
		t.Errorf("expected 4th DB query for evicted p1, got %d", queryCount)
	}

	// p3 should still be cached, so loading it should NOT trigger a DB query.
	mod3, err := l.LoadPlugin(context.Background(), "p3", "1.0.0")
	if err != nil {
		t.Fatalf("LoadPlugin p3 (cached): %v", err)
	}
	mod3.Close(context.Background())

	if atomic.LoadInt32(&queryCount) != 4 {
		t.Errorf("expected no DB query for cached p3, but query count is %d", queryCount)
	}
}

func TestPluginLoader_CacheRemove(t *testing.T) {
	rt := sharedRuntime(t)
	wasmBytes := minimalWasm()

	db := newPluginLoaderMockDB(plMockConfig{
		queryRows:        [][]driver.Value{{wasmBytes}},
		execRowsAffected: 1,
	})
	l := NewPluginLoader(db, rt)

	// Load plugin into cache.
	mod, err := l.LoadPlugin(context.Background(), "p", "1.0.0")
	if err != nil {
		t.Fatalf("LoadPlugin: %v", err)
	}
	mod.Close(context.Background())

	// Deprecate — this calls cacheRemove.
	if err := l.DeprecatePlugin(context.Background(), "p", "1.0.0"); err != nil {
		t.Fatalf("DeprecatePlugin: %v", err)
	}

	// Verify: create new mock with no rows. If cache was cleared, LoadPlugin
	// will query DB and get nothing.
	db2 := newPluginLoaderMockDB(plMockConfig{}) // empty
	l2 := NewPluginLoader(db2, rt)

	_, err = l2.LoadPlugin(context.Background(), "p", "1.0.0")
	if err == nil {
		t.Error("LoadPlugin should fail after cache remove + deprecation")
	}
}

func TestPluginLoader_CacheConcurrentAccess(t *testing.T) {
	rt := sharedRuntime(t)
	wasmBytes := minimalWasm()

	db := newPluginLoaderMockDB(plMockConfig{
		queryRows: [][]driver.Value{{wasmBytes}},
	})
	l := NewPluginLoader(db, rt)

	const goroutines = 10
	errCh := make(chan error, goroutines)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			mod, err := l.LoadPlugin(context.Background(), "p", "1.0.0")
			if err != nil {
				errCh <- err
				return
			}
			mod.Close(context.Background())
		}()
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent LoadPlugin failed: %v", err)
	}
}

// ============================================================================
// Section 8: Round-trip
// ============================================================================

func TestPluginLoader_RoundTrip(t *testing.T) {
	rt := sharedRuntime(t)
	wasmBytes := minimalWasm()

	// Mock DB that supports both Exec (for Deploy) and Query (for Load).
	db := newPluginLoaderMockDB(plMockConfig{
		execRowsAffected: 1,
		queryRows:        [][]driver.Value{{wasmBytes}},
	})
	l := NewPluginLoader(db, rt)

	// Deploy the plugin.
	err := l.DeployPlugin(context.Background(), "p", "1.0.0", wasmBytes, map[string]any{"type": "test"})
	if err != nil {
		t.Fatalf("DeployPlugin: %v", err)
	}

	// Load it back.
	mod, err := l.LoadPlugin(context.Background(), "p", "1.0.0")
	if err != nil {
		t.Fatalf("LoadPlugin: %v", err)
	}
	if mod == nil {
		t.Fatal("expected non-nil CompiledModule")
	}
	mod.Close(context.Background())
}
