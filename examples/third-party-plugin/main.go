// Package main implements a minimal third-party cleat plugin compiled to WASM.
//
// This plugin exposes two host functions:
//   - greet: returns a greeting for the given name
//   - reverse: reverses a string
//
// Build: GOOS=wasip1 GOARCH=wasm go build -o plugin.wasm
//
// The WASM module must export:
//   - cleat_abi_version: i32 (return the ABI version this plugin targets)
//   - greet: function (host function from the manifest)
//   - reverse: function (host function from the manifest)
//
// Host functions communicate with the cleat worker via WASM linear memory.
// Input and output are JSON strings passed through memory buffers.
//
// The ABI export/import layer (//go:wasmimport, //export) is provided by
// the cleat WASM SDK. This file contains the pure business logic that the
// ABI layer dispatches to. See docs/third-party-plugin-guide.md for the
// full host function ABI reference.
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// ---------------------------------------------------------------------------
// Types matching the host function input/output signatures in plugin.json.
// ---------------------------------------------------------------------------

// GreetInput is deserialized from the JSON input of the "greet" host function.
type GreetInput struct {
	Name string `json:"name"`
}

// GreetOutput is serialized to JSON as the output of the "greet" host function.
type GreetOutput struct {
	Message string `json:"message"`
}

// ReverseInput is deserialized from the JSON input of the "reverse" host function.
type ReverseInput struct {
	Text string `json:"text"`
}

// ReverseOutput is serialized to JSON as the output of the "reverse" host function.
type ReverseOutput struct {
	Reversed string `json:"reversed"`
}

// ---------------------------------------------------------------------------
// Pure business logic (testable without WASM)
// ---------------------------------------------------------------------------

// DoGreet returns a greeting message for the given name.
// If name is empty, defaults to "World".
func DoGreet(name string) string {
	if name == "" {
		name = "World"
	}
	return fmt.Sprintf("Hello, %s!", name)
}

// DoReverse reverses a UTF-8 string.
func DoReverse(text string) string {
	runes := []rune(text)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}

// ---------------------------------------------------------------------------
// Main entry point (used for native testing, replaced by _start in WASM)
// ---------------------------------------------------------------------------

func main() {
	// When compiled natively, run a quick demo.
	// The WASM build uses the _start export instead.
	greeting := DoGreet("World")
	fmt.Println(greeting)

	reversed := DoReverse("hello")
	fmt.Println(reversed)

	// When run as a library via the WASM ABI, the main function is not called.
	// The cleat worker calls exported host functions directly.

	// Simulate JSON-based host function interface:
	input := GreetInput{Name: "Plugin Author"}
	inJSON, _ := json.Marshal(input)

	var req GreetInput
	json.Unmarshal(inJSON, &req)
	result := GreetOutput{Message: DoGreet(req.Name)}
	outJSON, _ := json.Marshal(result)

	fmt.Fprintf(os.Stderr, "Host function greet(%s) -> %s\n", string(inJSON), string(outJSON))
}

// ---------------------------------------------------------------------------
// WASM ABI layer (active only when built with GOOS=wasip1)
//
// In a real WASM build, this file would be augmented with:
//
//   //export _start
//   func _start() { /* initialization */ }
//
//   //go:wasmimport env plugin_call
//   func plugin_call(...) uint64
//
//   //export greet
//   func greet(inputPtr, inputLen, outputPtr, outputMaxLen uint32) uint64 {
//       // read input from WASM memory
//       // call DoGreet/DoReverse
//       // write output to WASM memory
//   }
//
// Corrected 2026-09-07. This block previously read
//
//   //go:wasmimport cleat cleat_plugin_call
//
// which is wrong twice over: guest imports come from the "env" module, not
// "cleat" (wasm/generator.go emits `//go:wasmimport env %s`), and the host
// function is registered as `plugin_call` with no prefix -- `cleat_plugin_call`
// is the RUST binding's local name, which carries #[link_name = "plugin_call"].
// A module built from the old line would not instantiate.
//
// It also showed an `//export cleat_abi_version` stub, removed here because
// nothing reads it: `grep -rn '"cleat_abi_version"' --include='*.go' .` returns
// nothing. The ABI version travels in the `cleat.metadata` custom section.
//
// See docs/contributor/plugins/third-party-plugin-guide.md for the intended
// import surface -- but read its status note first: six of the seven names it
// documents are not registered by the engine, and WASM plugin execution is
// unwired (IMPROVEMENT-PLAN 3.315). ABI.md is the checked source for what a
// guest can actually import; scripts/check-doc-consistency.sh enforces it.
// ---------------------------------------------------------------------------
