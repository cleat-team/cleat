package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// One tenant, for the tests that predate the index being tenant-scoped and
// are not about tenancy.
const testCacheTenant = "00000000-0000-0000-0000-000000000000"

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
		c.StoreDef(testCacheTenant, string(rune('a'+i%26))+"-"+string(rune('0'+i/26)), i, data)
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

	c.StoreDef(testCacheTenant, "mywf", 1, data)

	got := c.LookupDef(testCacheTenant, "mywf", 1)
	if string(got) != string(data) {
		t.Fatalf("LookupDef = %q, want %q", got, data)
	}

	if c.LookupDef(testCacheTenant, "mywf", 2) != nil {
		t.Error("LookupDef for wrong version should be nil")
	}
	if c.LookupDef(testCacheTenant, "other", 1) != nil {
		t.Error("LookupDef for wrong name should be nil")
	}
}

func TestWasmDiskCache_StoreAndLookupBytes(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 100)
	data := []byte("some wasm module bytes")

	c.StoreDef(testCacheTenant, "wf", 1, data)

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

	c.StoreDef(testCacheTenant, "wf", 1, v1)
	c.StoreDef(testCacheTenant, "wf", 2, v2)

	if string(c.LookupDef(testCacheTenant, "wf", 1)) != string(v1) {
		t.Error("v1 bytes mismatch")
	}
	if string(c.LookupDef(testCacheTenant, "wf", 2)) != string(v2) {
		t.Error("v2 bytes mismatch")
	}
}

func TestWasmDiskCache_StoreIdempotent(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 100)
	data := []byte("idempotent test data")

	// Store twice — second call should be a no-op.
	c.StoreDef(testCacheTenant, "wf", 1, data)
	c.StoreDef(testCacheTenant, "wf", 1, data)

	got := c.LookupDef(testCacheTenant, "wf", 1)
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

	c.StoreDef(testCacheTenant, "wf", 1, bytesA)
	c.StoreDef(testCacheTenant, "wf", 1, bytesB)

	got := c.LookupDef(testCacheTenant, "wf", 1)
	if string(got) != string(bytesB) {
		t.Fatalf("overwrite: got %q, want %q", got, bytesB)
	}
}

// ---------------------------------------------------------------------------
// Miss and edge case tests
// ---------------------------------------------------------------------------

func TestWasmDiskCache_LookupDef_Miss(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 100)
	if c.LookupDef(testCacheTenant, "nope", 1) != nil {
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

	c.StoreDef(testCacheTenant, "wf", 1, data)
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
	c1.StoreDef(testCacheTenant, "wf", 1, data)

	// Create a second cache on the same directory.
	c2 := NewWasmDiskCache(dir, 100)
	got := c2.LookupDef(testCacheTenant, "wf", 1)
	if string(got) != string(data) {
		t.Fatalf("index persistence: got %q, want %q", got, data)
	}
}

func TestWasmDiskCache_IndexRoundTrip_ColonsInName(t *testing.T) {
	dir := t.TempDir()
	c := NewWasmDiskCache(dir, 100)
	data := []byte("namespaced workflow data")

	c.StoreDef(testCacheTenant, "ns:workflow", 3, data)

	// Verify direct index file contents.
	idxPath := filepath.Join(dir, "index.v2.json")
	raw, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatalf("read index.v2.json: %v", err)
	}
	var entries []indexEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("unmarshal index.v2.json: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Tenant == testCacheTenant && e.Name == "ns:workflow" && e.Version == 3 {
			found = true
			break
		}
	}
	if !found {
		t.Error("index.v2.json missing entry for ns:workflow v3")
	}

	// Reload from disk.
	c2 := NewWasmDiskCache(dir, 100)
	got := c2.LookupDef(testCacheTenant, "ns:workflow", 3)
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

	// Distinct mtimes, set explicitly rather than waited for.
	//
	// This loop used to be `StoreDef(...); time.Sleep(time.Millisecond)` with
	// the comment "small sleeps to guarantee distinct mtimes". It does not
	// guarantee that: filesystem mtime granularity is coarser than a
	// millisecond on some runners, and evictLRU sorts by mtime with no
	// tiebreak using sort.Slice, which is not stable. Two entries sharing an
	// mtime therefore evict in arbitrary order.
	//
	// It failed on a SQL Server CI run as exactly that -- "wf v2 should have
	// been evicted" together with "wf v3 should have survived" -- which is the
	// pair swapping places. CLAUDE.md's rule covers it exactly -- "if an
	// assertion depends on wall-clock time, remove the timing rather than
	// widening it" -- and the case that produced the rule was this same shape:
	// a 2 ms sleep in the zombie-writer scenario survived four CI runs and lost
	// the fifth. Widening the sleep would have hidden it again for a while.
	//
	// os.Chtimes removes the dependency instead: the ordering under test is now
	// stated, not raced for. The timestamps are in the past so that each
	// StoreDef's own eviction pass -- it evicts inline -- sees the file it just
	// wrote as the newest.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, e := range stored {
		c.StoreDef(testCacheTenant, e.name, e.version, e.data)
		ts := base.Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(c.cachePath(wasmCacheKey(e.data)), ts, ts); err != nil {
			t.Fatalf("Chtimes for %s v%d: %v", e.name, e.version, err)
		}
	}

	// First 2 entries should have been evicted.
	if c.LookupDef(testCacheTenant, "wf", 1) != nil {
		t.Error("entry wf v1 should have been evicted")
	}
	if c.LookupDef(testCacheTenant, "wf", 2) != nil {
		t.Error("entry wf v2 should have been evicted")
	}
	// Last 3 entries should survive (maxLen = 3).
	if c.LookupDef(testCacheTenant, "wf", 3) == nil {
		t.Error("entry wf v3 should have survived eviction")
	}
	if c.LookupDef(testCacheTenant, "wf", 4) == nil {
		t.Error("entry wf v4 should have survived eviction")
	}
	if c.LookupDef(testCacheTenant, "wf", 5) == nil {
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
		if c.LookupDef(testCacheTenant, "x", 1) != nil {
			t.Error("nil receiver LookupDef should return nil")
		}
	})
	t.Run("StoreDef", func(t *testing.T) {
		// Must not panic.
		c.StoreDef(testCacheTenant, "x", 1, []byte("data"))
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

	c.StoreDef(testCacheTenant, "x", 1, nil)
	c.StoreDef(testCacheTenant, "x", 1, []byte{})

	// No files should have been created.
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	// Only the index should exist (saveIndex is not called when wasmBytes is empty).
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
	// The key is a struct, so a name containing the old ":" delimiter is no
	// longer a parsing question at all -- which is why the string form went
	// away rather than growing a third field.
	if (defIndexKey{Tenant: "t1", Name: "mywf", Version: 1}) == (defIndexKey{Tenant: "t1", Name: "mywf", Version: 2}) {
		t.Error("versions must not collide")
	}
	if (defIndexKey{Tenant: "t1", Name: "a:b:c", Version: 0}) == (defIndexKey{Tenant: "t1", Name: "a:b", Version: 0}) {
		t.Error("names containing the old delimiter must not collide")
	}
	// The property this key exists for.
	if (defIndexKey{Tenant: "t1", Name: "orders", Version: 1}) == (defIndexKey{Tenant: "t2", Name: "orders", Version: 1}) {
		t.Error("two tenants' identically-named definitions must not share an index entry")
	}
}

// TestWasmDiskCache_TwoTenantsSameDefName is the defect cleat#1931's second
// half describes: the blobs are content-addressed and were never the hazard,
// but the index answered ("orders", 1) with whichever tenant wrote it first.
func TestWasmDiskCache_TwoTenantsSameDefName(t *testing.T) {
	dir := t.TempDir()
	c := NewWasmDiskCache(dir, 100)

	aBytes := []byte("tenant A's orders module")
	bBytes := []byte("tenant B's orders module")
	c.StoreDef("tenant-a", "orders", 1, aBytes)
	c.StoreDef("tenant-b", "orders", 1, bBytes)

	// KNOWN-POSITIVE FIRST: both are retrievable at all. Two nils are also
	// "not equal to each other's bytes", so without this the assertions below
	// pass against a cache that stored nothing.
	if got := c.LookupDef("tenant-a", "orders", 1); string(got) != string(aBytes) {
		t.Fatalf("tenant A cannot read back its OWN module: %q", got)
	}
	if got := c.LookupDef("tenant-b", "orders", 1); string(got) != string(bBytes) {
		t.Fatalf("tenant B got %q for its own module, want %q -- one tenant's "+
			"definition is being served to another", got, bBytes)
	}

	// And across a restart, which is the whole reason the disk layer exists.
	c2 := NewWasmDiskCache(dir, 100)
	if got := c2.LookupDef("tenant-b", "orders", 1); string(got) != string(bBytes) {
		t.Errorf("after reopening, tenant B got %q, want %q", got, bBytes)
	}
	if got := c2.LookupDef("tenant-c", "orders", 1); got != nil {
		t.Errorf("a tenant that stored nothing got %q; the index is answering "+
			"across the tenant boundary", got)
	}
}

// An index written before cleat#1931 records no tenant. It must not be read as
// if its entries belonged to the empty tenant -- "" is a real tenant here, the
// one an unset workflow row resolves to.
func TestWasmDiskCache_PreTenantIndexIsNotAdopted(t *testing.T) {
	dir := t.TempDir()
	// A v1 index, in the old shape, pointing at a blob that really is present.
	blob := []byte("bytes from before the tenant existed")
	hash := wasmCacheKey(blob)
	if err := os.WriteFile(filepath.Join(dir, hash+".wasm"), blob, 0644); err != nil {
		t.Fatal(err)
	}
	old := `[{"name":"orders","version":1,"hash":"` + hash + `"}]`
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(old), 0644); err != nil {
		t.Fatal(err)
	}

	c := NewWasmDiskCache(dir, 100)
	if got := c.LookupDef("", "orders", 1); got != nil {
		t.Errorf("an untenanted pre-cleat#1931 entry was served to the empty "+
			"tenant: %q. Those bytes belong to whichever tenant the worker "+
			"happened to be running as when they were written.", got)
	}
	// The control: the miss above must be the index being ignored, not the
	// blob being absent.
	if got := c.LookupByKey(hash); string(got) != string(blob) {
		t.Fatalf("the blob itself is unreadable, so the miss above proves nothing")
	}
}
