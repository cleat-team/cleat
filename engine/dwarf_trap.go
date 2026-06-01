package engine

import (
	"fmt"
	"strings"
)

// resolveWasmTrap attempts to resolve a WASM trap to a source location.
// wasmBytes is the compiled WASM binary (which may contain DWARF custom sections).
// trapInfo is the error message from wazero containing the trap details.
// Returns a human-readable string like "wasm trap: unreachable\n..."
// or "" if trapInfo is empty.
//
// wazero v1.9.0 already resolves DWARF source locations internally and embeds
// the stack trace (file:line) in the error message returned from fn.Call().
// This function enriches that message with consistent formatting and serves
// as a hook point for future custom DWARF parsing (e.g., reading the raw
// wasm binary's custom sections with debug/dwarf).
func resolveWasmTrap(wasmBytes []byte, trapInfo string) string {
	if trapInfo == "" {
		return ""
	}

	// If the error was already formatted by formatWasmCallError in runtime.go,
	// it will start with "wasm trap:" — return it as-is since wazero's internal
	// DWARF resolution is already included.
	if strings.HasPrefix(trapInfo, "wasm trap:") {
		return trapInfo
	}

	// Unformatted trap info — provide a consistent envelope.
	// This covers the panic-recovery path in CallExportWithSuspend and any
	// other edge case where the error hasn't been through formatWasmCallError.
	return enrichTrapMessage(trapInfo)
}

// enrichTrapMessage wraps a raw trap message with a consistent "WASM trap:" prefix.
func enrichTrapMessage(trapInfo string) string {
	if trapInfo == "" {
		return ""
	}
	return fmt.Sprintf("WASM trap: %s", trapInfo)
}
