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
#
# ---------------------------------------------------------------------------
# If this machine has NO desktop-linux context -- colima only
# ---------------------------------------------------------------------------
#
# The invocation above then cannot work at all, and the advice "use
# desktop-linux" has nowhere to go. Verified 2026-09-04 on the WS-3 checkout,
# where `docker context ls` offers only colima profiles. All six Python
# component tests pass this way; the recipe is three changes, none obvious:
#
#   1. STAGE THE TREE UNDER $HOME. colima mounts $HOME and does not mount
#      /Users/Shared/... or /tmp/colima. This is the empty-mount trap above,
#      so confirm the mount before trusting a result:
#
#        git archive <ref> | tar -x -C ~/cleat-run
#        docker --context colima run --rm -v ~/cleat-run:/src alpine ls /src
#
#      Mount ~/go/pkg/mod as /go/pkg/mod too, or the module download dominates.
#
#   2. --network host FOR THE DB-BACKED TESTS. TestPythonCronEndToEnd needs
#      Postgres, and the databases run INSIDE the colima VM -- so
#      host.docker.internal:5434 is NOT reachable while localhost:5434 is,
#      once the container shares the VM's network. Pass the DSN BY NAME so it
#      is inherited rather than written into argv:
#
#        docker --context colima run --rm --network host \
#          -v ~/cleat-run:/src -v ~/go/pkg/mod:/go/pkg/mod -w /src \
#          -e CGO_ENABLED=1 -e CLEAT_TEST_POSTGRES \
#          <tag> go test ./engine/ -run 'Python|Component' -count=1
#
#   3. REBUILD BEFORE TRUSTING AN EXISTING IMAGE, and this is the one that
#      costs a whole session. A `cleat-py-toolchain` built before the
#      wasm-tools layer above has componentize-py but no wasm-tools, and
#      toolsAvailable() gates on BOTH -- so every Python component test SKIPS
#      and the suite prints `ok`. A green run over code nothing executed is
#      exactly what CLAUDE.md's "Is this result real?" section is about.
#      Measured 2026-09-04: a stale image gave 6 skips, a rebuilt one gave
#      23 pass / 0 skip / 0 fail. There is no COPY or ADD in this file, so the
#      build context is unused and an empty directory builds it:
#
#        docker --context colima build -f scripts/docker/python-toolchain.Dockerfile \
#          -t cleat-py-toolchain $(mktemp -d)
#
# Why this is worth the lines: these are the only tests that exercise
# componentCallRun and the Component Model callback path (engine/component_cgo.go).
# Editing that file with them skipping means editing code nothing local covers.
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
