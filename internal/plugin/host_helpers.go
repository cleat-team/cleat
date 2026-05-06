package plugin

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/tetratelabs/wazero/api"
)

// ReadWasmString reads a UTF-8 string from WASM linear memory at (ptr, len).
func ReadWasmString(m api.Module, ptr, len uint32) string {
	if len == 0 {
		return ""
	}
	bytes, ok := m.Memory().Read(ptr, len)
	if !ok {
		return ""
	}
	return string(bytes)
}

// WriteWasmString writes a UTF-8 string to WASM linear memory at ptr.
// Returns the number of bytes written (truncated to maxLen if necessary).
func WriteWasmString(m api.Module, ptr, maxLen uint32, s string) uint32 {
	if maxLen == 0 || s == "" {
		return 0
	}
	data := []byte(s)
	if len(data) > int(maxLen) {
		data = data[:maxLen]
	}
	m.Memory().Write(ptr, data)
	return uint32(len(data))
}

// EncodeOK returns a packed i64 representing success (errCode=0, actualLen=0).
func EncodeOK() uint64 {
	return 0
}

// EncodeOKWithLen returns a packed i64 with errCode=0 and the given length.
func EncodeOKWithLen(actualLen uint32) uint64 {
	return uint64(actualLen) << 32
}

// EncodeError returns a packed i64 with errCode=1 and actualLen=0.
func EncodeError(err error) uint64 {
	return 1
}

// EncodeErrorWithMsg writes an error message to the output buffer and returns
// a packed i64 with errCode=1 and the message length.
func EncodeErrorWithMsg(m api.Module, outPtr, maxOutLen uint32, msg string) uint64 {
	errJSON := fmt.Sprintf(`{"error":"%s"}`, msg)
	written := WriteWasmString(m, outPtr, maxOutLen, errJSON)
	return (uint64(written) << 32) | 1
}

// EncodeSuspend returns the suspend sentinel value that tells the host
// to suspend the workflow.
func EncodeSuspend() uint64 {
	return 1 << 62
}

type tenantIDKeyType struct{}

var tenantIDKey = tenantIDKeyType{}

// WithTenant adds a tenant ID to a context.
func WithTenant(ctx context.Context, tenantID uuid.UUID) context.Context {
	return context.WithValue(ctx, tenantIDKey, tenantID)
}

// TenantFromContext extracts the tenant ID from a context.
func TenantFromContext(ctx context.Context) uuid.UUID {
	tid, ok := ctx.Value(tenantIDKey).(uuid.UUID)
	if !ok {
		return uuid.Nil
	}
	return tid
}
