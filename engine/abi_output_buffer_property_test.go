//go:build cgo

package engine

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A property over the whole output-buffer ABI, rather than one test per call.
//
// 31 of the 56 cleat_* host functions take a (ptr, maxLen) output-buffer pair:
// the guest hands the host a buffer and its capacity, and the host writes a
// result into it. #520 gave one of them -- cleat_create_promise -- a behavioural
// test, after §3.55 found the host had been registering it with a parameter no
// guest passed.
//
// The other 30 have the identical shape and no semantic coverage. The mutation
// that mattered in #520 was **swapping promiseIDPtr with promiseIDMaxLen**:
// both are i32, both are in range, so the module still links, the host writes
// at an offset the guest never nominated, and the guest reads a buffer nobody
// filled. Nothing in the suite would have seen that in any of the other 30.
//
// CLAUDE.md, on precisely this layer: "A backlog of 200 similar findings is
// usually one missing abstraction, not 200 fixes. Four real defects have come
// out of the ABI layer's integer-conversion sites and none of them was an
// overflow -- in every case the value meant the wrong thing on one side of the
// boundary, which a property test over that boundary would find faster than
// reading the remaining sites."
//
// So this asserts the invariant every wrapper shares, over every wrapper:
//
//	the closure's last two parameters are (ptr, maxLen), and they reach the
//	handler as its last two arguments, in that order, with the guest's memory
//	on the context
//
// Verified to hold with zero deviations before being encoded (2026-09-01): of
// 51 wrappers taking a *wasmtime.Caller, 31 end in a (Ptr, MaxLen) pair and 0
// end in a MaxLen whose preceding parameter is not its Ptr. The 5 host calls
// not matched here -- cleat_now, cleat_random, cleat_sleep, cleat_version,
// cleat_min_version -- take no Caller and no buffer.
//
// What this does NOT do: it reads source, so it cannot catch a wrong value that
// is nonetheless passed in the right position. That is what the behavioural
// test in create_promise_abi_test.go is for, on one call. The two are layered
// deliberately -- breadth here, depth there.

type wrapperInfo struct {
	name       string // cleat_*
	pos        string
	params     []string
	ptrParam   string
	maxLenPara string
	handlerAt  []*ast.CallExpr
}

// parseHostFuncWrappers reads the wasmtime host-function registrations.
func parseHostFuncWrappers(t *testing.T) []wrapperInfo {
	t.Helper()
	files, err := filepath.Glob("wasmtime_hostfuncs*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no wasmtime_hostfuncs*.go files found (err=%v).\n\n"+
			"An unmatched glob makes this test pass having examined nothing.", err)
	}
	sort.Strings(files)

	var out []wrapperInfo
	fset := token.NewFileSet()
	for _, path := range files {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "FuncWrap" || len(call.Args) < 3 {
				return true
			}
			mod, ok1 := stringLit(call.Args[0])
			name, ok2 := stringLit(call.Args[1])
			if !ok1 || !ok2 || mod != "env" || !strings.HasPrefix(name, "cleat_") {
				return true
			}
			lit, ok := call.Args[2].(*ast.FuncLit)
			if !ok {
				return true
			}

			w := wrapperInfo{name: name, pos: fset.Position(call.Pos()).String()}
			for _, field := range lit.Type.Params.List {
				for _, id := range field.Names {
					w.params = append(w.params, id.Name)
				}
			}
			// A handler call is one whose first argument carries the guest's
			// memory -- identified by ctxWithMem rather than by the receiver's
			// name, so renaming `h` cannot hide a call from this test.
			//
			// Two spellings, and missing the second was this test's own first
			// bug: four wrappers (cleat_call, cleat_poll_work, cleat_json_parse,
			// cleat_json_stringify) bind `callCtx := ctxWithMem(...)` to a local
			// and pass the variable. Matching only the inline form reported them
			// as "makes no handler call", which was a defect in the test, not in
			// them.
			ctxLocals := map[string]bool{}
			ast.Inspect(lit.Body, func(n2 ast.Node) bool {
				assign, ok := n2.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for i, rhs := range assign.Rhs {
					c, ok := rhs.(*ast.CallExpr)
					if !ok {
						continue
					}
					if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "ctxWithMem" && i < len(assign.Lhs) {
						if lhs, ok := assign.Lhs[i].(*ast.Ident); ok {
							ctxLocals[lhs.Name] = true
						}
					}
				}
				return true
			})
			ast.Inspect(lit.Body, func(n2 ast.Node) bool {
				inner, ok := n2.(*ast.CallExpr)
				if !ok || len(inner.Args) == 0 {
					return true
				}
				switch first := inner.Args[0].(type) {
				case *ast.CallExpr:
					if id, ok := first.Fun.(*ast.Ident); ok && id.Name == "ctxWithMem" {
						w.handlerAt = append(w.handlerAt, inner)
					}
				case *ast.Ident:
					if ctxLocals[first.Name] {
						w.handlerAt = append(w.handlerAt, inner)
					}
				}
				return true
			})
			out = append(out, w)
			return true
		})
	}
	return out
}

func isOutputBuffer(params []string) (ptr, maxLen string, ok bool) {
	if len(params) < 2 {
		return "", "", false
	}
	p, m := params[len(params)-2], params[len(params)-1]
	if !strings.HasSuffix(m, "MaxLen") && !strings.HasSuffix(m, "Max") {
		return "", "", false
	}
	if !strings.HasSuffix(p, "Ptr") && !strings.HasSuffix(p, "ptr") {
		return "", "", false
	}
	return p, m, true
}

// uint32ArgName returns the identifier inside a uint32(x) conversion.
func uint32ArgName(e ast.Expr) (string, bool) {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return "", false
	}
	id, ok := call.Fun.(*ast.Ident)
	if !ok || id.Name != "uint32" {
		return "", false
	}
	arg, ok := call.Args[0].(*ast.Ident)
	if !ok {
		return "", false
	}
	return arg.Name, true
}

// TestOutputBufferHostCallsPassPtrAndMaxLenInOrder is the property.
//
// For every host function that takes an output buffer, the guest's pointer and
// capacity must reach the handler last, in that order. A swap links cleanly and
// corrupts guest memory; #520 demonstrated it for one call and this covers the
// rest.
// directWriters are output-buffer host calls that write into the guest's memory
// themselves rather than delegating to the handler, so the argument-order
// property does not apply to them.
//
// The list is asserted to be exact, not merely tolerated: a wrapper that stops
// writing directly, or a new one that starts, changes what this test covers, and
// that should be a decision rather than a silent gap. CLAUDE.md: if a check
// bounds its own coverage, say what it dropped.
//
// cleat_poll_work is the only one (2026-09-01). It is also the *least* covered
// thing here and worth a behavioural test on its own: it takes TWO output
// buffers -- (entryNamePtr, entryNameMaxLen) and (argsPtr, argsMaxLen) -- and
// copies into both with hand-written bounds, which is exactly the shape where a
// mix-up writes past a guest's declared capacity.
var directWriters = map[string]bool{
	"cleat_poll_work": true,
}

func TestOutputBufferHostCallsPassPtrAndMaxLenInOrder(t *testing.T) {
	wrappers := parseHostFuncWrappers(t)
	sawDirect := map[string]bool{}

	// Vacuous-pass control: a broken parse would examine nothing and report
	// success, which is the failure mode this whole file guards against.
	// 51 wrappers / 31 with output buffers, measured 2026-09-01; the floors sit
	// below that so ordinary additions do not trip them.
	if len(wrappers) < 45 {
		t.Fatalf("parsed only %d env host-function wrappers, expected at least 45.\n\n"+
			"The AST walk looks for linker.FuncWrap with a literal \"env\" and a "+
			"function literal. If registration moved behind a helper, this test "+
			"examines a fraction of the surface and still passes.", len(wrappers))
	}

	var checked int
	for _, w := range wrappers {
		ptr, maxLen, ok := isOutputBuffer(w.params)
		if !ok {
			continue
		}
		checked++

		if len(w.handlerAt) == 0 {
			if directWriters[w.name] {
				sawDirect[w.name] = true
				continue
			}
			t.Errorf("%s (%s) takes an output buffer (%s, %s) but makes no handler call "+
				"with ctxWithMem.\n\n"+
				"Either it does not hand the guest's memory to the handler -- in which "+
				"case nothing it writes reaches the guest -- or it writes into the guest "+
				"buffer itself. The latter is legitimate but is not covered by the "+
				"argument-order property below, so add it to directWriters deliberately "+
				"rather than letting it escape unexamined.",
				w.name, w.pos, ptr, maxLen)
			continue
		}

		for _, call := range w.handlerAt {
			if len(call.Args) < 2 {
				t.Errorf("%s (%s): handler call has %d arguments, too few to carry the "+
					"output buffer", w.name, w.pos, len(call.Args))
				continue
			}
			gotPtr, okP := uint32ArgName(call.Args[len(call.Args)-2])
			gotMax, okM := uint32ArgName(call.Args[len(call.Args)-1])
			if !okP || !okM {
				t.Errorf("%s (%s): the handler call's last two arguments are not "+
					"uint32(...) conversions of parameters; got %s.\n\n"+
					"Every output-buffer wrapper passes uint32(ptr), uint32(maxLen) last. "+
					"If this one legitimately differs, say so here rather than loosening "+
					"the property.", w.name, w.pos, exprsToString(call.Args))
				continue
			}
			if gotPtr == maxLen && gotMax == ptr {
				t.Errorf("%s (%s): the output-buffer arguments are SWAPPED -- the handler "+
					"receives (%s, %s) where the guest passed (%s, %s).\n\n"+
					"This links cleanly: both are i32 and both are in range. The host then "+
					"writes at an offset the guest never nominated, and treats the guest's "+
					"pointer as a capacity. #520 demonstrated exactly this failure for "+
					"cleat_create_promise.", w.name, w.pos, gotPtr, gotMax, ptr, maxLen)
				continue
			}
			if gotPtr != ptr || gotMax != maxLen {
				t.Errorf("%s (%s): handler receives (%s, %s) as its last two arguments, "+
					"but the guest's output buffer is (%s, %s).",
					w.name, w.pos, gotPtr, gotMax, ptr, maxLen)
			}
		}
	}

	// The allowlist must be exact in both directions.
	for name := range directWriters {
		if !sawDirect[name] {
			t.Errorf("%s is listed in directWriters but was not found writing directly.\n\n"+
				"Either it now delegates to the handler -- in which case remove it, so the "+
				"argument-order property starts covering it -- or the name is wrong and an "+
				"entry is excusing nothing.", name)
		}
	}

	if checked < 25 {
		t.Fatalf("only %d wrappers were recognised as taking an output buffer, expected "+
			"at least 25 (31 as of 2026-09-01).\n\n"+
			"isOutputBuffer matches on parameter naming; a renaming convention would "+
			"quietly shrink this test's coverage to nothing while it kept passing.", checked)
	}
	if testing.Verbose() {
		t.Logf("checked %d output-buffer host calls of %d wrappers", checked, len(wrappers))
	}
}

func exprsToString(args []ast.Expr) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		switch v := a.(type) {
		case *ast.Ident:
			parts = append(parts, v.Name)
		case *ast.CallExpr:
			if id, ok := v.Fun.(*ast.Ident); ok {
				parts = append(parts, id.Name+"(...)")
			} else {
				parts = append(parts, "call(...)")
			}
		default:
			parts = append(parts, fmt.Sprintf("%T", a))
		}
	}
	return "(" + strings.Join(parts, ", ") + ")"
}
