// Package host provides a wazero-based WASM runtime for executing cleat
// workflow modules produced by the `cleat build` command.
//
// Architecture:
//
//	Runtime — wraps wazero, registers host function imports, manages modules
//	Engine  — cleat execution with checkpoint/replay on top of Runtime
//	HostHandler — per-execution session interface (carried in context)
//
// The host reads/writes strings in WASM linear memory using (ptr, len) pairs.
// All host function imports are registered on the "env" module. Per-execution
// state (replay history, step counter) is carried in context.Context.
package engine

import (
	"context"
	"fmt"
	"math"

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

// DefaultOutBufSize is the default WASM output buffer size (1 MiB).
const DefaultOutBufSize = 1048576

// OutBufSize is the output buffer size in bytes for WASM export calls.
// Set before creating any Runtime to configure. Default: 1 MiB.
var OutBufSize uint32 = 1048576

const wasmPageSize = 65536 // 64 KB WASM page size

// legacyScratchOffset is the floor for the host's scratch region.
//
// Some WASM SDKs (Java/TeaVM, AssemblyScript) hardcode a 10 MiB convention and
// break if the buffers move below it.
const legacyScratchOffset uint32 = 10 * 1024 * 1024

// scratchBaseFor returns the offset at which the host places its input and
// output scratch buffers, one guard page above the guest's current heap.
//
// It exists to make one line's arithmetic checkable. All three execution paths
// computed it inline, and all three did it in uint32:
//
//	scratchBase := uint32(currentSize + wasmPageSize)
//
// wasm32 linear memory can reach 4 GiB, and `--wasm-memory-max-mb` is not
// applied unless configured, so `currentSize` can come within a page of 2^32.
// The addition then wraps to a small number, falls below legacyScratchOffset,
// and gets clamped *up* to 10 MiB -- which is inside the guest's own heap. The
// bounds check on the following write passes, because 10 MiB really is within
// a 4 GiB memory, so the host would quietly overwrite guest data rather than
// fail. Corruption, not an error, and nothing in the failure would point here.
//
// Doing it in uint64 and refusing to place a region that will not fit turns
// that into a clean error. Returns the base offset; the caller's two buffers
// occupy [base, base+2*outBufSz).
func scratchBaseFor(currentSize uint64, outBufSz uint32) (uint32, error) {
	base := currentSize + wasmPageSize
	if base < uint64(legacyScratchOffset) {
		base = uint64(legacyScratchOffset)
	}
	// Both scratch buffers must fit above base, inside the 32-bit address
	// space the guest can actually address.
	if end := base + 2*uint64(outBufSz); end > math.MaxUint32 {
		return 0, fmt.Errorf("host: no room for scratch buffers: guest memory is %d bytes and "+
			"2x%d-byte buffers one page above it would end at %d, past the 4 GiB wasm32 limit",
			currentSize, outBufSz, end)
	}
	return uint32(base), nil
}

// DefaultMaxWasmStringLen is the default maximum WASM string length (1 MiB).
const DefaultMaxWasmStringLen = 1048576

// MaxWasmStringLen is the maximum size of any string parameter read from WASM
// linear memory. This prevents a malicious or buggy WASM module from causing
// the host to allocate excessive memory via a single host function call.
// Set before creating any Runtime to configure. Default: 1 MiB.
var MaxWasmStringLen uint32 = 1048576

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

// readWasmPayload reads a payload argument -- a request body, an HTTP body, a
// stored value -- from WASM linear memory.
//
// It differs from readWasmStringValidated in one respect: a zero length is
// accepted and yields ("", true). Emptiness is a property of a payload, not a
// defect in it. A durable call that takes no arguments, an HTTP GET with no
// body and a state entry set to the empty string are all ordinary things for a
// guest to ask for.
//
// readWasmStringValidated conflates the two, and every caller turns its false
// into errBadParam, so a guest passing "" was indistinguishable from a guest
// passing a corrupt pointer -- and was refused. That is what made
// testdata/basic's LongRunning fail on its first iteration for years. Use this
// for payloads and readServiceName/readWasmStringValidated for anything whose
// emptiness really is meaningless, such as a name or a key.
//
// See IMPROVEMENT-PLAN.md 2.10.
func readWasmPayload(mem api.Memory, ptr, length, maxLen uint32) (string, bool) {
	if length == 0 {
		return "", true
	}
	return readWasmStringValidated(mem, ptr, length, maxLen)
}

// readOptionalServiceName reads a name that the ABI allows to be absent, where
// absence is expressed as a zero length and carries its own meaning.
//
// A zero length yields ("", true). Anything else must still be a valid service
// name, so this relaxes only the emptiness rule and not the character set.
//
// Three host functions document behaviour that is selected by passing an empty
// name, and all three were unreachable from a guest because readServiceName
// refuses one:
//
//   - cleat_set_scope with both objectType and instanceKey empty clears the
//     scope (engine/scope.go, freshSetScope).
//   - cleat_child_workflow_with_options with an empty parentClosePolicy takes
//     the default, which wazero already allowed and wasmtime did not.
//
// See IMPROVEMENT-PLAN.md 2.13.
func readOptionalServiceName(mem api.Memory, ptr, length uint32) (string, bool) {
	if length == 0 {
		return "", true
	}
	return readServiceName(mem, ptr, length)
}

// readServiceName reads a service or operation name from WASM linear memory
// and validates both its length (must not exceed MaxWasmStringLen) and
// character set (must match [a-zA-Z0-9._-]+).
func readServiceName(mem api.Memory, ptr, length uint32) (string, bool) {
	s, ok := readWasmStringValidated(mem, ptr, length, MaxWasmStringLen)
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

// badParamDurableCall is the bad-parameter result for the host functions whose
// guest adapter decodes the packDurableCallResult layout: cleat_call,
// cleat_call_retry, cleat_call_heartbeat, cleat_plugin_call and
// cleat_plugin_call_streaming.
//
// Those five must not return the raw errBadParam sentinel. errBadParam is
// 0xFFFFFFFF_00000001 -- a value picked so that a decoder reading a low byte,
// or the low 32 bits, sees something nonzero. But this layout splits the word
// 24/32/8, and the sentinel lands across all three fields at once. A guest
// decoding it gets:
//
//	responseLen   = 0xFFFFFF     (16 MB, against a 64 KB response buffer)
//	callErrorCode = 0xFF000000   (not a cleat.CallErrorCode at all)
//	errCode       = 1
//
// which surfaced to workflow authors as `[4278190080] cleat_call: error 1
// (0=unknown 1=timeout ...)`: a malformed argument reported as a *retryable
// timeout*, carrying a Code that matches no enum member and so falls through
// every `switch e.Code`. The oversized responseLen is caught by the generated
// callErrorMessage's bounds check, so it degrades to the generic message
// rather than reading out of bounds -- but that bounds check was the only
// thing standing between this and a 16 MB out-of-range read.
//
// Encoding the refusal in the layout the caller actually decodes gives
// responseLen=0, a real CallErrorCode, and a nonzero errCode.
//
// See IMPROVEMENT-PLAN.md 2.10.
var badParamDurableCall = packDurableCallResult(0, callErrorInvalidRequest, 1)

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
