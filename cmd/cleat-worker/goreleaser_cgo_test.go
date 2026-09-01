package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// goreleaserConfig is the subset of .goreleaser.yml this guard reads. Fields
// not named here are ignored by yaml.Unmarshal.
type goreleaserConfig struct {
	Builds []struct {
		ID     string   `yaml:"id"`
		Main   string   `yaml:"main"`
		Env    []string `yaml:"env"`
		Goos   []string `yaml:"goos"`
		Goarch []string `yaml:"goarch"`
		Hooks  struct {
			Post []struct {
				Cmd string `yaml:"cmd"`
			} `yaml:"post"`
		} `yaml:"hooks"`
	} `yaml:"builds"`
}

func loadGoreleaser(t *testing.T) goreleaserConfig {
	t.Helper()
	// This test lives in cmd/cleat-worker; the config is at the repo root.
	path := filepath.Join("..", "..", ".goreleaser.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v\n\n"+
			"This guard is about the release config. If the file moved, point it at "+
			"the new path rather than deleting the test.", path, err)
	}
	var cfg goreleaserConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return cfg
}

// findWorkerBuild locates the build block that produces cmd/cleat-worker.
//
// It matches on `main`, not on `id`. An id is a label someone can rename; the
// main package is what actually determines whether the artifact is the worker.
// Matching on the label would let a rename turn every assertion below into a
// vacuous pass.
func findWorkerBuild(t *testing.T, cfg goreleaserConfig) int {
	t.Helper()
	for i, b := range cfg.Builds {
		if filepath.Clean(b.Main) == filepath.Clean("./cmd/cleat-worker") {
			return i
		}
	}
	t.Fatalf("no build in .goreleaser.yml has main: ./cmd/cleat-worker.\n\n"+
		"Either the worker stopped being released -- in which case delete this "+
		"guard deliberately, and say so -- or the path changed and every "+
		"assertion here just stopped checking anything. Builds found: %d", len(cfg.Builds))
	return -1
}

// TestReleasedWorkerIsBuiltWithCGO pins the defect that shipped every release
// before 2026-09-01.
//
// .goreleaser.yml built cleat-worker with CGO_ENABLED=0. That compiles out
// engine/backend_wasmtime.go (`//go:build cgo`), so NewWasmtimeBackend resolves
// to the stub in backend_wasmtime_stub.go and returns
// ErrWasmtimeCGOUnavailable. cmd/cleat-worker/main.go:789 logs "wasmtime is the
// only WASM backend cleat has, there is no fallback" and calls os.Exit(1).
//
// So every cleat-worker attached to a GitHub release exited 1 before reading a
// flag or opening a database. Measured 2026-09-01:
//
//	CGO_ENABLED=0 -> NewWasmtimeBackend err = "wasmtime backend requires CGO"
//	CGO_ENABLED=1 -> err = <nil>
//
// It survived for months because `CGO_ENABLED=0 go build ./...` exits 0 -- the
// failure is at startup, not at build time -- so the release pipeline was green
// the whole way. The Dockerfile had already been fixed for exactly this and
// carries a `RUN /cleat-worker --verify-backend` gate; goreleaser was missed.
// Fixed in one place, not the other, with nothing tying them together.
//
// scripts/verify-release-worker.sh now executes each artifact at release time,
// which is the stronger check. This test is the cheaper one: it runs on every
// PR, so a reintroduction is caught in review rather than at the next tag.
func TestReleasedWorkerIsBuiltWithCGO(t *testing.T) {
	cfg := loadGoreleaser(t)
	b := cfg.Builds[findWorkerBuild(t, cfg)]

	var sawCGO bool
	for _, e := range b.Env {
		key, val, ok := strings.Cut(e, "=")
		if !ok || strings.TrimSpace(key) != "CGO_ENABLED" {
			continue
		}
		sawCGO = true
		if strings.TrimSpace(val) != "1" {
			t.Errorf("cleat-worker is released with CGO_ENABLED=%s.\n\n"+
				"Such a binary cannot construct the wasmtime backend and exits 1 at "+
				"startup, before reading a flag. It is not a degraded worker, it is a "+
				"worker that does not run.", val)
		}
	}
	if !sawCGO {
		t.Error("the cleat-worker build sets no CGO_ENABLED at all.\n\n" +
			"Leaving it to the environment is how this broke: goreleaser runs on a " +
			"runner whose default is not guaranteed, and a CGO-less worker exits 1 " +
			"at startup. Set it explicitly to 1.")
	}
}

// TestReleasedWorkerIsLinuxOnly records a decision, so that reversing it is a
// deliberate act rather than an edit that quietly ships broken artifacts again.
//
// A CGO darwin binary cannot be linked on ubuntu-latest without osxcross, and
// every job in .github/workflows runs ubuntu-latest (36 of 36, measured
// 2026-09-01 with `grep -rhoE 'runs-on: .*' .github/workflows/ | sort | uniq -c`).
// Adding darwin back to this list without adding a macOS runner reproduces the
// original bug: goreleaser would fail to link, or -- worse, if CGO were turned
// off again to make it link -- publish binaries that cannot start.
//
// macOS users install with `go install`, which README.md:149 documents and
// which builds locally with CGO on.
func TestReleasedWorkerIsLinuxOnly(t *testing.T) {
	cfg := loadGoreleaser(t)
	b := cfg.Builds[findWorkerBuild(t, cfg)]

	if len(b.Goos) == 0 {
		t.Fatal("the cleat-worker build names no goos; goreleaser would default to " +
			"a set including darwin, which cannot be linked with CGO on ubuntu.")
	}
	for _, os := range b.Goos {
		if os != "linux" {
			t.Errorf("cleat-worker is released for goos %q.\n\n"+
				"Only linux can be built with CGO on the ubuntu-latest runner this "+
				"release uses. Adding %q needs a runner that can link it -- see the "+
				"comment on this build in .goreleaser.yml -- not just this line.", os, os)
		}
	}
}

// TestReleasedWorkerArtifactsAreExecuted is the guard on the guard.
//
// The two tests above check what the config says. This one checks that
// something executes the result, because "the config says CGO_ENABLED=1" is a
// claim about a file, not about a binary: a wrong CC, a musl base, or a broken
// wasmtime-go release would all satisfy the assertions above and still produce
// an artifact that exits 1.
//
// The whole reason this defect lived for months is that the release pipeline
// was green without ever running what it published.
func TestReleasedWorkerArtifactsAreExecuted(t *testing.T) {
	cfg := loadGoreleaser(t)
	b := cfg.Builds[findWorkerBuild(t, cfg)]

	const script = "verify-release-worker.sh"
	var found bool
	for _, h := range b.Hooks.Post {
		if strings.Contains(h.Cmd, script) {
			found = true
		}
	}
	if !found {
		t.Errorf("no post-build hook on cleat-worker runs %s.\n\n"+
			"Without it nothing executes the published binary, which is exactly how "+
			"a release shipped workers that exited 1 at startup. Static assertions "+
			"about .goreleaser.yml are not a substitute for running the artifact.", script)
	}

	// The hook is a path to a file; if it does not exist, goreleaser fails at
	// release time rather than here, which is far too late.
	if _, err := os.Stat(filepath.Join("..", "..", "scripts", script)); err != nil {
		t.Errorf("scripts/%s is referenced by .goreleaser.yml but not readable: %v", script, err)
	}
}
