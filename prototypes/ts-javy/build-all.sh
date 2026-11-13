#!/usr/bin/env bash
# Rebuild and deploy every prototype module from the CURRENT toolchain, so no evidence is produced by a module built before the
# shims, the pin or the deterministic flags existed. (A stale module is silent: `cleatctl deploy` says "WASM unchanged" for it.)
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
export GOWORK=off GOFLAGS=-mod=mod GOPROXY=off
export CLEAT_DB_URL="postgres://postgres:postgres@127.0.0.1:54392/cleat?sslmode=disable"
D=.work/cleatctl

./build-plugin-variant.sh full ""
cp .work/plugin-full.wasm .work/cleat-plugin.wasm      # identical to what build.sh would build: asserted in phase2_repro.sh via the pin
for v in "off-date:date" "off-random:random" "off-lockdown:lockdown" "off-all:date,random,lockdown"; do ./build-plugin-variant.sh "${v%%:*}" "${v##*:}"; done

for n in sleeper signaler parent racer finalizer journey busy; do ./build.sh "$n" "$n" | tail -1; $D deploy workflow "$n" ".work/$n.wasm" | tail -1; done
SUSPEND_MODE=throw OUT_NAME=finalizer_throw ./build.sh finalizer finalizer | tail -1; $D deploy workflow finalizer_throw .work/finalizer_throw.wasm | tail -1
./build.sh errors thrower,rejecter,late_thrower,raw_throw,oom | tail -1
for n in thrower rejecter late_thrower raw_throw oom; do $D deploy workflow "$n" .work/errors.wasm | tail -1; done
./build.sh nondet nondet | tail -1

# The Go twin for the engine-WARN control (cleat build, wasip1; kept in the nested module).
go run github.com/cleat-team/cleat/cmd/cleat build -o .work/gocontrol ./gocontrol/ >/dev/null 2>&1
cp .work/gocontrol/journey.wasm .work/go_journey.wasm
echo "built and deployed everything from the current toolchain"
