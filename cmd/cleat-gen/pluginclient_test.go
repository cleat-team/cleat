package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPluginClientSpec(t *testing.T) {
	spec, err := loadPluginClientSpec("./testdata/pluginclient/fixture", "thing-plugin")
	if err != nil {
		t.Fatalf("loadPluginClientSpec: %v", err)
	}

	if len(spec.Funcs) != 1 {
		t.Fatalf("got %d funcs, want 1 (%+v)", len(spec.Funcs), spec.Funcs)
	}
	fn := spec.Funcs[0]
	if fn.OpName != "do_thing" {
		t.Errorf("OpName = %q, want %q", fn.OpName, "do_thing")
	}
	if fn.VarName != "DoThing" {
		t.Errorf("VarName = %q, want %q", fn.VarName, "DoThing")
	}
	if fn.RequestType != "DoThingInput" || fn.ResponseType != "DoThingOutput" {
		t.Errorf("RequestType/ResponseType = %q/%q, want DoThingInput/DoThingOutput", fn.RequestType, fn.ResponseType)
	}

	names := map[string]bool{}
	for _, ty := range spec.Types {
		names[ty.Name] = true
	}
	// NestedInfo must be here too: it's declared in the fixture's own
	// package (same as DoThingOutput, which embeds it via a field), so it
	// has to be redeclared locally rather than imported -- see
	// collectNestedSamePackageTypes.
	for _, want := range []string{"DoThingInput", "DoThingOutput", "NestedInfo"} {
		if !names[want] {
			t.Errorf("spec.Types = %+v, missing %q", spec.Types, want)
		}
	}

	foundTimeImport := false
	for _, imp := range spec.Imports {
		if imp == "time" {
			foundTimeImport = true
		}
	}
	if !foundTimeImport {
		t.Errorf("spec.Imports = %v, want \"time\" (DoThingOutput.When is time.Time)", spec.Imports)
	}
}

// TestGeneratePluginClientCodeCompiles is the check cleat#2655 found missing
// from cmd/cleat-gen-plugin's own test suite: that file asserted substrings
// of generated source and never once built it, which is how that generator
// shipped broken. This builds the ACTUAL output with `go build`, not a
// parse or a substring match.
func TestGeneratePluginClientCodeCompiles(t *testing.T) {
	spec, err := loadPluginClientSpec("./testdata/pluginclient/fixture", "thing-plugin")
	if err != nil {
		t.Fatalf("loadPluginClientSpec: %v", err)
	}
	code, err := generatePluginClientCode(spec, "thingclient")
	if err != nil {
		t.Fatalf("generatePluginClientCode: %v", err)
	}

	// The generated file imports "github.com/cleat-team/cleat/cleat", so it
	// has to be compiled from inside this module for that import to
	// resolve -- t.TempDir() (outside the repo) would not find a go.mod.
	dir, err := os.MkdirTemp("testdata/pluginclient", "gen-scratch-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)

	if err := os.WriteFile(filepath.Join(dir, "client.go"), code, 0644); err != nil {
		t.Fatalf("writing generated client: %v", err)
	}

	cmd := exec.Command("go", "build", ".")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated client does not compile: %v\n%s\n---\n%s", err, out, code)
	}
}

func TestLoadPluginClientSpecRefusesUnexportedType(t *testing.T) {
	_, err := loadPluginClientSpec("./testdata/pluginclient/badunexported", "bad-plugin")
	if err == nil {
		t.Fatal("expected an error for an unexported request type, got nil")
	}
	if !strings.Contains(err.Error(), "unexported") {
		t.Errorf("error = %v, want it to mention \"unexported\"", err)
	}
}

func TestLoadPluginClientSpecRefusesNonLiteralOptsName(t *testing.T) {
	_, err := loadPluginClientSpec("./testdata/pluginclient/badoptsvar", "bad-plugin")
	if err == nil {
		t.Fatal("expected an error for a non-literal FuncOptions.Name, got nil")
	}
	if !strings.Contains(err.Error(), "string literal") {
		t.Errorf("error = %v, want it to mention \"string literal\"", err)
	}
}

func TestLoadPluginClientSpecRefusesNoCallSites(t *testing.T) {
	_, err := loadPluginClientSpec("./testdata/pluginclient/nocalls", "bad-plugin")
	if err == nil {
		t.Fatal("expected an error for a package with no RegisterTyped call sites, got nil")
	}
	if !strings.Contains(err.Error(), "no plugin.RegisterTyped call sites") {
		t.Errorf("error = %v, want it to say no call sites were found", err)
	}
}
