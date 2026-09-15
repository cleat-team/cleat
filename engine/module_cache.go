//go:build cgo

package engine

import (
	"container/list"
	"sync"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

// moduleLRU is a bounded, concurrent LRU over compiled wasmtime Modules.
//
// # Why this exists
//
// cleat#1563: the compiled-module cache was a bare sync.Map with no Delete
// anywhere, so it retained one entry per distinct WASM artifact the process had
// ever executed, for the life of the process, across every tenant. The resident
// cost is compiled native code, and nothing measured it -- the metric that
// looks like it covers this (cleat_wasm_cache_entries) measures the separate
// BYTE cache.
//
// # Evicting is safe, and that was measured rather than assumed
//
// backend_wasmtime.go warns "Do NOT close the module -- it's cached and
// shared". That is correct about SHARING and says nothing about whether an
// already-instantiated execution survives its module being dropped, which is
// the question an eviction policy actually needs answered.
//
// It does. wasmtime refcounts compiled code natively: an Instance holds its own
// reference, so a call into it succeeds after the module has been explicitly
// Closed -- verified with a negative control proving the Close really happened
// (using the closed module panics "object has been closed already"). Close is
// the STRONGEST form of eviction; what this cache does is strictly weaker, so
// evicting cannot disturb an in-flight execution.
//
// So eviction here drops the reference and never calls Close. The module stays
// alive for anyone already holding it and is finalised once the last reference
// goes. Calling Close on eviction would be wrong for the reason the original
// comment gives: a concurrent Load may have just handed the module out.
//
// # Bounded by COUNT, not bytes, and that is a constraint rather than a choice
//
// wasmLRUCache bounds by byte size because it holds []byte and the size is
// free. A compiled *wasmtime.Module exposes no cheap size; Serialize() would
// give one at the cost of a serialisation per insert. A byte bound here would
// therefore be a limit the code cannot actually enforce, so the flag says
// entries and means entries.
type moduleLRU struct {
	mu      sync.Mutex
	list    *list.List // front = most recently used
	index   map[string]*list.Element
	maxEnts int
}

type moduleEntry struct {
	key    string
	module *wasmtime.Module
}

func newModuleLRU(maxEntries int) *moduleLRU {
	if maxEntries <= 0 {
		maxEntries = DefaultModuleCacheMaxEntries
	}
	return &moduleLRU{
		list:    list.New(),
		index:   make(map[string]*list.Element, maxEntries),
		maxEnts: maxEntries,
	}
}

// load returns the cached module for key and marks it most-recently-used.
func (c *moduleLRU) load(key string) (*wasmtime.Module, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.index[key]
	if !ok {
		return nil, false
	}
	c.list.MoveToFront(elem)
	return elem.Value.(*moduleEntry).module, true
}

// store inserts or refreshes key, evicting the least recently used entries
// until the cache is within its bound.
func (c *moduleLRU) store(key string, module *wasmtime.Module) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.index[key]; ok {
		elem.Value.(*moduleEntry).module = module
		c.list.MoveToFront(elem)
		return
	}
	c.index[key] = c.list.PushFront(&moduleEntry{key: key, module: module})
	for c.list.Len() > c.maxEnts {
		oldest := c.list.Back()
		if oldest == nil {
			break
		}
		c.list.Remove(oldest)
		delete(c.index, oldest.Value.(*moduleEntry).key)
		// Deliberately NOT oldest.module.Close(). See the type comment: a
		// concurrent load may have just handed this module out, and the
		// instance that holds it keeps it alive on its own.
	}
}

// len reports the number of cached modules. It is the observable behind the
// compiled-module gauge.
func (c *moduleLRU) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.list.Len()
}

// CompiledModuleCacheEntries reports how many compiled modules this backend
// currently retains. It is the observable behind
// cleat_wasm_compiled_module_cache_entries.
//
// NOT ADDED TO THE WasmBackend INTERFACE, deliberately. Widening an interface
// to expose one implementation's introspection forces every implementation and
// every mock to grow a method they cannot answer -- the cost this repo already
// paid on WorkflowStore, which has 99 methods and ten implementations, all but
// four of them mocks. The caller type-asserts for this instead.
//
// The known cost of an optional interface is that a type which stops satisfying
// it degrades silently. Here that degradation is a gauge that stops being fed,
// and monitoring's TestEveryMetricHasAFeeder fails on an exported Set* method
// with no production caller -- so the silence is caught rather than shipped.
func (b *wasmtimeBackend) CompiledModuleCacheEntries() int {
	if b.moduleCache == nil {
		return 0
	}
	return b.moduleCache.len()
}
