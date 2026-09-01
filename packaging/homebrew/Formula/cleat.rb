# Homebrew formula for cleat.
#
# WHY THIS IS A SOURCE BUILD, AND NOT A BOTTLE OF THE RELEASE ARCHIVES
#
# cleat-worker requires CGO: wasmtime is the only WASM backend cleat has, and
# under CGO_ENABLED=0 engine/backend_wasmtime.go (`//go:build cgo`) is compiled
# out, NewWasmtimeBackend returns ErrWasmtimeCGOUnavailable, and the worker
# exits 1 during startup before it reads a flag.
#
# .goreleaser.yml therefore builds cleat-worker for **linux only** -- a CGO
# darwin binary cannot be linked on the ubuntu runner the release job uses
# without osxcross, and all of .github/workflows runs ubuntu. See
# IMPROVEMENT-PLAN.md 3.54.
#
# So there is no macOS cleat-worker in the release archives for a formula to
# repackage. Building from source is what closes that gap: it moves the CGO
# link to the install machine, which is the one place it is free -- Homebrew
# already requires the Xcode Command Line Tools, so a C toolchain is
# guaranteed to be present.
#
# `cleat` and `cleat-gen` link neither runtime and are built CGO-free here, the
# same way the release builds them, so that what a Homebrew user gets matches
# what a tarball user gets.
#
# VERIFICATION
#
# Measured 2026-09-01 on darwin/arm64, against this exact recipe run over the
# published v0.2.0 source tarball:
#
#   CGO_ENABLED=0 go build -trimpath -o out/cleat     ./cmd/cleat        -> OK
#   CGO_ENABLED=0 go build -trimpath -o out/cleat-gen ./cmd/cleat-gen    -> OK
#   CGO_ENABLED=1 go build -trimpath -o out/cleat-worker ./cmd/cleat-worker -> OK
#   file out/cleat-worker      -> Mach-O 64-bit executable arm64
#   ./out/cleat-worker --verify-backend
#                              -> verify-backend: OK: wasmtime backend available
#
# RELEASING A NEW VERSION
#
# `url`, `sha256` and `version` are hand-maintained; goreleaser's `brews:`
# generator cannot be used here because it packages built binaries, which is
# precisely what does not exist for macOS. On each release:
#
#   curl -sSLO https://github.com/cleat-team/cleat/archive/refs/tags/vX.Y.Z.tar.gz
#   shasum -a 256 vX.Y.Z.tar.gz
#
# and update all three. test/homebrew_formula_test.go checks that the version
# in `url` and the `version` field agree, so a half-done bump fails CI rather
# than shipping a formula that installs the wrong tag.
class Cleat < Formula
  desc "Durable workflow engine that runs workflows compiled to WebAssembly"
  homepage "https://github.com/cleat-team/cleat"
  url "https://github.com/cleat-team/cleat/archive/refs/tags/v0.2.0.tar.gz"
  sha256 "40fc912649623cafc3ce080ac11a36c5879215951968fa4397452c10cfbfb5be"
  license "Apache-2.0"
  head "https://github.com/cleat-team/cleat.git", branch: "develop"

  depends_on "go" => :build

  def install
    # cleat-worker: CGO on, deliberately. Without it the installed binary
    # cannot construct the wasmtime backend and exits 1 at startup. The test
    # block below is what stops that shipping silently.
    with_env(CGO_ENABLED: "1") do
      system "go", "build", *std_go_args(output: bin/"cleat-worker"), "./cmd/cleat-worker"
    end

    # cleat and cleat-gen link neither WASM runtime; built CGO-free to match
    # how .goreleaser.yml ships them.
    with_env(CGO_ENABLED: "0") do
      system "go", "build", *std_go_args(output: bin/"cleat"), "./cmd/cleat"
      system "go", "build", *std_go_args(output: bin/"cleat-gen"), "./cmd/cleat-gen"
    end
  end

  test do
    # The load-bearing assertion. `--verify-backend` constructs the wasmtime
    # backend for real and exits non-zero if it cannot, so this fails exactly
    # when the formula has produced the dead-on-arrival worker that
    # IMPROVEMENT-PLAN.md 3.54 is about.
    #
    # Asserting on the output as well as the exit code: a worker built without
    # CGO prints "verify-backend: FAIL" and exits 1, and this must not pass by
    # matching some other command's success.
    assert_match "verify-backend: OK", shell_output("#{bin}/cleat-worker --verify-backend")

    # `version` is a subcommand, not a flag -- cmd/cleat/main.go:88. Go's flag
    # package rejects an unregistered --version before args are parsed.
    assert_match "cleat", shell_output("#{bin}/cleat version")

    # cleat-gen has no --help and no zero-exit invocation: with no arguments it
    # prints usage and exits 1. `system bin/"cleat-gen", "--help"` therefore
    # fails the whole test block, which is how the first draft of this formula
    # broke. Assert the usage text and the exit status it really returns.
    assert_match "Usage: cleat-gen", shell_output("#{bin}/cleat-gen 2>&1", 1)
  end
end
