package main

import (
	"errors"

	"github.com/cleat-team/cleat/engine"
)

// wasmtimeFallbackDisposition classifies the result of engine.NewWasmtimeBackend
// so the caller can tell an expected, supported configuration apart from an
// unexpected degradation before deciding whether to run on wazero.
type wasmtimeFallbackDisposition int

const (
	// wasmtimeAvailable means NewWasmtimeBackend succeeded; there is no
	// fallback decision to make.
	wasmtimeAvailable wasmtimeFallbackDisposition = iota
	// wasmtimeFallbackExpected means NewWasmtimeBackend failed with
	// engine.ErrWasmtimeCGOUnavailable: this binary was built with
	// CGO_ENABLED=0, wazero is the documented fallback for that case, and
	// running on it is a deliberate, supported choice -- not a defect.
	wasmtimeFallbackExpected
	// wasmtimeFallbackUnexpected means NewWasmtimeBackend failed for any
	// other reason. Because the stub that returns ErrWasmtimeCGOUnavailable
	// only exists in a !cgo build, any other error here comes from a
	// cgo-enabled build where wasmtime -- the backend of record -- should
	// have initialized and did not. Falling back to wazero in that case
	// silently swaps in a backend that cannot fence a compute-bound guest
	// (CLAUDE.md), so it must never be treated the same as the expected
	// case.
	wasmtimeFallbackUnexpected
)

// classifyWasmtimeFallback turns the error from engine.NewWasmtimeBackend
// into a disposition. err may be nil (success) or wrapped (e.g. via
// fmt.Errorf("...: %w", engine.ErrWasmtimeCGOUnavailable)); errors.Is
// unwraps it either way.
func classifyWasmtimeFallback(err error) wasmtimeFallbackDisposition {
	if err == nil {
		return wasmtimeAvailable
	}
	if errors.Is(err, engine.ErrWasmtimeCGOUnavailable) {
		return wasmtimeFallbackExpected
	}
	return wasmtimeFallbackUnexpected
}
