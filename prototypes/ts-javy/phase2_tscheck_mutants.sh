#!/usr/bin/env bash
# Red controls for tools/tscheck's own tests: three mutants of the checker, each of which its tests must catch. The original is
# restored from a saved copy and verified BY CONTENT afterwards (a restore that does not restore reads as a passing suite).
set -u
cd "$(dirname "${BASH_SOURCE[0]}")/tools/tscheck"
export GOWORK=off GOFLAGS=-mod=mod GOPROXY=off
cp scan.go /tmp/scan.go.orig
fails=0
run_mutant() { # $1 label  $2 python expression body that edits scan.go  $3 test name expected to fail
  python3 -c "$2" || { echo "UNMEASURED: mutant '$1' did not apply"; cp /tmp/scan.go.orig scan.go; return 2; }
  if cmp -s scan.go /tmp/scan.go.orig; then echo "UNMEASURED: mutant '$1' left scan.go unchanged"; return 2; fi
  out=$(go test -count=1 -timeout 120s ./ 2>&1)
  if echo "$out" | grep -q -- "--- FAIL: $3"; then echo "caught   $1   (--- FAIL: $3)"; else echo "MISSED   $1   (expected --- FAIL: $3)"; fails=$((fails+1)); fi
  cp /tmp/scan.go.orig scan.go
}
run_mutant "never refuses (rules emptied)" "
s=open('scan.go').read(); a=s.index('var rules = map[string]rule{'); b=s.index('\n}\n',a)+3
open('scan.go','w').write(s[:a]+'var rules = map[string]rule{}\n'+s[b:])" TestKnownPositivesAreRefusedForTheDeterminismReason
run_mutant "refuses a method call on another object" "
s=open('scan.go').read(); old='if member && !qualifiers[prevIdent] {\n\t\t\t\t\tflag = false'
open('scan.go','w').write(s.replace(old,'if member && !qualifiers[prevIdent] {\n\t\t\t\t\tflag = true',1))" TestKnownNegativesAreNotOverReported
run_mutant "an unterminated comment ends the scan quietly" "
s=open('scan.go').read(); old='return result{unmeasured: fmt.Sprintf(\"unterminated block comment at %d:%d swallows the rest of the file\", line, col)}'
open('scan.go','w').write(s.replace(old,'i = len(src)',1))" TestACheckThatCouldNotLookIsNotAPass
cmp -s scan.go /tmp/scan.go.orig && echo "restored (content identical)" || { echo "RESTORE FAILED"; fails=$((fails+1)); }
go test -count=1 ./ 2>&1 | tail -1
[ $fails -eq 0 ]
