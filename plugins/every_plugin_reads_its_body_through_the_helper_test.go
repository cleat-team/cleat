package plugins_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// rawBodyFinding is one call that reads an *http.Request's Body directly,
// bypassing plugin.ReadBody / plugin.ReadJSONBody (plugin/body.go, cleat#2232).
type rawBodyFinding struct {
	Pos     string
	Problem string
}

// requestParamNames returns every identifier bound to a *http.Request
// parameter anywhere in file -- in a top-level FuncDecl or in a FuncLit, such
// as an inline mux.HandleFunc(pattern, func(w http.ResponseWriter, r
// *http.Request) {...}) registration.
//
// FILE-LEVEL, not per-function-scoped: a name is in the set if it is bound to
// *http.Request ANYWHERE in the file, and every io.ReadAll/json.NewDecoder
// call matching that name anywhere in the file is examined against it. That
// is shallower than tracking each function's own lexical scope, and it is
// enough here because it is what makes the scan NOT hardcode "r" -- the thing
// design review asked for -- without the added machinery of a real scope
// walk. It would over-report only if one file used the same identifier for a
// *http.Request parameter in one function and an *http.Response (or anything
// else with a .Body) in another; every plugin in this tree names its
// outbound response "resp" (or "tokenResp", "userResp") and never "r" or
// "req", so that collision does not occur today. See
// TestTheRawBodyScannerReportsOnlyRequestBodyReads for the case this
// distinguishes: an outbound io.ReadAll(resp.Body) beside an inbound
// io.ReadAll(r.Body) in the same file must flag only the second.
func requestParamNames(file *ast.File) map[string]bool {
	names := map[string]bool{}
	collect := func(ft *ast.FuncType) {
		if ft.Params == nil {
			return
		}
		for _, field := range ft.Params.List {
			star, ok := field.Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			sel, ok := star.X.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Request" {
				continue
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "http" {
				continue
			}
			for _, n := range field.Names {
				names[n.Name] = true
			}
		}
	}
	ast.Inspect(file, func(nd ast.Node) bool {
		switch fn := nd.(type) {
		case *ast.FuncDecl:
			collect(fn.Type)
		case *ast.FuncLit:
			collect(fn.Type)
		}
		return true
	})
	return names
}

// surveyRawBodyReads reports every io.ReadAll or json.NewDecoder call in one
// file whose argument is <name>.Body for some name bound to *http.Request in
// that file (see requestParamNames), and how many *http.Request parameters it
// found -- the latter is what lets the real-tree test tell "nothing to find"
// apart from "found nothing wrong": a file with zero *http.Request handlers
// contributes zero to both counts, and the floor in
// TestEveryPluginReadsItsRequestBodyThroughTheHelper is on handler count, not
// on findings, precisely because zero findings is the PASSING answer here,
// unlike the egress guard next to this file where zero clients was the
// unmeasured one.
func surveyRawBodyReads(fset *token.FileSet, path string, src []byte) (findings []rawBodyFinding, handlers int, err error) {
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, 0, err
	}
	reqNames := requestParamNames(file)
	handlers = len(reqNames)
	if handlers == 0 {
		return nil, 0, nil
	}

	isBodyOfRequest := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Body" {
			return false
		}
		ident, ok := sel.X.(*ast.Ident)
		return ok && reqNames[ident.Name]
	}

	ast.Inspect(file, func(nd ast.Node) bool {
		call, ok := nd.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || !isBodyOfRequest(call.Args[0]) {
			return true
		}
		switch {
		case pkg.Name == "io" && sel.Sel.Name == "ReadAll":
			findings = append(findings, rawBodyFinding{fset.Position(call.Pos()).String(),
				"reads the request body with io.ReadAll instead of plugin.ReadBody / plugin.ReadJSONBody"})
		case pkg.Name == "json" && sel.Sel.Name == "NewDecoder":
			findings = append(findings, rawBodyFinding{fset.Position(call.Pos()).String(),
				"decodes the request body with json.NewDecoder instead of plugin.ReadJSONBody"})
		}
		return true
	})
	return findings, handlers, nil
}

// TestEveryPluginReadsItsRequestBodyThroughTheHelper is cleat#2232 design item
// 2's second half. The 26 sites the bulk conversion touched are the ones
// existing when it ran; this is the guard against a 27th one repeating the
// same omission cleat#1338 already paid for once on the core API -- an
// oversized body read directly rather than through the shared helper reads as
// 500 or hangs allocating, not as a 413 naming the limit.
//
// AST, not text, for the same reason as the egress guard beside this file: a
// grep for "ReadAll(" cannot tell an inbound request body from an outbound
// response body, and plugins/{llm,kafkaconnect,email,...} read the latter in
// 16 places today (io.ReadAll(resp.Body) and friends) -- a text-based ban
// would either miss request bodies with an unusual variable name or flag
// every one of those 16 outbound reads as if they were the same defect.
func TestEveryPluginReadsItsRequestBodyThroughTheHelper(t *testing.T) {
	repo, all := pluginFiles(t)
	var files []string
	for _, f := range all {
		if strings.HasPrefix(f, "plugins/") {
			files = append(files, f)
		}
	}

	fset := token.NewFileSet()
	examinedFiles, handlerCount := 0, 0

	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		// FROM DISK, not `git show HEAD:` -- see pluginFiles's own comment on
		// why: reading HEAD passes a change that ADDS a violation and fails
		// the change that fixes one.
		body, err := os.ReadFile(filepath.Join(repo, f))
		if err != nil {
			t.Errorf("%s: listed by git ls-files and unreadable: %v", f, err)
			continue
		}
		findings, handlers, err := surveyRawBodyReads(fset, f, body)
		if err != nil {
			// UNMEASURED, not a skip: a file that fails to parse reads
			// exactly like a clean one if this is silently dropped, which is
			// the same failure this whole test exists to refuse.
			t.Errorf("%s: does not parse, so it was not surveyed: %v", f, err)
			continue
		}
		if handlers == 0 {
			continue
		}
		examinedFiles++
		handlerCount += handlers
		for _, fd := range findings {
			t.Errorf("%s: %s.\ncleat#2232: every plugin route reads its body through "+
				"plugin.ReadBody or plugin.ReadJSONBody, which is what turns an oversized "+
				"body into a 413 naming the limit instead of a hang or a 500. If this read "+
				"is not on an HTTP route's request body, move it off r.Body / <name>.Body so "+
				"this scan stops matching it.", fd.Pos, fd.Problem)
		}
	}

	// The floor is on HANDLERS EXAMINED, not on zero findings -- zero
	// findings is the passing answer this test wants on a clean tree, so it
	// cannot also be the signal that the scan looked at nothing.
	if examinedFiles == 0 || handlerCount < 20 {
		t.Fatalf("examined %d files with %d *http.Request handler(s) total; want at least "+
			"20 across the plugin tree -- the scan did not see the tree", examinedFiles, handlerCount)
	}
}

// TestTheRawBodyScannerReportsOnlyRequestBodyReads is the known-positive and
// negative-control half TestEveryPluginReadsItsRequestBodyThroughTheHelper
// cannot exercise on its own: every plugin route already goes through the
// helper, so a run over the real tree is green whether this scanner is
// right, wrong, or deleted outright. Nothing in the suite hands it a body
// read that SHOULD fail without these fixtures.
func TestTheRawBodyScannerReportsOnlyRequestBodyReads(t *testing.T) {
	const preamble = "package p\n\nimport (\n\t\"encoding/json\"\n\t\"io\"\n\t\"net/http\"\n\n" +
		"\t\"github.com/cleat-team/cleat/plugin\"\n)\n\n"

	cases := []struct {
		name     string
		body     string
		handlers int
		want     string // substring of the reported problem; "" means no finding
	}{{
		name:     "bare io.ReadAll on the request body",
		body:     "func h(w http.ResponseWriter, r *http.Request) {\n\tb, _ := io.ReadAll(r.Body)\n\t_ = b\n}",
		handlers: 1,
		want:     "reads the request body with io.ReadAll",
	}, {
		name:     "bare json.NewDecoder on the request body",
		body:     "func h(w http.ResponseWriter, r *http.Request) {\n\tvar v struct{}\n\t_ = json.NewDecoder(r.Body).Decode(&v)\n}",
		handlers: 1,
		want:     "decodes the request body with json.NewDecoder",
	}, {
		name:     "the request parameter named something other than r",
		body:     "func h(w http.ResponseWriter, req *http.Request) {\n\tb, _ := io.ReadAll(req.Body)\n\t_ = b\n}",
		handlers: 1,
		want:     "reads the request body with io.ReadAll",
	}, {
		name:     "the guarded helper, not a finding",
		body:     "func h(w http.ResponseWriter, r *http.Request) {\n\t_, _ = plugin.ReadBody(w, r)\n}",
		handlers: 1,
		want:     "",
	}, {
		name: "an OUTBOUND response body must not be mistaken for the request's, even named r",
		body: "func h(w http.ResponseWriter, req *http.Request) {\n" +
			"\tr, _ := http.Get(\"http://example.com\")\n" +
			"\tb, _ := io.ReadAll(r.Body)\n\t_ = b\n}",
		handlers: 1, // req is the only *http.Request param; r here is *http.Response
		want:     "",
	}, {
		name: "an outbound read in a function with no *http.Request parameter at all",
		body: "func fetch(url string) ([]byte, error) {\n" +
			"\tresp, err := http.Get(url)\n\tif err != nil {\n\t\treturn nil, err\n\t}\n" +
			"\treturn io.ReadAll(resp.Body)\n}",
		handlers: 0,
		want:     "",
	}, {
		name: "an inline HandleFunc closure, the shape every routes.go registers through",
		body: "var mux http.ServeMux\n\nfunc init() {\n" +
			"\tmux.HandleFunc(\"POST /x\", func(w http.ResponseWriter, r *http.Request) {\n" +
			"\t\tb, _ := io.ReadAll(r.Body)\n\t\t_ = b\n\t})\n}",
		handlers: 1,
		want:     "reads the request body with io.ReadAll",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			findings, handlers, err := surveyRawBodyReads(fset, "fixture.go", []byte(preamble+tc.body+"\n"))
			if err != nil {
				t.Fatalf("the fixture does not parse, so this case asserts nothing: %v", err)
			}
			if handlers != tc.handlers {
				t.Errorf("found %d *http.Request handler(s), want %d", handlers, tc.handlers)
			}
			got := rawBodyProblems(findings)
			if tc.want == "" {
				if len(findings) != 0 {
					t.Errorf("reported %v; this case must not be flagged", got)
				}
				return
			}
			if len(findings) == 0 {
				t.Fatalf("reported nothing. This case is the defect itself: a scan that " +
					"cannot see this call cannot see the 27th real one either.")
			}
			if !strings.Contains(strings.Join(got, " | "), tc.want) {
				t.Errorf("reported %v, which does not mention %q", got, tc.want)
			}
		})
	}
}

func rawBodyProblems(fs []rawBodyFinding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Problem)
	}
	sort.Strings(out)
	return out
}
