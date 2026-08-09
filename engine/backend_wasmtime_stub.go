//go:build !cgo

package engine

import (
	"context"
)

// NewWasmtimeBackend returns ErrWasmtimeCGOUnavailable when CGO is disabled.
// The real implementation is in backend_wasmtime.go (requires CGO).
// opts is accepted (and ignored) so callers built without CGO don't need a
// separate call site from CGO builds.
//
// Callers that fall back to wazero on error MUST check for
// ErrWasmtimeCGOUnavailable specifically (errors.Is) rather than treating
// any error the same way: this sentinel means "expected, CGO-less build";
// any other error out of a cgo-enabled NewWasmtimeBackend means the backend
// of record failed unexpectedly and falling back is a degradation, not a
// routine substitution.
func NewWasmtimeBackend(ctx context.Context, opts ...WasmtimeOption) (WasmBackend, error) {
	return nil, ErrWasmtimeCGOUnavailable
}
