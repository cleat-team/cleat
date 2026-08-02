//go:build !cgo

package engine

import (
	"context"
	"fmt"
)

// NewWasmtimeBackend returns an error when CGO is disabled.
// The real implementation is in backend_wasmtime.go (requires CGO).
// opts is accepted (and ignored) so callers built without CGO don't need a
// separate call site from CGO builds.
func NewWasmtimeBackend(ctx context.Context, opts ...WasmtimeOption) (WasmBackend, error) {
	return nil, fmt.Errorf("wasmtime backend requires CGO")
}
