package plugin

import (
	"context"
	"database/sql"
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

// CallContext carries per-invocation metadata injected by the engine
// before calling plugin host functions.
type CallContext struct {
	TenantID   uuid.UUID `json:"tenant_id"`
	WorkflowID string    `json:"workflow_id"`
	DB         *sql.DB   // tenant-scoped database connection
}

type callContextKeyType struct{}

// WithCallContext injects call context into the context.
func WithCallContext(ctx context.Context, cc *CallContext) context.Context {
	return context.WithValue(ctx, callContextKeyType{}, cc)
}

// CallContextFromContext extracts call context from the context.
// Returns nil if not present.
func CallContextFromContext(ctx context.Context) *CallContext {
	cc, _ := ctx.Value(callContextKeyType{}).(*CallContext)
	return cc
}
