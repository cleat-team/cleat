// Package wasmrw provides encoding helpers for WASM host function return values.
// Return values use a packed uint64: lower 32 bits = error code, upper 32 bits = byte length.
// Bit 62 indicates suspend (async host call).
package wasmrw

// OK returns the packed encoding for success with 0-length result.
func OK() uint64 { return 0 }

// OKWithLen returns the packed encoding for success with n bytes of output.
func OKWithLen(n uint32) uint64 { return uint64(n) << 32 }

// Error returns the packed encoding for a host error (errorCode=1).
func Error(err error) uint64 { return 1 }

// Suspend returns the packed encoding for a suspend (async) marker.
func Suspend() uint64 { return 1 << 62 }
