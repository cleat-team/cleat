package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewWasmDiskCache_EmptyDir(t *testing.T) {
	c := NewWasmDiskCache("", 10)
	if c != nil { t.Error("expected nil for empty cache dir") }
}

func TestNewWasmDiskCache_ValidDir(t *testing.T) {
	dir := t.TempDir()
	c := NewWasmDiskCache(dir, 10)
	if c == nil { t.Fatal("expected non-nil cache") }
	if c.dir != dir { t.Errorf("expected dir=%q, got %q", dir, c.dir) }
	if c.maxLen != 10 { t.Errorf("expected maxLen=10, got %d", c.maxLen) }
}

func TestNewWasmDiskCache_DefaultMaxLen(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 0)
	if c == nil { t.Fatal("expected non-nil cache") }
	if c.maxLen != 100 { t.Errorf("expected default maxLen=100, got %d", c.maxLen) }
}

func TestNewWasmDiskCache_NegativeMaxLen(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), -5)
	if c == nil { t.Fatal("expected non-nil cache") }
	if c.maxLen != 100 { t.Errorf("expected default maxLen=100, got %d", c.maxLen) }
}

func TestWasmCacheKey(t *testing.T) {
	k := wasmCacheKey([]byte("hello"))
	if len(k) != 64 { t.Errorf("expected SHA-256 hex length 64, got %d", len(k)) }
	k2 := wasmCacheKey([]byte("hello"))
	if k != k2 { t.Error("expected deterministic keys") }
}

func TestWasmCacheKey_DifferentInput(t *testing.T) {
	if wasmCacheKey([]byte("hello")) == wasmCacheKey([]byte("world")) {
		t.Error("different inputs should produce different keys")
	}
}

func TestDefIndexKey(t *testing.T) {
	if defIndexKey("wf", 3) != "wf:3" { t.Error("bad index key") }
}

func TestDefIndexKey_ColonInName(t *testing.T) {
	if defIndexKey("ns:wf", 3) != "ns:wf:3" { t.Error("bad colon index key") }
}

func TestWasmDiskCache_StoreDefAndLookupDef(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	wasm := []byte("fake-wasm-binary")
	c.StoreDef("wf", 1, wasm)
	r := c.LookupDef("wf", 1)
	if r == nil { t.Fatal("expected result") }
	if string(r) != string(wasm) { t.Errorf("mismatch: %q vs %q", wasm, r) }
}

func TestWasmDiskCache_LookupDef_Miss(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	if c.LookupDef("nope", 99) != nil { t.Error("expected nil") }
}

func TestWasmDiskCache_LookupDef_WrongVersion(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	c.StoreDef("wf", 1, []byte("v1"))
	if c.LookupDef("wf", 2) != nil { t.Error("expected nil for wrong version") }
}

func TestWasmDiskCache_LookupDef_NilReceiver(t *testing.T) {
	var c *WasmDiskCache
	if c.LookupDef("x", 1) != nil { t.Error("expected nil") }
}

func TestWasmDiskCache_StoreDef_Duplicate(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	wasm := []byte("data")
	c.StoreDef("wf", 1, wasm)
	c.StoreDef("wf", 1, wasm)
	if string(c.LookupDef("wf", 1)) != string(wasm) { t.Error("mismatch") }
}

func TestWasmDiskCache_StoreDef_EmptyBytes(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	c.StoreDef("wf", 1, []byte{})
	if c.LookupDef("wf", 1) != nil { t.Error("expected nil for empty bytes") }
}

func TestWasmDiskCache_StoreDef_NilReceiver(t *testing.T) {
	var c *WasmDiskCache
	c.StoreDef("wf", 1, []byte("data"))
}

func TestWasmDiskCache_StoreDef_UpdateVersion(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	v1 := []byte("v1-data"); v2 := []byte("v2-data-different")
	c.StoreDef("wf", 1, v1); c.StoreDef("wf", 2, v2)
	if string(c.LookupDef("wf", 1)) != string(v1) { t.Error("v1 lost") }
	if string(c.LookupDef("wf", 2)) != string(v2) { t.Error("v2 missing") }
}

func TestWasmDiskCache_LookupBytes(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	wasm := []byte("content")
	c.StoreDef("wf", 1, wasm)
	r := c.LookupBytes(wasm)
	if r == nil { t.Fatal("expected result") }
	if string(r) != string(wasm) { t.Error("mismatch") }
}

func TestWasmDiskCache_LookupBytes_Miss(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	if c.LookupBytes([]byte("unknown")) != nil { t.Error("expected nil") }
}

func TestWasmDiskCache_LookupBytes_NilReceiver(t *testing.T) {
	var c *WasmDiskCache
	if c.LookupBytes([]byte("x")) != nil { t.Error("expected nil") }
}

func TestWasmDiskCache_LookupByKey(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	wasm := []byte("by-key")
	c.StoreDef("wf", 1, wasm)
	r := c.LookupByKey(wasmCacheKey(wasm))
	if r == nil { t.Fatal("expected result") }
	if string(r) != string(wasm) { t.Error("mismatch") }
}

func TestWasmDiskCache_LookupByKey_Empty(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	if c.LookupByKey("") != nil { t.Error("expected nil for empty key") }
}

func TestWasmDiskCache_LookupByKey_Missing(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	if c.LookupByKey("no-such-hash") != nil { t.Error("expected nil") }
}

func TestWasmDiskCache_LookupByKey_NilReceiver(t *testing.T) {
	var c *WasmDiskCache
	if c.LookupByKey("abc") != nil { t.Error("expected nil") }
}

func TestWasmDiskCache_IndexRoundTrip(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	idx := map[string]string{"a:1": "h1", "b:2": "h2"}
	c.saveIndex(idx)
	loaded := c.loadIndex()
	if len(loaded) != 2 { t.Fatalf("expected 2 entries, got %d", len(loaded)) }
	if loaded["a:1"] != "h1" || loaded["b:2"] != "h2" { t.Error("mismatch") }
}

func TestWasmDiskCache_LoadIndex_Missing(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	if len(c.loadIndex()) != 0 { t.Error("expected empty") }
}

func TestWasmDiskCache_LoadIndex_Corrupted(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	os.WriteFile(c.indexFilePath(), []byte("{{{bad json"), 0644)
	if len(c.loadIndex()) != 0 { t.Error("expected empty for corrupted") }
	c.StoreDef("recover", 1, []byte("recovered"))
	if c.LookupDef("recover", 1) == nil { t.Error("cache should work after corrupted index") }
}

func TestWasmDiskCache_EvictLRU_UnderLimit(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 10)
	c.StoreDef("a", 1, []byte("d1")); c.StoreDef("b", 1, []byte("d2"))
	if c.LookupDef("a", 1) == nil { t.Error("a lost") }
	if c.LookupDef("b", 1) == nil { t.Error("b lost") }
}

func TestWasmDiskCache_EvictLRU_OverLimit(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 2)
	c.StoreDef("a", 1, []byte("da"))
	time.Sleep(10 * time.Millisecond)
	c.StoreDef("b", 1, []byte("db"))
	time.Sleep(10 * time.Millisecond)
	c.StoreDef("c", 1, []byte("dc"))
	if c.LookupDef("a", 1) != nil { t.Error("a should be evicted (oldest)") }
	if c.LookupDef("b", 1) == nil { t.Error("b should survive") }
	if c.LookupDef("c", 1) == nil { t.Error("c should survive") }
}

func TestWasmDiskCache_EvictLRU_IndexCleanup(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 1)
	c.StoreDef("a", 1, []byte("da"))
	time.Sleep(10 * time.Millisecond)
	c.StoreDef("b", 1, []byte("db"))
	if c.LookupDef("a", 1) != nil { t.Error("a evicted from disk") }
	idx := c.loadIndex()
	if _, ok := idx[defIndexKey("a", 1)]; ok { t.Error("a should be gone from index") }
	if _, ok := idx[defIndexKey("b", 1)]; !ok { t.Error("b should be in index") }
}

func TestWasmDiskCache_StoreDef_RespectsMaxLen(t *testing.T) {
	c := NewWasmDiskCache(t.TempDir(), 1)
	c.StoreDef("first", 1, []byte("f"))
	time.Sleep(10 * time.Millisecond)
	c.StoreDef("second", 1, []byte("s"))
	if c.LookupDef("first", 1) != nil { t.Error("first evicted") }
	if c.LookupDef("second", 1) == nil { t.Error("second missing") }
}

func TestWasmDiskCache_cachePath(t *testing.T) {
	c := &WasmDiskCache{dir: "/tmp"}
	if c.cachePath("abc") != filepath.Join("/tmp", "abc.wasm") { t.Error("bad path") }
}

func TestWasmDiskCache_indexFilePath(t *testing.T) {
	c := &WasmDiskCache{dir: "/tmp"}
	if c.indexFilePath() != filepath.Join("/tmp", "index.json") { t.Error("bad indexPath") }
}
