#!/usr/bin/env bash
# Runs the managed unit tests in the digest-pinned linux/amd64 SDK container, the same toolchain pin the build
# uses (coordinator decision, cleat#4080), never on a natively installed `dotnet`.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
pkg="$(cd "$here/.." && pwd)"
# shellcheck source=/dev/null
. "$pkg/toolchain.env"
docker run --rm --platform linux/amd64 \
  -v "${CLEAT_CS_NUGET_VOLUME:-cleat-cs-nuget}":/root/.nuget \
  -v "$pkg":/work -w /work/tests/Cleat.Tests \
  "$CLEAT_CS_SDK_IMAGE" dotnet test -c Release "$@"
