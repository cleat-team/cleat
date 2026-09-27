package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// WasmDiskCache provides a disk-backed cache for raw WASM module bytes.
// Cache entries are content-addressed by sha256(wasm_bytes), so version
// changes automatically invalidate stale entries.
//
// This cache stores raw WASM bytes on disk, avoiding database round-trips
// on worker restart. An index file maps (defName, defVersion) to the sha256
// content hash so lookups can be performed without knowing the content.
//
// Compiled module serialization is not supported by the current wazero
// version, so compilation still occurs on each worker start.
type WasmDiskCache struct {
	dir    string
	maxLen int // maximum number of cached files on disk

	mu sync.Mutex
}

// indexEntry maps a workflow definition to a content hash.
//
// Tenant is what makes the index safe to share between tenants. The blob
// files are content-addressed, so they were never the hazard; the index was,
// because a lookup of ("orders", 1) answered with whichever tenant's bytes
// were stored under that key first. Definition names are caller-chosen, so
// two tenants both deploying "orders" v1 is an ordinary thing to have, not a
// contrived collision. cleat#1931.
type indexEntry struct {
	Tenant  string `json:"tenant"`
	Name    string `json:"name"`
	Version int    `json:"version"`
	Hash    string `json:"hash"` // sha256 hex of WASM bytes
}

// defIndexKey identifies one tenant's definition in the in-memory index.
//
// A struct rather than a delimited string: definition names may contain the
// delimiter, so a string key has to be parsed back apart on save, and that
// parse is a second place for the format to be got wrong.
type defIndexKey struct {
	Tenant  string
	Name    string
	Version int
}

// NewWasmDiskCache creates a WasmDiskCache rooted at cacheDir. Returns nil
// if cacheDir is empty (caching disabled). maxLen is the maximum number of
// cached files to keep on disk (default 100).
func NewWasmDiskCache(cacheDir string, maxLen int) *WasmDiskCache {
	if cacheDir == "" {
		return nil
	}
	if maxLen <= 0 {
		maxLen = 100
	}
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		slog.WarnContext(context.Background(), "wasm disk cache: cannot create directory", "dir", cacheDir, "error", err)
		return nil
	}
	c := &WasmDiskCache{dir: cacheDir, maxLen: maxLen}
	c.evictLRU()
	return c
}

// cacheKey returns the hex-encoded sha256 of the WASM bytes.
func wasmCacheKey(wasmBytes []byte) string {
	h := sha256.Sum256(wasmBytes)
	return hex.EncodeToString(h[:])
}

// cachePath returns the filesystem path for a content-hash key.
func (c *WasmDiskCache) cachePath(hash string) string {
	return filepath.Join(c.dir, hash+".wasm")
}

// indexPath returns the path to the tenant-name-version index file.
//
// THE FILENAME CARRIES THE FORMAT VERSION, and that is the whole migration.
// Entries written before cleat#1931 have no tenant recorded, and "" is a
// legitimate tenant here (a workflow row written before tenant_id existed
// runs on the worker's own store), so an absent tenant cannot be told apart
// from an unset one by reading the entry. A new filename sidesteps the
// question: the old index is simply not consulted, every definition is
// re-read from its own tenant's store once, and the blobs it pointed at are
// content-addressed and age out through the normal LRU pass.
func (c *WasmDiskCache) indexFilePath() string {
	return filepath.Join(c.dir, "index.v2.json")
}

// loadIndex reads the tenant-name-version-to-hash index from disk.
func (c *WasmDiskCache) loadIndex() map[defIndexKey]string {
	data, err := os.ReadFile(c.indexFilePath())
	if err != nil {
		return make(map[defIndexKey]string)
	}
	var entries []indexEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return make(map[defIndexKey]string)
	}
	idx := make(map[defIndexKey]string, len(entries))
	for _, e := range entries {
		idx[defIndexKey{Tenant: e.Tenant, Name: e.Name, Version: e.Version}] = e.Hash
	}
	return idx
}

// saveIndex writes the tenant-name-version-to-hash index to disk.
func (c *WasmDiskCache) saveIndex(idx map[defIndexKey]string) {
	entries := make([]indexEntry, 0, len(idx))
	for key, hash := range idx {
		entries = append(entries, indexEntry{
			Tenant: key.Tenant, Name: key.Name, Version: key.Version, Hash: hash,
		})
	}
	data, err := json.Marshal(entries)
	if err != nil {
		slog.WarnContext(context.Background(), "wasm disk cache: marshal index failed", "error", err)
		return
	}
	tmpPath := c.indexFilePath() + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		slog.WarnContext(context.Background(), "wasm disk cache: write index failed", "error", err)
		os.Remove(tmpPath)
		return
	}
	if err := os.Rename(tmpPath, c.indexFilePath()); err != nil {
		slog.WarnContext(context.Background(), "wasm disk cache: rename index failed", "error", err)
		os.Remove(tmpPath)
	}
}

// LookupDef retrieves raw WASM bytes for one tenant's workflow definition by
// name and version. Returns nil on cache miss.
//
// tenantID scopes the lookup. It is the tenant of the STORE the bytes would
// otherwise be read from, which for a workflow whose own tenant_id is unset
// is the worker's tenant rather than "" -- see Worker.loadWASM.
func (c *WasmDiskCache) LookupDef(tenantID, name string, version int) []byte {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	idx := c.loadIndex()
	hash, ok := idx[defIndexKey{Tenant: tenantID, Name: name, Version: version}]
	c.mu.Unlock()
	if !ok {
		return nil
	}
	return c.LookupByKey(hash)
}

// StoreDef writes raw WASM bytes to the disk cache indexed by
// (tenant, name, version) and content-addressed by sha256. This is a no-op if
// the entry already exists.
func (c *WasmDiskCache) StoreDef(tenantID, name string, version int, wasmBytes []byte) {
	if c == nil || len(wasmBytes) == 0 {
		return
	}
	hash := wasmCacheKey(wasmBytes)

	// Store the content-addressed file.
	c.mu.Lock()
	path := c.cachePath(hash)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		tmpPath := path + ".tmp"
		if wErr := os.WriteFile(tmpPath, wasmBytes, 0644); wErr != nil {
			slog.WarnContext(context.Background(), "wasm disk cache: write failed", "hash", hash, "error", wErr)
			os.Remove(tmpPath)
		} else if wErr := os.Rename(tmpPath, path); wErr != nil {
			slog.WarnContext(context.Background(), "wasm disk cache: rename failed", "hash", hash, "error", wErr)
			os.Remove(tmpPath)
		}
	}

	// Update the index.
	idx := c.loadIndex()
	key := defIndexKey{Tenant: tenantID, Name: name, Version: version}
	if existingHash, exists := idx[key]; exists && existingHash == hash {
		c.mu.Unlock()
		return // already indexed
	}
	idx[key] = hash
	c.saveIndex(idx)
	c.mu.Unlock()

	// Evict LRU entries if we've exceeded the maximum.
	c.evictLRU()
}

// LookupBytes attempts to load raw WASM bytes from the disk cache by content.
// Returns the bytes if found, nil otherwise.
func (c *WasmDiskCache) LookupBytes(wasmBytes []byte) []byte {
	if c == nil {
		return nil
	}
	key := wasmCacheKey(wasmBytes)
	return c.LookupByKey(key)
}

// LookupByKey retrieves raw WASM bytes by their sha256 cache key.
// Returns nil if the entry does not exist.
func (c *WasmDiskCache) LookupByKey(key string) []byte {
	if c == nil || key == "" {
		return nil
	}
	path := c.cachePath(key)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return data
}

// evictLRU removes the least recently used (by mtime) cached files when the
// cache directory contains more than maxLen entries.
func (c *WasmDiskCache) evictLRU() {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}

	// Filter to only .wasm files (skip index.json, .tmp files).
	var wasmFiles []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if filepath.Ext(e.Name()) == ".wasm" {
			wasmFiles = append(wasmFiles, filepath.Join(c.dir, e.Name()))
		}
	}

	if len(wasmFiles) <= c.maxLen {
		return
	}

	// Sort by mtime ascending (oldest first).
	sort.Slice(wasmFiles, func(i, j int) bool {
		si, erri := os.Stat(wasmFiles[i])
		sj, errj := os.Stat(wasmFiles[j])
		if erri != nil || errj != nil {
			return false
		}
		return si.ModTime().Before(sj.ModTime())
	})

	// Build a set of hashes being evicted so we can remove them from the index.
	evictedHashes := make(map[string]bool)
	toRemove := len(wasmFiles) - c.maxLen
	for i := 0; i < toRemove; i++ {
		name := filepath.Base(wasmFiles[i])
		if len(name) > 5 { // strip .wasm
			evictedHashes[name[:len(name)-5]] = true
		}
		if err := os.Remove(wasmFiles[i]); err != nil {
			slog.WarnContext(context.Background(), "wasm disk cache: evict failed", "file", wasmFiles[i], "error", err)
		}
	}

	// Remove evicted entries from the index.
	idx := c.loadIndex()
	for key, hash := range idx {
		if evictedHashes[hash] {
			delete(idx, key)
		}
	}
	c.saveIndex(idx)
}
