package csharp

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// The real `cleat build --target csharp` (cleat#4080, C# PR 2): the digest-pinned linux/amd64 .NET SDK container, the package lock, a
// real component. The first run on a machine downloads the toolchain (about 2 minutes under emulation, a few hundred MB), and later
// runs reuse the cache volumes. cmd/cleat/build_csharp_test.go covers everything the CLI does around the container without docker.
//
// Nothing here claims C# is supported. It measures that the build chain does what its header says, including the refusals.

const requireToolchainEnv = "CLEAT_REQUIRE_TOOLCHAINS"

func toolchainRequired(name string) bool {
	for _, want := range strings.Split(os.Getenv(requireToolchainEnv), ",") {
		if strings.TrimSpace(want) == name {
			return true
		}
	}
	return false
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for d := dir; ; d = filepath.Dir(d) {
		if b, err := os.ReadFile(filepath.Join(d, "go.mod")); err == nil && strings.HasPrefix(string(b), "module github.com/cleat-team/cleat\n") {
			return d
		}
		if filepath.Dir(d) == d {
			t.Fatalf("no repository root above %s", dir)
		}
	}
}

// needDocker skips, or FAILS when the job declared csharp in CLEAT_REQUIRE_TOOLCHAINS, unless a docker daemon answers.
func needDocker(t *testing.T) {
	t.Helper()
	why := ""
	if _, err := exec.LookPath("docker"); err != nil {
		why = "docker is not on PATH"
	} else if err := exec.Command("docker", "info").Run(); err != nil {
		why = "the docker daemon does not answer"
	}
	if why == "" {
		return
	}
	if toolchainRequired("csharp") {
		t.Fatalf("%s, but %s declares csharp, so this job provides the C# toolchain: the setup step is broken, not the test", why, requireToolchainEnv)
	}
	t.Skip(why + " (the C# build runs in a pinned container; only the C# tier-2 job provides it)")
}

func cleatBinary(t *testing.T, root string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "cleat")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/cleat")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building cleat: %v\n%s", err, out)
	}
	return bin
}

func runCleat(t *testing.T, bin, root string, env []string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err == nil {
		return 0, out.String()
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), out.String()
	}
	t.Fatalf("could not run cleat: %v", err)
	return -1, ""
}

// TestCSharpBuildProducesAStampedComponent builds the hello project and reads the artifact back the way the engine will.
func TestCSharpBuildProducesAStampedComponent(t *testing.T) {
	needDocker(t)
	root := repoRoot(t)
	bin := cleatBinary(t, root)
	out := t.TempDir()
	code, log := runCleat(t, bin, root, nil, "build", "--target", "csharp", "-o", out, "--version", "7", "tests/csharp/testdata/hello")
	if code != 0 {
		t.Fatalf("cleat build --target csharp exited %d:\n%s", code, log)
	}
	b, err := os.ReadFile(filepath.Join(out, "hello.wasm"))
	if err != nil {
		t.Fatalf("no artifact: %v\n%s", err, log)
	}
	meta, err := wasm.ReadMetadata(b)
	if err != nil {
		t.Fatalf("the artifact has no cleat.metadata: %v", err)
	}
	if meta.Language != "csharp" || meta.WorkflowName != "hello" || meta.WorkflowVersion != 7 ||
		strings.Join(meta.EntryPoints, ",") != "Hello" {
		t.Errorf("stamp = %+v, want language csharp, name hello, version 7, entry points [Hello]", meta)
	}
	if err := meta.Validate(); err != nil {
		t.Errorf("the stamp fails Validate(), which `cleat deploy` runs: %v", err)
	}
	if got := wasm.DetectLanguage(b); got != "csharp" {
		t.Errorf("DetectLanguage = %q, want csharp", got)
	}
	// The reason the stamp is mandatory, measured on the REAL component and not a header: with the stamp removed, the same bytes
	// are not detected as csharp (cleat#4077 found them detected as python).
	bare, err := wasm.WriteMetadata(b, &wasm.Metadata{WorkflowName: "hello", WorkflowVersion: 1, ABIVersion: 1, MinCompatibleVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := wasm.DetectLanguage(bare); got == "csharp" {
		t.Errorf("without a language in the stamp the component is detected as %q: the negative control is meaningless", got)
	}
	t.Logf("component %d bytes; build log:\n%s", len(b), log)
}

// TestCSharpBuildRefusesAProjectThatDoesNotCompile is the known-positive for exit 1: a finding about the project, not UNMEASURED.
func TestCSharpBuildRefusesAProjectThatDoesNotCompile(t *testing.T) {
	needDocker(t)
	root := repoRoot(t)
	bin := cleatBinary(t, root)
	// Copy under the repo (the project refers to the WIT file by relative path), into a directory this test removes.
	proj, err := os.MkdirTemp(filepath.Join(root, "tests", "csharp", "testdata"), "broken-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(proj) })
	for _, f := range []string{"Hello.csproj", "Program.cs"} {
		b, err := os.ReadFile(filepath.Join(root, "tests", "csharp", "testdata", "hello", f))
		if err != nil {
			t.Fatal(err)
		}
		if f == "Program.cs" {
			b = append(b, []byte("\nthis is not C#;\n")...)
		}
		if err := os.WriteFile(filepath.Join(proj, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := t.TempDir()
	code, log := runCleat(t, bin, root, nil, "build", "--target", "csharp", "-o", out, proj)
	if code != 1 {
		t.Fatalf("a project that does not compile: exit %d, want 1 (a finding):\n%s", code, log)
	}
	if !strings.Contains(log, "error") {
		t.Errorf("the compiler's error is not in the output:\n%s", log)
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Errorf("a failed build left %d file(s) in the output directory", len(entries))
	}
}

// TestCSharpBuildRefusesAPackageThatIsNotTheLockedOne is the known-positive for the lock: a copy of the toolchain directory whose
// pinned hash for one package is wrong must make the build UNMEASURED (exit 2), naming the package, and write nothing. The copy
// reuses the populated cache volume, so the restore does not download again and the check is what is measured.
func TestCSharpBuildRefusesAPackageThatIsNotTheLockedOne(t *testing.T) {
	needDocker(t)
	root := repoRoot(t)
	bin := cleatBinary(t, root)
	// Make sure the real lock's cache exists first, so the tampered run below reuses it.
	warm := t.TempDir()
	if code, log := runCleat(t, bin, root, nil, "build", "--target", "csharp", "-o", warm, "tests/csharp/testdata/hello"); code != 0 {
		t.Fatalf("the warm-up build exited %d:\n%s", code, log)
	}
	src := filepath.Join(root, "packages", "cleat-csharp")
	cp := t.TempDir()
	for _, f := range []string{"build.sh", "toolchain.env", "toolchain.packages.tsv", "nuget.config"} {
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			t.Fatal(err)
		}
		if f == "toolchain.packages.tsv" {
			lines := strings.Split(string(b), "\n")
			for i, l := range lines {
				if strings.HasPrefix(l, "microsoft.net.illink.tasks\t") {
					cols := strings.Split(l, "\t")
					cols[2] = "AAAA" + cols[2][4:]
					lines[i] = strings.Join(cols, "\t")
				}
			}
			b = []byte(strings.Join(lines, "\n"))
			if !strings.Contains(string(b), "\tAAAA") {
				t.Fatal("the tampering did not apply: the control would measure nothing")
			}
		}
		if err := os.WriteFile(filepath.Join(cp, f), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The volume the real lock used is named for the lock's hash, which the tampered copy changes; name it explicitly.
	vol := realVolume(t, root)
	out := t.TempDir()
	code, log := runCleat(t, bin, root, []string{"CLEAT_CSHARP_DIR=" + cp, "CLEAT_CS_NUGET_VOLUME=" + vol},
		"build", "--target", "csharp", "-o", out, "tests/csharp/testdata/hello")
	if code != 2 {
		t.Fatalf("a package that is not the pinned one: exit %d, want 2 (UNMEASURED):\n%s", code, log)
	}
	if !strings.Contains(log, "microsoft.net.illink.tasks") || !strings.Contains(log, "does not match its pinned sha512") {
		t.Errorf("the refusal does not name the package and the mismatch:\n%s", log)
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Errorf("a refused build left %d file(s) in the output directory", len(entries))
	}
}

// realVolume names the cache volume build.sh derives for the real lock on this host: the architecture and the first 12 hex digits of
// the sha256 of the three lock files concatenated, the same computation build.sh makes. Computed here independently, and checked against docker so a disagreement fails.
func realVolume(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "packages", "cleat-csharp")
	cmd := exec.Command("bash", "-c", `cat toolchain.env toolchain.packages.tsv nuget.config | shasum -a 256 | cut -c1-12`)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	// build.sh builds for the host's architecture and names the volume for it.
	arch := "amd64"
	if runtime.GOARCH == "arm64" {
		arch = "arm64"
	}
	vol := "cleat-cs-nuget-" + arch + "-" + strings.TrimSpace(string(out))
	if o, err := exec.Command("docker", "volume", "inspect", vol).CombinedOutput(); err != nil {
		t.Fatalf("the cache volume %s that the warm-up build should have created does not exist: %v\n%s", vol, err, o)
	}
	return vol
}

// TestCSharpLockResolvesOnEveryPlatform asks, for each platform the lock lists, whether its image resolves, its packages restore, and
// every restored package (the host pack included) is the pinned one. It runs build.sh in restore-only mode, so nothing compiles: the
// second platform is the emulated one on an arm64 host (and the reverse), which is slow only in the restore. A platform added to
// CLEAT_CS_PLATFORMS with no hash for its host pack fails here, not on the first developer who has that machine.
func TestCSharpLockResolvesOnEveryPlatform(t *testing.T) {
	needDocker(t)
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "packages", "cleat-csharp", "toolchain.env"))
	if err != nil {
		t.Fatal(err)
	}
	var platforms []string
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "CLEAT_CS_PLATFORMS="); ok {
			platforms = strings.Fields(strings.Trim(v, `"`))
		}
	}
	if len(platforms) < 2 {
		t.Fatalf("toolchain.env lists %v; the lock covers more than one platform and this test is the proof for each", platforms)
	}
	for _, plat := range platforms {
		t.Run(strings.ReplaceAll(plat, "/", "_"), func(t *testing.T) {
			cmd := exec.Command("bash", filepath.Join(root, "packages", "cleat-csharp", "build.sh"), root, "tests/csharp/testdata/hello", t.TempDir())
			cmd.Env = append(os.Environ(), "CLEAT_CS_PLATFORM="+plat, "CLEAT_CS_RESTORE_ONLY=1")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s: restore against the lock failed: %v\n%s", plat, err, out)
			}
		})
	}
	// Known-positive: a platform the lock does not list is refused as UNMEASURED, not guessed at.
	cmd := exec.Command("bash", filepath.Join(root, "packages", "cleat-csharp", "build.sh"), root, "tests/csharp/testdata/hello", t.TempDir())
	cmd.Env = append(os.Environ(), "CLEAT_CS_PLATFORM=linux/riscv64", "CLEAT_CS_RESTORE_ONLY=1")
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 || !strings.Contains(string(out), "not one the lock covers") {
		t.Errorf("an unlisted platform: err=%v, want exit 2 naming the lock:\n%s", err, out)
	}
}

// TestCSharpBuildRefusesAToolchainThatLetsThePackageDownloadItsOwnWasiSdk is the known-positive for the wasi-sdk check. The
// componentize package computes ~/.wasi-sdk/wasi-sdk-29.0 itself and downloads a hardcoded, unverified x86_64 tarball when that
// directory is absent. A copy of the toolchain whose verified tree sits anywhere else (here a different version label) reproduces that
// exactly; the build must be UNMEASURED and write nothing. It uses a throwaway wasi-sdk volume and the populated package cache.
func TestCSharpBuildRefusesAToolchainThatLetsThePackageDownloadItsOwnWasiSdk(t *testing.T) {
	needDocker(t)
	root := repoRoot(t)
	bin := cleatBinary(t, root)
	if code, log := runCleat(t, bin, root, nil, "build", "--target", "csharp", "-o", t.TempDir(), "tests/csharp/testdata/hello"); code != 0 {
		t.Fatalf("the warm-up build exited %d:\n%s", code, log)
	}
	src := filepath.Join(root, "packages", "cleat-csharp")
	cp := t.TempDir()
	for _, f := range []string{"build.sh", "toolchain.env", "toolchain.packages.tsv", "nuget.config"} {
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			t.Fatal(err)
		}
		if f == "toolchain.env" {
			if !strings.Contains(string(b), "CLEAT_CS_WASISDK_VERSION=29.0\n") {
				t.Fatal("the version line is not where the control expects: it would measure nothing")
			}
			b = []byte(strings.Replace(string(b), "CLEAT_CS_WASISDK_VERSION=29.0\n", "CLEAT_CS_WASISDK_VERSION=29.9\n", 1))
		}
		if err := os.WriteFile(filepath.Join(cp, f), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	scratch := "cleat-cs-wasisdk-test-" + filepath.Base(t.TempDir())
	t.Cleanup(func() { exec.Command("docker", "volume", "rm", "-f", scratch).Run() })
	out := t.TempDir()
	code, log := runCleat(t, bin, root, []string{"CLEAT_CSHARP_DIR=" + cp, "CLEAT_CS_NUGET_VOLUME=" + realVolume(t, root), "CLEAT_CS_WASISDK_VOLUME=" + scratch},
		"build", "--target", "csharp", "-o", out, "tests/csharp/testdata/hello")
	if code != 2 || !strings.Contains(log, "downloaded its own wasi-sdk") {
		t.Fatalf("exit %d, want 2 naming the package's own download:\n%s", code, log)
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Errorf("a refused build left %d file(s) in the output directory", len(entries))
	}
}
