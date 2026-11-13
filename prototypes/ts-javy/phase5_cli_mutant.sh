#!/usr/bin/env bash
# Prove the `cleat build --target ts` unit tests can fail, and for the right reason: replace the entry-name rule with one that accepts
# anything, require TestParseTSEntry to go red naming an accepted spec, restore, and CHECK THE RESTORE BY CONTENT (a restore that does not
# restore reads as a fix that does not work). Exit 0 only if red-for-the-right-reason AND restored AND green again.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
F=cmd/cleat/build_ts.go; B=$(mktemp); cp "$F" "$B"
trap 'cp "$B" "$F"; rm -f "$B"' EXIT
export -n GOWORK GOFLAGS GOPROXY; unset GOWORK GOFLAGS GOPROXY
sed -i.bak 's#^var tsEntryName = regexp.MustCompile(.*#var tsEntryName = regexp.MustCompile(`.*`)#' "$F"; rm -f "$F.bak"
grep -q 'MustCompile(`.\*`)' "$F" || { echo "UNMEASURED: the mutation did not apply"; exit 2; }
out=$(go test ./cmd/cleat/ -run TestParseTSEntry -count=1 -timeout 120s 2>&1); rc=$?
echo "$out" | grep -E "accepted a spec|build failed|FAIL|^ok" | head -4
[ $rc -ne 0 ] && echo "$out" | grep -q "accepted a spec that must be refused" || { echo "FAIL: the mutant did not go red for the expected reason"; exit 1; }
cp "$B" "$F"; diff -q "$B" "$F" >/dev/null && echo "restored (content identical)" || { echo "FAIL: restore differs"; exit 1; }
go test ./cmd/cleat/ -run 'TestParseTSEntry|TestIsValidTargetKnowsTS|TestTSToolchain' -count=1 -timeout 120s 2>&1 | tail -1 | grep -q "^ok" && echo "green again after the restore" || { echo "FAIL: not green after the restore"; exit 1; }
