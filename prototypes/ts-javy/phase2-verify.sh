#!/usr/bin/env bash
# Phase 1, Phase 2 and Phase 3 (cross-language) end to end, from a CLEAN CHECKOUT: bootstrap.sh builds every input (worker, cleatctl,
# linker, javy, esbuild, the database and its key) from the committed tree and writes evidence/phase2/provenance.txt; a dirty tree
# or a missing tool stops the run as UNMEASURED., from one script, on one freshly built tree. ~30 minutes. Exit 0 only if every part exits 0.
#   evidence/phase1/*  evidence/phase2/*
# To run beside someone else's run (a reviewer re-deriving a checkpoint), give yours its own database and port, then export them:
#   export CLEAT_PROTO_PG_DB=cleat_mine CLEAT_PROTO_API_PORT=18773 CLEAT_PROTO_KEY_FILE=$PWD/.work/mine.key; ./phase2-verify.sh
# (just that: provision-db.sh needs .work/cleat-worker, which only bootstrap.sh builds, and bootstrap.sh already calls provision-db.sh)
# The stale-worker handling only ever touches workers THIS checkout started (pidfile + this checkout's binary path).
set -u
cd "$(dirname "${BASH_SOURCE[0]}")"
E2=evidence/phase2; mkdir -p "$E2" evidence/phase3
python3 phase1_lib.py stop     # only workers THIS checkout started; configurable port/database: CLEAT_PROTO_* (phase1_lib.py)
echo "settings: $(python3 phase1_lib.py settings)"
step() { # $1 label, rest: command. A step must start with no harness worker alive and leave none behind: a survivor claims the next
  # step's runs and writes them to a log nobody reads, which silently shortens every count taken from a log (measured 2026-11-13).
  local label=$1; shift
  python3 phase1_lib.py stop
  "$@"; local rc=$?
  local left; left=$(python3 phase1_lib.py leak)
  if [ -n "$left" ]; then echo "[$label] LEAK: worker(s) of this checkout still alive after the step: $left"; rc=3; python3 phase1_lib.py stop; fi
  echo "[$label] exit=$rc"; eval "rc_$label=$rc"; }
step bootstrap    bash -c "./bootstrap.sh > $E2/bootstrap.txt 2>&1; rc=\$?; cp -f .work/provenance.txt $E2/provenance.txt 2>/dev/null; exit \$rc"
step selftest     bash -c "python3 harness_selftest.py > evidence/phase3/harness-selftest.txt 2>&1"
step build        ./build-all.sh > "$E2/build-all.txt" 2>&1
step pathindep    bash -c "./phase3_pathindep.sh > evidence/phase3/pathindep.txt 2>&1"
step phase1       ./phase1-verify.sh > "$E2/phase1-verify-output.txt" 2>&1
step repro        bash -c "./phase2_repro.sh > $E2/repro.txt 2>&1"
step controls     bash -c "python3 phase2_controls.py > $E2/controls.txt 2>&1"
step pin          bash -c "python3 phase2_pin.py > $E2/pin.txt 2>&1"
step midexec      bash -c "python3 phase2_midexec.py > $E2/midexec.txt 2>&1"
step tscheck      bash -c "(GOWORK=off GOFLAGS=-mod=mod GOPROXY=off go test -count=1 -v ./tools/tscheck/ > $E2/tscheck-tests.txt 2>&1) && ./phase2_tscheck_mutants.sh > $E2/tscheck-mutants.txt 2>&1"
step gocontrol    bash -c "python3 phase2_go_control.py > $E2/go-control.txt 2>&1"
step xlang        bash -c "python3 phase3_verify.py > evidence/phase3/verify.txt 2>&1"
echo "exit codes: bootstrap=$rc_bootstrap selftest=$rc_selftest build=$rc_build pathindep=$rc_pathindep phase1=$rc_phase1 repro=$rc_repro controls=$rc_controls pin=$rc_pin midexec=$rc_midexec tscheck=$rc_tscheck gocontrol=$rc_gocontrol xlang=$rc_xlang"
[ $rc_bootstrap -eq 0 ] && [ $rc_selftest -eq 0 ] && [ $rc_build -eq 0 ] && [ $rc_pathindep -eq 0 ] && [ $rc_phase1 -eq 0 ] && [ $rc_repro -eq 0 ] && [ $rc_controls -eq 0 ] && [ $rc_pin -eq 0 ] && [ $rc_midexec -eq 0 ] && [ $rc_tscheck -eq 0 ] && [ $rc_gocontrol -eq 0 ] && [ $rc_xlang -eq 0 ]
