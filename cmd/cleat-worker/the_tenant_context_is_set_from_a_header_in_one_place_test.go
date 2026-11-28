package main

// cleat#3577. The behavioural test (a_key_beats_a_header_tenant_through_the_chain_as_main_composes_it_test.go)
// measures the order composeRequestChain builds. It cannot see a SECOND way for a client-supplied value to
// reach the request context, written inline somewhere composeRequestChain does not cover. This pins the
// other half: in cmd/cleat-worker's production code the identity setters of package auth are called in
// exactly one place, the header wrapper, which is the one composeRequestChain puts outermost.
//
// It reads the AST, not the text, so a comment or a string that mentions auth.WithTenantID( is not a call,
// and an import alias does not hide one.
//
// COUNT-KEYED, NOT SET-KEYED: "WithTenantID is only called from files on this list" passes a second call
// pasted beside the allowed one, and a sibling setter (WithSubject, WithOperator) that is not on the list
// of things being looked for. The allowed set here is exactly one site; two of the same is a finding.
//
// WHAT IT DOES NOT SEE: a setter reached through a function value or a method on an embedded type; a
// context key written by a package other than auth. Both would need auth to export something it does not.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const authImportPath = "github.com/cleat-team/cleat/auth"

// identitySetters are the functions of package auth that put an identity in a request context.
var identitySetters = map[string]bool{"WithTenantID": true, "WithSubject": true, "WithOperator": true}

type setterSite struct{ file, fn, setter string }

func (s setterSite) String() string {
	return fmt.Sprintf("%s: %s calls auth.%s", s.file, s.fn, s.setter)
}

// theOneAllowedSite is the header wrapper: composeRequestChain installs it outermost.
var theOneAllowedSite = setterSite{file: "auth_chain.go", fn: "tenantResolverMiddleware", setter: "WithTenantID"}

// identitySetterSites lists every call to an identity setter of package auth in the non-test Go files of dir.
func identitySetterSites(dir string) ([]setterSite, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var sites []setterSite
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, 0)
		if err != nil {
			return nil, err
		}
		local := ""
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == authImportPath {
				local = "auth"
				if imp.Name != nil {
					local = imp.Name.Name
				}
			}
		}
		if local == "" {
			continue
		}
		scan := func(fn string, root ast.Node) {
			ast.Inspect(root, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == local && identitySetters[sel.Sel.Name] {
					sites = append(sites, setterSite{file: name, fn: fn, setter: sel.Sel.Name})
				}
				return true
			})
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Body != nil {
					scan(d.Name.Name, d.Body)
				}
			case *ast.GenDecl:
				scan("<package level>", d)
			}
		}
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].String() < sites[j].String() })
	return sites, nil
}

// identitySetterFindings is empty when sites is exactly the one allowed site.
func identitySetterFindings(sites []setterSite) []string {
	var out []string
	allowed := 0
	for _, s := range sites {
		if s == theOneAllowedSite {
			allowed++
			if allowed > 1 {
				out = append(out, "a SECOND call at the allowed site: "+s.String())
			}
			continue
		}
		out = append(out, "unexpected: "+s.String())
	}
	if allowed == 0 {
		out = append(out, fmt.Sprintf("the header wrapper (%s) is missing, so this check measured nothing: nothing to compare the others to", theOneAllowedSite))
	}
	return out
}

// chainCall is a call, by bare name, to one of the functions that build the identity chain.
type chainCall struct{ file, in, callee string }

func (c chainCall) String() string { return fmt.Sprintf("%s: %s calls %s", c.file, c.in, c.callee) }

// chainCalls lists every call to a function in names, by bare identifier, in the non-test Go files of dir.
func chainCalls(dir string, names ...string) ([]chainCall, error) {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []chainCall
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, 0)
		if err != nil {
			return nil, err
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && want[id.Name] {
						out = append(out, chainCall{file: name, in: fd.Name.Name, callee: id.Name})
					}
				}
				return true
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

// chainCallFindings is empty when the chain is built in exactly one way: main() calls composeRequestChain
// once, and composeAuthChain and tenantResolverMiddleware are called only from composeRequestChain.
func chainCallFindings(calls []chainCall) []string {
	var out []string
	mainCalls := 0
	for _, c := range calls {
		switch {
		case c.callee == "composeRequestChain" && c.file == "main.go" && c.in == "main":
			mainCalls++
			if mainCalls > 1 {
				out = append(out, "a SECOND call of composeRequestChain from main: "+c.String())
			}
		case (c.callee == "composeAuthChain" || c.callee == "tenantResolverMiddleware") && c.in == "composeRequestChain":
			// the one place the order is written down
		default:
			out = append(out, "unexpected: "+c.String())
		}
	}
	if mainCalls == 0 {
		out = append(out, "main() does not call composeRequestChain, so the chain a worker runs is not the chain the tests measure")
	}
	return out
}

func TestTheChainAWorkerRunsIsTheChainTheTestsMeasure(t *testing.T) {
	calls, err := chainCalls(".", "composeRequestChain", "composeAuthChain", "tenantResolverMiddleware")
	if err != nil {
		t.Fatal(err)
	}
	if got := chainCallFindings(calls); len(got) != 0 {
		t.Errorf("the identity chain is built somewhere other than composeRequestChain, which main() calls once:\n  %s\n"+
			"An inline copy of that order is exactly what cleat#3577 could not see: the tests measure composeRequestChain.",
			strings.Join(got, "\n  "))
	}
}

func TestTheChainCallGuardSeesWhatItIsFor(t *testing.T) {
	const chainFile = "package main\nfunc composeRequestChain() { composeAuthChain(); tenantResolverMiddleware() }\n"
	const mainOK = "package main\nfunc main() { composeRequestChain() }\n"
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"the real shape passes", map[string]string{"auth_chain.go": chainFile, "main.go": mainOK}, ""},
		{"main builds the chain itself, in the right order", map[string]string{
			"auth_chain.go": chainFile,
			"main.go":       "package main\nfunc main() { composeRequestChain(); tenantResolverMiddleware(); composeAuthChain() }\n",
		}, "unexpected: main.go: main calls tenantResolverMiddleware"},
		{"main calls composeAuthChain directly instead", map[string]string{
			"auth_chain.go": chainFile,
			"main.go":       "package main\nfunc main() { composeAuthChain() }\n",
		}, "does not call composeRequestChain"},
		{"another function builds it", map[string]string{
			"auth_chain.go": chainFile, "main.go": mainOK,
			"other.go": "package main\nfunc serve() { composeAuthChain() }\n",
		}, "unexpected: other.go: serve calls composeAuthChain"},
		{"a duplicate of the allowed call in main", map[string]string{
			"auth_chain.go": chainFile,
			"main.go":       "package main\nfunc main() { composeRequestChain(); composeRequestChain() }\n",
		}, "a SECOND call of composeRequestChain"},
		{"a call inside a closure in main is still main's", map[string]string{
			"auth_chain.go": chainFile,
			"main.go":       "package main\nfunc main() { composeRequestChain(); func() { tenantResolverMiddleware() }() }\n",
		}, "unexpected: main.go: main calls tenantResolverMiddleware"},
		{"a mention in a comment or string is not a call", map[string]string{
			"auth_chain.go": chainFile, "main.go": mainOK,
			"other.go": "package main\n// composeAuthChain() is described here\nvar s = \"composeAuthChain()\"\n",
		}, ""},
		{"nothing at all: the check measured nothing", map[string]string{"other.go": "package main\n"}, "does not call composeRequestChain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, src := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			calls, err := chainCalls(dir, "composeRequestChain", "composeAuthChain", "tenantResolverMiddleware")
			if err != nil {
				t.Fatal(err)
			}
			got := chainCallFindings(calls)
			if tc.want == "" {
				if len(got) != 0 {
					t.Errorf("want no finding, got %v", got)
				}
				return
			}
			if !strings.Contains(strings.Join(got, "\n"), tc.want) {
				t.Errorf("want a finding containing %q, got %v", tc.want, got)
			}
		})
	}
}

func TestTheRequestTenantIsSetFromAClientValueInExactlyOnePlace(t *testing.T) {
	sites, err := identitySetterSites(".")
	if err != nil {
		t.Fatal(err)
	}
	if got := identitySetterFindings(sites); len(got) != 0 {
		t.Errorf("cmd/cleat-worker puts an identity in a request context somewhere other than the header wrapper that composeRequestChain installs outermost:\n  %s\n"+
			"A value set inside the chain can shadow the key the chain just authenticated (cleat#3577). Put it in the chain's order in auth_chain.go, with a test in a_key_beats_a_header_tenant_through_the_chain_as_main_composes_it_test.go.",
			strings.Join(got, "\n  "))
	}
}

// TestTheIdentitySetterGuardSeesWhatItIsFor probes the guard itself, because a guard that has never refused
// anything cannot be observed to be wrong. Each case is a directory of synthetic files.
func TestTheIdentitySetterGuardSeesWhatItIsFor(t *testing.T) {
	const allowedFile = `package main

import (
	"net/http"

	"github.com/cleat-team/cleat/auth"
)

func tenantResolverMiddleware(mode string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(auth.WithTenantID(r.Context(), [16]byte{}))
			next.ServeHTTP(w, r)
		})
	}
}
`
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string // a substring of a finding; "" means no finding
	}{
		{"the real shape: one site, passes", map[string]string{"auth_chain.go": allowedFile}, ""},
		{"a new site in another file", map[string]string{
			"auth_chain.go": allowedFile,
			"other.go":      "package main\nimport \"github.com/cleat-team/cleat/auth\"\nfunc h() { _ = auth.WithTenantID(nil, [16]byte{}) }\n",
		}, "other.go: h calls auth.WithTenantID"},
		{"a sibling API: WithSubject", map[string]string{
			"auth_chain.go": allowedFile,
			"main.go":       "package main\nimport \"github.com/cleat-team/cleat/auth\"\nfunc main() { _ = auth.WithSubject(nil, \"x\") }\n",
		}, "main.go: main calls auth.WithSubject"},
		{"a sibling API: WithOperator", map[string]string{
			"auth_chain.go": allowedFile,
			"main.go":       "package main\nimport \"github.com/cleat-team/cleat/auth\"\nfunc main() { _ = auth.WithOperator(nil, auth.Operator{}) }\n",
		}, "main.go: main calls auth.WithOperator"},
		{"a duplicate of the allowed line, beside it", map[string]string{
			"auth_chain.go": strings.Replace(allowedFile, "next.ServeHTTP(w, r)", "r = r.WithContext(auth.WithTenantID(r.Context(), [16]byte{}))\n\t\t\tnext.ServeHTTP(w, r)", 1),
		}, "a SECOND call at the allowed site"},
		{"the allowed call moved to a differently named function", map[string]string{
			"auth_chain.go": strings.Replace(allowedFile, "tenantResolverMiddleware", "somethingElse", 1),
		}, "unexpected: auth_chain.go: somethingElse calls auth.WithTenantID"},
		{"an aliased import does not hide a call", map[string]string{
			"auth_chain.go": allowedFile,
			"other.go":      "package main\nimport a \"github.com/cleat-team/cleat/auth\"\nfunc h() { _ = a.WithTenantID(nil, [16]byte{}) }\n",
		}, "other.go: h calls auth.WithTenantID"},
		{"a package-level initialiser is a site too", map[string]string{
			"auth_chain.go": allowedFile,
			"other.go":      "package main\nimport \"github.com/cleat-team/cleat/auth\"\nvar _ = auth.WithTenantID(nil, [16]byte{})\n",
		}, "<package level> calls auth.WithTenantID"},
		{"a comment and a string that MENTION it are not calls", map[string]string{
			"auth_chain.go": allowedFile,
			"other.go":      "package main\n// auth.WithTenantID( is mentioned here\nvar s = \"auth.WithTenantID(x)\"\n",
		}, ""},
		{"a test file is not production code", map[string]string{
			"auth_chain.go": allowedFile,
			"other_test.go": "package main\nimport \"github.com/cleat-team/cleat/auth\"\nfunc h() { _ = auth.WithTenantID(nil, [16]byte{}) }\n",
		}, ""},
		{"no header wrapper at all: the check measured nothing", map[string]string{
			"other.go": "package main\nfunc h() {}\n",
		}, "is missing, so this check measured nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, src := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			sites, err := identitySetterSites(dir)
			if err != nil {
				t.Fatal(err)
			}
			got := identitySetterFindings(sites)
			if tc.want == "" {
				if len(got) != 0 {
					t.Errorf("want no finding, got %v", got)
				}
				return
			}
			if !strings.Contains(strings.Join(got, "\n"), tc.want) {
				t.Errorf("want a finding containing %q, got %v", tc.want, got)
			}
		})
	}
}
