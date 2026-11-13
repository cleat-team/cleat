#!/usr/bin/env bash
# Item 6 (cleat-review, checkpoint 1): the plugin's hash must not depend on the directory it was built in, because two developers
# building the same source must deploy the same version. usage: phase3_pathindep.sh   (needs .work/plugin-full.wasm from build-all.sh)
# Exit 0 pass, 1 finding, 2 UNMEASURED (a build failed, so the check did not look).
#   A  this tree's plugin-full.wasm                                  (built where the tree lives)
#   B  the same plugin sources copied to a different directory, built there with the same flags      -> must equal A
#   C  known-positive: B's copy with the dependency written the OLD way (../../../crates/cleat-sdk) -> must DIFFER from A
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
ROOT=$(git rev-parse --show-toplevel); HERE=$(pwd)
unmeasured() { echo "UNMEASURED: $* (this is a failure of the check, not a finding about the tree)"; exit 2; }
[ -f .work/plugin-full.wasm ] || unmeasured ".work/plugin-full.wasm is missing; run build-all.sh first"
A=$(shasum -a 256 .work/plugin-full.wasm | cut -d' ' -f1)

base=$(mktemp -d "${TMPDIR:-/tmp}/cleat-pathindep.XXXXXX") || unmeasured "mktemp"
trap 'rm -rf "$base"' EXIT       # only the directory this script created
build_copy() {                   # $1 = label, $2 = "old" for the old-style dependency
  local d="$base/$1/some/other/place" ; mkdir -p "$d"
  ( cd "$ROOT" && tar --exclude=target -cf - prototypes/ts-javy/plugin crates/cleat-sdk crates/cleat-macro ) | tar -xf - -C "$d" || unmeasured "copy for $1"
  if [ "$2" = old ]; then sed -i.bak 's#path = "vendor/cleat-sdk"#path = "../../../crates/cleat-sdk"#' "$d/prototypes/ts-javy/plugin/Cargo.toml"; grep -q '"../../../crates/cleat-sdk"' "$d/prototypes/ts-javy/plugin/Cargo.toml" || unmeasured "could not write the old-style dependency"; fi
  ( cd "$d/prototypes/ts-javy/plugin" && CLEAT_SHIMS_OFF="" CLEAT_FAULT="" PLUGIN_DIR="$PWD" "$HERE/plugin-cargo.sh" ) >"$base/$1.log" 2>&1 || { cat "$base/$1.log"; unmeasured "cargo build for $1 failed"; }
  "$HERE/.tools/javy" init-plugin --deterministic "$d/prototypes/ts-javy/plugin/target/wasm32-wasip1/release/cleat_js_plugin.wasm" -o "$base/$1.wasm" || unmeasured "init-plugin for $1"
  shasum -a 256 "$base/$1.wasm" | cut -d' ' -f1
}
B=$(build_copy B new) || exit $?
C=$(build_copy C old) || exit $?
echo "A  this tree ($ROOT)                             $A"
echo "B  same sources, built under $base/B/...          $B"
echo "C  known-positive, old-style path dependency, $base/C/...   $C"
rc=0
if [ "$A" = "$B" ]; then echo "PASS  the plugin hash is the same in two directories"; else echo "FAIL  the plugin hash depends on the build directory"; rc=1; fi
if [ "$A" != "$C" ]; then echo "PASS  known-positive: the old dependency form DOES change the hash, so this check can see the defect"; else echo "FAIL  known-positive: the old dependency form gave the same hash, so this check cannot tell the two apart"; rc=1; fi
exit $rc
