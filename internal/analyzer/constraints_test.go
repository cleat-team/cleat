package analyzer

import (
	"testing"
)

func TestMatchFilenameSuffix(t *testing.T) {
	tests := []struct {
		name      string
		goos      string
		goarch    string
		wantMatch bool
		wantFound bool
	}{
		// No constraint
		{"foo.go", "wasip1", "wasm", false, false},
		{"bar.go", "linux", "amd64", false, false},
		// _test alone is not a constraint
		{"foo_test.go", "wasip1", "wasm", false, false},
		// Single GOOS suffix — excluded for wasip1 target
		{"foo_linux.go", "wasip1", "wasm", false, true},
		{"foo_windows.go", "wasip1", "wasm", false, true},
		{"foo_darwin.go", "wasip1", "wasm", false, true},
		// Single GOOS suffix — matching wasip1 target
		{"foo_wasip1.go", "wasip1", "wasm", true, true},
		// Single GOARCH suffix — excluded for wasm target
		{"foo_amd64.go", "wasip1", "wasm", false, true},
		{"foo_arm64.go", "wasip1", "wasm", false, true},
		// Single GOARCH suffix — matching wasm target
		{"foo_wasm.go", "wasip1", "wasm", true, true},
		// _GOOS_GOARCH suffix — both must match
		{"foo_linux_amd64.go", "wasip1", "wasm", false, true},
		{"foo_wasip1_wasm.go", "wasip1", "wasm", true, true},
		{"foo_linux_arm64.go", "wasip1", "wasm", false, true},
		{"foo_wasip1_amd64.go", "wasip1", "wasm", false, true},
		// _GOOS_GOARCH_test — _test stripped
		{"foo_linux_amd64_test.go", "wasip1", "wasm", false, true},
		{"foo_wasip1_wasm_test.go", "wasip1", "wasm", true, true},
		// _GOOS_test suffix
		{"foo_linux_test.go", "wasip1", "wasm", false, true},
		{"foo_wasip1_test.go", "wasip1", "wasm", true, true},
		// Matching for other targets
		{"foo_linux.go", "linux", "amd64", true, true},
		{"foo_amd64.go", "linux", "amd64", true, true},
		{"foo_linux_amd64.go", "linux", "amd64", true, true},
		// Non-matching for other targets
		{"foo_darwin.go", "linux", "amd64", false, true},
		// Edge: underscore prefix but no recognized GOOS/GOARCH
		{"foo_something.go", "wasip1", "wasm", false, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotMatch, gotFound := matchFilenameSuffix(tc.name, tc.goos, tc.goarch)
			if gotMatch != tc.wantMatch || gotFound != tc.wantFound {
				t.Errorf("matchFilenameSuffix(%q, %q, %q) = (%v, %v), want (%v, %v)",
					tc.name, tc.goos, tc.goarch,
					gotMatch, gotFound, tc.wantMatch, tc.wantFound)
			}
		})
	}
}

func TestMatchWasmBuildConstraint(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		content  string
		want     bool
	}{
		// No constraints — included by default
		{"no constraints", "foo.go", "package foo\n", true},
		// Filename-based: _linux excluded for wasip1
		{"linux suffix", "workflow_linux.go", "package workflow\n", false},
		// Filename-based: _wasm included
		{"wasm suffix", "workflow_wasm.go", "package workflow\n", true},
		// go:build wasm — included
		{"go:build wasm", "foo.go", "//go:build wasm\npackage foo\n", true},
		// go:build linux — excluded
		{"go:build linux", "foo.go", "//go:build linux\npackage foo\n", false},
		// go:build !wasm — excluded
		{"go:build !wasm", "foo.go", "//go:build !wasm\npackage foo\n", false},
		// go:build wasm && amd64 — excluded (amd64 doesn't match wasm GOARCH)
		{"go:build wasm && amd64", "foo.go", "//go:build wasm && amd64\npackage foo\n", false},
		// go:build wasm || linux — included (wasm matches)
		{"go:build wasm || linux", "foo.go", "//go:build wasm || linux\npackage foo\n", true},
		// Both filename and go:build — filename constraint takes priority
		{"linux suffix + go:build wasm", "workflow_linux.go", "//go:build wasm\npackage workflow\n", false},
		// go:build with wasip1 GOOS
		{"go:build wasip1", "foo.go", "//go:build wasip1\npackage foo\n", true},
		// Non-.go file — not subject to Go constraints
		{"c file", "foo.c", "int main() { return 0; }\n", true},
		// go:build unix — wasip1 is not unix
		{"go:build unix", "foo.go", "//go:build unix\npackage foo\n", false},
		// go:build cgo — no CGo in WASM
		{"go:build cgo", "foo.go", "//go:build cgo\npackage foo\n", false},
		// Empty content
		{"empty content", "foo.go", "", true},
		// go:build with version tag — assumed modern Go
		{"go:build go1.20", "foo.go", "//go:build go1.20\npackage foo\n", true},
		// Malformed go:build — conservative exclusion
		{"malformed go:build", "foo.go", "//go:build !!\npackage foo\n", false},
		// Legacy +build comment — constraint.Parse does not handle it
		{"legacy +build", "foo.go", "// +build wasm\npackage foo\n", true},
		// go:build with wasip1 and wasm both required
		{"go:build wasip1 && wasm", "foo.go", "//go:build wasip1 && wasm\npackage foo\n", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MatchWasmBuildConstraint(tc.filename, []byte(tc.content))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("MatchWasmBuildConstraint(%q, %q) = %v, want %v",
					tc.filename, tc.content, got, tc.want)
			}
		})
	}
}

func TestMatchBuildConstraintWithCustomTarget(t *testing.T) {
	// Verify matchBuildConstraint with non-wasm targets.
	// Linux file with linux target should match.
	ok, err := matchBuildConstraint("foo_linux.go", []byte("package foo\n"), "linux", "amd64")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("foo_linux.go should match linux/amd64 target")
	}

	// Darwin file with linux target should not match.
	ok, err = matchBuildConstraint("foo_darwin.go", []byte("package foo\n"), "linux", "amd64")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("foo_darwin.go should not match linux/amd64 target")
	}
}

func TestExtractGoBuildLine(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
		wantOk  bool
	}{
		{"wasm tag", "//go:build wasm\npackage foo\n", "//go:build wasm", true},
		{"composite tag", "//go:build linux && amd64\npackage foo\n", "//go:build linux && amd64", true},
		{"no go:build", "package foo\nfunc main() {}\n", "", false},
		{"whitespace indent", "  //go:build wasm\npackage foo\n", "//go:build wasm", true},
		{"only legacy +build", "// +build wasm\npackage foo\n", "", false},
		{"empty content", "", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, gotOk := extractGoBuildLine(tc.content)
			if got != tc.want || gotOk != tc.wantOk {
				t.Errorf("extractGoBuildLine(%q) = (%q, %v), want (%q, %v)",
					tc.content, got, gotOk, tc.want, tc.wantOk)
			}
		})
	}
}

func TestEvalBuildTag(t *testing.T) {
	const goos = "wasip1"
	const goarch = "wasm"

	tests := []struct {
		tag  string
		want bool
	}{
		// Direct GOOS/GOARCH matches
		{"wasip1", true},
		{"wasm", true},
		// Non-matching GOOS/GOARCH
		{"linux", false},
		{"windows", false},
		{"darwin", false},
		{"amd64", false},
		{"arm64", false},
		// wasip1 is not unix
		{"unix", false},
		// No CGo in WASM
		{"cgo", false},
		// Version tags — assume modern Go
		{"go1.18", true},
		{"go1.20", true},
		{"go1.25", true},
		{"go1.99", true},
		// Unknown tag — conservative exclusion
		{"customtag", false},
		{"experiment", false},
	}

	for _, tc := range tests {
		t.Run(tc.tag, func(t *testing.T) {
			got := evalBuildTag(tc.tag, goos, goarch)
			if got != tc.want {
				t.Errorf("evalBuildTag(%q, %q, %q) = %v, want %v",
					tc.tag, goos, goarch, got, tc.want)
			}
		})
	}
}

func TestWasmFilenameWarnings(t *testing.T) {
	tests := []struct {
		filename  string
		wantCount int
	}{
		{"foo.go", 0},
		{"foo_linux.go", 1},
		{"foo_amd64.go", 1},
		{"foo_linux_amd64.go", 1},
		{"foo.c", 0},
		{"foo_test.go", 0},
	}

	for _, tc := range tests {
		t.Run(tc.filename, func(t *testing.T) {
			got := WasmFilenameWarnings(tc.filename)
			if len(got) != tc.wantCount {
				t.Errorf("WasmFilenameWarnings(%q) returned %d warnings, want %d: %v",
					tc.filename, len(got), tc.wantCount, got)
			}
		})
	}
}

func TestFilenameConstrainedOut(t *testing.T) {
	tests := []struct {
		filename string
		want     bool
	}{
		{"foo.go", false},
		{"foo_linux.go", true},
		{"foo_amd64.go", true},
		{"foo_linux_amd64.go", true},
		{"foo_wasm.go", false},   // wasm matches GOARCH
		{"foo_wasip1.go", false}, // wasip1 matches GOOS
		{"foo_wasip1_wasm.go", false},
		{"foo.c", false},       // non-.go file
		{"foo_test.go", false}, // test alone is not constrained
	}

	for _, tc := range tests {
		t.Run(tc.filename, func(t *testing.T) {
			got := FilenameConstrainedOut(tc.filename)
			if got != tc.want {
				t.Errorf("FilenameConstrainedOut(%q) = %v, want %v",
					tc.filename, got, tc.want)
			}
		})
	}
}
