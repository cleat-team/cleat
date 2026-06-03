//go:build !cgo

package engine

import (
	"context"
	"fmt"
)

// NewWasmtimeBackend returns an error when CGO is disabled.
// The real implementation is in backend_wasmtime.go (requires CGO).
func NewWasmtimeBackend(ctx context.Context) (WasmBackend, error) {
	return nil, fmt.Errorf("wasmtime backend requires CGO")
}
