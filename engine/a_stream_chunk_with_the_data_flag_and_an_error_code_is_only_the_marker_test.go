package engine

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// cleat#4342: the empty-stream marker is a plugin_call_stream_chunk record with StreamData true and a NON-ZERO StreamErrCode, and it is a marker only because NO OTHER
// writer produces that combination. A chunk the plugin produced sets StreamData and never an error code; the synthetic failure chunk sets an error code and never StreamData.
// That is a claim about the source, and this test is the source scan that keeps it true: a new writer that set both would make a real chunk or failure read as an empty stream.
//
// WHAT IT SCANS, and the limit of it: every composite literal in the engine's non-test source that sets BOTH StreamData and StreamErrCode (the only place the two can be written
// together by construction), matched by its KEYS and whatever its type, in a function body or at package level (including an untyped element of a slice or map literal), and every
// assignment to either field. Writers are allowed by FUNCTION NAME, not by file: the marker builder, the conversion of a typed event to a record, and the
// two decoders that copy what a store or a compacted history already held. A way to write a record that is neither a literal nor an assignment (reflection, a copy of another
// record) is not seen; a copy cannot create the combination, it can only carry one.

var markerWriterAllowed = map[string]bool{
	"emptyStreamMarkerRecord": true, // the marker itself
	"EventRecordFromEvent":    true, // a typed event converted to a record: it carries the two fields it was given, and is built only from records
	// Decoders: they copy a persisted or compacted record's fields back, creating nothing.
	"populateFromPayload":            true,
	"buildFullHistoryFromCompaction": true,
	"extractCompactionStateFrom":     true,
	"eventRecordToPayload":           true,
}

// markerWriters returns "func: what" for every writer of both fields found in the sources.
func markerWriters(sources map[string]string) (findings []string, err error) {
	fset := token.NewFileSet()
	names := make([]string, 0, len(sources))
	for n := range sources {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f, perr := parser.ParseFile(fset, n, sources[n], 0)
		if perr != nil {
			return nil, perr
		}
		for _, d := range f.Decls {
			// A function body is scanned unless the function is an allowed writer; a PACKAGE-LEVEL declaration (a var or const initialiser) is always scanned, under the name
			// "(package level)": cleat-review3 found `var x = []EventRecord{{StreamData: true, StreamErrCode: 2}}` passed unseen when only function bodies were read.
			name := "(package level)"
			var root ast.Node
			switch x := d.(type) {
			case *ast.FuncDecl:
				if x.Body == nil || markerWriterAllowed[x.Name.Name] {
					continue
				}
				name, root = x.Name.Name, x.Body
			case *ast.GenDecl:
				root = x
			default:
				continue
			}
			fd := struct{ Name *ast.Ident }{ast.NewIdent(name)}
			ast.Inspect(root, func(node ast.Node) bool {
				switch x := node.(type) {
				case *ast.CompositeLit:
					keys := map[string]bool{}
					for _, el := range x.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if id, ok := kv.Key.(*ast.Ident); ok {
								keys[id.Name] = true
							}
						}
					}
					if keys["StreamData"] && keys["StreamErrCode"] {
						findings = append(findings, fmt.Sprintf("%s: %s sets StreamData and StreamErrCode in one literal", n, fd.Name.Name))
					}
				case *ast.AssignStmt:
					for _, lhs := range x.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && (sel.Sel.Name == "StreamData" || sel.Sel.Name == "StreamErrCode") {
							findings = append(findings, fmt.Sprintf("%s: %s assigns %s", n, fd.Name.Name, sel.Sel.Name))
						}
					}
				}
				return true
			})
		}
	}
	sort.Strings(findings)
	return findings, nil
}

func TestNoWriterOtherThanTheMarkerSetsTheDataFlagAndAnErrorCode(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("UNMEASURED: cannot read the engine directory: %v", err)
	}
	sources := map[string]string{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatalf("UNMEASURED: cannot read %s: %v", n, err)
		}
		sources[n] = string(b)
	}
	// Vacuity: the allowed writers must exist in the source, or the scan could be reading a different tree.
	for name := range markerWriterAllowed {
		found := false
		for _, src := range sources {
			if strings.Contains(src, "func "+name+"(") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("PRECONDITION FAILED: the allowed writer %s is not in the source any more; the list is stale", name)
		}
	}
	findings, err := markerWriters(sources)
	if err != nil {
		t.Fatalf("UNMEASURED: the engine source does not parse: %v", err)
	}
	for _, f := range findings {
		t.Errorf("%s\n  The empty-stream marker is a stream chunk with StreamData and a non-zero StreamErrCode, and is a marker only because no other writer sets both. If this writer must exist, it must not produce that combination, and it belongs on the allowed list only if it cannot (cleat#4342).", f)
	}
}

// KNOWN POSITIVE and CONTROL for the scan above, on synthetic source: a literal that sets both, an assignment to each field, and a clean writer.
func TestTheMarkerWriterScanSeesALiteralAndAssignmentsAndPassesAClean(t *testing.T) {
	src := `package engine
func badLiteral() EventRecord { return EventRecord{StreamData: true, StreamErrCode: 2} }
func badAssignData(r *EventRecord) { r.StreamData = true }
func badAssignCode(r *EventRecord) { r.StreamErrCode = 3 }
func clean() EventRecord { return EventRecord{StreamData: true} }
var packageLevel = []EventRecord{{StreamData: true, StreamErrCode: 2}}
func emptyStreamMarkerRecord() EventRecord { return EventRecord{StreamData: true, StreamErrCode: 2} }
`
	findings, err := markerWriters(map[string]string{"x.go": src})
	if err != nil {
		t.Fatalf("the synthetic source did not parse: %v", err)
	}
	want := map[string]bool{"(package level) sets": false, "badLiteral sets": false, "badAssignData assigns StreamData": false, "badAssignCode assigns StreamErrCode": false}
	for _, f := range findings {
		for w := range want {
			if strings.Contains(f, w) {
				want[w] = true
			}
		}
		if strings.Contains(f, "clean") || strings.Contains(f, "emptyStreamMarkerRecord") {
			t.Errorf("a clean or allowed writer was reported: %s", f)
		}
	}
	for w, saw := range want {
		if !saw {
			t.Errorf("the scan did not see %q: %q", w, findings)
		}
	}
}
