#!/usr/bin/env bash
# Phase 5 end to end (plan section 6): the unit tests and their mutant for `cleat build --target ts`, source maps (Q7), 4 npm packages
# (Q6), and the developer's flow through the CLI. From a checkout where bootstrap.sh has run; own database and port (CLEAT_PROTO_*).
# ~10 minutes. Exit 0 only if every step does.
set -u
cd "$(dirname "${BASH_SOURCE[0]}")"
export GOWORK=off GOFLAGS=-mod=mod GOPROXY=off
mkdir -p evidence/phase5
step() { local label=$1; shift; python3 phase1_lib.py stop; "$@"; local rc=$?; local left; left=$(python3 phase1_lib.py leak)
  if [ -n "$left" ]; then echo "[$label] LEAK: $left"; rc=3; python3 phase1_lib.py stop; fi; echo "[$label] exit=$rc"; eval "rc_$label=$rc"; }
step gotest  bash -c "(cd ../.. && env -u GOWORK -u GOFLAGS -u GOPROXY go test ./cmd/cleat/ -run 'TestParseTSEntry|TestIsValidTargetKnowsTS|TestTSToolchain|TestIsValidTarget' -count=1 -v > prototypes/ts-javy/evidence/phase5/cli-unit-tests.txt 2>&1) && ./phase5_cli_mutant.sh > evidence/phase5/cli-mutant.txt 2>&1"
step srcmap  bash -c "python3 phase5_srcmap.py > evidence/phase5/srcmap.txt 2>&1"
step npm     bash -c "python3 phase5_npm.py > evidence/phase5/npm.txt 2>&1"
step cli     bash -c "python3 phase5_cli.py > evidence/phase5/cli.txt 2>&1"
echo "exit codes: gotest=$rc_gotest srcmap=$rc_srcmap npm=$rc_npm cli=$rc_cli"
[ $rc_gotest -eq 0 ] && [ $rc_srcmap -eq 0 ] && [ $rc_npm -eq 0 ] && [ $rc_cli -eq 0 ]
