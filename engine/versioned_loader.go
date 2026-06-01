package engine

import (
	"container/list"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	"github.com/tetratelabs/wazero"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// defKey uniquely identifies a workflow definition in the cache.
type defKey struct {
	Name    string
	Version int
}

// cacheEntry holds a compiled module in the LRU cache.
type cacheEntry struct {
	key    defKey
	module wazero.CompiledModule
	elem   *list.Element // back-reference for O(1) removal
}

// lruListEntry is stored in container/list for LRU ordering.
type lruListEntry struct {
	key defKey
}

// CacheStats exposes cache performance for observability.
type CacheStats struct {
	Size   int   `json:"size"`
	MaxSize int  `json:"max_size"`
	Hits   int64 `json:"hits"`
	Misses int64 `json:"misses"`
}

// ---------------------------------------------------------------------------
// WorkflowLoader
// ---------------------------------------------------------------------------

// WorkflowLoader loads and caches compiled WASM modules for workflow
// definitions. It queries the workflow_defs table by (name, version),
// compiles the WASM bytes via the wazero runtime, and caches the compiled
// modules in an LRU cache keyed by (name, version).
//
// The loader also supports a disk-backed cache (WasmDiskCache) that survives
// worker restarts. Cache entries are content-addressed by sha256(wasm_bytes)
// so version changes automatically invalidate stale entries.
//
// Thread-safety: the LRU cache is protected by a mutex. Compiled modules
// themselves are safe for concurrent instantiation per wazero guarantees.
type WorkflowLoader struct {
	db  *sql.DB
	rt  *Runtime

	mu      sync.Mutex
	cache   map[defKey]*cacheEntry
	lruList *list.List
	maxSize int

	hits   atomic.Int64
	misses atomic.Int64

	// diskCache persists compiled modules to disk across restarts.
	diskCache *WasmDiskCache
}

// NewWorkflowLoader creates a WorkflowLoader backed by the given database
// connection and wazero runtime. maxSize is the maximum number of compiled
// modules to keep in the LRU cache (defaults to 100 if <= 0). diskCache is
// an optional disk-backed cache for persistence across restarts (nil disables).
func NewWorkflowLoader(db *sql.DB, rt *Runtime, diskCache *WasmDiskCache, maxSize ...int) *WorkflowLoader {
	sz := 100
	if len(maxSize) > 0 && maxSize[0] > 0 {
		sz = maxSize[0]
	}
	return &WorkflowLoader{
		db:        db,
		rt:        rt,
		cache:     make(map[defKey]*cacheEntry),
		lruList:   list.New(),
		maxSize:   sz,
		diskCache: diskCache,
	}
}

// Load returns a compiled WASM module for the given workflow definition.
// It first checks the LRU cache; on a miss it queries the database, compiles,
// and caches the result. Returns an error if the definition is not found or
// is deprecated.
//
// The disk cache (if configured) is checked after the LRU cache and before
// the database: if raw WASM bytes are found on disk, the database query is
// skipped. Compilation still occurs since the current wazero version does
// not support compiled module serialization.
//
// SQL: SELECT wasm_bytes, abi_version, plugin_deps, min_version
//      FROM workflow_defs
//      WHERE name = $1 AND version = $2 AND NOT deprecated
func (l *WorkflowLoader) Load(ctx context.Context, name string, version int) (wazero.CompiledModule, error) {
	key := defKey{Name: name, Version: version}

	// Fast path: check LRU cache.
	if cm, ok := l.cacheGet(key); ok {
		return cm, nil
	}

	// Check disk cache before querying database (saves DB round-trip).
	var wasmBytes []byte
	if l.diskCache != nil {
		wasmBytes = l.diskCache.LookupDef(name, version)
	}

	if wasmBytes == nil {
		// Query database for WASM bytes.
		var abiVersion int
		var pluginDepsJSON, minVer sql.NullInt64
		err := l.db.QueryRowContext(ctx, `
			SELECT wasm_bytes, abi_version, plugin_deps, min_version
			FROM workflow_defs
			WHERE name = $1 AND version = $2 AND NOT deprecated
		`, name, version).Scan(&wasmBytes, &abiVersion, &pluginDepsJSON, &minVer)
		if errors.Is(err, sql.ErrNoRows) {
			l.misses.Add(1)
			return nil, fmt.Errorf("workflow def not found or deprecated: %s v%d", name, version)
		}
		if err != nil {
			l.misses.Add(1)
			return nil, fmt.Errorf("load workflow def %s v%d: %w", name, version, err)
		}

		// Store to disk cache for future restarts.
		if l.diskCache != nil {
			l.diskCache.StoreDef(name, version, wasmBytes)
		}
	}

	// Compile the WASM module.
	compiled, err := l.rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("compile %s v%d: %w", name, version, err)
	}

	l.cachePut(key, compiled)

	return compiled, nil
}

// Deploy inserts a new workflow definition into the database.
// If the definition already exists, it is updated (upsert semantics).
//
// SQL: INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, plugin_deps, min_version)
//      VALUES ($1, $2, $3, $4, $5, $6)
//      ON CONFLICT (name, version) DO UPDATE SET
//        wasm_bytes = $3, abi_version = $4, plugin_deps = $5, min_version = $6,
//        deprecated = false, created_at = now()
func (l *WorkflowLoader) Deploy(ctx context.Context, name string, version int, wasmBytes []byte, pluginDeps map[string]string, minVersion int) error {
	pluginDepsJSON, err := json.Marshal(pluginDeps)
	if err != nil {
		return fmt.Errorf("deploy %s v%d: marshal plugin_deps: %w", name, version, err)
	}

	_, err = l.db.ExecContext(ctx, `
		INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, plugin_deps, min_version)
		VALUES ($1, $2, $3, 1, $4, $5)
		ON CONFLICT (name, version) DO UPDATE SET
			wasm_bytes = EXCLUDED.wasm_bytes,
			abi_version = EXCLUDED.abi_version,
			plugin_deps = EXCLUDED.plugin_deps,
			min_version = EXCLUDED.min_version,
			deprecated = false,
			created_at = now()
	`, name, version, wasmBytes, pluginDepsJSON, minVersion)
	if err != nil {
		return fmt.Errorf("deploy %s v%d: %w", name, version, err)
	}

	log.Printf("[workflow-loader] Deployed %s v%d (%d bytes, %d plugins, min_ver=%d)",
		name, version, len(wasmBytes), len(pluginDeps), minVersion)
	return nil
}

// Deprecate marks a workflow definition as deprecated.
// Active instances will continue to run, but new instances will not be
// created with this version (unless explicitly requested).
//
// SQL: UPDATE workflow_defs SET deprecated = true WHERE name = $1 AND version = $2
func (l *WorkflowLoader) Deprecate(ctx context.Context, name string, version int) error {
	result, err := l.db.ExecContext(ctx, `
		UPDATE workflow_defs SET deprecated = true WHERE name = $1 AND version = $2
	`, name, version)
	if err != nil {
		return fmt.Errorf("deprecate %s v%d: %w", name, version, err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("workflow def not found: %s v%d", name, version)
	}

	// Remove from cache if present.
	key := defKey{Name: name, Version: version}
	l.cacheRemove(key)

	log.Printf("[workflow-loader] Deprecated %s v%d", name, version)
	return nil
}

// ListVersions returns all deployed versions of a workflow definition,
// ordered by version descending.
//
// SQL: SELECT name, version, wasm_bytes, abi_version, plugin_deps, min_version, created_at, deprecated
//      FROM workflow_defs WHERE name = $1 ORDER BY version DESC
func (l *WorkflowLoader) ListVersions(ctx context.Context, name string) ([]WorkflowDef, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT name, version, wasm_bytes, abi_version, plugin_deps, min_version, created_at, deprecated
		FROM workflow_defs
		WHERE name = $1
		ORDER BY version DESC
	`, name)
	if err != nil {
		return nil, fmt.Errorf("list versions for %s: %w", name, err)
	}
	defer rows.Close()

	var defs []WorkflowDef
	for rows.Next() {
		var def WorkflowDef
		var pluginDeps sql.NullString
		if err := rows.Scan(&def.Name, &def.Version, &def.WASMBytes,
			&def.ABIVersion, &pluginDeps, &def.MinVersion,
			&def.CreatedAt, &def.Deprecated); err != nil {
			return nil, fmt.Errorf("scan workflow def: %w", err)
		}
		if pluginDeps.Valid {
			json.Unmarshal([]byte(pluginDeps.String), &def.PluginDeps)
		}
		defs = append(defs, def)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list versions rows: %w", err)
	}
	return defs, nil
}

// ActiveVersions returns the set of workflow definition versions that have
// active (ready or running) instances. Used to determine which versions
// are safe to garbage-collect or deprecate.
//
// SQL: SELECT def_name, def_version FROM workflow_instances
//      WHERE status IN ('ready', 'running')
//      GROUP BY def_name, def_version
func (l *WorkflowLoader) ActiveVersions(ctx context.Context) (map[string][]int, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT def_name, def_version
		FROM workflow_instances
		WHERE status IN ('ready', 'running')
		GROUP BY def_name, def_version
		ORDER BY def_name, def_version
	`)
	if err != nil {
		return nil, fmt.Errorf("active versions: %w", err)
	}
	defer rows.Close()

	active := make(map[string][]int)
	for rows.Next() {
		var name string
		var version int
		if err := rows.Scan(&name, &version); err != nil {
			return nil, fmt.Errorf("active versions scan: %w", err)
		}
		active[name] = append(active[name], version)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("active versions rows: %w", err)
	}
	return active, nil
}

// ResolveLatestVersion returns the highest non-deprecated version for a
// workflow definition. Returns 0 with no error if no non-deprecated version
// exists.
//
// SQL: SELECT COALESCE(MAX(version), 0) FROM workflow_defs
//      WHERE name = $1 AND NOT deprecated
func (l *WorkflowLoader) ResolveLatestVersion(ctx context.Context, name string) (int, error) {
	var version int
	err := l.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(version), 0) FROM workflow_defs
		WHERE name = $1 AND NOT deprecated
	`, name).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("resolve latest version for %s: %w", name, err)
	}
	return version, nil
}

// CacheStats returns current cache statistics for observability.
func (l *WorkflowLoader) CacheStats() CacheStats {
	l.mu.Lock()
	size := l.lruList.Len()
	l.mu.Unlock()
	return CacheStats{
		Size:    size,
		MaxSize: l.maxSize,
		Hits:    l.hits.Load(),
		Misses:  l.misses.Load(),
	}
}

// ---------------------------------------------------------------------------
// Internal cache methods
// ---------------------------------------------------------------------------

// cacheGet retrieves a compiled module from the LRU cache, promoting it
// to the front. Returns false on miss.
func (l *WorkflowLoader) cacheGet(key defKey) (wazero.CompiledModule, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, ok := l.cache[key]
	if !ok {
		l.misses.Add(1)
		return nil, false
	}

	// Move to front (most recently used).
	l.lruList.MoveToFront(entry.elem)
	l.hits.Add(1)
	return entry.module, true
}

// cachePut inserts a compiled module into the LRU cache, evicting the
// least recently used entry if at capacity.
func (l *WorkflowLoader) cachePut(key defKey, module wazero.CompiledModule) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// If already in cache, update and move to front.
	if existing, ok := l.cache[key]; ok {
		l.lruList.MoveToFront(existing.elem)
		existing.module.Close(context.Background())
		existing.module = module
		return
	}

	// Evict if at capacity.
	for l.lruList.Len() >= l.maxSize {
		l.evictLocked()
	}

	// Insert new entry.
	elem := l.lruList.PushFront(&lruListEntry{key: key})
	l.cache[key] = &cacheEntry{
		key:    key,
		module: module,
		elem:   elem,
	}
}

// cacheRemove removes an entry from the cache if present.
func (l *WorkflowLoader) cacheRemove(key defKey) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if entry, ok := l.cache[key]; ok {
		l.lruList.Remove(entry.elem)
		delete(l.cache, key)
		entry.module.Close(context.Background())
	}
}

// evictLocked removes the least recently used entry from the cache.
// Must be called with l.mu held.
func (l *WorkflowLoader) evictLocked() {
	elem := l.lruList.Back()
	if elem == nil {
		return
	}
	le := elem.Value.(*lruListEntry)
	l.lruList.Remove(elem)
	if entry, ok := l.cache[le.key]; ok {
		delete(l.cache, le.key)
		entry.module.Close(context.Background())
	}
}
