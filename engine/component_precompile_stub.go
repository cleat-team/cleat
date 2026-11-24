//go:build !cgo

package engine

import "io"

// RunSerializeComponentChild is the precompile child's entry point, which needs wasmtime. A worker built without cgo has no wasmtime backend at all (backend_wasmtime_stub.go), so there is nothing
// to compile with: it says so and exits nonzero rather than doing anything else.
func RunSerializeComponentChild(args []string, stdout, stderr io.Writer) int {
	io.WriteString(stderr, "serialize-component: this binary was built without cgo, so it has no wasmtime and cannot compile a component\n")
	return 2
}
