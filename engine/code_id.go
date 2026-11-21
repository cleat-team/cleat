package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/cespare/xxhash/v2"
)

// CodeID identifies the compiled code a wasm artifact becomes, independent of which engine or cache entry holds it. A held run carries it (ResidentRunInfo.CodeKey) and the
// module cache's eviction hook reports it (wasmtimeBackend.OnModuleEvicted), so the resident set can match the two (ResidentSet.EvictCode).
//
// It exists because THERE ARE TWO KEY SPACES in the cache and neither is computable from the bytes alone in the other's form. A core module is cached under the hex xxhash of
// its bytes (Execute's wKey); a Component Model component is cached under "component:<sha256 hex>:<engine id>" (componentCache.key), and the engine id is the backend's, which
// a worker does not have. A first version of the eviction hook assumed the xxhash key for everything: every unit test agreed with it and the real case (a held C# component
// evicted from the cache) did not suspend, because the hook reported a key no run carried. Both sides now go through this one definition.
func CodeID(wasmBytes []byte) string {
	if isComponentWasm(wasmBytes) {
		sum := sha256.Sum256(wasmBytes)
		return "component:" + hex.EncodeToString(sum[:])
	}
	return "module:" + moduleCacheKey(wasmBytes)
}

// moduleCacheKey is the key a core module is cached under: the hex xxhash of its bytes. Execute uses it for its cache lookups, and CodeID builds on it, so the two cannot
// drift.
func moduleCacheKey(wasmBytes []byte) string {
	return strconv.FormatUint(xxhash.Sum64(wasmBytes), 16)
}

// codeIDOfCacheKey maps the key an entry is cached under to the CodeID of the code in it. A component's key loses its trailing engine id, so an eviction from any engine's
// entry of those bytes matches the runs of those bytes (a held run on another engine of the same worker is sent back too: over-eviction costs one replay, under-eviction
// is the pin this exists to prevent).
func codeIDOfCacheKey(key string) string {
	if rest, ok := strings.CutPrefix(key, "component:"); ok {
		if i := strings.LastIndexByte(rest, ':'); i > 0 {
			return "component:" + rest[:i]
		}
		return "component:" + rest
	}
	return "module:" + key
}
