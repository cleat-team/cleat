package wasm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAnAbsoluteReplacePathIsNotNestedUnderTheProject is cleat#1322 item 3's
// second half.
//
// propagateReplaces rewrote every directory replace as
//
//	filepath.Abs(filepath.Join(projectRoot, r.New.Path))
//
// and filepath.Join does NOT special-case an already-absolute second argument:
//
//	filepath.Join("/proj", "/abs/dep")  ->  "/proj/abs/dep"
//
// So a workflow module with an absolute replace directive -- which `go mod
// edit -replace=foo=/abs/path` writes, and which is the natural form in a
// monorepo -- got its dependency rewritten to a path that does not exist, and
// the failure surfaced as an opaque `go mod tidy` error about a module it
// could not find.
//
// A relative replace must still be resolved against projectRoot, which is the
// case the old code got right and a naive fix would break.
func TestAnAbsoluteReplacePathIsNotNestedUnderTheProject(t *testing.T) {
	projectRoot := t.TempDir()
	outDir := t.TempDir()

	// A dependency that really exists, at an absolute path outside the project.
	absDep := t.TempDir()

	// And one reached relatively, to pin the behaviour the old code had right.
	relDep := filepath.Join(projectRoot, "sibling")
	if err := os.MkdirAll(relDep, 0o755); err != nil {
		t.Fatalf("mkdir relDep: %v", err)
	}

	writeMod := func(dir, body string) string {
		p := filepath.Join(dir, "go.mod")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
		return p
	}

	writeMod(projectRoot, "module example.com/proj\n\ngo 1.25\n\n"+
		"replace example.com/absdep => "+absDep+"\n"+
		"replace example.com/reldep => ./sibling\n")
	outMod := writeMod(outDir, "module example.com/out\n\ngo 1.25\n")

	if err := propagateReplaces(projectRoot, outDir, outMod, false); err != nil {
		t.Fatalf("propagateReplaces: %v", err)
	}

	got, err := os.ReadFile(outMod)
	if err != nil {
		t.Fatalf("reading the generated go.mod: %v", err)
	}
	out := string(got)
	t.Logf("generated go.mod:\n%s", out)

	// Vacuity: if nothing was appended, every "does not contain" check below
	// passes for the wrong reason.
	if !strings.Contains(out, "replace example.com/absdep") {
		t.Fatalf("no replace for absdep was propagated at all; this test measured "+
			"nothing:\n%s", out)
	}

	// THE DEFECT: projectRoot prepended to an already-absolute path.
	nested := filepath.Join(projectRoot, absDep)
	if strings.Contains(out, nested) {
		t.Errorf("the absolute replace was nested under the project root:\n  got  %s\n"+
			"  want %s\nfilepath.Join does not special-case an absolute second "+
			"argument (cleat#1322).", nested, absDep)
	}
	if !strings.Contains(out, "example.com/absdep => "+absDep) {
		t.Errorf("the absolute replace should be used as-is:\n  want ... => %s\n  in:\n%s",
			absDep, out)
	}

	// THE CONTROL: a RELATIVE replace must still resolve against projectRoot.
	// A fix that simply stopped joining would break this.
	if !strings.Contains(out, "example.com/reldep => "+relDep) {
		t.Errorf("a relative replace must still be resolved against the project root:\n"+
			"  want ... => %s\n  in:\n%s", relDep, out)
	}
}
