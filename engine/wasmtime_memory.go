//go:build cgo

package engine

import (
	"context"
	"fmt"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

func putUint32LE(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

func getUint32LE(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func wasmtimeReadString(buf []byte, ptr, length int32) string {
	if length <= 0 {
		return ""
	}
	pu, lu := uint32(ptr), uint32(length)
	if uint64(pu)+uint64(lu) > uint64(len(buf)) {
		return ""
	}
	return string(buf[pu : pu+lu])
}

func wasmtimeReadStringValidated(buf []byte, ptr, length, maxLen int32) (string, bool) {
	if length <= 0 || length > maxLen {
		return "", false
	}
	pu, lu := uint32(ptr), uint32(length)
	if uint64(pu)+uint64(lu) > uint64(len(buf)) {
		return "", false
	}
	return string(buf[pu : pu+lu]), true
}

// wasmtimeReadPayload is the wasmtime twin of readWasmPayload: a zero length
// yields ("", true) because an empty payload is a value, not a fault. See
// readWasmPayload in memory.go for why that distinction matters.
//
// A *negative* length is still rejected. These parameters arrive as i32, so
// negative is a corrupt argument rather than an empty payload, and it falls
// through to the strict reader which refuses it.
func wasmtimeReadPayload(buf []byte, ptr, length, maxLen int32) (string, bool) {
	if length == 0 {
		return "", true
	}
	return wasmtimeReadStringValidated(buf, ptr, length, maxLen)
}

func wasmtimeReadServiceName(buf []byte, ptr, length int32) (string, bool) {
	s, ok := wasmtimeReadStringValidated(buf, ptr, length, int32(MaxWasmStringLen))
	if !ok {
		return "", false
	}
	if !validServiceName(s) {
		return "", false
	}
	return s, true
}

func wasmtimeWriteString(buf []byte, ptr uint32, s string, maxLen uint32) (uint32, error) {
	data := []byte(s)
	if uint32(len(data)) > maxLen {
		data = data[:maxLen]
	}
	if len(data) > 0 {
		if uint64(ptr)+uint64(len(data)) > uint64(len(buf)) {
			return 0, fmt.Errorf("wasmtimeWriteString: write %d bytes at ptr %d exceeds buffer", len(data), ptr)
		}
		copy(buf[ptr:], data)
	}
	return uint32(len(data)), nil
}

func callerMemBuf(caller *wasmtime.Caller) ([]byte, *wasmtime.Memory, error) {
	export := caller.GetExport("memory")
	if export == nil {
		return nil, nil, fmt.Errorf("host: module has no exported memory")
	}
	mem := export.Memory()
	if mem == nil {
		return nil, nil, fmt.Errorf("host: memory export is not a memory")
	}
	buf := mem.UnsafeData(caller)
	return buf, mem, nil
}

func ctxWithMem(ctx context.Context, buf []byte) context.Context {
	return contextWithRawMemBuf(ctx, buf)
}

func putU32LE(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}
