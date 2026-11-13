package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/cleat-team/cleat/engine"
)

// wasmtimeBackendError reports why this binary cannot construct the wasmtime
// backend, or nil when it can.
//
// It constructs the backend for real rather than testing a build tag. The stub
// returns an error, but so does a genuine runtime failure (a missing shared
// library, an unsupported platform), and both mean the same thing here: this
// binary cannot run a workflow.
func wasmtimeBackendError() error {
	backend, err := engine.NewWasmtimeBackend(context.Background())
	if err != nil {
		return err
	}
	// Close it, and close it with the method it HAS. NewWasmtimeBackend starts an
	// epoch-ticker goroutine, and Close(ctx) is what stops it. This used to be a
	// type assertion to `interface{ Close() error }`, which no backend satisfies
	// (WasmBackend.Close takes a context), so it never closed anything: harmless
	// in --verify-backend, which exits, and a goroutine leaked per call once this
	// ran at every worker start (cleat#3788 review). Calling it directly is also
	// all the stub needs, so no assertion is required to build in both modes.
	_ = backend.Close(context.Background())
	return nil
}

// backendFixText says what to do about a missing wasmtime backend.
//
// It is written for a person who ran `go install ...@latest` on a machine they
// have never built cleat on, because that is who reaches it: Go turns cgo off
// by itself when it finds no C compiler, and the install exits 0. They did not
// set CGO_ENABLED=0 and "built with CGO_ENABLED=0" does not tell them what
// they did do, so the first line says what happened and the rest says what to
// run. cleat#3787.
func backendFixText(err error) string {
	if !errors.Is(err, engine.ErrWasmtimeCGOUnavailable) {
		return "Most likely cause: a musl base image (Alpine) -- wasmtime-go ships a glibc\n" +
			"libwasmtime.a and does not link against musl -- or a platform wasmtime-go has\n" +
			"no build for. Use a glibc image, or the prebuilt worker from\n" +
			"https://github.com/cleat-team/cleat/releases\n"
	}
	return "This binary was built without cgo. Go switches cgo off by itself, with no\n" +
		"warning, when it finds no C compiler, so this is what `go install` produces on a\n" +
		"machine that has none. It is not a degraded worker; it cannot run a workflow.\n\n" +
		"Either use a prebuilt worker, which needs no compiler:\n" +
		"    the cleat-worker .deb or .tar.gz at https://github.com/cleat-team/cleat/releases\n\n" +
		"or build one with a C toolchain and cgo on (Debian/Ubuntu: apt install\n" +
		"build-essential -- gcc alone is not enough; macOS: xcode-select --install), on\n" +
		"Go 1.27.3. Go 1.27.4 and newer cannot link wasmtime yet (cleat#3787, cleat#3701):\n" +
		"    GOTOOLCHAIN=go1.27.3 CGO_ENABLED=1 go install github.com/cleat-team/cleat/cmd/cleat-worker@latest\n"
}

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
	if err := wasmtimeBackendError(); err != nil {
		fmt.Fprintf(out, "verify-backend: FAIL: wasmtime backend unavailable: %v\n\n", err)
		fmt.Fprintf(out, "wasmtime is the only WASM backend cleat has and there is no fallback, so\n"+
			"cleat-worker refuses to start with this binary.\n\n%s", backendFixText(err))
		return 1
	}
	fmt.Fprintln(out, "verify-backend: OK: wasmtime backend available")
	return 0
}

// refuseWithoutWasmtimeBackend is the same check, run at startup. It reports
// whether the worker must stop, and says why on out.
//
// It runs before any mode that touches a database, including --migrate-only
// and --generate-api-key. Until cleat#3787 the backend was first constructed
// after both, so a worker with no wasmtime backend applied the schema, minted a
// key and only then exited 1 -- three steps into the quick-start, with a
// message about a variable the reader never set.
func refuseWithoutWasmtimeBackend(out io.Writer) bool {
	err := wasmtimeBackendError()
	if err == nil {
		return false
	}
	fmt.Fprintf(out, "cleat-worker: refusing to start: this build has no wasmtime backend: %v\n\n"+
		"wasmtime is the only WASM backend cleat has and there is no fallback.\n\n%s",
		err, backendFixText(err))
	return true
}
