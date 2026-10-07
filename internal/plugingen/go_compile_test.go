package plugingen

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/packages"
)

// cleat#2655: internal/plugingen/go_test.go used to assert only on
// substrings of the generated source ("expected Echo method on
// EchoPlugin") -- nothing parsed it, let alone type-checked it. A generator
// that produces text containing the right substrings but referencing an
// undefined type, missing an import, or passing the wrong argument type is
// invisible to that kind of test, which is exactly what shipped: an
// undefined `pluginCaller` type, a missing "context" import, and a
// []byte/string mismatch against the real PluginCall, all present for as
// long as this issue was open and none of it caught.
//
// assertGoCompiles writes generated code plus a caller-supplied stub (the
// plugin author's own business-logic methods) to real files on disk, in a
// throwaway package directory INSIDE this module tree, and loads that
// directory with golang.org/x/tools/go/packages -- which resolves
// "github.com/cleat-team/cleat/plugin" the same way `go build` would,
// against the real package, not a hand-maintained fake.
//
// This has to be ONE packages.Load call over both files together, not a
// separate go/importer.Default() for "context" plus a packages.Load for
// "plugin": those two loading paths do not share a type-checking universe,
// so plugin.PluginFunc's context.Context parameter and the generated
// wrapper's own context.Context are OBJECT-DISTINCT named types to
// go/types even though both come from the standard library "context"
// package -- assignability then correctly reports them as different types,
// which looks exactly like a real generator bug and is not one. Measured
// directly: that two-importer version reported
// "cannot use (func(ctx context.Context, ...)) as plugin.PluginFunc value"
// against generated code that `go build` accepts outright. Loading
// everything through one packages.Load call, as a real directory on disk,
// is what the issue means by "an actual go build of a scratch module", and
// side-steps the mismatch because there is only one context.Context in
// play.
//
// The directory name starts with "." so `go build ./...`/`go test ./...`
// elsewhere in this tree never sees it -- Go's own package-pattern rule
// (`go help packages`) skips "." and "_" prefixed directories, which
// matters because several other sessions build this repo concurrently.
func assertGoCompiles(t *testing.T, generated, stub string) {
	t.Helper()

	dir, err := os.MkdirTemp(".", ".compiletest-")
	if err != nil {
		t.Fatalf("creating scratch compile-check directory: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	if err := os.WriteFile(filepath.Join(dir, "generated.go"), []byte(generated), 0o644); err != nil {
		t.Fatalf("writing generated.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stub.go"), []byte(stub), 0o644); err != nil {
		t.Fatalf("writing stub.go: %v", err)
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("resolving scratch directory: %v", err)
	}

	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedSyntax |
			packages.NeedTypesInfo | packages.NeedDeps | packages.NeedImports,
		Dir: absDir,
	}, ".")
	if err != nil {
		t.Fatalf("UNMEASURED: loading the scratch package failed rather than reporting a compile "+
			"error -- this is a failure of the check, not a finding about the generator: %v", err)
	}
	if len(pkgs) != 1 {
		t.Fatalf("UNMEASURED: expected exactly one package from the scratch directory, got %d", len(pkgs))
	}
	pkg := pkgs[0]
	if len(pkg.Errors) > 0 {
		t.Fatalf("generated code does not compile:\n%v\n--- generated ---\n%s\n--- stub ---\n%s",
			pkg.Errors, generated, stub)
	}
}
