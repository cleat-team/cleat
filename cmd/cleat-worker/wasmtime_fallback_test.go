package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// TestClassifyWasmtimeFallback pins the three-way split main() relies on to
// decide whether a wasmtime init failure is the documented CGO_ENABLED=0
// fallback (WARN, keep going) or an unexpected degradation of the backend of
// record (fatal unless --allow-wazero-fallback). Getting this wrong in
// either direction is exactly the defect Stream K exists to close: treating
// every error the same way let a cgo-enabled binary silently downgrade to
// wazero, which cannot fence a compute-bound guest (CLAUDE.md).
func TestClassifyWasmtimeFallback(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want wasmtimeFallbackDisposition
	}{
		{"nil error means available", nil, wasmtimeAvailable},
		{"bare sentinel means expected CGO-less build", engine.ErrWasmtimeCGOUnavailable, wasmtimeFallbackExpected},
		{
			"wrapped sentinel still means expected",
			fmt.Errorf("construct backend: %w", engine.ErrWasmtimeCGOUnavailable),
			wasmtimeFallbackExpected,
		},
		{"unrelated error is unexpected", errors.New("mmap failed: out of memory"), wasmtimeFallbackUnexpected},
		{
			"error that merely mentions CGO in its text is still unexpected -- only the sentinel counts",
			errors.New("wasmtime backend requires CGO"),
			wasmtimeFallbackUnexpected,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyWasmtimeFallback(tc.err); got != tc.want {
				t.Errorf("classifyWasmtimeFallback(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
