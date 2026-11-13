#!/usr/bin/env bash
# Phase 1 and Phase 2 end to end, from one script, on one freshly built tree. ~30 minutes. Exit 0 only if every part exits 0.
#   evidence/phase1/*  evidence/phase2/*
set -u
cd "$(dirname "${BASH_SOURCE[0]}")"
E2=evidence/phase2; mkdir -p "$E2"
pkill -f "cleat-worker -db .*-api-addr :18771" 2>/dev/null; sleep 1
WORKERS='cleat-worker -db .*-api-addr :18771'
step() { # $1 label, rest: command. A step must start with no harness worker alive and leave none behind: a survivor claims the next
  # step's runs and writes them to a log nobody reads, which silently shortens every count taken from a log (measured 2026-11-13).
  local label=$1; shift
  pkill -f "$WORKERS" 2>/dev/null; sleep 1
  "$@"; local rc=$?
  local left; left=$(pgrep -f "$WORKERS" | tr '\n' ' ')
  if [ -n "$left" ]; then echo "[$label] LEAK: harness worker(s) still alive after the step: $left"; rc=3; pkill -f "$WORKERS"; fi
  echo "[$label] exit=$rc"; eval "rc_$label=$rc"; }
step build        ./build-all.sh > "$E2/build-all.txt" 2>&1
step phase1       ./phase1-verify.sh > "$E2/phase1-verify-output.txt" 2>&1
step repro        bash -c "./phase2_repro.sh > $E2/repro.txt 2>&1"
step controls     bash -c "python3 phase2_controls.py > $E2/controls.txt 2>&1"
step pin          bash -c "python3 phase2_pin.py > $E2/pin.txt 2>&1"
step midexec      bash -c "python3 phase2_midexec.py > $E2/midexec.txt 2>&1"
step tscheck      bash -c "(GOWORK=off GOFLAGS=-mod=mod GOPROXY=off go test -count=1 -v ./tools/tscheck/ > $E2/tscheck-tests.txt 2>&1) && ./phase2_tscheck_mutants.sh > $E2/tscheck-mutants.txt 2>&1"
step gocontrol    bash -c "python3 phase2_go_control.py > $E2/go-control.txt 2>&1"
echo "exit codes: build=$rc_build phase1=$rc_phase1 repro=$rc_repro controls=$rc_controls pin=$rc_pin midexec=$rc_midexec tscheck=$rc_tscheck gocontrol=$rc_gocontrol"
[ $rc_build -eq 0 ] && [ $rc_phase1 -eq 0 ] && [ $rc_repro -eq 0 ] && [ $rc_controls -eq 0 ] && [ $rc_pin -eq 0 ] && [ $rc_midexec -eq 0 ] && [ $rc_tscheck -eq 0 ] && [ $rc_gocontrol -eq 0 ]
