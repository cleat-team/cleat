package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/internal/plugingen"
)

// Flags are registered at package init time so they are available to both
// main() and tests. Default values match the original inline definitions.
var (
	manifestPath = flag.String("manifest", "", "Path to plugin.json")
	lang         = flag.String("lang", "typescript", "Target language (typescript, python, rust, go)")
	output       = flag.String("out", "", "Output file (default: stdout)")
)

func main() {
	flag.Parse()

	if *manifestPath == "" {
		fmt.Fprintln(os.Stderr, "error: --manifest is required")
		os.Exit(1)
	}

	m, err := plugin.LoadManifest(*manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading manifest: %v\n", err)
		os.Exit(1)
	}

	if err := plugin.ValidateManifest(m); err != nil {
		fmt.Fprintf(os.Stderr, "error validating manifest: %v\n", err)
		os.Exit(1)
	}

	ir, err := plugingen.FromManifest(m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error building IR: %v\n", err)
		os.Exit(1)
	}

	var code string
	switch *lang {
	case "typescript":
		code, err = plugingen.GenerateTypeScript(ir)
	case "python":
		code, err = plugingen.GeneratePython(ir)
	case "rust":
		code, err = plugingen.GenerateRust(ir)
	case "go":
		code, err = plugingen.GenerateGo(ir)
	default:
		fmt.Fprintf(os.Stderr, "unknown language: %s\n", *lang)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error generating code: %v\n", err)
		os.Exit(1)
	}

	if *output != "" {
		if err := os.WriteFile(*output, []byte(code), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "error writing output: %v\n", err)
			os.Exit(1)
		}
	} else {
		fmt.Print(code)
	}
}
