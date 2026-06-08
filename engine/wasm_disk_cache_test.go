package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// NewWasmDiskCache tests
// ---------------------------------------------------------------------------

func TestNewWasmDiskCache_EmptyDir(t *testing.T) {
	c := NewWasmDiskCache("", 10)
	if c != nil {
		t.Error("NewWasmDiskCache with empty dir should return nil")
	}
}

func TestNewWasmDiskCache_ValidDir(t *testing.T) {
	// Valid dir with explicit maxLen.
	c := NewWasmDiskCache(t.TempDir(), 10)
	if c == nil {
		t.Fatal("NewWasmDiskCache with valid dir should return non-nil")
	}
	if _, err := os.Stat(c.dir); err != nil {
		t.Errorf("cache dir should exist: %v", err)
	}

	// maxLen <= 0 defaults to 100.
	c2 := NewWasmDiskCache(t.TempDir(), 0)
	if c2 == nil || c2.maxLen != 100 {
		t.Errorf("maxLen=0 should default to 100, got %v", c2)
	}
	c3 := NewWasmDiskCache(t.TempDir(), -1)
	if c3 == nil || c3.maxLen != 100 {
		t.Errorf("maxLen=-1 should default to 100, got %v", c3)
	}
}

// ---------------------------------------------------------------------------
// StoreDef + LookupDef round-trip tests
// ---------------------------------------------------------------------------

func TestStoreLookupDef_RoundTrip(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	if c == nil {
		t.Fatal("NewWasmDiskCache returned nil")
	}

	wasmBytes := []byte("fake wasm module bytes for testing")
	c.StoreDef("my-workflow", 1, wasmBytes)

	got := c.LookupDef("my-workflow", 1)
	if got == nil {
		t.Fatal("LookupDef should return bytes after StoreDef")
	}
	if string(got) != string(wasmBytes) {
		t.Errorf("LookupDef returned wrong bytes: got %q, want %q", got, wasmBytes)
	}

	// Verify .wasm file exists on disk.
	hash := sha256Hash(wasmBytes)
	path := filepath.Join(c.dir, hash+".wasm")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Errorf(".wasm file should exist at %s", path)
	}
}

func TestStoreLookupDef_Miss(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	if c == nil {
		t.Fatal("NewWasmDiskCache returned nil")
	}

	if got := c.LookupDef("nonexistent", 1); got != nil {
		t.Error("LookupDef for missing entry should return nil")
	}
}

func TestStoreLookupDef_NilReceiver(t *testing.T) {
	var c *WasmDiskCache

	// StoreDef should not panic.
	c.StoreDef("wf", 1, []byte("bytes"))

	// LookupDef should return nil.
	if got := c.LookupDef("wf", 1); got != nil {
		t.Error("LookupDef on nil receiver should return nil")
	}
}

func TestStoreDef_EmptyBytes(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	if c == nil {
		t.Fatal("NewWasmDiskCache returned nil")
	}

	c.StoreDef("wf", 1, nil)
	c.StoreDef("wf", 1, []byte{})

	// Empty bytes should be a no-op.
	if got := c.LookupDef("wf", 1); got != nil {
		t.Error("LookupDef after empty StoreDef should return nil")
	}

	// No .wasm files should have been created.
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			t.Errorf("no .wasm files should exist after empty store, found %s", e.Name())
		}
	}
}

func TestStoreDef_ReVersion(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	if c == nil {
		t.Fatal("NewWasmDiskCache returned nil")
	}

	v1 := []byte("version 1 wasm bytes")
	v2 := []byte("version 2 wasm bytes with different content")

	c.StoreDef("wf", 1, v1)
	c.StoreDef("wf", 2, v2)

	got1 := c.LookupDef("wf", 1)
	got2 := c.LookupDef("wf", 2)

	if string(got1) != string(v1) {
		t.Errorf("version 1: got %q, want %q", got1, v1)
	}
	if string(got2) != string(v2) {
		t.Errorf("version 2: got %q, want %q", got2, v2)
	}

	// Two different .wasm files should exist.
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	wasmCount := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			wasmCount++
		}
	}
	if wasmCount != 2 {
		t.Errorf("expected 2 .wasm files, got %d", wasmCount)
	}
}

// ---------------------------------------------------------------------------
// LookupBytes tests
// ---------------------------------------------------------------------------

func TestLookupBytes_Hit(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	if c == nil {
		t.Fatal("NewWasmDiskCache returned nil")
	}

	wasmBytes := []byte("wasm module for LookupBytes test")
	c.StoreDef("wf", 1, wasmBytes)

	got := c.LookupBytes(wasmBytes)
	if got == nil {
		t.Fatal("LookupBytes should return bytes after store")
	}
	if string(got) != string(wasmBytes) {
		t.Errorf("got %q, want %q", got, wasmBytes)
	}
}

func TestLookupBytes_Miss(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	if c == nil {
		t.Fatal("NewWasmDiskCache returned nil")
	}

	if got := c.LookupBytes([]byte("never stored")); got != nil {
		t.Error("LookupBytes for missing content should return nil")
	}
}

func TestLookupBytes_NilReceiver(t *testing.T) {
	var c *WasmDiskCache
	if got := c.LookupBytes([]byte("bytes")); got != nil {
		t.Error("LookupBytes on nil receiver should return nil")
	}
}

// ---------------------------------------------------------------------------
// LookupByKey tests
// ---------------------------------------------------------------------------

func TestLookupByKey_Hit(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	if c == nil {
		t.Fatal("NewWasmDiskCache returned nil")
	}

	wasmBytes := []byte("wasm for LookupByKey test")
	c.StoreDef("wf", 1, wasmBytes)

	key := sha256Hash(wasmBytes)
	got := c.LookupByKey(key)
	if got == nil {
		t.Fatal("LookupByKey should return bytes")
	}
	if string(got) != string(wasmBytes) {
		t.Errorf("got %q, want %q", got, wasmBytes)
	}
}

func TestLookupByKey_Miss_EmptyKey(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	if c == nil {
		t.Fatal("NewWasmDiskCache returned nil")
	}

	if got := c.LookupByKey(""); got != nil {
		t.Error("LookupByKey with empty key should return nil")
	}
	if got := c.LookupByKey("nonexistent-hash"); got != nil {
		t.Error("LookupByKey with unknown key should return nil")
	}
}

func TestLookupByKey_NilReceiver(t *testing.T) {
	var c *WasmDiskCache
	if got := c.LookupByKey("abc123"); got != nil {
		t.Error("LookupByKey on nil receiver should return nil")
	}
}

// ---------------------------------------------------------------------------
// Index persistence tests
// ---------------------------------------------------------------------------

func TestIndex_SurvivesReload(t *testing.T) {
	dir := t.TempDir()

	c1 := NewWasmDiskCache(dir, 10)
	if c1 == nil {
		t.Fatal("NewWasmDiskCache returned nil")
	}

	bytes1 := []byte("workflow one")
	bytes2 := []byte("workflow two")
	c1.StoreDef("wf-a", 1, bytes1)
	c1.StoreDef("wf-b", 2, bytes2)

	// Create a new cache on the same dir — should load the existing index.
	c2 := NewWasmDiskCache(dir, 10)
	if c2 == nil {
		t.Fatal("second NewWasmDiskCache returned nil")
	}

	got1 := c2.LookupDef("wf-a", 1)
	got2 := c2.LookupDef("wf-b", 2)

	if string(got1) != string(bytes1) {
		t.Errorf("wf-a after reload: got %q, want %q", got1, bytes1)
	}
	if string(got2) != string(bytes2) {
		t.Errorf("wf-b after reload: got %q, want %q", got2, bytes2)
	}
}

func TestIndex_DedupContent(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	if c == nil {
		t.Fatal("NewWasmDiskCache returned nil")
	}

	sameBytes := []byte("shared content")
	c.StoreDef("wf-a", 1, sameBytes)
	c.StoreDef("wf-b", 1, sameBytes)

	if string(c.LookupDef("wf-a", 1)) != string(sameBytes) {
		t.Error("wf-a lookup failed")
	}
	if string(c.LookupDef("wf-b", 1)) != string(sameBytes) {
		t.Error("wf-b lookup failed")
	}

	// Only one .wasm file since both have the same content hash.
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	wasmCount := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			wasmCount++
		}
	}
	if wasmCount != 1 {
		t.Errorf("expected 1 .wasm file (dedup by content), got %d", wasmCount)
	}
}

// ---------------------------------------------------------------------------
// LRU eviction tests
// ---------------------------------------------------------------------------

func TestEvictLRU_Basic(t *testing.T) {
	dir := t.TempDir()
	c := NewWasmDiskCache(dir, 3)
	if c == nil {
		t.Fatal("NewWasmDiskCache returned nil")
	}

	// Store 5 entries with controlled mtimes so oldest are deterministic.
	entries := []struct {
		name    string
		version int
		bytes   []byte
	}{
		{"wf-1", 1, []byte("entry 1")},
		{"wf-2", 1, []byte("entry 2")},
		{"wf-3", 1, []byte("entry 3")},
		{"wf-4", 1, []byte("entry 4")},
		{"wf-5", 1, []byte("entry 5")},
	}

	for i, e := range entries {
		c.StoreDef(e.name, e.version, e.bytes)
		// Set mtime to a known-ordered value: entry 0 = oldest, entry 4 = newest.
		hash := sha256Hash(e.bytes)
		path := filepath.Join(dir, hash+".wasm")
		mt := time.Unix(1000+int64(i), 0)
		if err := os.Chtimes(path, mt, mt); err != nil {
			t.Fatalf("Chtimes(%s): %v", path, err)
		}
	}

	// Trigger eviction by storing one more entry.
	c.StoreDef("wf-6", 1, []byte("entry 6"))
	hash6 := sha256Hash([]byte("entry 6"))
	path6 := filepath.Join(dir, hash6+".wasm")
	mt6 := time.Unix(1006, 0)
	if err := os.Chtimes(path6, mt6, mt6); err != nil {
		t.Fatalf("Chtimes(%s): %v", path6, err)
	}

	// Only 3 .wasm files should remain.
	allEntries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	wasmCount := 0
	for _, e := range allEntries {
		if filepath.Ext(e.Name()) == ".wasm" {
			wasmCount++
		}
	}
	if wasmCount != 3 {
		t.Errorf("expected 3 .wasm files after eviction, got %d", wasmCount)
	}

	// Oldest entries (wf-1 through wf-3) should be evicted.
	for _, name := range []string{"wf-1", "wf-2", "wf-3"} {
		if got := c.LookupDef(name, 1); got != nil {
			t.Errorf("%s should have been evicted, but LookupDef returned data", name)
		}
	}
	// Newest entries (wf-4, wf-5, wf-6) should survive.
	for _, name := range []string{"wf-4", "wf-5", "wf-6"} {
		if got := c.LookupDef(name, 1); got == nil {
			t.Errorf("%s should survive eviction, but LookupDef returned nil", name)
		}
	}
}

func TestEvictLRU_ExistingOnInit(t *testing.T) {
	dir := t.TempDir()

	// First cache: store 5 entries with controlled mtimes.
	c1 := NewWasmDiskCache(dir, 5)
	if c1 == nil {
		t.Fatal("NewWasmDiskCache returned nil")
	}

	for i, content := range [][]byte{
		[]byte("oldest"),
		[]byte("old"),
		[]byte("mid"),
		[]byte("new"),
		[]byte("newest"),
	} {
		c1.StoreDef("wf", i+1, content)
		hash := sha256Hash(content)
		path := filepath.Join(dir, hash+".wasm")
		mt := time.Unix(1000+int64(i), 0)
		if err := os.Chtimes(path, mt, mt); err != nil {
			t.Fatalf("Chtimes(%s): %v", path, err)
		}
	}

	// Second cache with maxLen=2: constructor calls evictLRU.
	c2 := NewWasmDiskCache(dir, 2)
	if c2 == nil {
		t.Fatal("second NewWasmDiskCache returned nil")
	}

	wasmCount := 0
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			wasmCount++
		}
	}
	if wasmCount != 2 {
		t.Errorf("expected 2 .wasm files after init eviction, got %d", wasmCount)
	}

	// Oldest 3 entries should be gone, newest 2 should survive.
	if got := c2.LookupDef("wf", 1); got != nil {
		t.Error("wf v1 (oldest) should have been evicted")
	}
	if got := c2.LookupDef("wf", 2); got != nil {
		t.Error("wf v2 should have been evicted")
	}
	if got := c2.LookupDef("wf", 3); got != nil {
		t.Error("wf v3 should have been evicted")
	}
	if got := c2.LookupDef("wf", 4); got == nil {
		t.Error("wf v4 should survive eviction")
	}
	if got := c2.LookupDef("wf", 5); got == nil {
		t.Error("wf v5 (newest) should survive eviction")
	}
}

// ---------------------------------------------------------------------------
// wasmCacheKey tests
// ---------------------------------------------------------------------------

func TestWasmCacheKey(t *testing.T) {
	// Same bytes → same key.
	k1 := wasmCacheKey([]byte("hello"))
	k2 := wasmCacheKey([]byte("hello"))
	if k1 != k2 {
		t.Errorf("same bytes should produce same key: %q vs %q", k1, k2)
	}

	// Different values → different keys.
	k3 := wasmCacheKey([]byte("hello"))
	k4 := wasmCacheKey([]byte("world"))
	if k3 == k4 {
		t.Error("different bytes should produce different keys")
	}

	// Different lengths → different keys.
	k5 := wasmCacheKey([]byte("a"))
	k6 := wasmCacheKey([]byte("ab"))
	if k5 == k6 {
		t.Error("different length bytes should produce different keys")
	}

	// Empty bytes → still produces a valid hex key.
	k7 := wasmCacheKey([]byte{})
	if k7 == "" {
		t.Error("empty bytes should produce a non-empty key")
	}
	if len(k7) != 64 {
		t.Errorf("sha256 hex should be 64 chars, got %d", len(k7))
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func sha256Hash(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func countWasmFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".wasm" {
			n++
		}
	}
	return n
}

func TestStoreDef_Idempotent(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	if c == nil {
		t.Fatal("NewWasmDiskCache returned nil")
	}

	wasm := []byte("\x00asm\x01\x00\x00\x00idempotent")
	c.StoreDef("wf", 1, wasm)

	nBefore := countWasmFiles(t, c.dir)
	c.StoreDef("wf", 1, wasm)
	nAfter := countWasmFiles(t, c.dir)

	if nAfter != nBefore {
		t.Errorf("idempotent store changed wasm file count from %d to %d", nBefore, nAfter)
	}

	if got := c.LookupDef("wf", 1); got == nil || string(got) != string(wasm) {
		t.Errorf("LookupDef after idempotent store: got %q, want %q", got, wasm)
	}
}
