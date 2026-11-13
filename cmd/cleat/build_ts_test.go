package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The entry spec is the one place user text becomes shell arguments, so it is parsed strictly and the refusals are pinned.
func TestParseTSEntry(t *testing.T) {
	good := map[string][]string{
		"a/b/wf.ts:place_order":       {"place_order"},
		"wf.ts:one,two_three,x9":      {"one", "two_three", "x9"},
		"C:/dir with space/wf.ts:run": {"run"}, // the LAST colon separates the entry list
	}
	for spec, want := range good {
		_, got, err := parseTSEntry(spec)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("parseTSEntry(%q) = %v, %v; want %v", spec, got, err, want)
		}
	}
	for _, bad := range []string{
		"wf.ts", "wf.ts:", "wf.ts:PlaceOrder", "wf.ts:place-order", "wf.js:run", "wf.ts:a,,b", "wf.ts:a;rm -rf /", "wf.ts:$(id)", "wf.ts:_x", "wf.ts:x_",
	} {
		if _, _, err := parseTSEntry(bad); err == nil {
			t.Errorf("parseTSEntry(%q) accepted a spec that must be refused", bad)
		}
	}
}

func TestIsValidTargetKnowsTS(t *testing.T) {
	if !isValidTarget("ts") {
		t.Error(`isValidTarget("ts") = false`)
	}
	if isValidTarget("typescript") || isValidTarget("TS") {
		t.Error("only the exact name ts is a target")
	}
}

// A toolchain that is not built must be reported by name, not discovered three steps into a build.
func TestTSToolchainMissingNamesWhatIsAbsent(t *testing.T) {
	dir := t.TempDir()
	if got := tsToolchainMissing(dir); len(got) != 3 {
		t.Fatalf("empty dir: missing = %v, want all three", got)
	}
	for _, rel := range []string{".tools/javy", "node_modules/.bin/esbuild", "link/target/release/cleat-js-link"} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := tsToolchainMissing(dir); len(got) != 0 {
		t.Fatalf("complete dir: missing = %v, want none", got)
	}
}
