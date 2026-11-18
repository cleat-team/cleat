#!/usr/bin/env bash
# Builds a C# workflow project to a Component Model component, in the digest-pinned SDK container for the host's platform (cleat#4080, C# PR 2).
# This is what `cleat build --target csharp` runs; it stamps nothing (the stamp is the CLI's, in Go, so it is one mandatory step in
# one place).
#
#   build.sh <mount-root> <project-dir> <out-dir>
#
#   mount-root   a directory that holds the project AND whatever it references by relative path (the WIT file, the SDK sources).
#                Mounted read-write: the project's obj/ and bin/ are written there, and chowned back to the caller afterwards.
#                NOTE THE REACH OF THAT: the container runs as root with the WHOLE mount root writable, and the mount root is the
#                enclosing git checkout, not just the project. A project's build (MSBuild targets, source generators) can therefore
#                modify any file in the checkout. This build verifies the TOOLCHAIN (image, wasi-sdk, packages); it does not sandbox
#                the PROJECT, so build only a project you would be willing to run.
#   project-dir  the project's directory, relative to mount-root (a .csproj must be the only one in it).
#   out-dir      where to write <name>.wasm and entry-points.txt. Created if absent.
#
# Exit statuses (CLAUDE.md, "give 'I could not look' its own exit status"), the same three the TypeScript build keeps:
#   0  built
#   1  a finding: the project was refused (it does not compile, defines no CleatEntryPointsFile, or has no single .wasm output)
#   2  UNMEASURED: the environment or the pins failed, so nothing was learned about the project (no docker, the image or a package
#      could not be fetched, a hash did not match, an unpinned package was restored). A failure of the setup, not a finding.
#
# What it enforces, and where each is checked (all inside the container, in this order):
#   - wasi-sdk is downloaded once, checked against its pinned sha256, and installed at ~/.wasi-sdk/wasi-sdk-29.0 in the cache volume,
#     which is the path the componentize package uses; if the build downloads its own copy anyway, that is UNMEASURED;
#   - `dotnet restore` runs against packages/cleat-csharp/nuget.config ONLY (--configfile), so the project's own feeds are not used;
#   - every package the restore put in the cache is in toolchain.packages.tsv with the same sha512, and none is unpinned;
#   - `dotnet build --no-restore`, so nothing can be fetched after the check.
#
# The platform is the host's (arm64 hosts build natively, about 15 times faster than the emulated amd64); CLEAT_CS_PLATFORM overrides.
# CLEAT_CS_RESTORE_ONLY=1 stops after the package check (exit 0): it is how a test asks "does this platform's lock resolve" without
# a compile.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "$here/toolchain.env"
# Sourced, so shellcheck cannot see these assigned (SC2153 on CLEAT_CS_PLATFORMS, which resembles CLEAT_CS_PLATFORM below). Naming each
# one the lock must define makes a toolchain.env that lacks it an UNMEASURED refusal that says which, not an unbound-variable abort.
: "${CLEAT_CS_PLATFORMS:?UNMEASURED: toolchain.env does not define CLEAT_CS_PLATFORMS}"

if [ "$#" -ne 3 ]; then
  echo "usage: build.sh <mount-root> <project-dir> <out-dir>" >&2
  exit 2
fi
root="$(cd "$1" && pwd)" || { echo "UNMEASURED: mount root $1 does not exist" >&2; exit 2; }
proj="$2"
mkdir -p "$3"
out="$(cd "$3" && pwd)"

command -v docker >/dev/null 2>&1 || { echo "UNMEASURED: docker is not on PATH; the C# toolchain runs in a container." >&2; exit 2; }

# One cache per lock: a volume named for the hash of the lock files, so a package from an older lock can never sit in the cache of a
# newer one and be reported as unpinned (or, worse, used).
lock="$(cat "$here/toolchain.env" "$here/toolchain.packages.tsv" "$here/nuget.config" | shasum -a 256 | cut -c1-12)"

# The platform: the host's, unless the caller says otherwise. Anything outside the lock's list is refused, not guessed at.
if [ -z "${CLEAT_CS_PLATFORM:-}" ]; then
  case "$(uname -m)" in
    arm64|aarch64) CLEAT_CS_PLATFORM=linux/arm64 ;;
    *) CLEAT_CS_PLATFORM=linux/amd64 ;;
  esac
fi
case " $CLEAT_CS_PLATFORMS " in
  *" $CLEAT_CS_PLATFORM "*) ;;
  *) echo "UNMEASURED: platform $CLEAT_CS_PLATFORM is not one the lock covers ($CLEAT_CS_PLATFORMS)." >&2; exit 2 ;;
esac
case "$CLEAT_CS_PLATFORM" in
  linux/arm64) arch=arm64; wasisdk_url="$CLEAT_CS_WASISDK_URL_ARM64"; wasisdk_sha="$CLEAT_CS_WASISDK_SHA256_ARM64" ;;
  linux/amd64) arch=amd64; wasisdk_url="$CLEAT_CS_WASISDK_URL_AMD64"; wasisdk_sha="$CLEAT_CS_WASISDK_SHA256_AMD64" ;;
esac
# One cache per lock AND platform: a wasi-sdk or host pack of the other architecture can never be found there.
nuget_volume="${CLEAT_CS_NUGET_VOLUME:-cleat-cs-nuget-$arch-$lock}"
wasisdk_volume="${CLEAT_CS_WASISDK_VOLUME:-cleat-cs-wasisdk-$arch-$lock}"

# shellcheck disable=SC2016  # the inner script is single-quoted on purpose: it runs in the container, not here
inner='
set -uo pipefail
fail() { echo "$2" >&2; exit "$1"; }

# 1. wasi-sdk, once, hash-checked, from the volume, at the exact path the componentize package computes (see toolchain.env). A
# directory there WITHOUT our marker is not trusted: it is the package'"'"'s own unverified download (or another lock'"'"'s), so it is replaced.
sdk="/root/.wasi-sdk/wasi-sdk-${CLEAT_CS_WASISDK_VERSION}"
if [ ! -f "$sdk/.cleat-verified-$WASISDK_SHA" ]; then
  find /root/.wasi-sdk -mindepth 1 -maxdepth 1 -exec rm -rf {} +; rm -f /tmp/wasi.tgz
  curl -fsSL -o /tmp/wasi.tgz "$WASISDK_URL" || fail 2 "UNMEASURED: could not download wasi-sdk from $WASISDK_URL"
  echo "$WASISDK_SHA  /tmp/wasi.tgz" | sha256sum -c - >/dev/null 2>&1 \
    || fail 2 "UNMEASURED: the wasi-sdk archive does not match its pinned sha256 ($WASISDK_SHA); refusing to use it"
  mkdir -p "$sdk"
  tar xzf /tmp/wasi.tgz -C "$sdk" --strip-components=1 || fail 2 "UNMEASURED: could not extract wasi-sdk"
  rm -f /tmp/wasi.tgz
  [ -x "$sdk/bin/clang" ] || fail 2 "UNMEASURED: the wasi-sdk archive did not contain bin/clang"
  touch "$sdk/.cleat-verified-$WASISDK_SHA"
fi
export WASI_SDK_PATH="$sdk"

cd "/mnt/$PROJ" || fail 2 "UNMEASURED: the project directory $PROJ is not under the mount root"
n=$(ls -1 ./*.csproj 2>/dev/null | wc -l)
[ "$n" -eq 1 ] || fail 1 "Error: $PROJ must hold exactly one .csproj, found $n"

# 2. restore, from the lock'"'"'s feeds only.
if ! dotnet restore --configfile /pkg/nuget.config >/tmp/restore.log 2>&1; then
  grep -E "error" /tmp/restore.log | head -30 >&2
  # A restore that failed because the PROJECT is malformed is a finding about it; one that failed to reach a feed, or for any
  # reason this script does not recognise, is UNMEASURED: it fails closed toward "the check could not establish anything".
  if grep -q -E "error (MSB4025|MSB4068|MSB1009|NETSDK[0-9]+|NU1008|NU1604)" /tmp/restore.log; then
    fail 1 "Error: the C# project cannot be restored: it is malformed (see above)."
  fi
  fail 2 "UNMEASURED: dotnet restore failed (see above); nothing was built. A failure of the setup or the pins, not a finding about the project."
fi

# 3. every restored package is pinned, and its hash is the pinned one.
bad=0
for d in /root/.nuget/packages/*/*/; do
  [ -d "$d" ] || continue
  id=$(basename "$(dirname "$d")"); v=$(basename "$d")
  got=$(cat "$d$id.$v.nupkg.sha512" 2>/dev/null || true)
  want=$(awk -F"\t" -v i="$id" -v v="$v" "NR>1 && tolower(\$1)==i && \$2==v {print \$3}" /pkg/toolchain.packages.tsv)
  if [ -z "$want" ]; then echo "unpinned package in the cache: $id $v (not in toolchain.packages.tsv)" >&2; bad=1
  elif [ "$got" != "$want" ]; then echo "package $id $v does not match its pinned sha512" >&2; bad=1; fi
done
[ "$bad" -eq 0 ] || fail 2 "UNMEASURED: the restored packages are not the pinned ones (see above). If the cache volume predates this lock, remove it: docker volume rm $NUGET_VOLUME"
# This platform'"'"'s host pack must have been among them: a project that names no ILCompiler host pack for this platform restores
# nothing to check and then fails to compile for a reason that has nothing to do with the project.
[ -d "/root/.nuget/packages/runtime.$HOST_RID.microsoft.dotnet.ilcompiler.llvm" ] \
  || fail 1 "Error: the project did not restore runtime.$HOST_RID.microsoft.dotnet.ilcompiler.llvm. Reference it as runtime.\$(NETCoreSdkPortableRuntimeIdentifier).microsoft.dotnet.ilcompiler.llvm so it follows the build platform."
if [ "${CLEAT_CS_RESTORE_ONLY:-}" = "1" ]; then exit 0; fi

# 4. build. Nothing can be fetched now. A failure here is the PROJECT: it does not compile.
dotnet build -c Release --no-restore >/tmp/build.log 2>&1
rc=$?
# The componentize package downloads its own (x86_64, unverified) wasi-sdk when its path is absent. We put the verified tree there, so
# this must not have happened; if it did, whatever it produced was built with a compiler nothing here has checked.
# Judged by the ARTIFACT, not by the log: the package'"'"'s download lands in ~/.wasi-sdk, so the volume must still hold exactly our
# verified directory and nothing else. (An earlier version grepped the log for the word "Extracting", which a changed message or a
# quieter verbosity would have turned into a silent pass.)
extra=$(ls -A /root/.wasi-sdk | grep -v -x "wasi-sdk-${CLEAT_CS_WASISDK_VERSION}" || true)
if [ -n "$extra" ] || [ ! -f "$sdk/.cleat-verified-$WASISDK_SHA" ]; then
  fail 2 "UNMEASURED: the build downloaded its own wasi-sdk instead of using the verified one at $sdk (the volume also holds: ${extra:-nothing else; our marker is gone}); nothing was written."
fi
if [ "$rc" -ne 0 ]; then
  grep -E "error|Error" /tmp/build.log | head -40 >&2
  fail 1 "Error: the C# project does not build (dotnet build exited $rc)."
fi

# 5. the outputs: exactly one .wasm, and the entry-point list the project says it writes.
pub="bin/Release/net10.0/wasi-wasm/publish"
cnt=$(ls -1 "$pub"/*.wasm 2>/dev/null | wc -l)
[ "$cnt" -eq 1 ] || fail 1 "Error: expected exactly one .wasm in $pub, found $cnt"
ep=$(dotnet msbuild -getProperty:CleatEntryPointsFile -p:Configuration=Release 2>/dev/null | tr -d "\r")
[ -n "$ep" ] || fail 1 "Error: the project does not define CleatEntryPointsFile, so there is no entry-point list to stamp. A workflow project imports the Cleat C# SDK, which defines it."
[ -f "$ep" ] || fail 1 "Error: the project defines CleatEntryPointsFile=$ep but the build did not write it."
cp "$ep" /out/entry-points.txt
cp "$pub"/*.wasm /out/
chown -R "$HOST_UID:$HOST_GID" /out "/mnt/$PROJ/obj" "/mnt/$PROJ/bin" 2>/dev/null || true
exit 0
'

docker run --rm --platform "$CLEAT_CS_PLATFORM" \
  -v "$nuget_volume":/root/.nuget \
  -v "$wasisdk_volume":/root/.wasi-sdk \
  -v "$root":/mnt \
  -v "$here":/pkg:ro \
  -v "$out":/out \
  -e PROJ="$proj" -e NUGET_VOLUME="$nuget_volume" -e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" \
  -e HOST_RID="linux-$([ "$arch" = arm64 ] && echo arm64 || echo x64)" -e CLEAT_CS_RESTORE_ONLY="${CLEAT_CS_RESTORE_ONLY:-}" \
  -e CLEAT_CS_WASISDK_VERSION="$CLEAT_CS_WASISDK_VERSION" -e WASISDK_URL="$wasisdk_url" -e WASISDK_SHA="$wasisdk_sha" \
  "$CLEAT_CS_SDK_IMAGE" bash -c "$inner"
rc=$?
case "$rc" in
  0|1|2) exit "$rc" ;;
  *) echo "UNMEASURED: the container did not run to the end (docker run exited $rc): the image could not be pulled, the daemon stopped, the build was killed, or this host cannot execute $CLEAT_CS_PLATFORM (\"exec format error\" above: no binfmt/QEMU handler for it). A failure of the setup, not a finding about the project." >&2; exit 2 ;;
esac
