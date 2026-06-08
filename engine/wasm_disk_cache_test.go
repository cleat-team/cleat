package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Constructor tests
// ---------------------------------------------------------------------------

func TestWasmDiskCache_New_NilOnEmptyDir(t *testing.T) {
	c := NewWasmDiskCache("", 10)
	if c != nil {
		t.Fatal("NewWasmDiskCache with empty dir should return nil")
	}
}

func TestWasmDiskCache_New_CreatesDir(t *testing.T) {
	subdir := filepath.Join(t.TempDir(), "newsubdir")
	c := NewWasmDiskCache(subdir, 10)
	if c == nil {
		t.Fatal("NewWasmDiskCache with valid dir returned nil")
	}
	if _, err := os.Stat(subdir); err != nil {
		t.Fatalf("directory was not created: %v", err)
	}
}

func TestWasmDiskCache_New_DefaultMaxLen(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 0)
	if c == nil {
		t.Fatal("NewWasmDiskCache with maxLen=0 returned nil")
	}
	// Default maxLen is 100. Verify by storing 101 entries and
	// checking that at most 100 .wasm files remain.
	for i := 0; i < 101; i++ {
		data := []byte{byte(i), byte(i >> 8), byte(i >> 16)}
		// Each entry gets a unique name so each creates a distinct index entry.
		c.StoreDef(string(rune('a'+i%26))+"-"+string(rune('0'+i/26)), i, data)
		time.Sleep(time.Millisecond)
	}

	entries, err := os.ReadDir(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	var wasmCount int
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			wasmCount++
		}
	}
	if wasmCount > 100 {
		t.Errorf("expected <=100 .wasm files after storing 101 entries, got %d", wasmCount)
	}
}

// ---------------------------------------------------------------------------
// Core Store + Lookup tests
// ---------------------------------------------------------------------------

func TestWasmDiskCache_StoreAndLookupDef(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 100)
	data := []byte("def wasm bytes for mywf v1")

	c.StoreDef("mywf", 1, data)

	got := c.LookupDef("mywf", 1)
	if string(got) != string(data) {
		t.Fatalf("LookupDef = %q, want %q", got, data)
	}

	if c.LookupDef("mywf", 2) != nil {
		t.Error("LookupDef for wrong version should be nil")
	}
	if c.LookupDef("other", 1) != nil {
		t.Error("LookupDef for wrong name should be nil")
	}
}

func TestWasmDiskCache_StoreAndLookupBytes(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 100)
	data := []byte("some wasm module bytes")

	c.StoreDef("wf", 1, data)

	got := c.LookupBytes(data)
	if string(got) != string(data) {
		t.Fatalf("LookupBytes = %q, want %q", got, data)
	}

	if c.LookupBytes([]byte("never stored")) != nil {
		t.Error("LookupBytes for unknown content should be nil")
	}
}

func TestWasmDiskCache_StoreMultipleVersions(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 100)
	v1 := []byte("wasm v1 bytes")
	v2 := []byte("version 2 different content")

	c.StoreDef("wf", 1, v1)
	c.StoreDef("wf", 2, v2)

	if string(c.LookupDef("wf", 1)) != string(v1) {
		t.Error("v1 bytes mismatch")
	}
	if string(c.LookupDef("wf", 2)) != string(v2) {
		t.Error("v2 bytes mismatch")
	}
}

func TestWasmDiskCache_StoreIdempotent(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 100)
	data := []byte("idempotent test data")

	// Store twice — second call should be a no-op.
	c.StoreDef("wf", 1, data)
	c.StoreDef("wf", 1, data)

	got := c.LookupDef("wf", 1)
	if string(got) != string(data) {
		t.Fatalf("after idempotent store: got %q, want %q", got, data)
	}

	// Only one .wasm file should exist (same content hash).
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	var wasmCount int
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			wasmCount++
		}
	}
	if wasmCount != 1 {
		t.Errorf("expected 1 .wasm file after idempotent stores, got %d", wasmCount)
	}
}

func TestWasmDiskCache_StoreOverwrite(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 100)
	bytesA := []byte("original content")
	bytesB := []byte("updated replacement content")

	c.StoreDef("wf", 1, bytesA)
	c.StoreDef("wf", 1, bytesB)

	got := c.LookupDef("wf", 1)
	if string(got) != string(bytesB) {
		t.Fatalf("overwrite: got %q, want %q", got, bytesB)
	}
}

// ---------------------------------------------------------------------------
// Miss and edge case tests
// ---------------------------------------------------------------------------

func TestWasmDiskCache_LookupDef_Miss(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 100)
	if c.LookupDef("nope", 1) != nil {
		t.Error("LookupDef on empty cache should be nil")
	}
}

func TestWasmDiskCache_LookupBytes_Miss(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 100)
	if c.LookupBytes([]byte("absent")) != nil {
		t.Error("LookupBytes on empty cache should be nil")
	}
}

func TestWasmDiskCache_LookupByKey(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 100)
	data := []byte("key lookup test data")

	c.StoreDef("wf", 1, data)
	hash := wasmCacheKey(data)

	t.Run("happy", func(t *testing.T) {
		got := c.LookupByKey(hash)
		if string(got) != string(data) {
			t.Fatalf("LookupByKey = %q, want %q", got, data)
		}
	})

	t.Run("empty key", func(t *testing.T) {
		if c.LookupByKey("") != nil {
			t.Error("LookupByKey with empty string should be nil")
		}
	})

	t.Run("nonexistent key", func(t *testing.T) {
		if c.LookupByKey("deadbeef") != nil {
			t.Error("LookupByKey with nonexistent key should be nil")
		}
	})
}

// ---------------------------------------------------------------------------
// Index persistence tests
// ---------------------------------------------------------------------------

func TestWasmDiskCache_IndexPersistence(t *testing.T) {
	dir := t.TempDir()
	data := []byte("persistent wasm data")

	// Store in one cache instance.
	c1 := NewWasmDiskCache(dir, 100)
	c1.StoreDef("wf", 1, data)

	// Create a second cache on the same directory.
	c2 := NewWasmDiskCache(dir, 100)
	got := c2.LookupDef("wf", 1)
	if string(got) != string(data) {
		t.Fatalf("index persistence: got %q, want %q", got, data)
	}
}

func TestWasmDiskCache_IndexRoundTrip_ColonsInName(t *testing.T) {
	dir := t.TempDir()
	c := NewWasmDiskCache(dir, 100)
	data := []byte("namespaced workflow data")

	c.StoreDef("ns:workflow", 3, data)

	// Verify direct index file contents.
	idxPath := filepath.Join(dir, "index.json")
	raw, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatalf("read index.json: %v", err)
	}
	var entries []indexEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("unmarshal index.json: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Name == "ns:workflow" && e.Version == 3 {
			found = true
			break
		}
	}
	if !found {
		t.Error("index.json missing entry for ns:workflow v3")
	}

	// Reload from disk.
	c2 := NewWasmDiskCache(dir, 100)
	got := c2.LookupDef("ns:workflow", 3)
	if string(got) != string(data) {
		t.Fatalf("colons round-trip: got %q, want %q", got, data)
	}
}

// ---------------------------------------------------------------------------
// Eviction test
// ---------------------------------------------------------------------------

func TestWasmDiskCache_Eviction(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 3)

	type entry struct {
		name    string
		version int
		data    []byte
	}
	stored := []entry{
		{"wf", 1, []byte("first inserted - will be evicted")},
		{"wf", 2, []byte("second inserted - will be evicted")},
		{"wf", 3, []byte("third inserted - should survive")},
		{"wf", 4, []byte("fourth inserted - should survive")},
		{"wf", 5, []byte("fifth inserted - should survive")},
	}

	// Store with small sleeps to guarantee distinct mtimes.
	for _, e := range stored {
		c.StoreDef(e.name, e.version, e.data)
		time.Sleep(time.Millisecond)
	}

	// First 2 entries should have been evicted.
	if c.LookupDef("wf", 1) != nil {
		t.Error("entry wf v1 should have been evicted")
	}
	if c.LookupDef("wf", 2) != nil {
		t.Error("entry wf v2 should have been evicted")
	}
	// Last 3 entries should survive (maxLen = 3).
	if c.LookupDef("wf", 3) == nil {
		t.Error("entry wf v3 should have survived eviction")
	}
	if c.LookupDef("wf", 4) == nil {
		t.Error("entry wf v4 should have survived eviction")
	}
	if c.LookupDef("wf", 5) == nil {
		t.Error("entry wf v5 should have survived eviction")
	}

	// Verify only 3 .wasm files on disk.
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	var wasmCount int
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			wasmCount++
		}
	}
	if wasmCount != 3 {
		t.Errorf("expected 3 .wasm files after eviction, got %d", wasmCount)
	}

	// Verify index has 3 entries.
	idx := c.loadIndex()
	if len(idx) != 3 {
		t.Errorf("expected 3 index entries after eviction, got %d", len(idx))
	}
}

// ---------------------------------------------------------------------------
// Nil receiver safety tests
// ---------------------------------------------------------------------------

func TestWasmDiskCache_NilReceiver(t *testing.T) {
	var c *WasmDiskCache

	t.Run("LookupDef", func(t *testing.T) {
		if c.LookupDef("x", 1) != nil {
			t.Error("nil receiver LookupDef should return nil")
		}
	})
	t.Run("StoreDef", func(t *testing.T) {
		// Must not panic.
		c.StoreDef("x", 1, []byte("data"))
	})
	t.Run("LookupBytes", func(t *testing.T) {
		if c.LookupBytes([]byte("data")) != nil {
			t.Error("nil receiver LookupBytes should return nil")
		}
	})
	t.Run("LookupByKey", func(t *testing.T) {
		if c.LookupByKey("abc") != nil {
			t.Error("nil receiver LookupByKey should return nil")
		}
	})
}

// ---------------------------------------------------------------------------
// Empty / no-op store test
// ---------------------------------------------------------------------------

func TestWasmDiskCache_EmptyStore(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 100)

	c.StoreDef("x", 1, nil)
	c.StoreDef("x", 1, []byte{})

	// No files should have been created.
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	// Only index.json should exist (saveIndex is not called when wasmBytes is empty).
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			t.Errorf("no .wasm files expected after empty store, found %s", e.Name())
		}
	}
}

// ---------------------------------------------------------------------------
// Helper function tests
// ---------------------------------------------------------------------------

func TestWasmDiskCache_CacheKey(t *testing.T) {
	k1 := wasmCacheKey([]byte("hello"))
	k2 := wasmCacheKey([]byte("hello"))
	k3 := wasmCacheKey([]byte("world"))

	if k1 != k2 {
		t.Errorf("same input should produce same key: %q != %q", k1, k2)
	}
	if k1 == k3 {
		t.Errorf("different input should produce different key: %q == %q", k1, k3)
	}
	if len(k1) != 64 {
		t.Errorf("expected sha256 hex (64 chars), got %d chars", len(k1))
	}
}

func TestWasmDiskCache_DefIndexKey(t *testing.T) {
	if got := defIndexKey("mywf", 1); got != "mywf:1" {
		t.Errorf("defIndexKey(mywf, 1) = %q, want %q", got, "mywf:1")
	}
	if got := defIndexKey("foo:bar", 5); got != "foo:bar:5" {
		t.Errorf("defIndexKey(foo:bar, 5) = %q, want %q", got, "foo:bar:5")
	}
	if got := defIndexKey("a:b:c", 0); got != "a:b:c:0" {
		t.Errorf("defIndexKey(a:b:c, 0) = %q, want %q", got, "a:b:c:0")
	}
}
