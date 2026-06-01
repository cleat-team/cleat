package engine

import (
	"container/list"
	"sync"
)

// WASMCache is a size-bounded LRU cache for compiled WASM binaries.
// It is safe for concurrent use.
type WASMCache struct {
	mu         sync.Mutex
	maxEntries int
	maxBytes   int64
	totalBytes int64
	ll         *list.List
	cache      map[string]*list.Element
}

type wasmCacheEntry struct {
	key   string
	value []byte
}

// NewWASMCache creates a new WASM cache with the given limits.
func NewWASMCache(maxEntries int, maxBytes int64) *WASMCache {
	return &WASMCache{
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
		ll:         list.New(),
		cache:      make(map[string]*list.Element),
	}
}

// Get returns cached bytes for the given key, updating LRU order.
// Returns nil and false if the key is not in the cache.
func (c *WASMCache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if ele, ok := c.cache[key]; ok {
		c.ll.MoveToFront(ele)
		return ele.Value.(*wasmCacheEntry).value, true
	}
	return nil, false
}

// Put inserts data into the cache, evicting LRU entries if over limits.
func (c *WASMCache) Put(key string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// If key already exists, update in place.
	if ele, ok := c.cache[key]; ok {
		entry := ele.Value.(*wasmCacheEntry)
		c.totalBytes -= int64(len(entry.value))
		c.totalBytes += int64(len(data))
		entry.value = data
		c.ll.MoveToFront(ele)
		c.evict()
		return
	}

	entry := &wasmCacheEntry{key: key, value: data}
	ele := c.ll.PushFront(entry)
	c.cache[key] = ele
	c.totalBytes += int64(len(data))
	c.evict()
}

// evict removes the oldest entries until both limits are satisfied.
// Must be called with c.mu held.
func (c *WASMCache) evict() {
	for c.ll.Len() > c.maxEntries || (c.maxBytes > 0 && c.totalBytes > c.maxBytes) {
		ele := c.ll.Back()
		if ele == nil {
			break
		}
		entry := c.ll.Remove(ele).(*wasmCacheEntry)
		delete(c.cache, entry.key)
		c.totalBytes -= int64(len(entry.value))
	}
}

// Len returns the number of entries in the cache.
func (c *WASMCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// TotalBytes returns the total cached byte count.
func (c *WASMCache) TotalBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.totalBytes
}
