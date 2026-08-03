//go:build cgo && !wasmtime_component_cgo

package engine

import "fmt"

// ExecuteComponentCGo stubs out the native wasmtime Component Model C API
// fast path (implemented in component_cgo.go) when the wasmtime_component_cgo
// build tag is not set, which is the default. See the comment at the top of
// component_cgo.go for why that tag is opt-in rather than always-on.
//
// backend_wasmtime.go's Execute already treats a non-nil error from this
// method as "fast path unavailable" and falls back to ExecuteComponent (the
// pure wasmtime-go decomposition path), so component-model WASM execution is
// unaffected by default -- this stub only disables the optimization.
func (b *wasmtimeBackend) ExecuteComponentCGo(
	wasmBytes []byte, entryPoint string, input []byte, outBufSz uint32,
) (*ExecResult, error) {
	return nil, fmt.Errorf("wasmtime component CGo fast path not built (build with -tags wasmtime_component_cgo)")
}
