//go:build cgo

package engine

import (
	"container/list"
	"sync"

	"github.com/bytecodealliance/wasmtime-go/v48"
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
// # Bounded by ESTIMATED BYTES, and by a count as well
//
// This said "bounded by COUNT, not bytes, and that is a constraint rather than
// a choice", on the grounds that a compiled *wasmtime.Module exposes no cheap
// size and Serialize() would cost a serialisation per insert. Both premises are
// true. The conclusion did not follow: the bound does not need the compiled
// size, it needs AN ESTIMATE, and the input size is already in scope at the
// insert.
//
// What the count alone could not express, measured on the repo's own artifacts
// (compile with NewConfig() + SetEpochInterruption(true), then len(Serialize())):
//
//	widget-store (AssemblyScript)      9,496 ->     86,400   9.1x
//	as-workflow (AssemblyScript)      26,367 ->    141,904   5.4x
//	rust-workflow (release)          149,027 ->    457,952   3.1x
//	hostcallsrust (release)          252,411 ->    679,152   2.7x
//	java-workflow                    299,189 ->    694,184   2.3x
//	saga-java-port                   369,459 ->    841,336   2.3x
//	hostcallsjava                    506,181 ->  1,044,072   2.1x
//	call_all_plugins (Python)     19,300,914 -> 46,019,864   2.4x
//	rust-workflow (DEBUG)          5,244,200 ->  1,113,200   0.2x
//
// So a hundred entries is 8.6 MB of AssemblyScript or 4.6 GB of Python -- a
// ~500x spread in what one flag value costs, which is exactly the thing an
// entry count cannot say.
//
// BOTH BOUNDS, not one. Bytes is what a deployment actually has to size, and
// the count still bounds the map and list overhead independently of size, so a
// deployment with thousands of tiny artifacts stays bounded too.
type moduleLRU struct {
	mu       sync.Mutex
	list     *list.List // front = most recently used
	index    map[string]*list.Element
	maxEnts  int
	maxBytes int64
	bytes    int64 // sum of every entry's estimate
}

type moduleEntry struct {
	key    string
	module *wasmtime.Module
	// held and release are set instead of module for an artifact the cache OWNS A
	// REFERENCE TO rather than a Go object the garbage collector tracks: a compiled
	// Component Model component (cleat#3748), a C pointer with an explicit delete.
	// release drops the cache's own reference and nothing else; see storeHeld.
	held    any
	release func()
	// bytes is the ESTIMATE, not a measurement: CompiledSizeEstimate over the
	// wasm this module was compiled from. Stored per entry because eviction
	// has to subtract exactly what insertion added, and recomputing from the
	// module is the thing that is not cheap.
	bytes int64
}

func newModuleLRU(maxEntries int, maxBytes int64) *moduleLRU {
	if maxEntries <= 0 {
		maxEntries = DefaultModuleCacheMaxEntries
	}
	if maxBytes <= 0 {
		maxBytes = DefaultModuleCacheMaxBytes
	}
	return &moduleLRU{
		list:     list.New(),
		index:    make(map[string]*list.Element, maxEntries),
		maxEnts:  maxEntries,
		maxBytes: maxBytes,
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
// until the cache is within BOTH of its bounds.
//
// wasmLen is the length of the wasm this module was compiled from, which the
// caller has in hand; the entry's cost is estimated from it. Passing 0 falls
// back to the count bound alone, which is what a caller with no source length
// gets rather than a silent zero-cost entry.
func (c *moduleLRU) store(key string, module *wasmtime.Module, wasmLen int) {
	var releases []func()
	c.mu.Lock()
	est := CompiledSizeEstimate(wasmLen)
	if elem, ok := c.index[key]; ok {
		entry := elem.Value.(*moduleEntry)
		if entry.release != nil {
			releases = append(releases, entry.release)
			entry.held, entry.release = nil, nil
		}
		c.bytes += est - entry.bytes
		entry.module = module
		entry.bytes = est
		c.list.MoveToFront(elem)
	} else {
		c.index[key] = c.list.PushFront(&moduleEntry{key: key, module: module, bytes: est})
		c.bytes += est
		releases = append(releases, c.evictLocked()...)
	}
	c.mu.Unlock()
	runReleases(releases)
}

// evictLocked drops the least recently used entries until the cache is within
// BOTH of its bounds, and returns the release functions of the entries that
// held a reference. The caller runs them AFTER unlocking: a release can free
// tens of megabytes of compiled code, and doing that under the cache's mutex
// would stall every lookup behind it.
//
// A SINGLE entry over the byte bound is kept, not evicted on insert: the
// alternative is a cache that compiles a large artifact, immediately drops
// it, and recompiles it on the next call forever. The bound is a target
// for the SET, and one oversized member is better held than thrashed.
func (c *moduleLRU) evictLocked() []func() {
	var releases []func()
	for c.list.Len() > 1 && (c.list.Len() > c.maxEnts || c.bytes > c.maxBytes) {
		oldest := c.list.Back()
		if oldest == nil {
			break
		}
		c.list.Remove(oldest)
		entry := oldest.Value.(*moduleEntry)
		delete(c.index, entry.key)
		c.bytes -= entry.bytes
		// A MODULE is deliberately NOT closed. See the type comment: a
		// concurrent load may have just handed it out, and the instance that
		// holds it keeps it alive on its own. A HELD artifact is released, and
		// that is safe for the same reason by a different mechanism: every
		// caller took its own reference under the lock before the entry could be
		// evicted, so dropping the cache's reference cannot free what a running
		// execution is using.
		if entry.release != nil {
			releases = append(releases, entry.release)
		}
	}
	if c.bytes < 0 {
		c.bytes = 0
	}
	return releases
}

func runReleases(releases []func()) {
	for _, r := range releases {
		r()
	}
}

// loadHeld returns a NEW reference to the artifact cached under key, taken by
// acquire while the cache's lock is held, and marks the entry most recently
// used.
//
// The lock is what makes this safe: eviction needs it too, so the cache's own
// reference cannot be released between finding the entry and acquire taking the
// caller's. acquire must be cheap, because it runs under the lock; for a
// component it is a reference-count increment, measured at 21 microseconds.
func (c *moduleLRU) loadHeld(key string, acquire func(held any) any) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.index[key]
	if !ok {
		return nil, false
	}
	entry := elem.Value.(*moduleEntry)
	if entry.held == nil {
		return nil, false
	}
	c.list.MoveToFront(elem)
	return acquire(entry.held), true
}

// storeHeld inserts an artifact the cache owns one reference to. release drops
// that reference, and runs when the entry is evicted, replaced or drained,
// never under the lock. wasmLen is the length of the wasm it was compiled from,
// which sets its share of the byte bound exactly as for a module.
//
// The artifacts share the modules' budget on purpose: one entry bound and one
// byte bound a deployment can size against, not two that add up to twice what it
// was told.
func (c *moduleLRU) storeHeld(key string, held any, release func(), wasmLen int) {
	var releases []func()
	c.mu.Lock()
	est := CompiledSizeEstimate(wasmLen)
	if elem, ok := c.index[key]; ok {
		entry := elem.Value.(*moduleEntry)
		if entry.release != nil {
			releases = append(releases, entry.release)
		}
		c.bytes += est - entry.bytes
		entry.module, entry.held, entry.release, entry.bytes = nil, held, release, est
		c.list.MoveToFront(elem)
	} else {
		c.index[key] = c.list.PushFront(&moduleEntry{key: key, held: held, release: release, bytes: est})
		c.bytes += est
		releases = append(releases, c.evictLocked()...)
	}
	c.mu.Unlock()
	runReleases(releases)
}

// drainHeld releases and removes every held artifact. Modules are left alone,
// as they always were. It is what a backend's Close calls: a Go-tracked module
// is reclaimed by the collector, but a held reference is a C allocation that
// nothing else will free.
func (c *moduleLRU) drainHeld() {
	var releases []func()
	c.mu.Lock()
	for elem := c.list.Front(); elem != nil; {
		next := elem.Next()
		entry := elem.Value.(*moduleEntry)
		if entry.held != nil {
			c.list.Remove(elem)
			delete(c.index, entry.key)
			c.bytes -= entry.bytes
			releases = append(releases, entry.release)
		}
		elem = next
	}
	if c.bytes < 0 {
		c.bytes = 0
	}
	c.mu.Unlock()
	runReleases(releases)
}

// estimatedBytes reports the cache's estimated resident cost. It is the
// observable behind cleat_wasm_compiled_module_cache_bytes.
func (c *moduleLRU) estimatedBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
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

// CompiledModuleCacheBytes reports the cache's ESTIMATED resident cost, the
// observable behind cleat_wasm_compiled_module_cache_bytes.
//
// It exists because the entries gauge could not answer the question an
// operator actually has. cleat#1563 opens on a metric that looked like it
// covered this cache and measured a different one; shipping a byte bound with
// only a count to watch it by would repeat that.
//
// ESTIMATED, and the name says so at every layer: it is
// CompiledSizeEstimate over each entry's wasm, not a measurement of native
// code. See that function for the measurements behind the multiplier.
func (b *wasmtimeBackend) CompiledModuleCacheBytes() int64 {
	if b.moduleCache == nil {
		return 0
	}
	return b.moduleCache.estimatedBytes()
}
