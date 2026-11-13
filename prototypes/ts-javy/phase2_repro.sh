#!/usr/bin/env bash
# Build reproducibility: the same source built repeatedly, with and without `javy build -C deterministic`.
# GREEN = with the flag, every build has one hash. RED CONTROL = without it, builds differ. Exits 1 if either half is not as stated.
set -u
cd "$(dirname "${BASH_SOURCE[0]}")"
name=${1:-journey}
hashes() { # $1 = y|n
  for i in 1 2 3; do JAVY_DETERMINISTIC=$1 ./build.sh "$name" "$name" >/dev/null 2>&1 || { echo "BUILD FAILED"; return 1; }; shasum -a 256 ".work/$name.wasm" | cut -c1-16; done
}
on=$(hashes y) || { echo "UNMEASURED: build failed"; exit 2; }
off=$(hashes n) || { echo "UNMEASURED: build failed"; exit 2; }
echo "with    -C deterministic=y : $(echo $on | tr ' ' ' ')   distinct=$(echo "$on" | sort -u | wc -l | tr -d ' ')"
echo "without (-C deterministic=n): $(echo $off | tr ' ' ' ')   distinct=$(echo "$off" | sort -u | wc -l | tr -d ' ')"
JAVY_DETERMINISTIC=y ./build.sh "$name" "$name" >/dev/null 2>&1   # leave the tree on the reproducible build

# The plugin binary is pre-initialised too (`javy init-plugin`), so it needs the same flag. A pin that hashes the plugin is
# only a pin if rebuilding the plugin gives the same hash.
phash() { for i in 1 2 3; do PLUGIN_DETERMINISTIC_FLAG="$1" ./build-plugin-variant.sh "repro-$i" "" >/dev/null 2>&1 || { echo "BUILD FAILED"; return 1; }; shasum -a 256 ".work/plugin-repro-$i.wasm" | cut -c1-16; done; rm -f .work/plugin-repro-*.wasm; }
pon=$(phash --deterministic) || { echo "UNMEASURED: plugin build failed"; exit 2; }
poff=$(phash "") || { echo "UNMEASURED: plugin build failed"; exit 2; }
echo "plugin with    init-plugin --deterministic : $(echo $pon)   distinct=$(echo "$pon" | sort -u | wc -l | tr -d ' ')"
echo "plugin without                             : $(echo $poff)   distinct=$(echo "$poff" | sort -u | wc -l | tr -d ' ')"
./build-plugin-variant.sh full "" >/dev/null 2>&1
n() { echo "$1" | sort -u | wc -l | tr -d ' '; }
[ "$(n "$on")" = 1 ] && [ "$(n "$off")" -gt 1 ] && [ "$(n "$pon")" = 1 ] && [ "$(n "$poff")" -gt 1 ]
