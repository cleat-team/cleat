package plugin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// EVERY TYPE THAT EMBEDS http.ResponseWriter MUST IMPLEMENT Flush AND Unwrap (cleat#2254).
//
// Embedding an interface promotes only that interface's methods. http.ResponseWriter has no Flush, so a
// wrapper that embeds it and adds nothing hides the real writer's http.Flusher, and every handler behind
// it that does `w.(http.Flusher)` is told streaming is not supported. That is what happened: every plugin
// middleware wraps the CORE mux (cleat#1569), audit-log's wrapper had no Flush, and on every default build
// GET /api/workflows/:id/stream answered 500 while its own tests, which use a recorder that has a Flush,
// passed. Unwrap is the other half: it is how http.ResponseController reaches the real writer for
// SetWriteDeadline and friends.
//
// This reads the source of every tracked, non-test Go file. It cannot see a wrapper built some other way
// (a struct with a NAMED ResponseWriter field, a generated type), and says nothing about a wrapper that
// declares Flush and forgets to delegate; the regression test in cmd/cleat-worker serves a stream through
// the real plugin chain and covers that.
func TestEveryResponseWriterWrapperImplementsFlushAndUnwrap(t *testing.T) {
	// From the repo root, so the paths are the tracked ones whatever this package's directory is.
	root := repoRoot(t)
	list, err := exec.Command("git", "-C", root, "ls-files", "-z", "--", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	type key struct{ dir, typ string }
	embeds := map[key]string{} // wrappers found, with where
	methods := map[key]map[string]bool{}
	files := 0
	for _, rel := range strings.Split(string(list), "\x00") {
		if rel == "" || strings.HasSuffix(rel, "_test.go") || strings.Contains(rel, "/testdata/") || strings.HasPrefix(rel, ".claude/") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if err != nil {
			t.Errorf("%s does not parse, so its wrappers were not checked: %v", rel, err)
			continue
		}
		files++
		dir := filepath.Dir(rel)
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				for _, sp := range d.Specs {
					ts, ok := sp.(*ast.TypeSpec)
					if !ok {
						continue
					}
					if st, ok := ts.Type.(*ast.StructType); ok && embedsResponseWriter(st) {
						embeds[key{dir, ts.Name.Name}] = rel + ":" + itoa(fset.Position(ts.Pos()).Line)
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil || len(d.Recv.List) != 1 {
					continue
				}
				name := recvTypeName(d.Recv.List[0].Type)
				if name == "" {
					continue
				}
				k := key{dir, name}
				if methods[k] == nil {
					methods[k] = map[string]bool{}
				}
				methods[k][d.Name.Name] = true
			}
		}
	}
	if files < 100 {
		t.Fatalf("read only %d Go files: the file list is broken, not the tree", files)
	}
	if len(embeds) == 0 {
		t.Fatal("found no type embedding http.ResponseWriter: the scan is broken (there are at least three in this repo)")
	}
	var bad []string
	for k, where := range embeds {
		var missing []string
		for _, m := range []string{"Flush", "Unwrap"} {
			if !methods[k][m] {
				missing = append(missing, m)
			}
		}
		if len(missing) > 0 {
			bad = append(bad, where+"  type "+k.typ+" is missing "+strings.Join(missing, " and "))
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("these types embed http.ResponseWriter and hide the real writer's Flush (streaming answers "+
			"500) and Unwrap (http.ResponseController cannot reach it):\n  %s\n\n"+
			"Add `func (w *T) Flush() { if f, ok := w.ResponseWriter.(http.Flusher); ok { f.Flush() } }` and "+
			"`func (w *T) Unwrap() http.ResponseWriter { return w.ResponseWriter }`. cleat#2254.",
			strings.Join(bad, "\n  "))
	}
}

func embedsResponseWriter(st *ast.StructType) bool {
	for _, fld := range st.Fields.List {
		if len(fld.Names) != 0 {
			continue // a named field is not embedding
		}
		if sel, ok := fld.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "ResponseWriter" {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "http" {
				return true
			}
		}
	}
	return false
}

func recvTypeName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.StarExpr:
		return recvTypeName(e.X)
	case *ast.Ident:
		return e.Name
	}
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", wd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}
