package csharp

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

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

// execFormatError reports whether docker's output says the host cannot execute the container's architecture: no binfmt/QEMU handler
// registered for it. It is the one failure that is a fact about the RUNNER and not about the lock, so it is the only one a platform leg
// may skip on. The wording is the kernel's (ENOEXEC), which docker passes through verbatim.
func execFormatError(out string) bool {
	return strings.Contains(out, "exec format error")
}

// needPlatform skips a platform leg when this host cannot execute that platform's image, and FAILS when anything else is wrong or when
// the job declared csharp-<arch> in CLEAT_REQUIRE_TOOLCHAINS (a job that promises the emulator cannot silently skip the leg).
// Probing with `true` keeps "cannot run this architecture" apart from "the lock does not resolve": build.sh reports both as exit 2
// (UNMEASURED), and a test that counted the first as a finding about the lock would be red on every runner without emulation.
func needPlatform(t *testing.T, root, plat string) {
	t.Helper()
	image := ""
	b, err := os.ReadFile(filepath.Join(root, "packages", "cleat-csharp", "toolchain.env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "CLEAT_CS_SDK_IMAGE="); ok {
			image = strings.TrimSpace(v)
		}
	}
	if image == "" {
		t.Fatal("toolchain.env defines no CLEAT_CS_SDK_IMAGE")
	}
	out, err := exec.Command("docker", "run", "--rm", "--platform", plat, "--entrypoint", "true", image).CombinedOutput()
	if err == nil {
		return
	}
	arch := plat[strings.LastIndex(plat, "/")+1:]
	if !execFormatError(string(out)) {
		t.Fatalf("%s: could not even start the SDK image to probe it (%v), which is not a missing emulator:\n%s", plat, err, out)
	}
	if toolchainRequired("csharp-" + arch) {
		t.Fatalf("%s cannot be executed on this host (exec format error), but %s declares csharp-%s, so this job promises the emulator: the setup step is broken, not the test\n%s", plat, requireToolchainEnv, arch, out)
	}
	t.Skipf("%s cannot be executed on this host (exec format error: no binfmt/QEMU handler); the lock row for it is NOT verified here. A job that provides the emulator declares csharp-%s in %s", plat, arch, requireToolchainEnv)
}

// TestExecFormatErrorIsRecognisedAndNothingElseIs is the known-positive and the negative control for the one string the platform skip
// hangs on. The positive is the line a CI runner printed on #4096 (x86_64, no binfmt, linux/arm64 image); the negatives are failures
// that must NOT be skipped, because each says something about the lock or the setup.
func TestExecFormatErrorIsRecognisedAndNothingElseIs(t *testing.T) {
	if !execFormatError("exec /usr/bin/bash: exec format error") {
		t.Error("the runner's actual message is not recognised, so a runner without binfmt would FAIL the arm64 leg")
	}
	for _, other := range []string{
		"",
		"Unable to find image 'x' locally\ndocker: Error response from daemon: no matching manifest for linux/riscv64",
		"UNMEASURED: the restored packages are not the pinned ones",
		"Cannot connect to the Docker daemon",
	} {
		if execFormatError(other) {
			t.Errorf("%q is read as a missing emulator; a leg would be skipped over a real failure", other)
		}
	}
}

// toolchainFiles are the files of packages/cleat-csharp that a COPY of the toolchain needs to be runnable: the tests that tamper with
// one lock file copy the directory (CLEAT_CSHARP_DIR) and must carry the rest unchanged. One list, because a file added to the build
// (resolved-packages.pl, cleat#4099) that a copy forgets makes build.sh fail for a reason that has nothing to do with what the test
// is measuring.
var toolchainFiles = []string{"build.sh", "resolved-packages.pl", "toolchain.env", "toolchain.packages.tsv", "nuget.config"}

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
	for _, f := range toolchainFiles {
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
			needPlatform(t, root, plat)
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
	for _, f := range toolchainFiles {
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

// throwawayNugetVolume returns a package-cache volume name no other build uses, and removes it when the test ends. Only the volume this
// test names is ever removed.
func throwawayNugetVolume(t *testing.T) string {
	t.Helper()
	vol := fmt.Sprintf("cleat-cs-test-nuget-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "-f", vol).Run() })
	return vol
}

// TestCSharpAnUnpinnedPackageFromOneProjectDoesNotFailAnother is the regression for cleat#4099, and the known-positive for the pin
// check beside it. The cache volume is shared by every build with the same lock. build.sh used to check every package in the cache
// directory, so one project whose dependency was not in the lock left that package behind (the restore runs before the check) and
// every LATER build with that lock failed "unpinned package", including projects that needed none of it, until someone removed the
// volume.
//
// Three facts, in order, each of which has to hold for the last to mean anything:
//  1. a project with an unpinned dependency is still refused (exit 2) and the package is named: the check has not been weakened;
//  2. the unpinned package really IS in the cache afterwards: the premise, so the next step is not passing because nothing was left;
//  3. an unrelated project, restored from the same cache volume, is accepted (exit 0). Before the fix it was refused.
func TestCSharpAnUnpinnedPackageFromOneProjectDoesNotFailAnother(t *testing.T) {
	needDocker(t)
	root := repoRoot(t)
	vol := throwawayNugetVolume(t)
	build := func(project string) (int, string) {
		cmd := exec.Command("bash", filepath.Join(root, "packages", "cleat-csharp", "build.sh"), root, project, t.TempDir())
		cmd.Env = append(os.Environ(), "CLEAT_CS_NUGET_VOLUME="+vol, "CLEAT_CS_RESTORE_ONLY=1")
		out, err := cmd.CombinedOutput()
		if err == nil {
			return 0, string(out)
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), string(out)
		}
		t.Fatalf("could not run build.sh: %v", err)
		return -1, ""
	}

	code, log := build("tests/csharp/testdata/unpinned")
	// The wording is not asserted beyond "unpinned package" and the name: an earlier build.sh said "in the cache" and a test pinned to the
	// new sentence would fail HERE for the old code, never reaching step 3, which is the step that tells the two apart.
	if code != 2 || !strings.Contains(log, "unpinned package") || !strings.Contains(log, "newtonsoft.json") {
		t.Fatalf("1. a project with an unpinned dependency: exit %d, want 2 naming newtonsoft.json:\n%s", code, log)
	}

	image := ""
	b, err := os.ReadFile(filepath.Join(root, "packages", "cleat-csharp", "toolchain.env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "CLEAT_CS_SDK_IMAGE="); ok {
			image = strings.TrimSpace(v)
		}
	}
	ls, err := exec.Command("docker", "run", "--rm", "-v", vol+":/n:ro", "--entrypoint", "ls", image, "/n/packages").CombinedOutput()
	if err != nil || !strings.Contains(string(ls), "newtonsoft.json") {
		t.Fatalf("2. the unpinned package is not in the cache after the refusal, so step 3 would pass for the wrong reason (%v):\n%s", err, ls)
	}

	code, log = build("tests/csharp/testdata/hello")
	if code != 0 {
		t.Fatalf("3. an unrelated project, from the cache the first one left newtonsoft.json in: exit %d, want 0 (cleat#4099):\n%s", code, log)
	}
}

// TestCSharpResolvedPackagesReader runs the two readers inside the SDK image (where build.sh runs them: perl and core JSON::PP are
// there, jq and python are not) against small fixtures. The failure cases matter as much as the success: a reader that printed nothing
// and exited 0 for a file it could not understand would make the pin check examine nothing and pass.
func TestCSharpResolvedPackagesReader(t *testing.T) {
	needDocker(t)
	root := repoRoot(t)
	image := ""
	b, err := os.ReadFile(filepath.Join(root, "packages", "cleat-csharp", "toolchain.env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "CLEAT_CS_SDK_IMAGE="); ok {
			image = strings.TrimSpace(v)
		}
	}
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("ok.json", `{"version":3,"libraries":{
	  "Foo.Bar/1.2.3":{"type":"package","path":"foo.bar/1.2.3","sha512":"x"},
	  "MyProj/1.0.0":{"type":"project","path":"../MyProj/MyProj.csproj"},
	  "Baz/2.0.0-rc.1":{"type":"package","path":"baz/2.0.0-rc.1"}},
	 "downloadDependencies":[{"name":"Some.Pack","version":"[10.0.1, 10.0.1]"}]}`)
	write("projectonly.json", `{"libraries":{"MyProj/1.0.0":{"type":"project","path":"../MyProj/MyProj.csproj"}}}`)
	write("nolibraries.json", `{"version":3}`)
	write("notjson.json", `this is not json`)
	// What `dotnet msbuild -getProperty:ProjectAssetsFile -getItem:ProjectReference` prints, shortened.
	write("graph-two.json", `{"Properties":{"ProjectAssetsFile":"/mnt/a/obj/project.assets.json"},"Items":{"ProjectReference":[
	  {"Identity":"../g/G.csproj","FullPath":"/mnt/g/G.csproj","ReferenceOutputAssembly":"false"},
	  {"Identity":"../h/H.csproj","FullPath":"/mnt/h/H.csproj"},
	  {"Identity":"../g/G.csproj","FullPath":"/mnt/g/G.csproj"}]}}`)
	write("graph-none.json", `{"Properties":{"ProjectAssetsFile":"/mnt/a/obj/project.assets.json"},"Items":{"ProjectReference":[]}}`)
	write("graph-noassets.json", `{"Properties":{},"Items":{"ProjectReference":[]}}`)
	write("graph-emptyassets.json", `{"Properties":{"ProjectAssetsFile":""},"Items":{"ProjectReference":[]}}`)
	write("graph-nofullpath.json", `{"Properties":{"ProjectAssetsFile":"/x"},"Items":{"ProjectReference":[{"Identity":"../g/G.csproj"}]}}`)
	run := func(mode, file string) (int, string) {
		cmd := exec.Command("docker", "run", "--rm", "-v", filepath.Join(root, "packages", "cleat-csharp")+":/pkg:ro", "-v", dir+":/f:ro",
			"--entrypoint", "perl", image, "/pkg/resolved-packages.pl", mode, "/f/"+file)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return 0, string(out)
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), string(out)
		}
		t.Fatalf("could not run docker: %v", err)
		return -1, ""
	}
	code, out := run("packages", "ok.json")
	want := "baz\t2.0.0-rc.1\tbaz/2.0.0-rc.1\nfoo.bar\t1.2.3\tfoo.bar/1.2.3\nsome.pack\t10.0.1\tsome.pack/10.0.1\n"
	if code != 0 || out != want {
		t.Errorf("packages, a normal assets file: exit %d, got %q, want %q (packages and downloadDependencies, the project library skipped, ids lower-cased)", code, out, want)
	}
	if code, out := run("packages", "projectonly.json"); code != 0 || out != "" {
		t.Errorf("packages, a file with no package: exit %d, output %q; it must print nothing and exit 0 so build.sh's own guard names the empty list", code, out)
	}
	for _, bad := range []string{"nolibraries.json", "notjson.json", "missing.json"} {
		if code, out := run("packages", bad); code != 2 {
			t.Errorf("packages %s: exit %d (output %q), want 2: a file it cannot read must never read as 'nothing resolved'", bad, code, out)
		}
	}
	code, out = run("graph", "graph-two.json")
	wantGraph := "ASSETS\t/mnt/a/obj/project.assets.json\nREF\t/mnt/g/G.csproj\nREF\t/mnt/h/H.csproj\n"
	if code != 0 || out != wantGraph {
		t.Errorf("graph, two references (one listed twice): exit %d, got %q, want %q", code, out, wantGraph)
	}
	if code, out := run("graph", "graph-none.json"); code != 0 || out != "ASSETS\t/mnt/a/obj/project.assets.json\n" {
		t.Errorf("graph, no references: exit %d, got %q", code, out)
	}
	for _, bad := range []string{"graph-noassets.json", "graph-emptyassets.json", "graph-nofullpath.json", "notjson.json", "missing.json"} {
		if code, out := run("graph", bad); code != 2 {
			t.Errorf("graph %s: exit %d (output %q), want 2: a project it cannot name the files of must never be skipped", bad, code, out)
		}
	}
	if code, _ := run("bogus", "ok.json"); code != 2 {
		t.Errorf("an unknown mode must be a usage error (exit 2), got %d", code)
	}
}

// TestCSharpBuildChecksThePackagesOfAReferencedProject is the regression for cleat-review's finding on #4104. Every SDK project
// references its source generator as OutputItemType="Analyzer" ReferenceOutputAssembly="false", and a reference of that shape does not
// put the generator's packages in the referencing project's assets file, so a pin check that read only the top project compiled the
// generator against packages nothing had checked. The fixture (testdata/withref -> testdata/refgen) has the same shape and a package
// the lock does not pin in the REFERENCED project only.
//
// Four steps, because "refused" alone cannot tell "the referenced project is examined" from "something else refuses":
//  1. the unpinned package of the referenced project is refused (exit 2) and named, with the project it came from;
//  2. the same build with those packages pinned at their REAL hashes is accepted: they are examined and pass;
//  3. the same pins with one hash corrupted are refused as "does not match its pinned sha512": the HASH of a referenced project's
//     package is checked too, which is the case cleat-review measured (a corrupted pin for a generator package built, exit 0);
//  4. nothing in the referenced project, nothing unexamined: an unreferenced-project build (hello) is still accepted from this volume.
func TestCSharpBuildChecksThePackagesOfAReferencedProject(t *testing.T) {
	needDocker(t)
	root := repoRoot(t)
	vol := throwawayNugetVolume(t)
	image := ""
	b, err := os.ReadFile(filepath.Join(root, "packages", "cleat-csharp", "toolchain.env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "CLEAT_CS_SDK_IMAGE="); ok {
			image = strings.TrimSpace(v)
		}
	}
	runBuild := func(script, project string, env ...string) (int, string) {
		cmd := exec.Command("bash", script, root, project, t.TempDir())
		cmd.Env = append(os.Environ(), append([]string{"CLEAT_CS_NUGET_VOLUME=" + vol, "CLEAT_CS_RESTORE_ONLY=1"}, env...)...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return 0, string(out)
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), string(out)
		}
		t.Fatalf("could not run build.sh: %v", err)
		return -1, ""
	}
	real := filepath.Join(root, "packages", "cleat-csharp", "build.sh")

	code, log := runBuild(real, "tests/csharp/testdata/withref")
	if code != 2 || !strings.Contains(log, "unpinned package") || !strings.Contains(log, "newtonsoft.json") || !strings.Contains(log, "Refgen.csproj") {
		t.Fatalf("1. a package that only the REFERENCED project uses: exit %d, want 2 naming newtonsoft.json and Refgen.csproj:\n%s", code, log)
	}

	// The packages the build said were unpinned, with the hashes NuGet recorded for them in the cache.
	re := regexp.MustCompile(`unpinned package used by the build: (\S+) (\S+) \(`)
	var rows []string
	for _, m := range re.FindAllStringSubmatch(log, -1) {
		id, ver := m[1], m[2]
		sum, err := exec.Command("docker", "run", "--rm", "-v", vol+":/n:ro", "--entrypoint", "cat", image,
			"/n/packages/"+id+"/"+ver+"/"+id+"."+ver+".nupkg.sha512").Output()
		if err != nil || len(strings.TrimSpace(string(sum))) < 20 {
			t.Fatalf("no recorded hash for %s %s in the cache (%v): the pinned run below would pin nothing", id, ver, err)
		}
		rows = append(rows, id+"\t"+ver+"\t"+strings.TrimSpace(string(sum)))
	}
	if len(rows) == 0 {
		t.Fatal("step 1 named no package, so there is nothing to pin")
	}

	// A copy of the toolchain whose lock pins those rows. Its lock hash differs, so name the (shared, verified) wasi-sdk volume of the real
	// lock explicitly instead of downloading it again; the nuget volume is already this test's own.
	copyWith := func(rows []string) string {
		cp := t.TempDir()
		for _, f := range toolchainFiles {
			body, err := os.ReadFile(filepath.Join(root, "packages", "cleat-csharp", f))
			if err != nil {
				t.Fatal(err)
			}
			if f == "toolchain.packages.tsv" {
				body = append(bytes.TrimRight(body, "\n"), []byte("\n"+strings.Join(rows, "\n")+"\n")...)
			}
			if err := os.WriteFile(filepath.Join(cp, f), body, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return filepath.Join(cp, "build.sh")
	}
	wasi := strings.Replace(realVolume(t, root), "cleat-cs-nuget-", "cleat-cs-wasisdk-", 1)
	code, log = runBuild(copyWith(rows), "tests/csharp/testdata/withref", "CLEAT_CS_WASISDK_VOLUME="+wasi)
	if code != 0 {
		t.Fatalf("2. the referenced project's packages pinned at their real hashes: exit %d, want 0 (they are examined and pass):\n%s", code, log)
	}

	bad := append([]string(nil), rows...)
	for i, r := range bad {
		if strings.HasPrefix(r, "newtonsoft.json\t") {
			cols := strings.Split(r, "\t")
			cols[2] = "AAAA" + cols[2][4:]
			bad[i] = strings.Join(cols, "\t")
		}
	}
	code, log = runBuild(copyWith(bad), "tests/csharp/testdata/withref", "CLEAT_CS_WASISDK_VOLUME="+wasi)
	if code != 2 || !strings.Contains(log, "newtonsoft.json") || !strings.Contains(log, "does not match its pinned sha512") {
		t.Fatalf("3. a corrupted pin for a REFERENCED project's package: exit %d, want 2 'does not match its pinned sha512' (cleat-review measured exit 0):\n%s", code, log)
	}

	code, log = runBuild(real, "tests/csharp/testdata/hello")
	if code != 0 {
		t.Fatalf("4. a project with no references, from the cache the referenced-project build left packages in: exit %d, want 0:\n%s", code, log)
	}
}
