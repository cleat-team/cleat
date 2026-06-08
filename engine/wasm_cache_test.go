package engine

import (
	"sync"
	"testing"
)

func TestNewWASMCache(t *testing.T) {
	c := NewWASMCache(10, 1024)
	if c == nil { t.Fatal("NewWASMCache returned nil") }
	if c.Len() != 0 { t.Errorf("expected Len 0, got %d", c.Len()) }
	if c.TotalBytes() != 0 { t.Errorf("expected TotalBytes 0, got %d", c.TotalBytes()) }
}

func TestWASMCache_Get_Miss(t *testing.T) {
	c := NewWASMCache(10, 1024)
	data, ok := c.Get("nonexistent")
	if ok { t.Error("expected false for missing key") }
	if data != nil { t.Error("expected nil data for missing key") }
}

func TestWASMCache_PutAndGet(t *testing.T) {
	c := NewWASMCache(10, 1024)
	c.Put("key1", []byte("hello"))
	data, ok := c.Get("key1")
	if !ok { t.Fatal("expected true for existing key") }
	if string(data) != "hello" { t.Errorf("expected 'hello', got %q", string(data)) }
}

func TestWASMCache_Get_LRUUpdate(t *testing.T) {
	c := NewWASMCache(2, 1024)
	c.Put("key1", []byte("a"))
	c.Put("key2", []byte("b"))
	c.Get("key1")
	c.Put("key3", []byte("c"))
	if _, ok := c.Get("key1"); !ok { t.Error("key1 should survive LRU access") }
	if _, ok := c.Get("key2"); ok { t.Error("key2 should be evicted") }
	if _, ok := c.Get("key3"); !ok { t.Error("key3 should exist") }
}

func TestWASMCache_Put_UpdateExisting(t *testing.T) {
	c := NewWASMCache(10, 1024)
	c.Put("key1", []byte("small"))
	initialBytes := c.TotalBytes()
	c.Put("key1", []byte("much-larger-value"))
	if c.Len() != 1 { t.Errorf("Len should be 1, got %d", c.Len()) }
	if c.TotalBytes() <= initialBytes { t.Error("TotalBytes should increase") }
	data, ok := c.Get("key1")
	if !ok { t.Fatal("key1 missing") }
	if string(data) != "much-larger-value" { t.Errorf("wrong value: %q", string(data)) }
}

func TestWASMCache_Evict_CountLimit(t *testing.T) {
	c := NewWASMCache(2, 1024)
	c.Put("a", []byte("1")); c.Put("b", []byte("2")); c.Put("c", []byte("3"))
	if c.Len() > 2 { t.Errorf("Len should be <=2, got %d", c.Len()) }
	if _, ok := c.Get("a"); ok { t.Error("a should be evicted") }
}

func TestWASMCache_Evict_ByteLimit(t *testing.T) {
	c := NewWASMCache(100, 10)
	c.Put("a", []byte("hello"))
	c.Put("b", []byte("world"))
	c.Put("c", []byte("!"))
	if c.TotalBytes() > 10 { t.Errorf("TotalBytes should be <=10, got %d", c.TotalBytes()) }
}

func TestWASMCache_Evict_NoByteLimit(t *testing.T) {
	c := NewWASMCache(2, 0)
	big := make([]byte, 10000)
	c.Put("a", big); c.Put("b", big)
	if c.Len() != 2 { t.Errorf("Len should be 2, got %d", c.Len()) }
}

func TestWASMCache_Evict_MultipleEntries(t *testing.T) {
	c := NewWASMCache(100, 10)
	c.Put("a", []byte("12345"))
	c.Put("b", []byte("12345"))
	c.Put("c", []byte("1234567890"))
	if c.Len() != 1 { t.Errorf("Len should be 1, got %d", c.Len()) }
}

func TestWASMCache_Evict_CountLimitZero(t *testing.T) {
	c := NewWASMCache(0, 1024)
	c.Put("key1", []byte("data"))
	if c.Len() != 0 { t.Errorf("Len should be 0, got %d", c.Len()) }
}

func TestWASMCache_Len(t *testing.T) {
	c := NewWASMCache(10, 1024)
	if c.Len() != 0 { t.Error("initial Len not 0") }
	c.Put("a", []byte("1")); c.Put("b", []byte("2"))
	if c.Len() != 2 { t.Errorf("Len should be 2, got %d", c.Len()) }
}

func TestWASMCache_TotalBytes(t *testing.T) {
	c := NewWASMCache(10, 1024)
	if c.TotalBytes() != 0 { t.Error("initial TotalBytes not 0") }
	c.Put("a", []byte("hello"))
	if c.TotalBytes() != 5 { t.Errorf("TotalBytes should be 5, got %d", c.TotalBytes()) }
}

func TestWASMCache_Put_ZeroByteValue(t *testing.T) {
	c := NewWASMCache(10, 1024)
	c.Put("empty", []byte{})
	if c.Len() != 1 { t.Errorf("Len should be 1, got %d", c.Len()) }
}

func TestWASMCache_Concurrent(t *testing.T) {
	c := NewWASMCache(100, 1024*1024)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			c.Put(string(rune('a'+(id%26))), []byte{byte(id)})
			c.Get(string(rune('a'+(id%26))))
			c.Len()
			c.TotalBytes()
		}(i)
	}
	wg.Wait()
}
