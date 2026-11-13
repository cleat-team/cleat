// tsmeta stamps a cleat.metadata section and the entry-point list onto a linked Javy module, using the repository's
// own wasm package so the section is byte-for-byte what `cleat build` would write. Prototype only (proto/ts-javy).
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/cleat-team/cleat/wasm"
)

func main() {
	in := flag.String("in", "", "linked module")
	out := flag.String("out", "", "stamped module")
	name := flag.String("name", "", "workflow name")
	entries := flag.String("entries", "", "comma-separated entry point names")
	language := flag.String("language", "rust", "language label; the engine routes on it and accepts only the labels in engine.WasmtimeLanguages")
	flag.Parse()
	if *in == "" || *out == "" || *name == "" || *entries == "" {
		fmt.Fprintln(os.Stderr, "usage: tsmeta -in a.wasm -out b.wasm -name N -entries e1,e2 [-language L]")
		os.Exit(2)
	}
	raw, err := os.ReadFile(*in)
	if err != nil {
		fatal(err)
	}
	names := strings.Split(*entries, ",")
	meta := &wasm.Metadata{
		WorkflowName:         *name,
		WorkflowVersion:      1,
		ABIVersion:           wasm.CurrentABIVersion,
		MinCompatibleVersion: 1,
		Language:             *language,
		EntryPoints:          names,
	}
	if err := meta.Validate(); err != nil {
		fatal(err)
	}
	b, err := wasm.WriteMetadata(raw, meta)
	if err != nil {
		fatal(err)
	}
	b, err = wasm.WriteEntryPointsSection(b, names)
	if err != nil {
		fatal(err)
	}
	if _, err := wasm.ReadEntryPointsSection(b); err != nil {
		fatal(fmt.Errorf("round trip: %w", err))
	}
	if got, err := wasm.ReadMetadata(b); err != nil || got.WorkflowName != *name {
		fatal(fmt.Errorf("metadata round trip failed: %v", err))
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "tsmeta:", err)
	os.Exit(1)
}
