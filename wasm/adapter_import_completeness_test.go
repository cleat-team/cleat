package wasm

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestEveryHostCallGeneratesACompilableAdapterAlone builds the host adapter for
// each host call ON ITS OWN and checks that what it uses, it imports.
//
// Alone is the whole point. Import decisions are made per generated FILE, so a
// missing import is invisible the moment any second host call happens to pull
// the same import in. PluginCallStreaming was the only adapter with an output
// buffer and no unsafe.String; it generated `unsafe.Pointer(...)` with no
// `import "unsafe"` and failed with "undefined: unsafe" -- but only for a
// workflow that used that call and nothing else. Every existing test and every
// real workflow used at least one other call, so nothing ever saw it.
//
// Parsing is not enough on its own -- an unused import parses -- so this checks
// both directions: every package qualifier the body uses must be imported, and
// every import must be used.
func TestEveryHostCallGeneratesACompilableAdapterAlone(t *testing.T) {
	// The qualifiers whose imports this generator decides.
	watched := map[string]string{
		"unsafe": "unsafe",
		"fmt":    "fmt",
		"json":   "encoding/json",
		"time":   "time",
	}

	for _, hf := range hostFunctions {
		if _, ok := adapterDefs[hf.FieldName]; !ok {
			continue
		}
		t.Run(hf.FieldName, func(t *testing.T) {
			usage := &UsageInfo{
				Used:  map[string]bool{hf.ImportName: true},
				Funcs: []HostFunction{hf},
			}
			src := string(GenerateHostAdapter("main", usage, "go"))

			file, err := parser.ParseFile(token.NewFileSet(), "gen.go", src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("generated adapter does not parse: %v\n\n%s", err, src)
			}

			imported := map[string]bool{}
			for _, imp := range file.Imports {
				imported[strings.Trim(imp.Path.Value, `"`)] = true
			}

			// Strip the import block before scanning for qualifiers, so an
			// import path does not count as a use of itself.
			body := src
			if i := strings.Index(body, ")\n\n"); i >= 0 {
				body = body[i:]
			}

			for qualifier, path := range watched {
				used := strings.Contains(body, qualifier+".")
				switch {
				case used && !imported[path]:
					t.Errorf("the generated adapter for %s alone uses %s. but does not import %q, "+
						"so it does not compile: undefined: %s.\n\nImports are decided per generated "+
						"file, so this is invisible as soon as any second host call pulls %q in. "+
						"That is why it has to be generated ALONE.",
						hf.FieldName, qualifier, path, qualifier, path)
				case !used && imported[path]:
					t.Errorf("the generated adapter for %s alone imports %q and never uses it, "+
						"which does not compile either.", hf.FieldName, path)
				}
			}
		})
	}
}
