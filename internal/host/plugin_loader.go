package host

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"golang.org/x/mod/semver"

	"github.com/rcownie/cleat/internal/plugin"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// PluginDef is a row from the plugin_defs table.
type PluginDef struct {
	Name       string          `json:"name"`
	Version    string          `json:"version"` // semver string, e.g. "1.2.3"
	WASMBytes  []byte          `json:"wasm_bytes,omitempty"`
	Config     json.RawMessage `json:"config"`
	CreatedAt  time.Time       `json:"created_at"`
	Deprecated bool            `json:"deprecated"`
}

// pluginCacheKey uniquely identifies a plugin version in the cache.
type pluginCacheKey struct {
	Name    string
	Version string
}

// PluginLoader loads plugin WASM modules from the plugin_defs table and
// resolves semver constraints to find the best matching plugin version.
//
// Plugin versioning uses semver because plugins are consumed as libraries
// with compatibility ranges. This is distinct from workflow versioning which
// uses monotonic integers for discrete business process versions.
type PluginLoader struct {
	db *sql.DB
	rt *Runtime

	// LRU cache for compiled plugin modules (keyed by name+version).
	mu       sync.Mutex
	cache    map[pluginCacheKey]wazero.CompiledModule
	maxSize  int

	// limits defines the maximum capabilities for WASM plugins loaded
	// through this loader. If zero-valued (default), no capability
	// restrictions are enforced.
	limits plugin.CapabilityLimits
}

// NewPluginLoader creates a PluginLoader backed by the given database
// connection and wazero runtime. maxSize is the maximum number of compiled
// plugin modules to cache (defaults to 50 if <= 0).
func NewPluginLoader(db *sql.DB, rt *Runtime, maxSize ...int) *PluginLoader {
	sz := 50
	if len(maxSize) > 0 && maxSize[0] > 0 {
		sz = maxSize[0]
	}
	return &PluginLoader{
		db:      db,
		rt:      rt,
		cache:   make(map[pluginCacheKey]wazero.CompiledModule),
		maxSize: sz,
	}
}

// ---------------------------------------------------------------------------
// Constraint parsing and resolution
// ---------------------------------------------------------------------------

// constraintRange represents a semver version range.
type constraintRange struct {
	Min string // minimum version (inclusive), "v"-prefixed semver
	Max string // maximum version (exclusive), "v"-prefixed semver; empty = no upper bound
}

// parseConstraint parses a semver constraint string and returns the
// version range it represents.
//
// Supported constraint forms:
//
//	>=1.2.0   — any version >= 1.2.0
//	~1.2.0    — >=1.2.0, <1.3.0  (patch-level only)
//	^1.2.0    — >=1.2.0, <2.0.0  (minor-level, backward-compatible API)
//	=1.2.0    — exactly 1.2.0
//	1.2.0     — bare version, treated as =1.2.0
func parseConstraint(constraint string) (constraintRange, error) {
	c := strings.TrimSpace(constraint)
	if c == "" || c == "*" {
		// No constraint = any version (wildcard).
		return constraintRange{Min: "v0.0.0"}, nil
	}

	switch {
	case strings.HasPrefix(c, ">="):
		v := ensureVPrefix(strings.TrimPrefix(c, ">="))
		if !semver.IsValid(v) {
			return constraintRange{}, fmt.Errorf("invalid semver in constraint %q", constraint)
		}
		return constraintRange{Min: v}, nil

	case strings.HasPrefix(c, "~"):
		v := ensureVPrefix(strings.TrimPrefix(c, "~"))
		if !semver.IsValid(v) {
			return constraintRange{}, fmt.Errorf("invalid semver in constraint %q", constraint)
		}
		major, minor, _ := splitSemver(v)
		nextMinor := minor + 1
		max := fmt.Sprintf("v%d.%d.0", major, nextMinor)
		return constraintRange{Min: v, Max: max}, nil

	case strings.HasPrefix(c, "^"):
		v := ensureVPrefix(strings.TrimPrefix(c, "^"))
		if !semver.IsValid(v) {
			return constraintRange{}, fmt.Errorf("invalid semver in constraint %q", constraint)
		}
		major, _, _ := splitSemver(v)
		nextMajor := major + 1
		max := fmt.Sprintf("v%d.0.0", nextMajor)
		return constraintRange{Min: v, Max: max}, nil

	case strings.HasPrefix(c, "="):
		v := ensureVPrefix(strings.TrimPrefix(c, "="))
		if !semver.IsValid(v) {
			return constraintRange{}, fmt.Errorf("invalid semver in constraint %q", constraint)
		}
		return constraintRange{Min: v, Max: v}, nil

	default:
		// Bare version — treat as exact match.
		v := ensureVPrefix(c)
		if !semver.IsValid(v) {
			return constraintRange{}, fmt.Errorf("invalid semver version %q", constraint)
		}
		return constraintRange{Min: v, Max: v}, nil
	}
}

// versionInRange reports whether semver version v falls within the given range.
func versionInRange(v string, r constraintRange) bool {
	if !semver.IsValid(v) {
		return false
	}
	if semver.Compare(v, r.Min) < 0 {
		return false
	}
	if r.Max != "" && semver.Compare(v, r.Max) >= 0 {
		return false
	}
	return true
}

// ensureVPrefix adds a "v" prefix if not present.
func ensureVPrefix(v string) string {
	if !strings.HasPrefix(v, "v") {
		return "v" + v
	}
	return v
}

// splitSemver splits a "v"-prefixed semver into major, minor, patch integers.
func splitSemver(v string) (major, minor, patch int) {
	// Strip leading "v".
	s := strings.TrimPrefix(v, "v")
	// Strip pre-release suffix if present (e.g., "1.2.3-beta").
	if idx := strings.Index(s, "-"); idx >= 0 {
		s = s[:idx]
	}
	parts := strings.Split(s, ".")
	if len(parts) > 0 {
		major, _ = strconv.Atoi(parts[0])
	}
	if len(parts) > 1 {
		minor, _ = strconv.Atoi(parts[1])
	}
	if len(parts) > 2 {
		patch, _ = strconv.Atoi(parts[2])
	}
	return
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// ResolvePlugin finds the best matching plugin version for the given semver
// constraint. It queries all non-deprecated versions of the named plugin and
// returns the highest version that satisfies the constraint.
//
//	sql: SELECT name, version, wasm_bytes, config, created_at, deprecated
//	     FROM plugin_defs
//	     WHERE name = $1 AND NOT deprecated
//	     ORDER BY version
//
// Resolution is done in Go using semver comparison after filtering by the
// constraint range.
func (l *PluginLoader) ResolvePlugin(ctx context.Context, name string, constraint string) (string, *PluginDef, error) {
	// Parse the constraint.
	cr, err := parseConstraint(constraint)
	if err != nil {
		return "", nil, fmt.Errorf("resolve plugin %s: %w", name, err)
	}

	// Query all non-deprecated versions.
	rows, err := l.db.QueryContext(ctx, `
		SELECT name, version, wasm_bytes, config, created_at, deprecated
		FROM plugin_defs
		WHERE name = $1 AND NOT deprecated
		ORDER BY version
	`, name)
	if err != nil {
		return "", nil, fmt.Errorf("resolve plugin %s: query: %w", name, err)
	}
	defer rows.Close()

	var candidates []PluginDef
	for rows.Next() {
		var p PluginDef
		if err := rows.Scan(&p.Name, &p.Version, &p.WASMBytes,
			&p.Config, &p.CreatedAt, &p.Deprecated); err != nil {
			return "", nil, fmt.Errorf("resolve plugin %s: scan: %w", name, err)
		}
		candidates = append(candidates, p)
	}
	if err := rows.Err(); err != nil {
		return "", nil, fmt.Errorf("resolve plugin %s: rows: %w", name, err)
	}

	// Filter by constraint and find the highest matching version.
	var best *PluginDef
	var bestVersion string

	for i := range candidates {
		p := &candidates[i]
		v := ensureVPrefix(p.Version)
		if !semver.IsValid(v) {
			continue
		}
		if !versionInRange(v, cr) {
			continue
		}
		if best == nil || semver.Compare(v, bestVersion) > 0 {
			best = p
			bestVersion = v
		}
	}

	if best == nil {
		return "", nil, fmt.Errorf("no matching version for plugin %s with constraint %q", name, constraint)
	}

	return strings.TrimPrefix(bestVersion, "v"), best, nil
}

// LoadPlugin loads a compiled WASM module for a specific plugin version.
// Results are cached in an LRU cache keyed by (name, version).
//
//	sql: SELECT wasm_bytes FROM plugin_defs
//	     WHERE name = $1 AND version = $2 AND NOT deprecated
func (l *PluginLoader) LoadPlugin(ctx context.Context, name string, version string) (wazero.CompiledModule, error) {
	key := pluginCacheKey{Name: name, Version: version}

	// Fast path: check cache.
	l.mu.Lock()
	if cm, ok := l.cache[key]; ok {
		l.mu.Unlock()
		return cm, nil
	}
	l.mu.Unlock()

	// Slow path: query database.
	var wasmBytes []byte
	err := l.db.QueryRowContext(ctx, `
		SELECT wasm_bytes FROM plugin_defs
		WHERE name = $1 AND version = $2 AND NOT deprecated
	`, name, version).Scan(&wasmBytes)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("plugin not found or deprecated: %s v%s", name, version)
	}
	if err != nil {
		return nil, fmt.Errorf("load plugin %s v%s: %w", name, version, err)
	}

	if wasmBytes == nil {
		return nil, fmt.Errorf("plugin %s v%s is host-native (no wasm bytes)", name, version)
	}

	// Compile the WASM module.
	compiled, err := l.rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("compile plugin %s v%s: %w", name, version, err)
	}

	// Cache the compiled module.
	l.mu.Lock()
	if len(l.cache) >= l.maxSize {
		// Evict a random entry if at capacity.
		for k := range l.cache {
			delete(l.cache, k)
			break
		}
	}
	l.cache[key] = compiled
	l.mu.Unlock()

	return compiled, nil
}

// SetLimits configures the maximum capabilities allowed for WASM plugins
// loaded through this loader. If limits is the zero value, no capability
// restrictions are enforced.
func (l *PluginLoader) SetLimits(limits plugin.CapabilityLimits) {
	l.limits = limits
}

// DeployPlugin inserts a new plugin definition into the database.
// If the definition already exists, it is updated (upsert semantics).
//
//	sql: INSERT INTO plugin_defs (name, version, wasm_bytes, config)
//	     VALUES ($1, $2, $3, $4)
//	     ON CONFLICT (name, version) DO UPDATE SET
//	       wasm_bytes = $3, config = $4, deprecated = false, created_at = now()
func (l *PluginLoader) DeployPlugin(ctx context.Context, name string, version string, wasmBytes []byte, config map[string]interface{}) error {
	configJSON, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("deploy plugin %s v%s: marshal config: %w", name, version, err)
	}

	_, err = l.db.ExecContext(ctx, `
		INSERT INTO plugin_defs (name, version, wasm_bytes, config)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (name, version) DO UPDATE SET
			wasm_bytes = EXCLUDED.wasm_bytes,
			config = EXCLUDED.config,
			deprecated = false,
			created_at = now()
	`, name, version, wasmBytes, configJSON)
	if err != nil {
		return fmt.Errorf("deploy plugin %s v%s: %w", name, version, err)
	}

	log.Printf("[plugin-loader] Deployed %s v%s (%d bytes)", name, version, len(wasmBytes))
	return nil
}

// DeployPluginWithCapabilities is like DeployPlugin but additionally validates
// the declared capabilities against configured limits before deploying.
// If the capabilities violate the limits, the deployment is refused.
func (l *PluginLoader) DeployPluginWithCapabilities(ctx context.Context, name string, version string, wasmBytes []byte, config map[string]interface{}, declared plugin.Capabilities) error {
	// If limits are set, validate declared capabilities.
	if l.limits.IsSet() {
		// Convert declared Capabilities to CapabilityLimits for validation.
		declaredLimits := plugin.CapabilityLimits{
			Database:         declared.Database,
			StartWorkflow:    declared.StartWorkflow,
			SignalWorkflow:   declared.SignalWorkflow,
			HTTPRoutes:       declared.HTTPRoutes,
			HTTPMiddleware:   declared.HTTPMiddleware,
			BackgroundWorker: declared.BackgroundWorker,
			CallPlugin:       declared.CallPlugin,
		}
		if err := plugin.ValidateCapabilities(declaredLimits, l.limits); err != nil {
			return fmt.Errorf("deploy plugin %s v%s: capability check failed: %w", name, version, err)
		}
	}

	return l.DeployPlugin(ctx, name, version, wasmBytes, config)
}

// DeprecatePlugin marks a plugin version as deprecated.
//
//	sql: UPDATE plugin_defs SET deprecated = true WHERE name = $1 AND version = $2
func (l *PluginLoader) DeprecatePlugin(ctx context.Context, name string, version string) error {
	result, err := l.db.ExecContext(ctx, `
		UPDATE plugin_defs SET deprecated = true WHERE name = $1 AND version = $2
	`, name, version)
	if err != nil {
		return fmt.Errorf("deprecate plugin %s v%s: %w", name, version, err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("plugin not found: %s v%s", name, version)
	}

	// Remove from cache.
	l.mu.Lock()
	delete(l.cache, pluginCacheKey{Name: name, Version: version})
	l.mu.Unlock()

	log.Printf("[plugin-loader] Deprecated %s v%s", name, version)
	return nil
}

// ListPluginVersions returns all deployed versions of a plugin,
// ordered by semver descending.
//
//	sql: SELECT name, version, wasm_bytes, config, created_at, deprecated
//	     FROM plugin_defs WHERE name = $1 ORDER BY version DESC
func (l *PluginLoader) ListPluginVersions(ctx context.Context, name string) ([]PluginDef, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT name, version, wasm_bytes, config, created_at, deprecated
		FROM plugin_defs
		WHERE name = $1
		ORDER BY version DESC
	`, name)
	if err != nil {
		return nil, fmt.Errorf("list plugin versions for %s: %w", name, err)
	}
	defer rows.Close()

	var plugins []PluginDef
	for rows.Next() {
		var p PluginDef
		if err := rows.Scan(&p.Name, &p.Version, &p.WASMBytes,
			&p.Config, &p.CreatedAt, &p.Deprecated); err != nil {
			return nil, fmt.Errorf("scan plugin def for %s: %w", name, err)
		}
		plugins = append(plugins, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list plugin versions rows for %s: %w", name, err)
	}

	// Sort by semver descending.
	sort.Slice(plugins, func(i, j int) bool {
		return semver.Compare("v"+plugins[i].Version, "v"+plugins[j].Version) > 0
	})

	return plugins, nil
}
