//go:build cgo

package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// Every component dispatcher must read the length field its handler wrote.
//
// IMPROVEMENT-PLAN 3.33's G115 backlog is 229 integer conversions, and
// CLAUDE.md's ruling on it is that the defects in this layer have never been
// overflows -- "in every case the value meant the wrong thing on one side of
// the boundary, which a property test over that boundary would find faster
// than reading the remaining sites". This is that test for the one boundary
// where the mistake is silent.
//
// # The mistake this catches
//
// A host call's result word carries the response length, and the two layouts
// disagree about where: packDurableCallResult puts it at bits 40-63,
// packSimpleResult at bits 32-63. The component bridge has one extractor per
// layout, and every dispatcher in component_callbacks.go picks one BY HAND --
// 25 of them at the time of writing.
//
// Pick wrong and nothing errors. Reading a bit-32 length from bit 40 yields
// zero for any response under 256 bytes, so the guest gets an EMPTY SUCCESSFUL
// response; the reverse yields a huge length that clamps to the buffer and
// returns trailing garbage. TestComponentShortStringResultsAreNotTruncated
// exists because that shipped once, for one of the 25. This test covers the
// other 24, and every one added later.
//
// # Why both sides are measured rather than declared
//
// The obvious way to write this is a table -- "packSimpleResult means 32,
// extractStringFromPacked means 40" -- and that table is a third copy of the
// thing under test. It would agree with a shift that had changed underneath
// it, which is the §1.1 trap: a check the defect also satisfies.
//
// So both sides are probed instead. lengthShiftOfPacker packs a distinctive
// length and finds where it landed; lengthShiftOfExtractor hands an extractor
// words built at each candidate shift and sees which one it honours. Nothing
// here states a shift; the code under test states it and this test reads it
// back.
//
// The only declared thing is the call graph -- which handler each dispatcher
// invokes -- and that comes from the AST rather than a list.

// lengthShiftOfExtractor reports which bit position an extractor reads its
// length from, by giving it a word with a known length at each candidate shift
// and seeing which one it acts on.
//
// The probe length is 3 and the buffer is distinct bytes, so a wrong shift
// cannot accidentally return the right prefix: at the other shift the decoded
// length is either 0 (empty) or enormous (clamped to the whole buffer), and
// neither equals "abc".
func lengthShiftOfExtractor(t *testing.T, extract func(int64, []byte) string) int {
	t.Helper()
	const probe = 3
	buf := []byte("abcdefghij")
	var found []int
	for _, shift := range []int{32, 40} {
		if extract(int64(uint64(probe)<<uint(shift)), buf) == "abc" {
			found = append(found, shift)
		}
	}
	if len(found) != 1 {
		t.Fatalf("could not determine the length shift of an extractor: it honoured %v.\n\n"+
			"This test probes bits 32 and 40 because those are the two layouts the "+
			"component bridge has. A third layout, or an extractor that reads neither, "+
			"means this test no longer describes the boundary and must be extended "+
			"rather than deleted.", found)
	}
	return found[0]
}

// lengthShiftOfPacker reports where a packer puts its length, by packing a
// distinctive value and finding the shift that reproduces the word.
func lengthShiftOfPacker(t *testing.T, name string, pack func(int) int64) int {
	t.Helper()
	const probe = 3
	got := uint64(pack(probe))
	var found []int
	for _, shift := range []int{32, 40} {
		if got&(uint64(probe)<<uint(shift)) == uint64(probe)<<uint(shift) {
			// Distinguish 32 from 40 properly: 3<<32 and 3<<40 do not overlap,
			// so a match on one is exclusive unless the packer sets both.
			found = append(found, shift)
		}
	}
	if len(found) != 1 {
		t.Fatalf("could not determine where %s puts its length: matched %v (word %#x)",
			name, found, got)
	}
	return found[0]
}

// dispatcherFacts is what the AST says about one dispatch function.
type dispatcherFacts struct {
	extractors []string // extractStringFrom* identifiers it calls
	handlers   []string // b.handler.X methods it invokes
}

// parseDispatchers reads component_callbacks.go and reports, per dispatch
// method, which extractors it uses and which handler methods it calls.
func parseDispatchers(t *testing.T) map[string]dispatcherFacts {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "component_callbacks.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing component_callbacks.go: %v", err)
	}
	out := map[string]dispatcherFacts{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || !strings.HasPrefix(fn.Name.Name, "dispatch") {
			continue
		}
		var facts dispatcherFacts
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				if strings.HasPrefix(fun.Name, "extractStringFrom") {
					facts.extractors = append(facts.extractors, fun.Name)
				}
			case *ast.SelectorExpr:
				// b.handler.Method(...)
				if inner, ok := fun.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "handler" {
					facts.handlers = append(facts.handlers, fun.Sel.Name)
				}
			}
			return true
		})
		// decodeCallOutcome takes the extractor as an argument, so it appears
		// as a plain Ident rather than a call -- pick those up too.
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if ok && strings.HasPrefix(id.Name, "extractStringFrom") {
				facts.extractors = append(facts.extractors, id.Name)
			}
			return true
		})
		if len(facts.extractors) > 0 {
			out[fn.Name.Name] = facts
		}
	}
	return out
}

// sessionMethods maps each execSession method to the packers it reaches and
// the sibling methods it delegates to.
type sessionMethod struct {
	packers []string
	callees []string
}

func parseSessionMethods(t *testing.T) map[string]sessionMethod {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	out := map[string]sessionMethod{}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			continue // a file behind a different build tag; not this test's business
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil || !isExecSessionRecv(fn.Recv) {
				continue
			}
			var m sessionMethod
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					if strings.HasPrefix(fun.Name, "pack") {
						m.packers = append(m.packers, fun.Name)
					}
				case *ast.SelectorExpr:
					if x, ok := fun.X.(*ast.Ident); ok && x.Name == "s" {
						m.callees = append(m.callees, fun.Sel.Name)
					}
				}
				return true
			})
			out[fn.Name.Name] = m
		}
	}
	return out
}

func isExecSessionRecv(recv *ast.FieldList) bool {
	if len(recv.List) == 0 {
		return false
	}
	star, ok := recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	return ok && id.Name == "execSession"
}

// TestEveryComponentDispatcherReadsTheFieldItsHandlerWrote is the property.
func TestEveryComponentDispatcherReadsTheFieldItsHandlerWrote(t *testing.T) {
	// Both sides measured, never declared. See the file comment.
	extractorShift := map[string]int{
		"extractStringFromPacked":       lengthShiftOfExtractor(t, extractStringFromPacked),
		"extractStringFromSimplePacked": lengthShiftOfExtractor(t, extractStringFromSimplePacked),
	}
	packerShift := map[string]int{
		"packDurableCallResult":  lengthShiftOfPacker(t, "packDurableCallResult", func(n int) int64 { return packDurableCallResult(n, 0, 0) }),
		"packSimpleResult":       lengthShiftOfPacker(t, "packSimpleResult", func(n int) int64 { return packSimpleResult(0, uint32(n)) }),
		"packAwaitChildResult":   lengthShiftOfPacker(t, "packAwaitChildResult", func(n int) int64 { return packAwaitChildResult(uint32(n), 0) }),
		"packAwaitPromiseResult": lengthShiftOfPacker(t, "packAwaitPromiseResult", func(n int) int64 { return packAwaitPromiseResult(uint32(n), false, 0) }),
	}
	if extractorShift["extractStringFromPacked"] == extractorShift["extractStringFromSimplePacked"] {
		t.Fatalf("both extractors read the same bit position (%d). This test can no "+
			"longer tell a mispairing from a correct one, which makes it vacuous -- "+
			"the two layouts have presumably been unified, and this test should be "+
			"deleted rather than left passing.", extractorShift["extractStringFromPacked"])
	}

	dispatchers := parseDispatchers(t)
	if len(dispatchers) < 20 {
		t.Fatalf("found only %d dispatchers that extract a string; there were 25 when "+
			"this was written. A parse that matches almost nothing passes vacuously, "+
			"so this is a failure: re-point the AST walk rather than lowering the bound.",
			len(dispatchers))
	}
	session := parseSessionMethods(t)

	// Bit positions reachable from a handler method, following delegation.
	var shiftsOf func(name string, seen map[string]bool, depth int) map[int]bool
	shiftsOf = func(name string, seen map[string]bool, depth int) map[int]bool {
		got := map[int]bool{}
		if depth > 3 || seen[name] {
			return got
		}
		seen[name] = true
		m, ok := session[name]
		if !ok {
			return got
		}
		for _, p := range m.packers {
			if s, ok := packerShift[p]; ok {
				got[s] = true
			}
		}
		for _, c := range m.callees {
			for s := range shiftsOf(c, seen, depth+1) {
				got[s] = true
			}
		}
		return got
	}

	// PollCancellation and PollSignal build their result word inline
	// (`uint64(written)<<32 | flags`) instead of calling a packer, so nothing
	// above can resolve them. They are correct today -- bit 32, matching the
	// simple extractor -- and listed here rather than silently skipped, because
	// an unresolvable site is exactly where the next mispairing would hide.
	handRolled := map[string]bool{"PollCancellation": true, "PollSignal": true}

	checked := 0
	for dispatch, facts := range dispatchers {
		want := map[int]bool{}
		for _, e := range facts.extractors {
			if s, ok := extractorShift[e]; ok {
				want[s] = true
			}
		}
		if len(want) != 1 {
			t.Errorf("%s uses extractors at %v; a dispatcher reading two different "+
				"length fields cannot be right for both", dispatch, want)
			continue
		}
		for _, h := range facts.handlers {
			if handRolled[h] {
				continue
			}
			got := shiftsOf(h, map[string]bool{}, 0)
			if len(got) == 0 {
				t.Errorf("%s -> %s: could not determine where the handler writes its "+
					"length.\n\nAn unresolved site is not a pass. Either the handler now "+
					"packs through a helper this test does not follow, or it builds the "+
					"word inline like PollCancellation does -- in which case add it to "+
					"handRolled WITH the reason, so the exception is visible.", dispatch, h)
				continue
			}
			checked++
			for s := range got {
				if !want[s] {
					t.Errorf("MISPAIRED: %s extracts the length from bit %v, but its "+
						"handler %s writes it at bit %d.\n\n"+
						"Nothing will error at runtime. Reading a bit-32 length from bit "+
						"40 gives 0 for any response under 256 bytes -- an empty "+
						"SUCCESSFUL response -- and the reverse gives a length that "+
						"clamps to the buffer and returns trailing garbage. See "+
						"TestComponentShortStringResultsAreNotTruncated for the time "+
						"this shipped.", dispatch, keys(want), h, s)
				}
			}
		}
	}
	if checked < 15 {
		t.Fatalf("only %d dispatcher/handler pairings were actually compared; 23 "+
			"resolved when this was written. A test that resolves nothing reports "+
			"success, so this is a failure rather than a quiet pass.", checked)
	}
	t.Logf("compared %d dispatcher/handler pairings across %d dispatchers", checked, len(dispatchers))
}

func keys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestThePackExtractParityCheckCanFail is the control, and without it the test
// above is worth nothing: a checker that never reports a mismatch passes on a
// codebase that is entirely mispaired.
//
// It does not mutate the tree. It asserts the two properties the checker rests
// on -- that the extractors disagree about where the length lives, and that
// each one returns the WRONG answer for the other's layout. If either stopped
// holding, the comparison above would be comparing nothing.
func TestThePackExtractParityCheckCanFail(t *testing.T) {
	buf := []byte("abcdefghij")

	// A bit-32 length of 3, read by the bit-40 extractor.
	simpleWord := int64(uint64(3) << 32)
	if got := extractStringFromPacked(simpleWord, buf); got == "abc" {
		t.Fatalf("extractStringFromPacked recovered %q from a bit-32 word. The two "+
			"layouts no longer differ in a way this test can detect, so the parity "+
			"test above cannot fail and must be reconsidered.", got)
	} else if got != "" {
		t.Logf("bit-32 word read at bit 40 gives %q (expected empty for a short response)", got)
	}

	// A bit-40 length of 3, read by the bit-32 extractor: a huge length,
	// clamped to the buffer.
	durableWord := int64(uint64(3) << 40)
	if got := extractStringFromSimplePacked(durableWord, buf); got == "abc" {
		t.Fatalf("extractStringFromSimplePacked recovered %q from a bit-40 word; the "+
			"mispairing this test exists to catch would be invisible.", got)
	} else if len(got) != len(buf) {
		t.Logf("bit-40 word read at bit 32 gives %d bytes (clamped to the buffer)", len(got))
	}
}
