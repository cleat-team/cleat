package main

import (
	"fmt"
	"os"

	"github.com/cleat-team/cleat/wasm"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: wit-rewrite <wasm-file>\n")
		os.Exit(1)
	}
	path := os.Args[1]
	wasmBytes, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading %s: %v\n", path, err)
		os.Exit(1)
	}
	rewritten, err := wasm.RewriteWitImports(wasmBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error rewriting imports: %v\n", err)
		os.Exit(1)
	}
	if rewritten == nil {
		return
	}
	if err := os.WriteFile(path, rewritten, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing %s: %v\n", path, err)
		os.Exit(1)
	}
}
