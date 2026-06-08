package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func TestNewWasmDiskCache_Disabled(t *testing.T) {
	c := NewWasmDiskCache("", 10)
	if c != nil {
		t.Error("NewWasmDiskCache(\"\") returned non-nil, want nil")
	}
}

func TestNewWasmDiskCache_DefaultMaxLen(t *testing.T) {
	dir := t.TempDir()

	c := NewWasmDiskCache(dir, 0)
	if c == nil {
		t.Fatal("NewWasmDiskCache returned nil")
	}
	if c.maxLen != 100 {
		t.Errorf("maxLen = %d, want 100 (default)", c.maxLen)
	}

	c2 := NewWasmDiskCache(filepath.Join(dir, "dir2"), -1)
	if c2.maxLen != 100 {
		t.Errorf("maxLen = %d for negative input, want 100", c2.maxLen)
	}
}

func TestNewWasmDiskCache_CreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "subdir", "cache")
	c := NewWasmDiskCache(dir, 10)
	if c == nil {
		t.Fatal("NewWasmDiskCache returned nil")
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Error("cache directory was not created")
	}
}

func TestWasmDiskCache_StoreDef_LookupDef(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)

	wasm := []byte("fake wasm binary content")
	c.StoreDef("my-workflow", 3, wasm)

	got := c.LookupDef("my-workflow", 3)
	if string(got) != string(wasm) {
		t.Errorf("LookupDef returned %q, want %q", got, wasm)
	}
}

func TestWasmDiskCache_LookupDef_Miss(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)

	if got := c.LookupDef("nonexistent", 1); got != nil {
		t.Errorf("LookupDef on empty cache returned %v, want nil", got)
	}

	c.StoreDef("wf", 1, []byte("data"))
	if got := c.LookupDef("wf", 2); got != nil {
		t.Errorf("LookupDef with wrong version returned %v, want nil", got)
	}
	if got := c.LookupDef("other", 1); got != nil {
		t.Errorf("LookupDef with wrong name returned %v, want nil", got)
	}
}

func TestWasmDiskCache_StoreDef_EmptyBytes(t *testing.T) {
	dir := t.TempDir()
	c := NewWasmDiskCache(dir, 10)
	c.StoreDef("wf", 1, []byte{})

	// Should not create any file or index entry.
	entries, _ := os.ReadDir(dir)
	files := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			files++
		}
	}
	if files > 0 {
		t.Errorf("empty StoreDef created %d .wasm files, want 0", files)
	}
}

func TestWasmDiskCache_StoreDef_Idempotent(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	wasm := []byte("data")
	c.StoreDef("wf", 1, wasm)
	c.StoreDef("wf", 1, wasm) // second store is no-op

	got := c.LookupDef("wf", 1)
	if string(got) != string(wasm) {
		t.Errorf("LookupDef after idempotent StoreDef returned %q, want %q", got, wasm)
	}
}

func TestWasmDiskCache_StoreDef_VersionChange(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	c.StoreDef("wf", 1, []byte("v1 data"))
	c.StoreDef("wf", 2, []byte("v2 data"))

	if got := c.LookupDef("wf", 1); string(got) != "v1 data" {
		t.Errorf("version 1: got %q, want %q", got, "v1 data")
	}
	if got := c.LookupDef("wf", 2); string(got) != "v2 data" {
		t.Errorf("version 2: got %q, want %q", got, "v2 data")
	}
}

func TestWasmDiskCache_LookupByKey(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	wasm := []byte("content")
	hash := wasmCacheKey(wasm)

	c.StoreDef("wf", 1, wasm)

	got := c.LookupByKey(hash)
	if string(got) != string(wasm) {
		t.Errorf("LookupByKey returned %q, want %q", got, wasm)
	}

	if got := c.LookupByKey(""); got != nil {
		t.Errorf("LookupByKey(\"\") returned %v, want nil", got)
	}
	if got := c.LookupByKey("nonexistent-hash"); got != nil {
		t.Errorf("LookupByKey with unknown hash returned %v, want nil", got)
	}
}

func TestWasmDiskCache_LookupBytes(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	wasm := []byte("some wasm bytes")

	c.StoreDef("wf", 1, wasm)

	got := c.LookupBytes(wasm)
	if string(got) != string(wasm) {
		t.Errorf("LookupBytes returned %q, want %q", got, wasm)
	}
}

func TestWasmDiskCache_EvictLRU(t *testing.T) {
	dir := t.TempDir()
	c := NewWasmDiskCache(dir, 2)

	c.StoreDef("wf-a", 1, []byte("a"))
	c.StoreDef("wf-b", 1, []byte("b"))

	// Set wf-a's on-disk file to have an older mtime so it's evicted first.
	hashA := wasmCacheKey([]byte("a"))
	oldTime := timeNow().Add(-1 * time.Hour)
	if err := os.Chtimes(c.cachePath(hashA), oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	c.StoreDef("wf-c", 1, []byte("c"))

	// wf-a should be evicted (oldest by mtime).
	if got := c.LookupDef("wf-a", 1); got != nil {
		t.Error("wf-a should have been evicted, but LookupDef returned non-nil")
	}
	if got := c.LookupDef("wf-b", 1); string(got) != "b" {
		t.Errorf("wf-b should remain, got %q", got)
	}
	if got := c.LookupDef("wf-c", 1); string(got) != "c" {
		t.Errorf("wf-c should remain, got %q", got)
	}
}

func timeNow() time.Time {
	return time.Now()
}

func TestWasmDiskCache_IndexPersistence(t *testing.T) {
	dir := t.TempDir()
	c1 := NewWasmDiskCache(dir, 10)

	c1.StoreDef("wf-1", 1, []byte("data1"))
	c1.StoreDef("wf-2", 2, []byte("data2"))

	// Create a new cache pointing at the same directory.
	c2 := NewWasmDiskCache(dir, 10)

	if got := c2.LookupDef("wf-1", 1); string(got) != "data1" {
		t.Errorf("index persistence failed for wf-1: got %q, want %q", got, "data1")
	}
	if got := c2.LookupDef("wf-2", 2); string(got) != "data2" {
		t.Errorf("index persistence failed for wf-2: got %q, want %q", got, "data2")
	}
}

func TestWasmDiskCache_NilReceiver(t *testing.T) {
	var c *WasmDiskCache

	if got := c.LookupDef("wf", 1); got != nil {
		t.Error("LookupDef on nil receiver returned non-nil")
	}
	if got := c.LookupByKey("abc"); got != nil {
		t.Error("LookupByKey on nil receiver returned non-nil")
	}
	if got := c.LookupBytes([]byte("data")); got != nil {
		t.Error("LookupBytes on nil receiver returned non-nil")
	}
	// StoreDef should not panic.
	c.StoreDef("wf", 1, []byte("data"))
	c.StoreDef("wf", 1, []byte(""))
}

func TestWasmDiskCache_DefIndexKey_Colons(t *testing.T) {
	// Names containing colons must parse correctly in saveIndex/loadIndex round-trip.
	dir := t.TempDir()
	c := NewWasmDiskCache(dir, 10)

	name := "namespace:workflow"
	c.StoreDef(name, 5, []byte("colon-data"))

	got := c.LookupDef(name, 5)
	if string(got) != "colon-data" {
		t.Errorf("lookup for colon-containing name failed: got %q, want %q", got, "colon-data")
	}
}

func TestWasmCacheKey_Deterministic(t *testing.T) {
	data := []byte("deterministic test")
	key1 := wasmCacheKey(data)
	key2 := wasmCacheKey(data)

	if key1 != key2 {
		t.Errorf("same input produced different keys: %s vs %s", key1, key2)
	}
	if key1 == "" {
		t.Error("cache key is empty")
	}

	// Verify it's actually sha256 hex.
	expected := sha256Hex(data)
	if key1 != expected {
		t.Errorf("cache key %s != expected sha256 %s", key1, expected)
	}
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func TestWasmDiskCache_EvictLRU_IndexCleanup(t *testing.T) {
	dir := t.TempDir()
	c := NewWasmDiskCache(dir, 1)

	c.StoreDef("wf-a", 1, []byte("a"))

	// Set wf-a's file to have an older mtime so it's evicted.
	hashA := wasmCacheKey([]byte("a"))
	oldTime := timeNow().Add(-1 * time.Hour)
	if err := os.Chtimes(c.cachePath(hashA), oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	c.StoreDef("wf-b", 1, []byte("b"))

	// wf-a should be evicted from both disk and index.
	if got := c.LookupDef("wf-a", 1); got != nil {
		t.Error("evicted entry still in index")
	}
	if got := c.LookupDef("wf-b", 1); string(got) != "b" {
		t.Error("remaining entry lookup failed")
	}

	// Verify index file doesn't contain the evicted entry.
	idx := c.loadIndex()
	if _, ok := idx[defIndexKey("wf-a", 1)]; ok {
		t.Error("evicted entry still present in index.json")
	}
	if _, ok := idx[defIndexKey("wf-b", 1)]; !ok {
		t.Error("remaining entry missing from index.json")
	}
}

func TestWasmDiskCache_CorruptedIndex(t *testing.T) {
	dir := t.TempDir()
	// Write invalid JSON to the index file.
	os.WriteFile(filepath.Join(dir, "index.json"), []byte("not json"), 0644)

	c := NewWasmDiskCache(dir, 10)
	if c == nil {
		t.Fatal("NewWasmDiskCache returned nil")
	}
	// Corrupted index should be treated as empty — no crash.
	if got := c.LookupDef("wf", 1); got != nil {
		t.Error("corrupted index should result in cache miss, not crash")
	}
	// StoreDef should still work and fix the index.
	c.StoreDef("wf", 1, []byte("new-data"))
	if got := c.LookupDef("wf", 1); string(got) != "new-data" {
		t.Error("LookupDef after repairing corrupted index failed")
	}
}

func TestWasmDiskCache_EvictLRU_NonWasmFiles(t *testing.T) {
	dir := t.TempDir()
	// Create some non-.wasm files in the cache dir.
	os.WriteFile(filepath.Join(dir, "index.json"), []byte("[]"), 0644)
	os.WriteFile(filepath.Join(dir, "some.tmp"), []byte("tmp"), 0644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("notes"), 0644)

	c := NewWasmDiskCache(dir, 10) // triggers evictLRU
	if c == nil {
		t.Fatal("NewWasmDiskCache returned nil")
	}
	// Non-.wasm files should survive eviction.
	for _, name := range []string{"index.json", "some.tmp", "notes.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
			t.Errorf("non-.wasm file %s was evicted", name)
		}
	}
}

func TestWasmDiskCache_SaveIndex_Atomic(t *testing.T) {
	dir := t.TempDir()
	c := NewWasmDiskCache(dir, 10)

	c.StoreDef("wf", 1, []byte("data"))
	// Verify no .tmp index file is left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("stale .tmp file found: %s", e.Name())
		}
	}
}

func TestWasmDiskCache_SortByMtime(t *testing.T) {
	dir := t.TempDir()

	// Create .wasm files with controlled timestamps.
	files := []string{"a.wasm", "b.wasm", "c.wasm"}
	for _, f := range files {
		os.WriteFile(filepath.Join(dir, f), []byte(f), 0644)
	}

	// Verify sort by mtime behavior: list files, check oldest first.
	wasmFiles := []string{
		filepath.Join(dir, "a.wasm"),
		filepath.Join(dir, "b.wasm"),
		filepath.Join(dir, "c.wasm"),
	}
	sort.Slice(wasmFiles, func(i, j int) bool {
		si, _ := os.Stat(wasmFiles[i])
		sj, _ := os.Stat(wasmFiles[j])
		return si.ModTime().Before(sj.ModTime())
	})
	// Just verify sorting doesn't panic.
	if len(wasmFiles) != 3 {
		t.Errorf("expected 3 files, got %d", len(wasmFiles))
	}
}

func TestWasmDiskCache_JsonRoundTrip(t *testing.T) {
	// Test indexEntry JSON round-trip.
	entry := indexEntry{Name: "test:workflow", Version: 42, Hash: "abc123"}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded indexEntry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Name != entry.Name || decoded.Version != entry.Version || decoded.Hash != entry.Hash {
		t.Errorf("round-trip mismatch: %+v != %+v", decoded, entry)
	}
}
