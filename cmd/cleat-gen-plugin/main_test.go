package main

import (
	"flag"
	"testing"
)

func TestFlags_Manifest(t *testing.T) {
	manifest := flag.Lookup("manifest")
	if manifest == nil {
		t.Fatal("expected -manifest flag to be defined")
	}
	if manifest.DefValue != "" {
		t.Errorf("expected default '' got %q", manifest.DefValue)
	}
}

func TestFlags_Lang(t *testing.T) {
	lang := flag.Lookup("lang")
	if lang == nil {
		t.Fatal("expected -lang flag to be defined")
	}
	if lang.DefValue != "typescript" {
		t.Errorf("expected default 'typescript' got %q", lang.DefValue)
	}
}

func TestFlags_Out(t *testing.T) {
	out := flag.Lookup("out")
	if out == nil {
		t.Fatal("expected -out flag to be defined")
	}
	if out.DefValue != "" {
		t.Errorf("expected default '' got %q", out.DefValue)
	}
}

func TestAllFlagsDefined(t *testing.T) {
	// Verify that all three flags are present.
	flags := []string{"manifest", "lang", "out"}
	for _, name := range flags {
		if flag.Lookup(name) == nil {
			t.Errorf("flag -%s is not defined", name)
		}
	}
}
