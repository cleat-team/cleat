package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Template .go files carry `//go:build ignore` so they do not compile as
// packages of this repository -- see stripScaffoldBuildTag's doc for why that
// matters (it is what breaks the root <-> cleat/ module cycle). The constraint
// must not survive into a user's project, though, or their brand-new scaffold
// contains zero buildable Go files.
//
// That was not hypothetical. Before this was fixed, `cleat init --template
// agent` copied templates/agent/workflow.go verbatim, and `go build ./...` in
// the generated project reported:
//
//	go: warning: "./..." matched no packages
//
// which is the worst possible failure mode for a scaffold: it looks like it
// worked, and the error names nothing that suggests a template bug.
func TestScaffoldedGoFilesHaveNoBuildConstraint(t *testing.T) {
	for _, tmpl := range []struct {
		name     string
		scaffold func(string)
	}{
		{"agent", scaffoldAgent},
		{"workflow", scaffoldWorkflow},
		{"basic", scaffoldBasic},
	} {
		t.Run(tmpl.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "proj")
			// The scaffold functions chdir-free but write relative to cwd.
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatalf("getwd: %v", err)
			}
			t.Cleanup(func() { _ = os.Chdir(cwd) })
			if err := os.Chdir(t.TempDir()); err != nil {
				t.Fatalf("chdir: %v", err)
			}
			tmpl.scaffold(filepath.Base(dir))

			var sawGo bool
			err = filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
					return err
				}
				sawGo = true
				b, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				head := string(b)
				if i := strings.Index(head, "\npackage "); i >= 0 {
					head = head[:i]
				}
				for _, bad := range []string{"//go:build ignore", "// +build ignore"} {
					if strings.Contains(head, bad) {
						t.Errorf("%s contains %q before its package clause; "+
							"a scaffolded project with an excluded file has no buildable "+
							"packages and `go build ./...` reports \"matched no packages\"",
							path, bad)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatalf("walk: %v", err)
			}
			if !sawGo && tmpl.name != "basic" {
				t.Errorf("template %q scaffolded no .go files at all", tmpl.name)
			}
		})
	}
}

func TestStripScaffoldBuildTag(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"go:build with blank line", "//go:build ignore\n\npackage main\n", "package main\n"},
		{"go:build no blank line", "//go:build ignore\npackage main\n", "package main\n"},
		{"legacy +build", "// +build ignore\n\npackage main\n", "package main\n"},
		{"no constraint is untouched", "package main\n\nfunc main() {}\n", "package main\n\nfunc main() {}\n"},
		// A constraint that is not at the top is not a build constraint at all
		// (Go only honours them before the package clause), so leaving it alone
		// is correct -- stripping it would silently edit a user's comment.
		{"not leading", "package main\n//go:build ignore\n", "package main\n//go:build ignore\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(stripScaffoldBuildTag([]byte(tt.in))); got != tt.want {
				t.Errorf("stripScaffoldBuildTag(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
