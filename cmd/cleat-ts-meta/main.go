// tsmeta stamps a cleat.metadata section and the entry-point list onto a linked Javy module, using the repository's
// own wasm package so the section is byte-for-byte what `cleat build` would write. In development, not supported: see tiers.yaml (cleat#3825).
package main

import (
	"encoding/json"
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
	language := flag.String("language", "javascript", "language label; the engine routes on it and accepts only the labels in engine.WasmtimeLanguages")
	pin := flag.String("pin", "", "JSON file describing the exact engine this module carries; stored as the custom section "+pinSection)
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
	if *pin != "" {
		p, err := os.ReadFile(*pin)
		if err != nil {
			fatal(err)
		}
		if !json.Valid(p) {
			fatal(fmt.Errorf("%s is not valid JSON", *pin))
		}
		b = appendCustomSection(b, pinSection, p)
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		fatal(err)
	}
}

// pinSection names the custom section that records the exact engine (Javy, QuickJS, plugin and SDK hashes) a module carries.
// The stock engine ignores sections it does not know, so this adds no engine mechanism; see PHASE2.md for what that means for
// "refuse or replay identically".
const pinSection = "cleat.js-engine"

func uleb(n int) []byte {
	var out []byte
	for {
		b := byte(n & 0x7f)
		n >>= 7
		if n != 0 {
			out = append(out, b|0x80)
			continue
		}
		return append(out, b)
	}
}

// appendCustomSection appends a WebAssembly custom section (id 0, size, name, payload) to a module.
func appendCustomSection(mod []byte, name string, payload []byte) []byte {
	body := append(uleb(len(name)), name...)
	body = append(body, payload...)
	out := append([]byte{}, mod...)
	out = append(out, 0)
	out = append(out, uleb(len(body))...)
	return append(out, body...)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "tsmeta:", err)
	os.Exit(1)
}
