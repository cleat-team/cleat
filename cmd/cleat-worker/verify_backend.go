package main

import (
	"context"
	"fmt"
	"io"

	"github.com/cleat-team/cleat/engine"
)

// runVerifyBackend reports whether this binary can construct the wasmtime
// backend, and returns the process exit code.
//
// It exists because the failure it guards against is silent. The Dockerfile
// built with CGO_ENABLED=0 for a long time, which compiles out
// engine/backend_wasmtime.go (`//go:build cgo`) entirely. The worker then logs
// one WARN line and runs happily on wazero -- which cannot interrupt a guest
// that never calls into the host, so `--wasm-instance-timeout` silently stops
// bounding anything. A workflow with a 2-second budget ran for 2m35s and was
// reported a success. See IMPROVEMENT-PLAN.md 2.28.
//
// A comment saying "keep CGO on" would not have caught that. A build step that
// exits non-zero does.
func runVerifyBackend(out io.Writer) int {
	// Construct it for real rather than testing a build tag. The stub returns
	// an error, but so does a genuine runtime failure (a missing shared
	// library, an unsupported platform), and both mean the same thing here:
	// this binary will fall back to an unfenced backend.
	backend, err := engine.NewWasmtimeBackend(context.Background())
	if err != nil {
		fmt.Fprintf(out, "verify-backend: FAIL: wasmtime backend unavailable: %v\n", err)
		fmt.Fprintf(out, "\nThis binary would fall back to wazero, where --wasm-instance-timeout\n"+
			"cannot interrupt a WASM guest that does not call into the host: a runaway\n"+
			"workflow holds its concurrency slot until the process dies.\n\n"+
			"Most likely cause: built with CGO_ENABLED=0, or on a musl base image --\n"+
			"wasmtime-go ships a glibc libwasmtime.a and does not link against musl.\n")
		return 1
	}
	// Via `any`: NewWasmtimeBackend returns the WasmBackend interface from the
	// no-cgo stub but a concrete *wasmtimeBackend under cgo, and a type
	// assertion on a concrete type does not compile. This file must build in
	// both modes -- it is the thing that tells them apart.
	var b any = backend
	if closer, ok := b.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
	fmt.Fprintln(out, "verify-backend: OK: wasmtime backend available")
	return 0
}
