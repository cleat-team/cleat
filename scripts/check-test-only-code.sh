#!/usr/bin/env bash
#
# Detect code that only tests call.
#
# The failure mode this repo keeps hitting is not broken code -- it is code
# that is written, tested, passing, and wired to nothing. Some examples found
# in one scan:
#
#   engine/flush.go       (*Engine).flushCallIntent
#       The write half of crash recovery. The read half is live and correct,
#       so the detector looks for a sentinel that nothing ever writes. In a
#       real crash there is nothing to find. (IMPROVEMENT-PLAN.md 1.4)
#
#   engine/mssql_retry.go mssqlRetry, and engine/mssql_errors.go's whole
#       error-classification family. SQL Server transient-error retry, with
#       ~12 passing test cases covering deadlock retry, backoff, context
#       cancellation and retry exhaustion -- and no production caller. Every
#       SQL Server deadlock surfaces as a hard error.
#
# A test suite cannot catch this: the tests pass precisely because they are
# the only callers. staticcheck's U1000 can, if you tell it to ignore test
# files -- then anything used only from _test.go reads as unused.
#
# U1000 does not report exported identifiers in library packages, so a public
# API with no internal caller is not flagged. That is the desired behaviour
# here.
#
# Usage:
#   scripts/check-test-only-code.sh              # fail on entries not in the baseline
#   scripts/check-test-only-code.sh --update     # rewrite the baseline
#
# The baseline exists because there is a backlog. New entries fail the build;
# clearing the existing ones is tracked in IMPROVEMENT-PLAN.md. Removing an
# entry from the baseline (by wiring the code up or deleting it) never fails.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT" || exit 1

BASELINE="scripts/deadcode-baseline.txt"

# Pinned: an unpinned @latest would turn an upstream release into a
# spontaneous CI failure on an unrelated PR.
#
# The pin is not fire-and-forget, though: staticcheck reads the toolchain's
# export data, whose format version advances with Go. 2025.1.1 predates Go
# 1.26 and every scan under it died with
#
#   internal error in importing "cmp" (cannot decode "cmp", export data
#   version 4 is greater than maximum supported version 2)
#
# which the vacuous-pass check below turned into a hard failure -- correctly,
# but on every PR. When the repo's Go version moves, this pin has to move
# with it.
STATICCHECK="honnef.co/go/tools/cmd/staticcheck@2026.2.1"

# Key on "<package dir><TAB><symbol>" rather than the raw staticcheck line,
# so that moving a function within its file does not churn the baseline.
# Input:  engine/flush.go:186:18: func (*Engine).flushCallIntent is unused (U1000)
# Output: engine<TAB>func (*Engine).flushCallIntent
#
# The analysis is pinned to GOOS=linux so the baseline is portable. Build
# constraints mean staticcheck sees a different set of files per platform, so a
# baseline generated on darwin does not match one generated on the CI runner
# and the difference surfaces as phantom "new" entries.
#
# The tool itself must still be built for the host: `GOOS=linux go run` would
# cross-compile staticcheck and then fail to execute it ("exec format error").
# So it is installed for the host first and only its analysis is retargeted.
#
# LC_ALL=C pins the collation, which otherwise differs between a developer's
# locale and the runner's.
#
# THE CGO BLIND SPOT, AND WHY THERE ARE NOW TWO PASSES.
#
# CGO_ENABLED=0 is forced by the cross-compile, and it hides every file behind
# `//go:build cgo` -- 16 files and ~6,000 lines, including the whole wasmtime
# backend, which is the ONLY backend a production worker runs.
#
# This comment used to describe that blind spot in one direction: a helper
# whose only CALLER is cgo-gated is reported unused when it is not.
# `contextWithRawMemBuf` and `guestErrorText` were baselined for exactly that
# reason. True, and only half of it.
#
# The other half is worse, because it is silent. A symbol DEFINED inside a
# cgo-gated file is not reported at all -- the scan cannot see the definition
# either, so dead code there is invisible rather than mis-reported. Measured on
# ./engine/ at the time this was written:
#
#   CGO off (what this guard saw):   19 findings
#   CGO on  (native):                33 findings
#   only with CGO on:                18, all in cgo-gated files
#   only with CGO off:                4, all cgo-caller false positives
#
# The 18 included `isExecutionLimit`, whose doc comment describes gating a
# fallback that no longer exists, and thirteen `_dispatcher*` constants.
#
# So the scan runs TWICE and the results are combined per file, rather than one
# pass being trusted everywhere:
#
#   file is `//go:build cgo`   -> only the CGO-on pass can see it; trust it
#   file is `//go:build !cgo`  -> only the CGO-off pass can see it; trust it
#   file has no build tag      -> BOTH passes see it, so a finding counts only
#                                 if both report it. One pass alone reporting
#                                 it means the other pass found a caller, which
#                                 is precisely the false positive above.
#
# That last rule is what lets the four cgo-caller entries leave the baseline
# instead of living there forever with a comment explaining why they are wrong.
#
# PORTABILITY. The second pass cannot set GOOS, because cgo cross-compilation
# needs a cross toolchain, so it runs natively. That is safe HERE and the
# reason is measured, not assumed: this repo has no OS-specific build tags and
# no OS- or arch-suffixed filenames, so the file set depends on CGO_ENABLED
# alone and a native scan sees the same files on darwin and on the runner.
# Re-derive before trusting it:
#
#   grep -rh '^//go:build' --include='*.go' . | sort | uniq -c
#   find . -name '*.go' | grep -cE '_(linux|darwin|windows|amd64|arm64)(_test)?\.go$'
#
# If either ever stops holding, this second pass stops being portable and the
# baseline will churn per platform.
TOOLDIR="$(mktemp -d)"
trap 'rm -rf "$TOOLDIR"' EXIT

# Every Go module in the repo, same set the govulncheck and go-mod-tidy guards
# use. `./...` stops at a module boundary, so a single invocation from the repo
# root sees the root module ONLY -- examples/, tests/plugin-harness/ and the
# rest are separate modules and are simply not scanned. That is not a
# theoretical gap: the baseline carried seven `examples/*` entries from an
# environment where they were reached, and a root-only scan drops all seven
# while still reporting "no new test-only code". Silently covering less than
# the baseline assumes is the same vacuous pass this script exists to prevent,
# so the scan iterates modules explicitly.
modules() {
  find . -name go.mod \
      -not -path './node_modules/*' -not -path '*/node_modules/*' \
      -not -path './.claude/*' |
    sed 's|/go\.mod$||; s|^\./||; s|^$|.|' |
    LC_ALL=C sort
}

# Emitted by scan() when it cannot produce a trustworthy result, so callers can
# distinguish a failed scan from a clean tree across the command-substitution
# boundary. Defined ABOVE scan() rather than below it: under `set -u` a use
# before assignment is a fatal error, and scan() now references it on the
# install path as well as at the end.
SCAN_FAILED="__scan_failed__"

scan() {
  local cgo="$1"
  if [ ! -x "$TOOLDIR/staticcheck" ]; then
    if ! GOBIN="$TOOLDIR" go install "$STATICCHECK" >&2; then
      echo "ERROR: could not install $STATICCHECK" >&2
      # NOT exit -- same reason as the sentinel below, which this line used to
      # contradict: scan() runs inside a command substitution, so `exit` ends
      # only the subshell. This script sets -uo pipefail and NOT -e, so the
      # caller carried on with an empty result and printed
      #   OK: no new test-only code (0 known entries in the baseline).
      # and exited 0 -- a vacuous pass by the guard against vacuous passes,
      # in the one function that documents the hazard (cleat#1707).
      echo "$SCAN_FAILED"
      return
    fi
  fi

  local out="" all="" broken=""
  local m prefix mod_out noise
  while IFS= read -r m; do
    [ -n "$m" ] || continue
    if [ "$cgo" = "on" ]; then
      # NATIVE: cgo cannot cross-compile without a cross toolchain. Safe here
      # because the repo has no OS-specific tags or filenames -- see the note
      # above, which carries the commands that re-derive that.
      mod_out="$(cd "$m" && LC_ALL=C CGO_ENABLED=1 GOWORK=off \
        "$TOOLDIR/staticcheck" -checks=U1000 -tests=false ./... 2>&1)"
    else
      mod_out="$(cd "$m" && LC_ALL=C GOOS=linux CGO_ENABLED=0 GOWORK=off \
        "$TOOLDIR/staticcheck" -checks=U1000 -tests=false ./... 2>&1)"
    fi

    # Anything that is not a U1000 finding is staticcheck failing to analyse,
    # not a clean module. The 2025.1.1 pin died this way on every package
    # ("export data version 4 is greater than maximum supported version 2")
    # and a per-module scan would otherwise report that module as clean.
    # A module with no non-test packages is legitimately empty under
    # -tests=false: tests/cross-language is a single _test.go and nothing
    # else. Said out loud rather than filtered silently, so a module that
    # becomes empty by accident is visible in the log.
    if printf '%s\n' "$mod_out" | grep -q '^warning: "\./\.\.\." matched no packages$'; then
      echo "note: $m has no non-test packages to scan" >&2
      mod_out="$(printf '%s\n' "$mod_out" | grep -v '^warning: "\./\.\.\." matched no packages$' || true)"
    fi

    noise="$(printf '%s\n' "$mod_out" | grep -v '(U1000)$' | grep -v '^[[:space:]]*$' || true)"
    if [ -n "$noise" ]; then
      broken="$broken$m"$'\n'
      out="$out$(printf 'module %s:\n%s\n' "$m" "$noise")"
      continue
    fi

    # Re-root each finding on the repo so the baseline key is repo-relative:
    #   examples + datapipeline/pipeline.go:22:5  ->  examples/datapipeline/...
    if [ "$m" = "." ]; then prefix=""; else prefix="$m/"; fi
    all="$all$(printf '%s\n' "$mod_out" | grep '(U1000)$' | sed "s|^|$prefix|")"$'\n'
  done <<EOF
$(modules)
EOF

  if [ -n "$broken" ]; then
    echo "ERROR: staticcheck did not complete in:" >&2
    printf '%s' "$broken" | sed 's/^/    /' >&2
    echo "Raw output follows:" >&2
    printf '%s\n' "$out" | head -20 | sed 's/^/    /' >&2
    echo "$SCAN_FAILED"
    return
  fi

  local findings
  findings="$(printf '%s\n' "$all" | grep '(U1000)$' | LC_ALL=C sort -u)"

  # A scan that finds nothing is far more likely to be a broken scan than a
  # clean tree -- a cross-compile failure, a build error, a changed message
  # format. Treating that as "no findings" would leave the guard passing
  # vacuously, which is the exact failure mode it exists to catch. This repo
  # has a real backlog, so zero is never legitimate.
  if [ -z "$findings" ]; then
    echo "ERROR: staticcheck reported no U1000 findings at all." >&2
    echo "That almost certainly means the scan failed rather than that the" >&2
    echo "tree is clean. Raw output follows:" >&2
    printf '%s\n' "$out" | head -20 | sed 's/^/    /' >&2
    # NOT exit: scan runs inside a command substitution, so exit would only
    # leave the subshell and the caller would carry on with an empty result
    # and report OK -- a vacuous pass by the guard against vacuous passes.
    # Callers check for the sentinel instead.
    echo "$SCAN_FAILED"
    return
  fi

  printf '%s\n' "$findings"
}

# stale_entries prints the baseline lines the current scan does not produce.
# cleat#1746's shape, on this guard's baseline.
#
# WHY THE OTHER DIRECTION IS NOT ENOUGH. The comparison below asks only
# "current - baseline": is anything reported that has not been granted. A
# baseline entry the scan stopped producing is invisible to it, and that is the
# entry that matters most here -- it is a STANDING GRANT to be test-only,
# sitting on a symbol that someone has since wired into production. Unwire it
# again tomorrow and this guard stays green, which is precisely the regression
# the grant was written to record.
#
# WHOLE-LINE, so no key-prefix hazard. check-skips.sh's equivalent keys on two
# of three tab-separated fields and has to terminate the key with a literal tab
# to stop `engine Setup` matching `engine SetupForTenant`. A baseline line here
# is the whole key -- "<dir><TAB><symbol>" -- so -Fx compares the entire line
# and a longer symbol cannot satisfy a shorter one.
#
# grep -Fxv rather than comm, for the reason stated at the "new" comparison
# below: comm requires both inputs sorted in its own collation and emits
# garbage when they disagree, which is what a darwin-generated baseline did
# against the CI runner. Set membership has no ordering requirement.
#
# A SEPARATE FUNCTION so --self-test can drive it against fixtures. Exercising
# it through the real scan costs two minutes and a staged repo, and a check
# whose only assertion is that it says nothing on a healthy tree is the defect
# this change is about.
#
# $1 is the current scan; the baseline path comes from $BASELINE, or $2 when
# given, which is what the self-test passes.
stale_entries() {
  local current="$1" baseline="${2:-$BASELINE}"
  # An empty $current would make every baseline line stale. That cannot reach
  # here -- scan() returns the sentinel instead, and die_if_scan_failed exits
  # first -- but the guard is cheap and the failure would be maximally noisy.
  if [ -z "$(printf '%s' "$current" | tr -d '[:space:]')" ]; then
    echo "ERROR: stale_entries called with an empty scan result." >&2
    return 1
  fi
  grep -Fxv -f <(printf '%s\n' "$current") "$baseline" | grep -v '^[[:space:]]*$' || true
}

# buildTagOf classifies a repo-relative .go file by the constraint that decides
# which pass can see it. Only the leading build-tag block matters, so the first
# few lines are enough.
buildTagOf() {
  local f="$1"
  if [ ! -f "$f" ]; then
    echo "missing"
    return
  fi
  local tag
  tag="$(head -8 "$f" | grep -m1 '^//go:build ' || true)"
  case "$tag" in
    '//go:build cgo') echo "cgo" ;;
    '//go:build !cgo') echo "nocgo" ;;
    *) echo "none" ;;
  esac
}

# combine applies the per-file rule described at the top of this file to the two
# raw scans, then reduces what survives to the baseline key.
#
# The key is "<package dir><TAB><symbol>", deliberately dropping the filename
# and line so that moving a function does not churn the baseline.
combine() {
  local off="$1" on="$2"
  local line file tag keep

  {
    # Everything either pass reported, considered once.
    printf '%s\n%s\n' "$off" "$on" | LC_ALL=C sort -u | while IFS= read -r line; do
      [ -n "$line" ] || continue
      file="${line%%:*}"
      tag="$(buildTagOf "$file")"
      keep=no
      case "$tag" in
        cgo)
          # Invisible to the CGO-off pass; the CGO-on pass is the only witness.
          printf '%s\n' "$on" | grep -qxF "$line" && keep=yes
          ;;
        nocgo)
          printf '%s\n' "$off" | grep -qxF "$line" && keep=yes
          ;;
        *)
          # Both passes can see this file, so both must agree. One pass alone
          # means the other found a caller behind the opposite constraint --
          # the cgo-caller false positive this rule exists to drop.
          if printf '%s\n' "$off" | grep -qxF "$line" &&
             printf '%s\n' "$on" | grep -qxF "$line"; then
            keep=yes
          fi
          ;;
      esac
      [ "$keep" = yes ] && printf '%s\n' "$line"
    done
  } | sed -E 's|^([^:]*)/[^/:]*\.go:[0-9]+:[0-9]+: (.*) is unused \(U1000\)$|\1\t\2|' |
    LC_ALL=C sort -u
}

die_if_scan_failed() {
  if [ "$1" = "$SCAN_FAILED" ]; then
    exit 1
  fi
}

# --self-test: a KNOWN-POSITIVE, not a negative control.
#
# CLAUDE.md's rule is that "it passes when everything is fine" is satisfied by
# every broken version of a guard, so the case to assert is one already known to
# be broken. Here that case is cleat#1707: with the tool uninstallable this
# script printed its own ERROR line, then "OK", then exited 0.
#
# The failure is forced with BOTH an empty module cache and GOPROXY=off.
# GOPROXY=off alone is not deterministic -- on a machine where staticcheck is
# already in the module cache `go install` succeeds offline, and the self-test
# would silently stop exercising the path it exists to exercise.
#
# Both assertions are on PRESENCE, never on absence: an exit status alone cannot
# tell "the guard failed for the right reason" from "the harness never started"
# (PATH broken, wrong directory, script not executable). The ERROR line is the
# evidence that the install path was actually reached.
if [ "${1:-}" = "--self-test" ]; then
  st_cache="$(mktemp -d)"
  st_out="$(GOPROXY=off GOMODCACHE="$st_cache/modcache" "$0" 2>&1)"
  st_rc=$?
  rm -rf "$st_cache"

  st_fails=0
  if ! printf '%s\n' "$st_out" | grep -q "^ERROR: could not install "; then
    echo "SELF-TEST FAIL: the run never reached the install path." >&2
    echo "  Without that line the exit status below says nothing." >&2
    printf '%s\n' "$st_out" | tail -5 | sed 's/^/    /' >&2
    st_fails=$((st_fails + 1))
  fi
  if [ "$st_rc" = 0 ]; then
    echo "SELF-TEST FAIL: could not install the tool, yet exited 0 (cleat#1707)." >&2
    printf '%s\n' "$st_out" | tail -3 | sed 's/^/    /' >&2
    st_fails=$((st_fails + 1))
  fi

  # The stale-entry check, driven against fixtures rather than the real scan.
  #
  # A KNOWN-POSITIVE FIRST. "It says nothing on a healthy tree" is satisfied by
  # a stale_entries that returns nothing ever, which is the version this guard
  # effectively shipped with for as long as it has had a baseline. So the case
  # asserted is one constructed to be stale.
  st_base="$(mktemp)"
  printf 'engine\tfunc live\nengine\tfunc GoneAway\nwasm\tconst live2\n' > "$st_base"
  st_scan="$(printf 'engine\tfunc live\nwasm\tconst live2\n')"
  st_got="$(stale_entries "$st_scan" "$st_base")"

  if ! printf '%s\n' "$st_got" | grep -qF 'GoneAway'; then
    echo "SELF-TEST FAIL: a baseline entry the scan does not produce was not reported." >&2
    st_fails=$((st_fails + 1))
  fi
  # The negative control, and it is the half that catches an over-eager check:
  # report a live entry and every run fails, which gets the guard switched off.
  if printf '%s\n' "$st_got" | grep -qE 'live'; then
    echo "SELF-TEST FAIL: a baseline entry the scan DOES produce was reported stale." >&2
    st_fails=$((st_fails + 1))
  fi
  # Substring safety, and the FIXTURE DIRECTION is the whole assertion. Drop the
  # -x and grep matches a baseline line whenever any scan line appears ANYWHERE
  # in it -- so a SHORT scan line silently vouches for a LONGER baseline entry.
  # The baseline therefore holds the long name and the scan produces the short
  # one.
  #
  # Written the other way round first, and it passed against a deliberately
  # broken -F: a long scan line cannot be CONTAINED IN a short baseline entry,
  # so that fixture is green under both the correct comparison and the broken
  # one. An assertion that cannot distinguish them is not an assertion.
  #
  # The pair is constructed, and deliberately: no two entries in today's
  # baseline contain one another, so waiting for a real one is waiting for the
  # defect. Re-derive with
  #   python3 -c "ls=[l.rstrip() for l in open('scripts/deadcode-baseline.txt') if l.strip()];
  #               print(sum(1 for a in ls for b in ls if a!=b and a in b))"   # 0
  printf 'engine\tfunc isDeadlockError\n' > "$st_base"
  if ! stale_entries "$(printf 'engine\tfunc is\n')" "$st_base" |
      grep -qF 'func isDeadlockError'; then
    echo "SELF-TEST FAIL: a baseline entry was satisfied by a scan line it merely contains." >&2
    st_fails=$((st_fails + 1))
  fi
  rm -f "$st_base"

  if [ "$st_fails" != 0 ]; then
    exit 1
  fi
  echo "OK: self-test passed, an uninstallable tool fails the guard (exit $st_rc)"
  echo "    and a baseline entry the scan no longer produces is reported."
  exit 0
fi

if [ "${1:-}" = "--update" ]; then
  fresh_off="$(scan off)"
  die_if_scan_failed "$fresh_off"
  fresh_on="$(scan on)"
  die_if_scan_failed "$fresh_on"
  fresh="$(combine "$fresh_off" "$fresh_on")"
  printf '%s\n' "$fresh" > "$BASELINE"
  echo "Wrote $(wc -l < "$BASELINE" | tr -d ' ') entries to $BASELINE"
  exit 0
fi

if [ ! -f "$BASELINE" ]; then
  echo "ERROR: $BASELINE is missing. Generate it with:" >&2
  echo "  scripts/check-test-only-code.sh --update" >&2
  exit 1
fi

current_off="$(scan off)"
die_if_scan_failed "$current_off"
current_on="$(scan on)"
die_if_scan_failed "$current_on"
current="$(combine "$current_off" "$current_on")"

# Anything present now but absent from the baseline is new.
#
# grep -Fxv rather than comm: comm requires both inputs to be sorted in the
# *same* collation it uses, and silently emits "file 1 is not in sorted order"
# plus garbage results when they disagree -- which is exactly what happened
# between a darwin-generated baseline and the CI runner. A set-membership test
# has no ordering requirement at all.
new="$(printf '%s\n' "$current" | grep -Fxv -f "$BASELINE" || true)"

if [ -n "$new" ]; then
  echo "ERROR: new code that only tests reference:" >&2
  echo >&2
  printf '%s\n' "$new" | sed 's/^/  /' >&2
  echo >&2
  echo "Either wire it into production, delete it, or -- if it is genuinely" >&2
  echo "meant to be called only from tests -- add it to $BASELINE with" >&2
  echo "  scripts/check-test-only-code.sh --update" >&2
  echo "and say why in the commit message." >&2
  exit 1
fi

# And the other direction: a grant that covers nothing. cleat#1746.
stale="$(stale_entries "$current")"

if [ -n "$stale" ]; then
  echo "ERROR: $BASELINE lists entries the scan no longer reports:" >&2
  echo >&2
  printf '%s\n' "$stale" | sed 's/^/  /' >&2
  echo >&2
  echo "Each of these is a standing grant to be called only from tests, sitting" >&2
  echo "on a symbol that is no longer test-only. Nothing is wrong with the tree;" >&2
  echo "the baseline is describing a state that has been fixed. Left in place the" >&2
  echo "grant outlives what it granted, and the same code going test-only again" >&2
  echo "would not fail this guard." >&2
  echo >&2
  echo "Refresh the baseline with:" >&2
  echo "  scripts/check-test-only-code.sh --update" >&2
  echo "Rebase FIRST -- a regeneration from a stale base reinstates whatever the" >&2
  echo "other side removed, and merges cleanly doing it (WORKSTREAM.md R6a)." >&2
  exit 1
fi

# BOTH counts, because they are the two sets that were just compared and this
# line used to print one of them under the other's name: it reported
# `$current | grep -c .` as "known entries in the baseline". On a clean tree the
# two agree, so the label was never wrong where anyone looked. On develop today
# it printed 52 against a 60-line file -- the guard stating the size of its own
# blind spot, in the reassuring direction, and exiting 0.
echo "OK: no new test-only code ($(grep -c . "$BASELINE") baseline entries, $(printf '%s\n' "$current" | grep -c . ) reported by the scan, none stale)."
