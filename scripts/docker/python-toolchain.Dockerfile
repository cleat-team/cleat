# A Linux toolchain for building Python workflow components.
#
# Why this exists: componentize-py cannot run on the macOS dev machines. Its embedded
# wasmtime installs a mach exception handler into a guarded port and the process dies
# with EXC_GUARD / GUARD_TYPE_MACH_PORT. That guard is a Darwin kernel feature with no
# Linux equivalent, which is why the Linux CI runners have always been able to build
# Python components while a developer's Mac could not.
#
# The consequence was that four Python tests skipped locally forever, and tier 1's
# contract is that a skip is a failure. This image removes the asymmetry: the same
# toolchain runs on a Mac (inside a Linux VM), on a Linux workstation, and in CI.
#
# The output of a componentize run is a .wasm file, which is architecture-independent,
# so an arm64 container producing a component for an amd64 runner is fine.
#
# Build:  docker build -f scripts/docker/python-toolchain.Dockerfile -t cleat-py-toolchain .
# Use:    docker --context desktop-linux run --rm -v "$PWD":/src -w /src -e CGO_ENABLED=1 \
#           cleat-py-toolchain go test ./engine/ -run 'TestPython'
#
# --context desktop-linux is not optional on a Mac that also has colima, and
# getting it wrong does not look like a mount problem. Colima cannot bind-mount
# these paths and says nothing: -v "$PWD":/src produces an *empty* directory, so
# the run fails with
#
#   go: go.mod file not found in current directory or any parent directory
#
# which reads as a broken checkout. Mounting the repo root under colima is worse
# -- it succeeds and serves a different tree entirely. Verified 2026-08-06:
#
#   docker run --rm -v "$PWD":/src alpine ls /src               # (nothing)
#   docker --context desktop-linux run --rm -v "$PWD":/src alpine ls /src
#     ABI.md  ARCHITECTURE.md  BRANCH-TRIAGE.md  CHANGELOG.md  CLAUDE.md ...
#
# CGO_ENABLED=1 for the same reason it is pinned everywhere else: without it
# NewWasmtimeBackend is compiled out and the Python tests would run on wazero,
# which is not what they are there to check. With it the run reports
#
#   wasmtime registered for [go assemblyscript java rust python]
FROM golang:1.26-bookworm

RUN apt-get update && apt-get install -y --no-install-recommends \
      python3 python3-pip python3-venv ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

# wasm-tools is a SECOND prerequisite, and a separate one. The engine's Python tests
# (component_fence_test.go, python_wasm_e2e_test.go) check for it independently of
# componentize-py and skip on "missing: wasm-tools" -- so installing componentize-py
# alone still leaves them skipping, just with a different message. It ships as a
# prebuilt binary; `cargo install wasm-tools` (what the skip message suggests) would
# drag a whole Rust toolchain into this image for one executable.
ARG WASM_TOOLS_VERSION=1.255.0
RUN set -eux; \
    case "$(uname -m)" in \
      aarch64) arch=aarch64 ;; \
      x86_64)  arch=x86_64 ;; \
      *) echo "unsupported arch $(uname -m)" >&2; exit 1 ;; \
    esac; \
    curl -sSLf "https://github.com/bytecodealliance/wasm-tools/releases/download/v${WASM_TOOLS_VERSION}/wasm-tools-${WASM_TOOLS_VERSION}-${arch}-linux.tar.gz" \
      | tar xz -C /tmp; \
    mv /tmp/wasm-tools-*/wasm-tools /usr/local/bin/wasm-tools; \
    rm -rf /tmp/wasm-tools-*; \
    wasm-tools --version

# A venv, because bookworm's python is externally-managed (PEP 668) and pip refuses
# to install into it without --break-system-packages.
ENV VIRTUAL_ENV=/opt/venv
RUN python3 -m venv "$VIRTUAL_ENV"
ENV PATH="$VIRTUAL_ENV/bin:$PATH"

RUN pip install --no-cache-dir --upgrade pip \
    && pip install --no-cache-dir componentize-py

# Fail the build rather than the test run if the toolchain is not actually usable.
RUN componentize-py --version

WORKDIR /src
