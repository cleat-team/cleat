//go:build !cgo

package engine

// isExecutionInterruptTrap always reports false without CGO: epoch
// interruption is a wasmtime feature and there is no trap type to match.
//
// Nothing reaches this. Its only caller is TestIntegrationWorkflowMaxDuration,
// which obtains its backend from withWasmtimeBackend, and that skips the test
// before the assertion when the build has CGO disabled. The stub exists so
// integration_test.go compiles in both configurations, not to provide
// behaviour.
func isExecutionInterruptTrap(err error) bool { return false }
