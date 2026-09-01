#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# verify-release-worker.sh — make a cleat-worker artifact prove it can start
#
# Called as a goreleaser post-build hook, once per cleat-worker binary, before
# the publish stage. A non-zero exit aborts the release.
#
# Usage:
#   ./scripts/verify-release-worker.sh <path-to-binary> <goarch>
#
# Why this exists
# ---------------
# .goreleaser.yml built cleat-worker with CGO_ENABLED=0. That compiles out
# engine/backend_wasmtime.go (`//go:build cgo`), leaving the stub that returns
# ErrWasmtimeCGOUnavailable, and cmd/cleat-worker/main.go:789 turns that into
# os.Exit(1). Every cleat-worker attached to a release exited 1 before reading a
# flag.
#
# It survived because nothing ever ran the artifact. `CGO_ENABLED=0 go build
# ./...` exits 0 — the failure is at startup, not at build time — so a green
# release pipeline meant nothing about whether the binary worked. The fix for a
# check that measured nothing is a check that executes the real thing.
#
# Negative control (CLAUDE.md, "a verification script needs its own negative
# control"). Measured 2026-09-01 on darwin/arm64, against this script:
#
#   CGO_ENABLED=1 go build -o /tmp/w ./cmd/cleat-worker
#   ./scripts/verify-release-worker.sh /tmp/w arm64     -> exit 0
#
#   CGO_ENABLED=0 go build -o /tmp/w ./cmd/cleat-worker
#   ./scripts/verify-release-worker.sh /tmp/w arm64     -> exit 1
#                                                         "wasmtime backend
#                                                          requires CGO"
#
# Re-derive with those four commands. A version of this script that cannot
# produce the second result is not guarding anything.
# ---------------------------------------------------------------------------
set -euo pipefail

if [ "$#" -ne 2 ]; then
	echo "usage: $0 <path-to-cleat-worker> <goarch>" >&2
	exit 2
fi

bin=$1
arch=$2

if [ ! -x "$bin" ]; then
	echo "verify-release-worker: $bin is not an executable file" >&2
	exit 1
fi

echo "verify-release-worker: executing $bin ($arch) with --verify-backend"

# No skip branch, deliberately.
#
# The tempting shape here is "if we cannot execute this architecture, skip" —
# which would pass the arm64 binary through unexamined on an amd64 runner and
# report success, reproducing the exact failure mode this script was written
# for. CLAUDE.md: a skip that hides a crash is not a skip.
#
# Cross-architecture execution is therefore a hard requirement. The Release
# workflow registers binfmt_misc handlers via docker/setup-qemu-action before
# goreleaser runs, so the arm64 binary executes under qemu-user. If that setup
# is missing the exec fails with "Exec format error" and the release stops,
# which is the correct outcome: we cannot vouch for a binary we did not run.
#
# Capture the status with `|| status=$?` rather than `if ! "$bin" ...`. Inside
# an `if !` block, `$?` is the status of the negation — 0 — so the natural
# looking form would have exited 0 on failure and published the binary anyway.
status=0
"$bin" --verify-backend || status=$?

if [ "$status" -ne 0 ]; then
	echo >&2
	echo "verify-release-worker: FAILED for $arch." >&2
	echo >&2
	echo "This binary cannot construct the wasmtime backend, so it would exit 1" >&2
	echo "at startup. Refusing to publish it." >&2
	echo >&2
	echo "Most likely causes:" >&2
	echo "  * the cleat-worker build in .goreleaser.yml lost CGO_ENABLED=1" >&2
	echo "  * the CC template no longer matches the installed cross compiler" >&2
	echo "    (arm64 needs aarch64-linux-gnu-gcc)" >&2
	echo "  * binfmt/qemu is not registered on the runner, so an 'Exec format" >&2
	echo "    error' is being reported here rather than a backend failure" >&2
	exit "$status"
fi

echo "verify-release-worker: OK ($arch)"
