#!/usr/bin/env bash
# Install the TypeScript guest's pinned toolchain into packages/cleat-ts/.tools and node_modules, verifying
# every download against toolchain.lock BEFORE it is executed, and failing closed on a mismatch.
#
#   usage: packages/cleat-ts/bootstrap.sh             install, verify, report
#          packages/cleat-ts/bootstrap.sh --self-test prove the verifier refuses a tampered file
#   exit:  0 ready; 1 a finding (--self-test only: the verifier accepted what it must refuse);
#          2 UNMEASURED: a tool is missing, the platform has no pin, a download failed, or a checksum did not
#            match (the last is a refusal to run, and says so; it is not a finding about cleat)
#
# A checksum mismatch exits 2 and removes nothing it did not create: the unverified file is left where it was
# downloaded so it can be inspected, and is never made executable.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
LOCK=toolchain.lock
unmeasured() { echo "UNMEASURED: $*" >&2; echo "  (this is a failure of the setup, not a finding about cleat)" >&2; exit 2; }

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | cut -d' ' -f1
  else unmeasured "neither sha256sum nor shasum is available"; fi
}

# verify FILE WANT: exit 0 only if the file's sha256 equals WANT. WANT must be 64 hex characters, so an empty
# or garbled lock entry can never "match" an empty or failed hash.
verify() {
  local file=$1 want=$2 got
  [[ "$want" =~ ^[0-9a-f]{64}$ ]] || { echo "refusing: the pinned checksum '$want' is not 64 hex characters" >&2; return 2; }
  got=$(sha256_of "$file")
  [[ "$got" =~ ^[0-9a-f]{64}$ ]] || { echo "refusing: could not compute a checksum for $file" >&2; return 2; }
  [ "$got" = "$want" ] || { echo "refusing: $file has sha256 $got, toolchain.lock pins $want" >&2; return 1; }
}

if [ "${1:-}" = "--self-test" ]; then
  t=$(mktemp -d "${TMPDIR:-/tmp}/cleat-ts-selftest.XXXXXX")
  printf 'a pinned artefact\n' > "$t/good"
  want=$(sha256_of "$t/good")
  bad=0
  if verify "$t/good" "$want" 2>/dev/null; then echo "  ok    the file whose checksum is pinned is accepted"; else echo "  FAIL  a correct file was refused"; bad=1; fi
  printf 'a pinned artefact!\n' > "$t/tampered"
  verify "$t/tampered" "$want" 2>/dev/null && { echo "  FAIL  a tampered file was ACCEPTED"; bad=1; } || echo "  ok    a tampered file is refused"
  verify "$t/good" "" 2>/dev/null && { echo "  FAIL  an empty pin was accepted"; bad=1; } || echo "  ok    an empty pin is refused"
  verify "$t/good" "not-a-checksum" 2>/dev/null && { echo "  FAIL  a malformed pin was accepted"; bad=1; } || echo "  ok    a malformed pin is refused"
  verify "$t/nonexistent" "$want" 2>/dev/null && { echo "  FAIL  a missing file was accepted"; bad=1; } || echo "  ok    a missing file is refused"
  rm -f "$t/good" "$t/tampered"; rmdir "$t"
  [ "$bad" = 0 ] && { echo "self-test: the verifier accepts the pinned file and refuses every case above"; exit 0; }
  echo "self-test: FAILED" >&2; exit 1
fi

[ -f "$LOCK" ] || unmeasured "$LOCK is missing"
lock() { awk -v k="$1" '$1==k{print $2; exit}' "$LOCK"; }
lock_asset() { awk -v k="$1" '$1==k{print $3; exit}' "$LOCK"; }

missing=()
for t in node npm curl gunzip; do command -v "$t" >/dev/null 2>&1 || missing+=("$t"); done
[ ${#missing[@]} -eq 0 ] || unmeasured "missing: ${missing[*]}"

case "$(uname -s)-$(uname -m)" in
  Darwin-arm64)             plat=darwin-arm64 ;;
  Linux-x86_64)             plat=linux-x86_64 ;;
  Linux-aarch64|Linux-arm64) plat=linux-arm64 ;;
  *) unmeasured "no pinned javy for $(uname -s)-$(uname -m) in $LOCK" ;;
esac
ver=$(lock javy.version); want=$(lock "javy.sha256.$plat"); asset=$(lock_asset "javy.sha256.$plat")
[ -n "$ver" ] && [ -n "$want" ] && [ -n "$asset" ] || unmeasured "$LOCK has no javy pin for $plat"

mkdir -p .tools
if [ ! -x .tools/javy ] || [ "$(.tools/javy --version 2>/dev/null)" != "javy $ver" ]; then
  curl -fsSL -o ".tools/$asset" "https://github.com/bytecodealliance/javy/releases/download/v$ver/$asset" \
    || unmeasured "could not download $asset"
  # Verified BEFORE it is decompressed or made executable.
  verify ".tools/$asset" "$want" || unmeasured "the downloaded $asset does not match $LOCK; nothing was executed"
  gunzip -c ".tools/$asset" > .tools/javy
  chmod +x .tools/javy
fi
[ "$(.tools/javy --version)" = "javy $ver" ] || unmeasured "javy --version is '$(.tools/javy --version)', $LOCK pins $ver"
echo "javy $ver ($plat), archive sha256 $want"

npm ci --no-audit --no-fund >/dev/null 2>&1 || unmeasured "npm ci failed"
ewant=$(lock esbuild.version)
egot=$(npx esbuild --version)
[ "$egot" = "$ewant" ] || unmeasured "esbuild is $egot, $LOCK pins $ewant"
echo "esbuild $egot"
