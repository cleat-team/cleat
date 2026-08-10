package engine

import "errors"

// ErrWasmtimeCGOUnavailable is the error NewWasmtimeBackend returns when the
// running binary was built with CGO disabled (CGO_ENABLED=0), so the
// cgo-only wasmtime backend was never compiled in (see
// backend_wasmtime_stub.go, guarded by "//go:build !cgo").
//
// This is a deliberate, supported configuration (CLAUDE.md: "wazero — the
// CGO-less fallback and nothing else"), not a failure worth alarming on. It
// exists so callers can tell it apart from every other error
// NewWasmtimeBackend can return on a CGO-enabled build (guarded by
// "//go:build cgo", backend_wasmtime.go) — those indicate the backend of
// record failed to initialize despite being compiled in, which is a
// safety-relevant degradation: wazero cannot fence a compute-bound guest
// (see CLAUDE.md, "measured three ways, all failing"), so silently falling
// back to it should never be treated the same as an expected CGO-less build.
var ErrWasmtimeCGOUnavailable = errors.New("wasmtime backend requires CGO (binary built with CGO_ENABLED=0)")
