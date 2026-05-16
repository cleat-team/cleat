// Package host provides a wazero-based WASM runtime for executing cleat
// workflow modules produced by the `cleat build` command.
//
// Architecture:
//   Runtime — wraps wazero, registers host function imports, manages modules
//   Engine  — cleat execution with checkpoint/replay on top of Runtime
//   HostHandler — per-execution session interface (carried in context)
//
// The host reads/writes strings in WASM linear memory using (ptr, len) pairs.
// All host function imports are registered on the "env" module. Per-execution
// state (replay history, step counter) is carried in context.Context.
package host

import (
	"context"
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

// wasmMemBufKey is a context key for overriding the WASM linear memory buffer
// used by writeWasmString. This allows the wasmtime backend to redirect memory
// writes to wasmtime's memory without passing an api.Module.
type wasmMemBufKey struct{}

// contextWithRawMemBuf returns a context that makes writeWasmString write to
// buf (a raw byte slice of WASM linear memory) instead of the api.Memory
// argument. Used by the wasmtime backend where api.Module is not available.
func contextWithRawMemBuf(ctx context.Context, buf []byte) context.Context {
	return context.WithValue(ctx, wasmMemBufKey{}, buf)
}

const outBufSize = 1048576 // 1 MB; increased to reduce truncation risk
const wasmPageSize = 65536   // 64 KB WASM page size

// Maximum size of any string parameter read from WASM linear memory.
// This prevents a malicious or buggy WASM module from causing the host to
// allocate excessive memory via a single host function call.
const maxWasmStringLen = 1048576 // 1 MB

// validServiceName checks that a name contains only allowed characters:
// alphanumeric, dot, underscore, and hyphen. Service and operation names
// must be non-empty and match [a-zA-Z0-9._-]+.
func validServiceName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

// errBadParam is a sentinel uint64 that host functions return when a WASM
// parameter fails validation. This causes the workflow to see a non-zero
// error code, which propagates as an error in the workflow's error handling.
const errBadParam uint64 = 0xFFFFFFFF_00000001

// errSignalAuthRequired is returned by cleat_signal_workflow when the caller
// is not authorized to signal the target workflow (requireSignalAuth is enabled
// and the caller's defName is not in the target's allowed_signals).
const errSignalAuthRequired uint64 = 0xFFFFFFFF_00000002

// errSignalAuthRequiredInt is the int64 equivalent of errSignalAuthRequired.
// errSignalAuthRequired overflows int64 so it cannot be used directly in
// execSession methods that return int64.
const errSignalAuthRequiredInt int64 = -4294967294

// readWasmString reads a Go string from WASM linear memory at (ptr, length).
func readWasmString(mem api.Memory, ptr, length uint32) string {
	if length == 0 {
		return ""
	}
	data, ok := mem.Read(ptr, length)
	if !ok {
		return ""
	}
	return string(data)
}

// readWasmStringValidated reads a string from WASM linear memory and validates it.
// Returns ("", false) if the string is empty, exceeds maxLen, or cannot be read.
func readWasmStringValidated(mem api.Memory, ptr, length, maxLen uint32) (string, bool) {
	if length == 0 {
		return "", false
	}
	if length > maxLen {
		return "", false
	}
	data, ok := mem.Read(ptr, length)
	if !ok {
		return "", false
	}
	return string(data), true
}

// readServiceName reads a service or operation name from WASM linear memory
// and validates both its length (must not exceed maxWasmStringLen) and
// character set (must match [a-zA-Z0-9._-]+).
func readServiceName(mem api.Memory, ptr, length uint32) (string, bool) {
	s, ok := readWasmStringValidated(mem, ptr, length, maxWasmStringLen)
	if !ok {
		return "", false
	}
	if !validServiceName(s) {
		return "", false
	}
	return s, true
}

// writeWasmString writes s into WASM linear memory at ptr, up to maxLen bytes.
// Returns the number of bytes actually written, or an error if the memory write fails.
func writeWasmString(mem api.Memory, ptr uint32, s string, maxLen uint32) (uint32, error) {
	data := []byte(s)
	if uint32(len(data)) > maxLen {
		data = data[:maxLen]
	}
	if len(data) > 0 {
		if ok := mem.Write(ptr, data); !ok {
			return 0, fmt.Errorf("writeWasmString: failed to write %d bytes at ptr %d", len(data), ptr)
		}
	}
	return uint32(len(data)), nil
}

// writeWasmStringOrTrap calls writeWasmString and returns the error on failure.
func writeWasmStringOrTrap(mem api.Memory, ptr uint32, s string, maxLen uint32) (uint32, error) {
	return writeWasmString(mem, ptr, s, maxLen)
}

// packDurableCallResult matches adapter.go DurableCall result encoding:
//
//	bits 40-63 = responseLen (24 bits)
//	bits 8-39  = callErrorCode (32 bits)
//	bits 0-7   = errCode (8 bits)
func packDurableCallResult(responseLen int, callErrorCode, errCode byte) int64 {
	return int64(uint64(responseLen)<<40 | uint64(callErrorCode)<<8 | uint64(errCode))
}

// packSimpleResult matches adapter.go for functions returning only an errCode
// with optional extra data in the upper bits.
func packSimpleResult(errCode byte, extra ...uint32) int64 {
	var v uint64
	if len(extra) > 0 {
		v = uint64(extra[0]) << 32
	}
	return int64(v | uint64(errCode))
}

// decodeExportResult matches exports.go writeJSONOut/writeErrorOut encoding:
//
//	bits 0-31  = errCode (0 = success)
//	bits 32-63 = actual output length
func decodeExportResult(result uint64) (errCode, actualLen uint32) {
	return uint32(result & 0xFFFFFFFF), uint32(result >> 32)
}

func minU32(a, b uint32) uint32 {
	if a < b {
		return a
	}
	return b
}
