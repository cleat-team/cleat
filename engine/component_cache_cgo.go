//go:build cgo

package engine

// #cgo CFLAGS: -I${SRCDIR}/wasmtimeinc
// #include <wasmtime.h>
// #include <wasmtime/component/component.h>
import "C"

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"sync"
	"sync/atomic"
)

// componentCache keeps compiled Component Model components so a resumed Python
// workflow does not pay a full compile of its ~19 MB guest on every Execute and
// every Replay (cleat#3748).
//
// # What it replaces
//
// ExecuteComponentCGo compiled the component on every call and deleted it on
// return. Measured on the checked-in 19,300,914-byte call_all_plugins.wasm, on
// an Apple M4 that was not idle: 1175 to 1334 ms per compile, median 1239 ms,
// and about 45 MB of resident memory for the result. NOT measured on x86-64
// Linux or a worker's hardware; the core path has had a module cache for as long
// as it has existed, and this was the path that did not.
//
// # One budget, not a second cache
//
// The entries live in the same moduleLRU as compiled modules, under a distinct
// key prefix, so the entry bound and the byte bound a deployment sets
// (--wasm-module-cache-max-* / WithWasmtimeModuleCache*) are ONE set of limits.
// A component's cost is CompiledSizeEstimate over its wasm like any module's;
// that estimate is 4x, which gives 77 MB for the Python component against about
// 45 MB held, so it over-counts, which is the safe direction.
//
// # Ownership: the cache holds a reference, every execution holds its own
//
// A wasmtime_component_t is a C object with an explicit delete, and it is
// reference counted: wasmtime_component_clone bumps the count and the clone
// shares the one compiled copy (measured 21 microseconds and 80 KB). The cache
// holds one reference. An execution takes a clone, under the cache's lock, and
// deletes it when it returns. Eviction and Close delete only the cache's own
// reference. So an execution that is still running when its component is
// evicted keeps what it is using, and the code is freed when the last reference
// goes. That is what the module cache relies on too, there by wasmtime
// refcounting an Instance's module.
//
// # The key
//
// SHA-256 of the wasm, and the identity of the engine the cache belongs to.
//
// What a compile depends on is the bytes and the Engine's configuration (epoch
// interruption, fuel, the enabled features). The configuration is fixed when the
// engine is made, so binding the key to the engine is what makes a change of
// configuration unable to reuse a stale artifact: a cache shared between two
// engines misses instead of handing one engine another's code. It does not rely
// on every caller remembering that.
//
// SHA-256 and not the xxhash64 the module cache uses. This cache decides which
// code a tenant's call runs, so how hard it is to make two artifacts share a key
// is worth a stronger answer than 64 non-cryptographic bits, and the hash costs
// 6.4 ms over the Python component against a 1239 ms compile. The module cache's
// key is not changed here.
//
// # What sharing means across tenants
//
// The cache is shared by every tenant a worker serves, as the module cache is,
// so two tenants that deploy identical bytes share one compiled artifact. That is
// safe for correctness: the bytes are the same code. It does let a tenant tell,
// from the latency of its first compile, that another tenant on the same worker
// already deployed those exact bytes. The module cache has the same property.
type componentCache struct {
	lru      *moduleLRU
	engineID uint64
	// compiles counts compilations actually performed, which is what the tests
	// assert on instead of a timing: a second Execute that costs less than the
	// first proves nothing about why.
	compiles atomic.Int64
}

// nextEngineID hands each backend's engine a distinct identity. A counter and
// not the engine's address: an address can be reused once an engine is freed,
// and a key that can alias a dead engine is the thing the key exists to prevent.
var nextEngineID atomic.Uint64

func newComponentCache(lru *moduleLRU) *componentCache {
	return &componentCache{lru: lru, engineID: nextEngineID.Add(1)}
}

// key is the cache key for wasmBytes on this cache's engine.
func (cc *componentCache) key(wasmBytes []byte) string {
	sum := sha256.Sum256(wasmBytes)
	return "component:" + hex.EncodeToString(sum[:]) + ":" + strconv.FormatUint(cc.engineID, 10)
}

// cloneHeldComponent is loadHeld's acquire for a component: a reference-count
// increment, cheap enough to run under the cache's lock.
func cloneHeldComponent(held any) any {
	return C.wasmtime_component_clone(held.(*C.wasmtime_component_t))
}

// acquireComponent returns a compiled component that the CALLER owns one
// reference to and must delete, compiling it only if no execution on this
// engine has already.
//
// With no cache (a backend assembled without one) it compiles every time, which
// is the behaviour this replaced, so a backend that lacks the field degrades to
// the old cost and not to an error.
func (b *wasmtimeBackend) acquireComponent(wasmBytes []byte) (*C.wasmtime_component_t, error) {
	cc := b.components
	if cc == nil || cc.lru == nil {
		return componentCompile(b.engine, wasmBytes)
	}
	key := cc.key(wasmBytes)

	if held, ok := cc.lru.loadHeld(key, cloneHeldComponent); ok {
		return held.(*C.wasmtime_component_t), nil
	}

	// Compile once per key. Without this, N workflows resuming at the same moment
	// on a cold cache would each pay the full compile and then each insert, which
	// is the stampede compileLocks exists to prevent on the module path.
	muI, _ := b.compileLocks.LoadOrStore(key, new(sync.Mutex))
	mu := muI.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	if held, ok := cc.lru.loadHeld(key, cloneHeldComponent); ok {
		return held.(*C.wasmtime_component_t), nil
	}

	compiled, err := componentCompile(b.engine, wasmBytes)
	if err != nil {
		// A failed compile is not cached: the next call compiles again and gets
		// the same error, rather than a stale refusal outliving a fixed engine.
		return nil, err
	}
	cc.compiles.Add(1)

	// The caller's reference is taken BEFORE the cache is given the compiled
	// one: once stored, the cache may evict and release it at any moment, and
	// the caller must already hold its own by then.
	mine := C.wasmtime_component_clone(compiled)
	cc.lru.storeHeld(key, compiled, func() { C.wasmtime_component_delete(compiled) }, len(wasmBytes))
	return mine, nil
}

// close releases every component the cache holds. A module is reclaimed by the
// garbage collector; a held reference is a C allocation that nothing else will
// free, so a backend's Close has to.
func (cc *componentCache) close() {
	if cc == nil || cc.lru == nil {
		return
	}
	cc.lru.drainHeld()
}
