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
	"github.com/tetratelabs/wazero/api"
)

const outBufSize = 65536

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

// writeWasmString writes s into WASM linear memory at ptr, up to maxLen bytes.
// Returns the number of bytes actually written.
func writeWasmString(mem api.Memory, ptr uint32, s string, maxLen uint32) uint32 {
	data := []byte(s)
	if uint32(len(data)) > maxLen {
		data = data[:maxLen]
	}
	if len(data) > 0 {
		mem.Write(ptr, data)
	}
	return uint32(len(data))
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
