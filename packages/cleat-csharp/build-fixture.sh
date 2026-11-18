#!/usr/bin/env bash
# Builds a fixture project to a real component, in the digest-pinned linux/amd64 SDK container (the compiler only
# runs on x64 Linux or Windows). Usage: build-fixture.sh [fixture-dir] ; writes out/<name>.wasm, where <name> is
# $CLEAT_CS_OUT_NAME or the directory's name. Extra `dotnet build` arguments go in $CLEAT_CS_BUILD_ARGS
# (for example -p:DefineConstants=MANY).
#
# wasi-sdk 29 is downloaded by the componentize package at build time, every build, unless ~/.wasi-sdk persists:
# the wasi-sdk volume keeps it. PR 2 of cleat#4080 pins it by hash; this script is the dev loop, not the lock.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
# shellcheck source=/dev/null
. "$here/toolchain.env"
fixture="${1:-fixture}"
rel="packages/cleat-csharp/$fixture"
mkdir -p "$here/out"
docker run --rm --platform linux/amd64 \
  -v "${CLEAT_CS_NUGET_VOLUME:-cleat-cs-nuget}":/root/.nuget \
  -v "${CLEAT_CS_WASISDK_VOLUME:-cleat-cs-wasisdk}":/root/.wasi-sdk \
  -v "$repo":/repo -w "/repo/$rel" \
  -e CLEAT_CS_BUILD_ARGS="${CLEAT_CS_BUILD_ARGS:-}" \
  "$CLEAT_CS_SDK_IMAGE" bash -c '
    set -euo pipefail
    if ! ls -d /root/.wasi-sdk/wasi-sdk-29.0* >/dev/null 2>&1; then
      mkdir -p /root/.wasi-sdk && cd /root/.wasi-sdk
      curl -sSL -o w.tgz https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-29/wasi-sdk-29.0-x86_64-linux.tar.gz
      echo "87d1d1a2879d139cdc624b968efad3d4a97b8078cdff95e63ac88ecafd1a0171  w.tgz" | sha256sum -c -
      tar xzf w.tgz && rm w.tgz && cd -
    fi
    export WASI_SDK_PATH="$(ls -d /root/.wasi-sdk/wasi-sdk-29.0* | head -1)"
    dotnet build -c Release $CLEAT_CS_BUILD_ARGS 2>&1 | grep -v -E "^\s*$" | grep -v "Generating \"" | tail -40
  '
name="${CLEAT_CS_OUT_NAME:-$(basename "$fixture")}"
cp "$here/$fixture/bin/Release/net10.0/wasi-wasm/publish/"*.wasm "$here/out/$name.wasm"
ls -l "$here/out/$name.wasm"
